package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
	"github.com/coder/websocket"
)

func TestSignupAndLoginIssueBearerSessions(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "owner@example.test", "setupToken": srv.adminToken, "password": "correct horse battery staple",
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

func TestAuthStatusReportsOnlyWhetherSetupIsRequired(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	var status map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/auth/status", "", nil, http.StatusOK, &status)
	if len(status) != 1 || status["setupRequired"] != true {
		t.Fatalf("initial auth status = %#v, want only setupRequired=true", status)
	}

	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "owner@example.test", "setupToken": srv.adminToken, "password": "correct horse battery staple",
	}, http.StatusCreated, nil)
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/auth/status", "", nil, http.StatusOK, &status)
	if len(status) != 1 || status["setupRequired"] != false {
		t.Fatalf("auth status after owner creation = %#v, want only setupRequired=false", status)
	}
}

func TestHandlerReturnsJSONNotFoundForRegistryPaths(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	for _, path := range []string{"/v2", "/v2/", "/v2/library/image/manifests/latest"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
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
		"email": "owner@example.test", "setupToken": srv.adminToken, "password": "correct horse battery staple",
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
		"email": "owner@example.test", "setupToken": srv.adminToken, "password": "correct horse battery staple",
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
		"email": "owner@example.test", "setupToken": srv.adminToken, "password": "correct horse battery staple",
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
		"email": "owner@example.test", "setupToken": srv.adminToken, "password": "correct horse battery staple",
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
		"email": "owner@example.test", "setupToken": srv.adminToken, "password": "correct horse battery staple",
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
		"email": "owner@example.test", "setupToken": srv.adminToken, "password": "correct horse battery staple",
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
				"email": "owner@example.test", "setupToken": srv.adminToken, "password": password,
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
		"email": "owner@example.test", "setupToken": srv.adminToken, "password": "123456789012",
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

func TestSignupAfterBootstrapRequiresOwnerBearer(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	var owner map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "owner@example.test", "setupToken": srv.adminToken, "password": "correct horse battery staple",
	}, http.StatusCreated, &owner)
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{
		"email": "intruder@example.test", "setupToken": srv.adminToken, "password": "correct horse battery staple",
	}, http.StatusForbidden, nil)
	token, _ := owner["token"].(string)
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", token, map[string]any{
		"email": "member@example.test", "setupToken": srv.adminToken, "password": "correct horse battery staple",
	}, http.StatusCreated, nil)
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

func TestMachineReturnsLocalMachineMetricsAndRequiresAuthentication(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	lastSeen := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	node := types.Node{ID: "nd_localmachine", Name: "local", Status: "healthy", CPUs: 8, MemoryBytes: 16 << 30,
		MemAvailableBytes: 9 << 30, DiskFreeBytes: 40 << 30, SwapFreeBytes: 2 << 30,
		MemoryPressureSomeAvg10: 0.25, LastSeen: lastSeen}
	if err := srv.store.CreateNode(context.Background(), store.NodeRecord{Node: node, TokenHash: tokenHash("local-agent-token")}); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/machine", "", nil, http.StatusUnauthorized, nil)
	var response map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/machine", addPlatformSession(t, srv), nil, http.StatusOK, &response)
	machine, ok := response["machine"].(map[string]any)
	if !ok {
		t.Fatalf("machine response = %#v", response)
	}
	for key, want := range map[string]any{"cpus": float64(8), "memoryBytes": float64(16 << 30), "memAvailableBytes": float64(9 << 30),
		"diskFreeBytes": float64(40 << 30), "swapFreeBytes": float64(2 << 30), "memoryPressureSomeAvg10": 0.25,
		"status": "healthy", "runningBoxes": float64(0), "lastSeen": lastSeen.Format(time.RFC3339)} {
		if machine[key] != want {
			t.Errorf("machine[%q] = %#v, want %#v", key, machine[key], want)
		}
	}
}
