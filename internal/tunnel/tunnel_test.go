package tunnel

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestAgentHTTPRequestsTravelThroughTunnel(t *testing.T) {
	hub := &Hub{}
	hubHandler := hub.Handler(func(token string) (string, bool) {
		return "nd_test", token == "node-secret"
	})
	hubServer := httptest.NewServer(hubHandler)
	defer hubServer.Close()

	agentHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"nodeId":"nd_test","status":"healthy"}`)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunForever(ctx, websocketURL(hubServer.URL), "node-secret", agentHandler)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("RunForever did not stop after context cancellation")
		}
	})

	client := waitForClient(t, hub, "nd_test")
	response, err := client.Get("http://agent/v1/health")
	if err != nil {
		t.Fatalf("GET through tunnel: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var health struct {
		NodeID string `json:"nodeId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if health.NodeID != "nd_test" {
		t.Fatalf("nodeId = %q, want nd_test", health.NodeID)
	}
}

func TestWebSocketUpgradeTravelsThroughTunnel(t *testing.T) {
	hub := &Hub{}
	hubServer := httptest.NewServer(hub.Handler(func(token string) (string, bool) { return "nd_test", token == "node-secret" }))
	defer hubServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunForever(ctx, websocketURL(hubServer.URL), "node-secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			for {
				typ, message, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				if err := conn.Write(r.Context(), typ, message); err != nil {
					return
				}
			}
		}))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("RunForever did not stop after context cancellation")
		}
	})

	client := waitForClient(t, hub, "nd_test")
	conn, _, err := websocket.Dial(context.Background(), "http://agent/echo", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatalf("WebSocket dial through tunnel: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	if err := conn.Write(context.Background(), websocket.MessageText, []byte("tunnel websocket")); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	_, got, err := conn.Read(context.Background())
	if err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if string(got) != "tunnel websocket" {
		t.Fatalf("echo = %q, want %q", got, "tunnel websocket")
	}
}

func TestRunForeverReconnectsAfterTunnelDrops(t *testing.T) {
	var connects atomic.Int32
	var disconnects atomic.Int32
	hub := &Hub{}
	hubHandler := hub.Handler(func(token string) (string, bool) { return "nd_test", token == "node-secret" })
	hub.OnConnect = func(string) { connects.Add(1) }
	hub.OnDisconnect = func(string) { disconnects.Add(1) }
	hubServer := httptest.NewServer(hubHandler)
	defer hubServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunForever(ctx, websocketURL(hubServer.URL), "node-secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("RunForever did not stop after context cancellation")
		}
	})

	waitForClient(t, hub, "nd_test")
	closeNodeSession(t, hub, "nd_test")
	waitForCondition(t, "agent to reconnect", func() bool {
		return connects.Load() >= 2 && hubHasClient(hub, "nd_test")
	})
	waitForCondition(t, "disconnect callback", func() bool { return disconnects.Load() >= 1 })
}

func TestRunForeverReconnectsAfterServerClosesTunnelWithOpenStreams(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)

	var connects atomic.Int32
	var handlers atomic.Int32
	var serverConn atomic.Value
	hub := &Hub{}
	hubHandler := hub.Handler(func(token string) (string, bool) { return "nd_busy", token == "secret" })
	hub.OnConnect = func(string) { connects.Add(1) }
	hubServer := httptest.NewUnstartedServer(hubHandler)
	hubServer.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateHijacked {
			serverConn.Store(conn)
		}
	}
	hubServer.Start()
	defer hubServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunForever(ctx, websocketURL(hubServer.URL), "secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlers.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
		}))
	}()

	client := waitForClient(t, hub, "nd_busy")
	const streamCount = 10
	var bodyWriters []*io.PipeWriter
	for i := 0; i < streamCount; i++ {
		bodyReader, bodyWriter := io.Pipe()
		bodyWriters = append(bodyWriters, bodyWriter)
		go func() {
			response, err := client.Post("http://agent/hold", "application/octet-stream", bodyReader)
			if err == nil {
				_ = response.Body.Close()
			}
		}()
	}
	t.Cleanup(func() {
		for _, writer := range bodyWriters {
			_ = writer.Close()
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("RunForever did not stop after context cancellation")
		}
	})

	waitForCondition(t, "all tunnel handlers to hold open streams", func() bool { return handlers.Load() >= streamCount })
	for _, writer := range bodyWriters {
		if _, err := writer.Write([]byte("x")); err != nil {
			t.Fatalf("write to held request body: %v", err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	conn, ok := serverConn.Load().(net.Conn)
	if !ok {
		t.Fatal("hub did not expose the hijacked websocket connection")
	}
	_ = conn.Close()
	waitForCondition(t, "agent to reconnect after server closed tunnel", func() bool {
		return connects.Load() >= 2 && hubHasClient(hub, "nd_busy")
	})
}

func websocketURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func waitForClient(t *testing.T, hub *Hub, nodeID string) *http.Client {
	t.Helper()
	var client *http.Client
	waitForCondition(t, "agent connection", func() bool {
		var ok bool
		client, ok = hub.Client(nodeID)
		return ok
	})
	return client
}

func hubHasClient(hub *Hub, nodeID string) bool {
	_, ok := hub.Client(nodeID)
	return ok
}

func waitForCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func closeNodeSession(t *testing.T, hub *Hub, nodeID string) {
	t.Helper()
	hub.mu.RLock()
	entry := hub.sessions[nodeID]
	hub.mu.RUnlock()
	if entry == nil {
		t.Fatal("node has no active session")
	}
	if err := entry.session.Close(); err != nil {
		t.Fatalf("close node session: %v", err)
	}
}

func TestLargeBodiesTravelThroughTunnel(t *testing.T) {
	hub := &Hub{}
	hubServer := httptest.NewServer(hub.Handler(func(token string) (string, bool) { return "nd_big", true }))
	defer hubServer.Close()
	const size = 8 << 20
	agentHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, strings.Repeat("x", int(n)))
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = RunForever(ctx, websocketURL(hubServer.URL), "t", agentHandler) }()
	client := waitForClient(t, hub, "nd_big")
	response, err := client.Post("http://agent/echo", "application/octet-stream", strings.NewReader(strings.Repeat("y", size)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer response.Body.Close()
	n, err := io.Copy(io.Discard, response.Body)
	if err != nil || n != size {
		t.Fatalf("read %d bytes, err %v; want %d", n, err, size)
	}
}
