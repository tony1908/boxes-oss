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
