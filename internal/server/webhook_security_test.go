package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookURLsRejectInternalHostsAndReservedIPRanges(t *testing.T) {
	for _, raw := range []string{"http://localhost/hook", "http://foo.localhost/hook", "http://127.0.0.1/hook", "http://10.1.2.3/hook", "http://169.254.169.254/latest", "http://100.64.0.1/hook", "http://[fc00::1]/hook", "http://[fe80::1]/hook", "http://[::]/hook", "http://[ff02::1]/hook", "http://metadata.google.internal/"} {
		if validWebhookURL(raw) {
			t.Errorf("validWebhookURL(%q) = true", raw)
		}
	}
	if !validWebhookURL("https://hooks.example.test/boxes") {
		t.Fatal("public HTTPS webhook URL was rejected")
	}
}

func TestWebhookDialBlocksPrivateAddressesAtConnectionTime(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	_, err = webhookDialContext(false)(context.Background(), "tcp", listener.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "private or reserved") {
		t.Fatalf("private address dial error = %v", err)
	}
	conn, err := webhookDialContext(true)(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("private address opt out dial: %v", err)
	}
	_ = conn.Close()
}

func TestCreateWebhookRejectsInternalDestinationWithClearError(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	token := addPlatformSession(t, srv)
	request := httptest.NewRequest(http.MethodPost, "/webhooks", strings.NewReader(`{"url":"http://127.0.0.1/hook","events":["sandbox.ready"]}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	srv.APIHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "private or reserved") {
		t.Fatalf("internal webhook response = %d %s", recorder.Code, recorder.Body.String())
	}
}
