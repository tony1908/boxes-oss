package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateHostRejectsPortsUsedByBox(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	for _, port := range []int{3000, 3001, 7070} {
		var result map[string]any
		requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/bx_any/hosts", token,
			map[string]any{"port": port, "private": false}, http.StatusBadRequest, &result)
		errBody, _ := result["error"].(map[string]any)
		if errBody["code"] != "reserved_port" {
			t.Fatalf("port %d error = %#v, want reserved_port", port, result)
		}
	}
}
