package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestFirstSignupRequiresAdminSetupToken(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	handler := srv.APIHandler()
	for _, token := range []string{"", "wrong"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/auth/signup", strings.NewReader(`{"email":"owner@example.test","password":"correct horse battery staple"}`))
		if token != "" {
			request = httptest.NewRequest(http.MethodPost, "/auth/signup", strings.NewReader(`{"email":"owner@example.test","password":"correct horse battery staple","setupToken":"wrong"}`))
		}
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), `"setup_token_required"`) {
			t.Fatalf("setup token %q status/body = %d %s", token, recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/auth/signup", strings.NewReader(`{"email":"owner@example.test","password":"correct horse battery staple","setupToken":"`+srv.adminToken+`"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("valid setup token status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestLoginEmailRateLimitUsesClockAndSuccessfulLoginClearsFailures(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/signup", "", map[string]any{"email": "owner@example.test", "password": "correct horse battery staple", "setupToken": srv.adminToken}, http.StatusCreated, nil)
	for i := 0; i < 5; i++ {
		requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/login", "", map[string]any{"email": "owner@example.test", "password": "wrong"}, http.StatusUnauthorized, nil)
	}
	response := doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/login", nil, map[string]any{"email": "OWNER@example.test", "password": "correct horse battery staple"})
	response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") == "" {
		t.Fatalf("limited login status/header = %d %q", response.StatusCode, response.Header.Get("Retry-After"))
	}
	now = now.Add(16 * time.Minute)
	for i := 0; i < 4; i++ {
		requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/login", "", map[string]any{"email": "owner@example.test", "password": "wrong"}, http.StatusUnauthorized, nil)
	}
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/login", "", map[string]any{"email": "owner@example.test", "password": "correct horse battery staple"}, http.StatusOK, nil)
	for i := 0; i < 5; i++ {
		requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/login", "", map[string]any{"email": "owner@example.test", "password": "wrong"}, http.StatusUnauthorized, nil)
	}
	response = doJSONRequest(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/auth/login", nil, map[string]any{"email": "owner@example.test", "password": "correct horse battery staple"})
	response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("email limit status = %d", response.StatusCode)
	}
}

func TestAuthRateLimitUsesForwardedIPOnlyFromTrustedPeer(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.RemoteAddr = "192.0.2.4:443"
	request.Header.Set("X-Forwarded-For", "203.0.113.8")
	if got := clientIP(request); got != "192.0.2.4" {
		t.Fatalf("untrusted forwarded IP = %q", got)
	}
	request.RemoteAddr = "127.0.0.1:8080"
	if got := clientIP(request); got != "203.0.113.8" {
		t.Fatalf("trusted forwarded IP = %q", got)
	}
}

func TestDashboardSecurityHeadersAllowExportedInlineHydrationScripts(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	for name, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"} {
		if got := recorder.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	csp := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP does not allow the static export hydration scripts or deny framing: %q", csp)
	}
	if recorder.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HTTP public URL received HSTS")
	}
	if !strings.Contains(recorder.Body.String(), "<script>") {
		t.Fatal("embedded static export no longer contains inline scripts")
	}
	for _, match := range regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`).FindAllStringSubmatch(recorder.Body.String(), -1) {
		attrs := match[1]
		if strings.Contains(attrs, "src=") {
			if !strings.Contains(csp, "script-src 'self'") {
				t.Fatalf("external script is not allowed by CSP: %s", attrs)
			}
		} else if !strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
			t.Fatalf("inline dashboard script is not allowed by CSP: %q", match[2])
		}
	}
}
