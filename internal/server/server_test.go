package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/tunnel"
	"github.com/boxes-oss/boxes/internal/types"
	"github.com/coder/websocket"
)

func TestSignupAndLoginIssueBearerSessions(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "owner@example.test", "password": "correct horse battery staple",
	}, http.StatusCreated, nil)

	var login map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/login", "", map[string]any{
		"email": "owner@example.test", "password": "correct horse battery staple",
	}, http.StatusOK, &login)
	token, _ := login["token"].(string)
	if token == "" {
		t.Fatal("login response has no bearer token")
	}
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/me", token, nil, http.StatusOK, nil)
}

func TestHandlerReturnsJSONNotFoundForRegistryPaths(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	for _, path := range []string{"/v2", "/v2/", "/v2/library/image/manifests/latest"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()
			srv.Handler().ServeHTTP(response, request)

			if response.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
			}
			if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			if strings.Contains(response.Body.String(), "<html") || strings.Contains(response.Body.String(), "Boxes") {
				t.Fatalf("registry path returned web UI content: %q", response.Body.String())
			}
		})
	}
}

func TestRuntimeAdminRequiresLocalAdminToken(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.APIHandler())
	defer httpServer.Close()

	url := httpServer.URL + "/admin/runtime/stop"
	response := doJSONRequest(t, httpServer.Client(), http.MethodPost, url, nil, map[string]any{})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated runtime stop status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode runtime stop response: %v", err)
	}
	response.Body.Close()
	if body["error"] == nil {
		t.Fatalf("runtime stop response has no JSON error: %#v", body)
	}

	response = doJSONRequest(t, httpServer.Client(), http.MethodPost, url, map[string]string{
		"Authorization": "Bearer " + srv.adminToken,
	}, map[string]any{})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated runtime stop status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode authenticated runtime stop response: %v", err)
	}
	if stopped, ok := body["stopped"].([]any); !ok || len(stopped) != 0 {
		t.Fatalf("stopped boxes = %#v, want an empty list", body["stopped"])
	}
}

func TestAuthResponsesSetSessionCookie(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	response := doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", nil, map[string]any{
		"email": "owner@example.test", "password": "correct horse battery staple",
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("signup status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	response.Body.Close()

	response = doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/login", map[string]string{
		"X-Forwarded-Proto": "https",
	}, map[string]any{"email": "owner@example.test", "password": "correct horse battery staple"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	cookie := sessionCookie(t, response)
	if cookie.Value != result["token"] {
		t.Fatalf("session cookie token = %q, want JSON token %q", cookie.Value, result["token"])
	}
	if cookie.Path != "/" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || !cookie.Secure || cookie.MaxAge != int(sessionLifetime.Seconds()) {
		t.Fatalf("session cookie attributes = %#v, want Path=/ HttpOnly SameSite=Lax Secure Max-Age=%d", cookie, int(sessionLifetime.Seconds()))
	}
}

func TestCookieSessionAuthenticationCSRFAndLogout(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	signup := doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", nil, map[string]any{
		"email": "owner@example.test", "password": "correct horse battery staple",
	})
	cookie := sessionCookie(t, signup)
	signup.Body.Close()
	cookieHeader := map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}

	me := doJSONRequest(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/me", cookieHeader, nil)
	if me.StatusCode != http.StatusOK {
		t.Fatalf("cookie-authenticated /me status = %d, want %d", me.StatusCode, http.StatusOK)
	}
	me.Body.Close()

	unsafe := doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/api-keys", cookieHeader, map[string]any{"name": "csrf test"})
	assertErrorCode(t, unsafe, http.StatusForbidden, "csrf_failed")
	unsafe.Body.Close()

	crossOriginHeaders := map[string]string{
		"Cookie": cookie.Name + "=" + cookie.Value,
		"Origin": "https://attacker.example.test",
	}
	unsafe = doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/api-keys", crossOriginHeaders, map[string]any{"name": "cross origin"})
	assertErrorCode(t, unsafe, http.StatusForbidden, "csrf_failed")
	unsafe.Body.Close()

	sameOriginHeaders := map[string]string{
		"Cookie": cookie.Name + "=" + cookie.Value,
		"Origin": httpServer.URL,
	}
	createdKey := doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/api-keys", sameOriginHeaders, map[string]any{"name": "same origin"})
	if createdKey.StatusCode != http.StatusCreated {
		bytes, _ := io.ReadAll(createdKey.Body)
		t.Fatalf("same-origin cookie-authenticated POST status = %d, want %d; body: %s", createdKey.StatusCode, http.StatusCreated, bytes)
	}
	createdKey.Body.Close()

	logoutHeaders := map[string]string{
		"Cookie": cookie.Name + "=" + cookie.Value,
		"Origin": httpServer.URL,
	}
	logout := doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/logout", logoutHeaders, nil)
	if logout.StatusCode != http.StatusNoContent {
		bytes, _ := io.ReadAll(logout.Body)
		t.Fatalf("logout status = %d, want %d; body: %s", logout.StatusCode, http.StatusNoContent, bytes)
	}
	cleared := sessionCookie(t, logout)
	if cleared.Value != "" || cleared.MaxAge != -1 || cleared.Path != "/" || !strings.Contains(logout.Header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout cookie = %#v, want empty value, Path=/, Max-Age=0", cleared)
	}
	logout.Body.Close()

	me = doJSONRequest(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/me", cookieHeader, nil)
	assertErrorCode(t, me, http.StatusUnauthorized, "unauthorized")
	me.Body.Close()
}

func TestAPIKeyInSessionCookieIsRejected(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	var signup map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "owner@example.test", "password": "correct horse battery staple",
	}, http.StatusCreated, &signup)
	var created map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/api-keys", signup["token"].(string), map[string]any{"name": "cookie must reject"}, http.StatusCreated, &created)
	key, _ := created["apiKey"].(map[string]any)
	keyToken, _ := key["key"].(string)
	if keyToken == "" {
		t.Fatalf("API key response has no key: %#v", created)
	}

	response := doJSONRequest(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/me", map[string]string{"Cookie": "boxes_session=" + keyToken}, nil)
	assertErrorCode(t, response, http.StatusUnauthorized, "unauthorized")
	response.Body.Close()
}

func TestAPIKeyAuthenticatesEveryCLISandboxRoute(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	var signup map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "owner@example.test", "password": "correct horse battery staple",
	}, http.StatusCreated, &signup)
	var created map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/api-keys", signup["token"].(string), map[string]any{"name": "CLI"}, http.StatusCreated, &created)
	apiKey := created["apiKey"].(map[string]any)["key"].(string)

	routes := []struct {
		name, method, path string
		body               any
	}{
		{"exec", http.MethodPost, "/sandboxes/bx_missing/commands", map[string]any{"cmd": "true"}},
		{"files get", http.MethodGet, "/sandboxes/bx_missing/files?path=/tmp/file", nil},
		{"files put", http.MethodPut, "/sandboxes/bx_missing/files?path=/tmp/file", map[string]any{"content": "test"}},
		{"prompt", http.MethodPost, "/sandboxes/bx_missing/prompt", map[string]any{"prompt": "hello", "provider": "claude"}},
		{"events", http.MethodGet, "/sandboxes/bx_missing/events?after=0", nil},
		{"ssh-key", http.MethodPost, "/sandboxes/bx_missing/ssh-key", map[string]any{"publicKey": "ssh-ed25519 invalid"}},
		{"desktop", http.MethodPost, "/sandboxes/bx_missing/desktop", map[string]any{}},
		{"hosts", http.MethodGet, "/sandboxes/bx_missing/hosts", nil},
		{"hosts create", http.MethodPost, "/sandboxes/bx_missing/hosts", map[string]any{"port": 8080}},
		{"stop", http.MethodPost, "/sandboxes/bx_missing/stop", nil},
		{"resume", http.MethodPost, "/sandboxes/bx_missing/resume", map[string]any{}},
		{"fork", http.MethodPost, "/sandboxes/bx_missing/fork", map[string]any{}},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			response := doJSONRequest(t, httpServer.Client(), route.method, httpServer.URL+"/api/v1"+route.path, map[string]string{
				"Authorization": "Bearer " + apiKey,
			}, route.body)
			defer response.Body.Close()
			if response.StatusCode == http.StatusUnauthorized {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("API key was rejected with 401: %s", body)
			}
		})
	}
}

func TestAPIKeyAuthenticatesTCPProxyWebSocket(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	var signup map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "owner@example.test", "password": "correct horse battery staple",
	}, http.StatusCreated, &signup)
	var created map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/api-keys", signup["token"].(string), map[string]any{"name": "CLI TCP"}, http.StatusCreated, &created)
	apiKey := created["apiKey"].(map[string]any)["key"].(string)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/api/v1/sandboxes/bx_missing/tcp/22"
	_, response, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPClient: httpServer.Client(),
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + apiKey}},
	})
	if err == nil {
		t.Fatal("WebSocket unexpectedly connected to a missing sandbox")
	}
	if response == nil {
		t.Fatalf("WebSocket response is nil: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("API-key WebSocket status = %d, want 404 after authentication; body: %s", response.StatusCode, body)
	}
}

func TestCookieCSRFAllowsSameOriginReferer(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	response := doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", nil, map[string]any{
		"email": "owner@example.test", "password": "correct horse battery staple",
	})
	cookie := sessionCookie(t, response)
	response.Body.Close()

	response = doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/api-keys", map[string]string{
		"Cookie":  cookie.Name + "=" + cookie.Value,
		"Referer": httpServer.URL + "/settings/",
	}, map[string]any{"name": "same referer"})
	if response.StatusCode != http.StatusCreated {
		bytes, _ := io.ReadAll(response.Body)
		t.Fatalf("same-origin Referer POST status = %d, want %d; body: %s", response.StatusCode, http.StatusCreated, bytes)
	}
	response.Body.Close()
}

func TestSignupRejectsPasswordsShorterThanTwelveCharacters(t *testing.T) {
	for _, password := range []string{"12345678901", "ééééééééééé"} {
		t.Run(password, func(t *testing.T) {
			srv, cleanup := newTestServer(t)
			defer cleanup()
			httpServer := httptest.NewServer(srv.Handler())
			defer httpServer.Close()

			var response map[string]any
			requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
				"email": "owner@example.test", "password": password,
			}, http.StatusBadRequest, &response)
			errorValue, _ := response["error"].(map[string]any)
			if errorValue["code"] != "weak_password" {
				t.Fatalf("error code = %v, want weak_password", errorValue["code"])
			}
		})
	}
}

func TestSignupAcceptsTwelveCharacterPassword(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "owner@example.test", "password": "123456789012",
	}, http.StatusCreated, nil)
}

func TestInstallAssetsAreServedPublicly(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	for _, asset := range []struct {
		path    string
		content string
	}{
		{path: "/install/node-install.sh", content: "#!/usr/bin/env bash"},
		{path: "/install/traefik.yml", content: "certificatesResolvers:"},
	} {
		t.Run(asset.path, func(t *testing.T) {
			response, err := httpServer.Client().Get(httpServer.URL + asset.path)
			if err != nil {
				t.Fatalf("get install asset: %v", err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d", asset.path, response.StatusCode, http.StatusOK)
			}
			if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
				t.Fatalf("GET %s Content-Type = %q, want text/plain", asset.path, contentType)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read install asset: %v", err)
			}
			if !strings.Contains(string(body), asset.content) {
				t.Fatalf("GET %s body does not contain %q", asset.path, asset.content)
			}
		})
	}
}

func TestAgentBinariesAreServedFromDataDirectory(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	binDir := filepath.Join(srv.cfg.DataDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("agent-binary-content")
	if err := os.WriteFile(filepath.Join(binDir, "boxes-agent-linux-amd64"), want, 0o755); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	tests := []struct {
		path       string
		statusCode int
		body       string
	}{
		{path: "/install/agent/boxes-agent-linux-amd64", statusCode: http.StatusOK, body: string(want)},
		{path: "/install/agent/boxes-agent-linux-arm64", statusCode: http.StatusNotFound, body: "Agent binary boxes-agent-linux-arm64 is not installed"},
		{path: "/install/agent/boxes-agent-linux-amd64/extra", statusCode: http.StatusNotFound},
		{path: "/install/agent/boxes-agent-darwin-amd64", statusCode: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			response, err := httpServer.Client().Get(httpServer.URL + test.path)
			if err != nil {
				t.Fatalf("GET %s: %v", test.path, err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			if response.StatusCode != test.statusCode {
				t.Fatalf("GET %s status = %d, want %d; body: %s", test.path, response.StatusCode, test.statusCode, body)
			}
			if test.body != "" && !strings.Contains(string(body), test.body) {
				t.Fatalf("GET %s body %q does not contain %q", test.path, body, test.body)
			}
			if response.StatusCode == http.StatusOK && string(body) != string(want) {
				t.Fatalf("agent body = %q, want %q", body, want)
			}
		})
	}
}

func TestNodeInstallCommandUsesSecureTunnelForHTTPSPublicURL(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	srv.cfg.PublicURL = "https://boxes.example.test/"
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	var signup map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "owner@example.test", "password": "correct horse battery staple",
	}, http.StatusCreated, &signup)
	token, _ := signup["token"].(string)
	var join map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/nodes", token, map[string]any{"name": "worker"}, http.StatusCreated, &join)

	installCommand, _ := join["installCommand"].(string)
	if !strings.HasPrefix(installCommand, "curl -fsSL 'https://boxes.example.test/install/node-install.sh' | sudo env ") || !strings.Contains(installCommand, "BOXES_SERVER_URL='wss://boxes.example.test/api/v1/agent/connect'") || !strings.Contains(installCommand, "BOXES_PUBLIC_URL='https://boxes.example.test'") || !strings.Contains(installCommand, "BOXES_NODE_TOKEN='") {
		t.Fatalf("install command does not use the secure tunnel endpoint: %q", installCommand)
	}
}

func TestSignupAfterBootstrapRequiresOwnerBearer(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	var owner map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "owner@example.test", "password": "correct horse battery staple",
	}, http.StatusCreated, &owner)
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "intruder@example.test", "password": "correct horse battery staple",
	}, http.StatusForbidden, nil)
	token, _ := owner["token"].(string)
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", token, map[string]any{
		"email": "member@example.test", "password": "correct horse battery staple",
	}, http.StatusCreated, nil)
}

func TestNodeJoinAndSandboxProvisioningUseEncryptedSecretsAndIdempotency(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	var signup map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "owner@example.test", "password": "correct horse battery staple",
	}, http.StatusCreated, &signup)
	token, _ := signup["token"].(string)

	var join map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/nodes", token, map[string]any{
		"name": "worker", "hostBase": "worker.example.test",
	}, http.StatusCreated, &join)
	joinToken, _ := join["joinToken"].(string)
	if joinToken == "" {
		t.Fatal("node response has no one-time join token")
	}
	nodeValue, _ := join["node"].(map[string]any)
	nodeID, _ := nodeValue["id"].(string)
	if nodeID == "" {
		t.Fatalf("node response has no node id: %#v", join)
	}
	nodeRecord, err := srv.store.GetNode(context.Background(), nodeID)
	if err != nil || nodeRecord.TokenHash != tokenHash(joinToken) || nodeRecord.TokenHash == joinToken {
		t.Fatalf("node token hash = %q, err = %v", nodeRecord.TokenHash, err)
	}
	installCommand, _ := join["installCommand"].(string)
	wantInstallCommand := nodeInstallCommand("http://boxes.example.test", "http://boxes.example.test/install/node-install.sh", "ws://boxes.example.test/api/v1/agent/connect", joinToken, srv.storageConfig(), srv.registryConfig(), srv.defaultImages())
	if installCommand != wantInstallCommand {
		t.Fatalf("install command = %q, want %q", installCommand, wantInstallCommand)
	}

	restoredFrom := make(chan string, 1)
	runtimeValues := make(chan string, 2)
	startCalls := make(chan struct{}, 1)
	containerEvents := make(chan string, 4)
	pumpRequests := make(chan string, 128)
	var failNextStop atomic.Bool
	var failNextCreate atomic.Bool
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
			_, _ = io.WriteString(w, `{"cpus":8,"memoryBytes":7516192768,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":7516192768,"diskFreeBytes":100000000000,"runtime":"runc","boxes":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode agent create request: %v", err)
			}
			boxID, _ := body["boxId"].(string)
			boxRecord, stateErr := srv.store.GetBox(context.Background(), boxID)
			wantState := types.StateProvisioning
			if body["fromSnapshot"] != nil {
				wantState = types.StateResuming
			}
			if stateErr != nil || boxRecord.Box.State != wantState {
				t.Errorf("box state at agent create = %q, err = %v; want %q", boxRecord.Box.State, stateErr, wantState)
			}
			if failNextCreate.Swap(false) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":{"code":"box_not_ready","message":"boxd did not become ready"}}`)
				return
			}
			if body["desktopPassword"] == "" || body["boxdToken"] == "" {
				t.Errorf("agent create request is missing box credentials: %#v", body)
			}
			runtime, _ := body["runtime"].(string)
			runtimeValues <- runtime
			if value, ok := body["fromSnapshot"].(string); ok {
				restoredFrom <- value
				containerEvents <- "restore"
			} else {
				containerEvents <- "create"
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"containerId":"container-1"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			var request struct {
				Snapshot bool `json:"snapshot"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode stop request: %v", err)
			}
			if !request.Snapshot {
				t.Error("server did not request a snapshot from the agent")
			}
			if failNextStop.Swap(false) {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":{"code":"snapshot_failed","message":"capture failed"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"snapshot":{"snapshotId":"restic-latest","sizeBytes":123,"createdAt":"2026-09-24T12:00:00Z"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/snapshot"):
			_, _ = io.WriteString(w, `{"snapshotId":"fork-snapshot","sizeBytes":123,"createdAt":"2026-09-24T12:00:00Z"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			startCalls <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/boxes/"):
			containerEvents <- "delete"
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/boxd/events"):
			boxID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/boxes/"), "/boxd/events")
			select {
			case pumpRequests <- boxID:
			default:
			}
			time.Sleep(20 * time.Millisecond)
			_, _ = io.WriteString(w, `{"events":[],"cursor":0}`)
		default:
			http.NotFound(w, r)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tunnel.Dial(ctx, websocketURL(httpServer.URL)+"/agent/connect", joinToken, agent) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent tunnel did not stop after cancellation")
		}
	})
	waitFor(t, func() bool {
		_, ok := srv.hub.Client(nodeID)
		return ok
	})

	body := map[string]any{"type": "small", "ttlSeconds": 0}
	var created map[string]any
	requestJSONWithHeaders(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes", token, "request-1", body, http.StatusCreated, &created)
	boxID, _ := created["id"].(string)
	if boxID == "" || created["state"] != string(types.StateReady) || created["ttlSeconds"] != nil || created["autoStop"] != false {
		t.Fatalf("created sandbox = %#v", created)
	}

	removeReservations := addCapacityReservations(t, srv, nodeID)
	capacityMessage := "no node has room for a small box"
	forkCapacityMessage := "no node has room for a small box"
	noCapacityCreateResponse := doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes", map[string]string{
		"Authorization": "Bearer " + token,
	}, map[string]any{"type": "small"})
	assertErrorMessage(t, noCapacityCreateResponse, http.StatusConflict, "no_capacity", capacityMessage)
	noCapacityCreateResponse.Body.Close()

	forkFailure := doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/fork", map[string]string{
		"Authorization": "Bearer " + token,
	}, map[string]any{})
	assertErrorMessage(t, forkFailure, http.StatusConflict, "no_capacity", capacityMessage)
	forkFailure.Body.Close()
	forkSnapshots, err := srv.store.ListSnapshots(context.Background(), boxID)
	if err != nil {
		t.Fatalf("list snapshots captured for capacity fork: %v", err)
	}
	for _, snapshot := range forkSnapshots {
		if snapshot.ResticID == "fork-snapshot" {
			if err := srv.store.DeleteSnapshot(context.Background(), snapshot.ID); err != nil {
				t.Fatalf("delete capacity-fork snapshot: %v", err)
			}
		}
	}
	removeReservations()

	waitForPump := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case got := <-pumpRequests:
				if got == want {
					return
				}
			case <-deadline:
				t.Fatalf("event pump did not request events for %s", want)
			}
		}
	}
	waitForPump(boxID)
	if event := <-containerEvents; event != "create" {
		t.Fatalf("initial container event = %q, want create", event)
	}
	if runtime := <-runtimeValues; runtime != "" {
		t.Errorf("server forced runtime %q; the agent's configured runtime should be used", runtime)
	}
	var repeated map[string]any
	requestJSONWithHeaders(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes", token, "request-1", body, http.StatusCreated, &repeated)
	if repeated["id"] != boxID {
		t.Fatalf("idempotent create id = %v, want %s", repeated["id"], boxID)
	}
	failNextStop.Store(true)
	var stopFailure map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/stop", token,
		nil, http.StatusConflict, &stopFailure)
	if errorBody, _ := stopFailure["error"].(map[string]any); errorBody["code"] != "snapshot_failed" || !strings.Contains(errorBody["message"].(string), "agent snapshot_failed: capture failed") {
		t.Fatalf("stop failure = %#v, want snapshot_failed", stopFailure)
	}
	boxAfterFailure, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil || boxAfterFailure.Box.State != types.StateReady {
		t.Fatalf("box after failed capture = %#v, err = %v; want ready", boxAfterFailure.Box, err)
	}
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/resume", token,
		map[string]any{}, http.StatusConflict, nil)
	select {
	case <-startCalls:
		t.Fatal("resume started the previous container without a snapshot")
	default:
	}
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/stop", token,
		nil, http.StatusOK, nil)
	removeReservations = addCapacityReservations(t, srv, nodeID)
	resumeCapacityFailure := doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/resume", map[string]string{
		"Authorization": "Bearer " + token,
	}, map[string]any{})
	assertErrorMessage(t, resumeCapacityFailure, http.StatusConflict, "no_capacity", forkCapacityMessage)
	resumeCapacityFailure.Body.Close()
	removeReservations()
	if !srv.beginSnapshot(boxID) {
		t.Fatal("could not hold snapshot slot for resume regression")
	}
	resumeDone := make(chan *http.Response, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/resume", strings.NewReader(`{"ttlSeconds":0}`))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := httpServer.Client().Do(request)
		if err != nil {
			resumeDone <- nil
			return
		}
		resumeDone <- response
	}()
	select {
	case snapshotID := <-restoredFrom:
		t.Fatalf("resume used snapshot %q before the active snapshot operation finished", snapshotID)
	case <-time.After(50 * time.Millisecond):
	}
	srv.endSnapshot(boxID)
	resumeResponse := <-resumeDone
	if resumeResponse == nil {
		t.Fatal("resume request failed")
	}
	defer resumeResponse.Body.Close()
	if resumeResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resumeResponse.Body)
		t.Fatalf("resume status = %d: %s", resumeResponse.StatusCode, body)
	}
	var resumed map[string]any
	if err := json.NewDecoder(resumeResponse.Body).Decode(&resumed); err != nil {
		t.Fatalf("decode resume response: %v", err)
	}
	waitForPump(boxID)
	if sandbox, _ := resumed["sandbox"].(map[string]any); sandbox["state"] != string(types.StateReady) {
		t.Fatalf("resumed sandbox = %#v", resumed)
	} else if sandbox["ttlSeconds"] != nil || sandbox["autoStop"] != false {
		t.Fatalf("resume without ttlSeconds did not preserve the disabled timer: %#v", sandbox)
	}
	select {
	case snapshotID := <-restoredFrom:
		if snapshotID != "restic-latest" {
			t.Fatalf("resume used snapshot %q, want restic-latest", snapshotID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not create a new container from its latest snapshot")
	}
	if runtime := <-runtimeValues; runtime != "" {
		t.Errorf("server forced runtime %q during resume; the agent's configured runtime should be used", runtime)
	}
	if event := <-containerEvents; event != "delete" {
		t.Fatalf("resume container event = %q, want deletion of the old container", event)
	}
	if event := <-containerEvents; event != "restore" {
		t.Fatalf("resume container event = %q, want creation from the snapshot", event)
	}
	record, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil {
		t.Fatalf("load persisted box: %v", err)
	}
	if len(record.DesktopPasswordEnc) == 0 || len(record.BoxdTokenEnc) == 0 || strings.Contains(string(record.DesktopPasswordEnc), "password") || strings.Contains(string(record.BoxdTokenEnc), "token") {
		t.Fatalf("box secrets were not encrypted: %#v", record)
	}
	password, err := srv.secrets.decrypt(record.DesktopPasswordEnc)
	if err != nil || len(password) == 0 {
		t.Fatalf("decrypt desktop password: %v", err)
	}
	boxdToken, err := srv.secrets.decrypt(record.BoxdTokenEnc)
	if err != nil || len(boxdToken) == 0 {
		t.Fatalf("decrypt boxd token: %v", err)
	}
	failNextCreate.Store(true)
	var createFailure map[string]any
	requestJSONWithHeaders(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes", token, "request-not-ready", body, http.StatusServiceUnavailable, &createFailure)
	if errorValue, _ := createFailure["error"].(map[string]any); errorValue["code"] != "box_not_ready" {
		t.Fatalf("create failure = %#v, want box_not_ready", createFailure)
	}
}

func TestTunnelConnectFencesBoxReportedByAnotherNode(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	var signup map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "owner@example.test", "password": "correct horse battery staple",
	}, http.StatusCreated, &signup)
	token, _ := signup["token"].(string)
	var join map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/nodes", token, map[string]any{"name": "worker"}, http.StatusCreated, &join)
	joinToken, _ := join["joinToken"].(string)
	nodeValue, _ := join["node"].(map[string]any)
	nodeID, _ := nodeValue["id"].(string)
	otherNode := types.Node{ID: "nd_old", Name: "old", Status: "offline"}
	if err := srv.store.CreateNode(context.Background(), store.NodeRecord{Node: otherNode, TokenHash: "other-hash"}); err != nil {
		t.Fatalf("create foreign node: %v", err)
	}
	const foreignBoxID = "bx_foreignbox12"
	if err := srv.store.CreateBox(context.Background(), store.BoxRecord{Box: types.Box{ID: foreignBoxID, NodeID: otherNode.ID, Type: "small", State: types.StateReady, Image: "boxes/test", CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatalf("create foreign box: %v", err)
	}
	deleted := make(chan string, 1)
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/health" {
			_, _ = io.WriteString(w, `{"nodeId":"`+otherNode.ID+`","cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":1000,"boxes":[{"boxId":"`+foreignBoxID+`","containerId":"old-container","state":"running"}]}`)
			return
		}
		if r.Method == http.MethodDelete && r.URL.Path == "/v1/boxes/"+foreignBoxID {
			deleted <- r.URL.Path
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tunnel.Dial(ctx, websocketURL(httpServer.URL)+"/agent/connect", joinToken, agent) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent tunnel did not stop after cancellation")
		}
	})
	select {
	case path := <-deleted:
		t.Fatalf("fenced offline node box at %q", path)
	case <-time.After(250 * time.Millisecond):
	}
	connectedNode, err := srv.store.GetNode(context.Background(), nodeID)
	if err != nil || connectedNode.Node.Status != "healthy" {
		t.Fatalf("token-authenticated node status = %q, err = %v; want healthy", connectedNode.Node.Status, err)
	}
	reportedNode, err := srv.store.GetNode(context.Background(), otherNode.ID)
	if err != nil || reportedNode.Node.Status != "offline" {
		t.Fatalf("agent-reported node status = %q, err = %v; want offline", reportedNode.Node.Status, err)
	}
}

func TestProtectedEndpointRejectsMissingBearerToken(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/nodes", "", nil, http.StatusUnauthorized, nil)
}

func TestUndrainNodeIsOwnerOnlyAndClearsDrainedState(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ownerToken := addPlatformSession(t, srv)
	member := types.User{ID: "usr_node_member", Email: "node-member@example.test", Role: "user", PasswordHash: "unused", CreatedAt: time.Now().UTC()}
	if err := srv.store.CreateUser(context.Background(), member); err != nil {
		t.Fatal(err)
	}
	memberToken := "node-member-session"
	if err := srv.store.CreateSession(context.Background(), store.Session{ID: tokenHash(memberToken), UserID: member.ID, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	node := types.Node{ID: "nd_undrain", Name: "drained-node", Status: "healthy", Drained: true}
	if err := srv.store.CreateNode(context.Background(), store.NodeRecord{Node: node}); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	endpoint := httpServer.URL + "/api/v1/nodes/" + node.ID + "/undrain"

	requestJSON(t, httpServer.Client(), http.MethodPost, endpoint, memberToken, map[string]any{}, http.StatusForbidden, nil)

	var result map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, endpoint, ownerToken, map[string]any{}, http.StatusOK, &result)
	returnedNode := result["node"].(map[string]any)
	if returnedNode["drained"] != false {
		t.Fatalf("undrain response drained = %#v, want false", returnedNode["drained"])
	}
	stored, err := srv.store.GetNode(context.Background(), node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Node.Drained {
		t.Fatal("undrain left node drained in storage")
	}
}

func TestBootstrapNodeUsesJoinTokenHashAndIsIdempotent(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/server.db")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	defer st.Close()

	config := Config{
		Store: st, DataDir: t.TempDir(), PublicURL: "https://boxes.example.test",
		BootstrapJoinToken: "bootstrap-secret", BootstrapNodeName: "primary",
		BootstrapHostBase: "203.0.113.5.sslip.io",
	}
	first, err := New(config)
	if err != nil {
		t.Fatalf("create server with bootstrap node: %v", err)
	}
	first.Close()

	second, err := New(config)
	if err != nil {
		t.Fatalf("restart server with bootstrap node: %v", err)
	}
	defer second.Close()

	nodes, err := st.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("list bootstrap nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("bootstrap node count = %d, want 1", len(nodes))
	}
	if nodes[0].TokenHash != tokenHash(config.BootstrapJoinToken) {
		t.Fatalf("bootstrap node token hash = %q, want hash of token", nodes[0].TokenHash)
	}
	if nodes[0].TokenHash == config.BootstrapJoinToken {
		t.Fatal("bootstrap node stores the raw join token")
	}
	if nodes[0].Node.Name != "primary" || nodes[0].Node.HostBase != "203.0.113.5.sslip.io" {
		t.Fatalf("bootstrap node = %#v, want configured name and host base", nodes[0].Node)
	}
}

func TestBootstrapNodeDefaultsNameToLocal(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/server.db")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	defer st.Close()

	srv, err := New(Config{Store: st, DataDir: t.TempDir(), BootstrapJoinToken: "bootstrap-secret"})
	if err != nil {
		t.Fatalf("create server with bootstrap node: %v", err)
	}
	defer srv.Close()
	nodes, err := st.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("list bootstrap nodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Node.Name != "local" {
		t.Fatalf("bootstrap nodes = %#v, want one node named local", nodes)
	}
}

func newTestServer(t *testing.T) (*Server, func()) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/server.db")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	srv, err := New(Config{Store: st, DataDir: t.TempDir(), PublicURL: "http://boxes.example.test", Image: "boxes/box:test"})
	if err != nil {
		_ = st.Close()
		t.Fatalf("create server: %v", err)
	}
	return srv, func() {
		srv.Close()
		_ = st.Close()
	}
}

func requestJSON(t *testing.T, client *http.Client, method, url, token string, body any, status int, result *map[string]any) {
	requestJSONWithHeaders(t, client, method, url, token, "", body, status, result)
}

func requestJSONWithHeaders(t *testing.T, client *http.Client, method, url, token, idempotency string, body any, status int, result *map[string]any) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	req, err := http.NewRequest(method, url, strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		bytes, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s status = %d, want %d; body: %s", method, url, response.StatusCode, status, bytes)
	}
	if result != nil {
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
}

func doJSONRequest(t *testing.T, client *http.Client, method, url string, headers map[string]string, body any) *http.Response {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	request, err := http.NewRequest(method, url, strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return response
}

func sessionCookie(t *testing.T, response *http.Response) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Cookies() {
		if cookie.Name == "boxes_session" {
			return cookie
		}
	}
	t.Fatalf("response has no boxes_session cookie; Set-Cookie = %q", response.Header.Values("Set-Cookie"))
	return nil
}

func assertErrorCode(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	if response.StatusCode != status {
		bytes, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want %d; body: %s", response.StatusCode, status, bytes)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	errorValue, _ := payload["error"].(map[string]any)
	if errorValue["code"] != code {
		t.Fatalf("error code = %v, want %q", errorValue["code"], code)
	}
}

func assertErrorMessage(t *testing.T, response *http.Response, status int, code, message string) {
	t.Helper()
	if response.StatusCode != status {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want %d; body: %s", response.StatusCode, status, body)
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if payload.Error.Code != code || !strings.HasPrefix(payload.Error.Message, message) {
		t.Fatalf("error = %#v, want code %q and message %q", payload.Error, code, message)
	}
}

func addCapacityReservations(t *testing.T, srv *Server, nodeID string) func() {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 10)
	for index := range ids {
		ids[index] = types.NewID("bx_")
		box := types.Box{ID: ids[index], NodeID: nodeID, Type: "default", State: types.StateReady, CreatedAt: time.Now().UTC()}
		if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: box}); err != nil {
			t.Fatalf("create capacity reservation %s: %v", ids[index], err)
		}
	}
	return func() {
		t.Helper()
		for _, id := range ids {
			if err := srv.store.DeleteBox(ctx, id); err != nil {
				t.Errorf("delete capacity reservation %s: %v", id, err)
			}
		}
	}
}

func websocketURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
