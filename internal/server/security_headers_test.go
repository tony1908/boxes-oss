package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
)

func TestSecurityHeadersIncludeCachedNodeHostBases(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	srv.cfg.PublicURL = "https://dashboard.example.com"
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }

	createNode := func(id, hostBase string) {
		t.Helper()
		err := srv.store.CreateNode(context.Background(), store.NodeRecord{Node: types.Node{
			ID: id, Name: id, HostBase: hostBase, Status: "offline",
		}})
		if err != nil {
			t.Fatalf("create node %q: %v", id, err)
		}
	}
	requestCSP := func() string {
		t.Helper()
		recorder := httptest.NewRecorder()
		srv.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		return recorder.Header().Get("Content-Security-Policy")
	}

	createNode("nd_sslip", "203.0.113.10.sslip.io")
	createNode("nd_malformed", "sslip.io; script-src 'none'")
	csp := requestCSP()
	for _, want := range []string{"https://*.dashboard.example.com", "https://*.203.0.113.10.sslip.io"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "script-src 'none'") || strings.Contains(csp, "sslip.io; script-src") {
		t.Fatalf("malformed node host base was included in CSP: %s", csp)
	}

	// A node created during the cache window is picked up when the TTL expires.
	createNode("nd_later", "later.example.net")
	if got := requestCSP(); strings.Contains(got, "https://*.later.example.net") {
		t.Fatalf("node host base appeared before the cache TTL expired: %s", got)
	}
	now = now.Add(nodeHostBasesCacheTTL)
	if got := requestCSP(); !strings.Contains(got, "https://*.later.example.net") {
		t.Fatalf("node host base missing after cache TTL expired: %s", got)
	}
}
