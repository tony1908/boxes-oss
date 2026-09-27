package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAgentClientPreservesSanitizedErrorCodeAndMessage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"snapshot_failed","message":"restic backup failed: token=secret-value"}}`))
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &agentClient{http: &http.Client{Transport: agentTestTransport{target: target}}}
	err = client.json(context.Background(), http.MethodPost, "/v1/boxes/bx_abcdefgh2345/stop", nil, nil)
	if err == nil {
		t.Fatal("agent error was not returned")
	}
	if got := agentErrorCode(err); got != "snapshot_failed" {
		t.Fatalf("agent error code = %q, want snapshot_failed", got)
	}
	if got := agentErrorStatus(err); got != http.StatusConflict {
		t.Fatalf("agent status = %d, want %d", got, http.StatusConflict)
	}
	if got := err.Error(); !strings.Contains(got, "snapshot_failed") || !strings.Contains(got, "restic backup failed") || strings.Contains(got, "secret-value") {
		t.Fatalf("agent error = %q, want sanitized code and message", got)
	}
}

type agentTestTransport struct{ target *url.URL }

func (t agentTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.URL.Scheme = t.target.Scheme
	copy.URL.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(copy)
}
