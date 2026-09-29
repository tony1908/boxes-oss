package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesEmbeddedUIAndFallsBackForApplicationRoutes(t *testing.T) {
	for _, path := range []string{"/", "/sandboxes/bx_123456789abc", "/settings/"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()
			Handler().ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if !strings.Contains(response.Body.String(), "Boxes") {
				t.Fatalf("response body does not contain embedded UI: %q", response.Body.String())
			}
		})
	}
}

func TestHandlerSetsCacheHeaders(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/_next/static/chunks/310vm2bl3xxpt.js", want: "public, max-age=31536000, immutable"},
		{path: "/", want: "no-cache"},
		{path: "/sandboxes/bx_123456789abc", want: "no-cache"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			response := httptest.NewRecorder()
			Handler().ServeHTTP(response, request)

			if got := response.Header().Get("Cache-Control"); got != tt.want {
				t.Errorf("Cache-Control = %q, want %q", got, tt.want)
			}
		})
	}
}
