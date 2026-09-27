package tunnel

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// Hub keeps the active multiplexed connection for each authenticated node.
type Hub struct {
	mu       sync.RWMutex
	sessions map[string]*hubSession

	OnConnect    func(nodeID string)
	OnDisconnect func(nodeID string)
}

type hubSession struct {
	session *yamux.Session
}

// Handler accepts authenticated agent WebSocket connections and serves their
// yamux streams as HTTP requests.
func (h *Hub) Handler(authn func(token string) (nodeID string, ok bool)) http.Handler {
	h.mu.Lock()
	if h.sessions == nil {
		h.sessions = make(map[string]*hubSession)
	}
	h.mu.Unlock()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authn == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		nodeID, ok := authn(strings.TrimPrefix(header, "Bearer "))
		if !ok || nodeID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
		session, err := yamux.Client(conn, nil)
		if err != nil {
			_ = ws.Close(websocket.StatusInternalError, "tunnel setup failed")
			return
		}

		entry := &hubSession{session: session}
		h.mu.Lock()
		previous := h.sessions[nodeID]
		h.sessions[nodeID] = entry
		h.mu.Unlock()
		if previous != nil {
			_ = previous.session.Close()
		}
		if h.OnConnect != nil {
			h.OnConnect(nodeID)
		}

		<-session.CloseChan()
		_ = session.Close()
		h.mu.Lock()
		if h.sessions[nodeID] == entry {
			delete(h.sessions, nodeID)
			h.mu.Unlock()
			if h.OnDisconnect != nil {
				h.OnDisconnect(nodeID)
			}
			return
		}
		h.mu.Unlock()
	})
}

// Client returns an HTTP client that sends each request over a new yamux
// stream to the connected node.
func (h *Hub) Client(nodeID string) (*http.Client, bool) {
	h.mu.RLock()
	entry := h.sessions[nodeID]
	h.mu.RUnlock()
	if entry == nil || entry.session.IsClosed() {
		return nil, false
	}

	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return entry.session.Open()
		},
	}
	return &http.Client{Transport: transport}, true
}

// Connected returns the IDs of nodes with active tunnel sessions.
func (h *Hub) Connected() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	connected := make([]string, 0, len(h.sessions))
	for nodeID, entry := range h.sessions {
		if !entry.session.IsClosed() {
			connected = append(connected, nodeID)
		}
	}
	return connected
}
