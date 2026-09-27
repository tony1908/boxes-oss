package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageSetTakesCredentialsOnlyFromEnvironmentAndHidesThemFromOutput(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "admin.token"), []byte("admin-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/storage" {
			t.Errorf("admin request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer admin-secret" {
			t.Errorf("admin Authorization header = %q, want bearer token", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"s3","endpoint":"https://s3.example.test","bucket":"boxes","prefix":"archive"}`)
	}))
	defer server.Close()
	var output strings.Builder
	err := runStorageSet([]string{"--data-dir", dataDir, "--s3-endpoint", "https://s3.example.test", "--s3-bucket", "boxes", "--s3-prefix", "archive", "--s3-region", "region-1"}, server.URL+"/storage", "access-env", "secret-env", &output)
	if err != nil {
		t.Fatal(err)
	}
	if received["accessKey"] != "access-env" || received["secretKey"] != "secret-env" || received["endpoint"] != "https://s3.example.test" {
		t.Fatalf("admin request payload = %#v", received)
	}
	if strings.Contains(output.String(), "access-env") || strings.Contains(output.String(), "secret-env") {
		t.Fatalf("storage command output leaked credentials: %s", output.String())
	}
}

func TestStorageSetRequiresAdminTokenFile(t *testing.T) {
	var output strings.Builder
	err := runStorageSet([]string{"--data-dir", t.TempDir(), "--s3-endpoint", "https://s3.example.test", "--s3-bucket", "boxes"}, "http://127.0.0.1:8080/storage", "access-env", "secret-env", &output)
	if err == nil || !strings.Contains(err.Error(), "admin token") {
		t.Fatalf("storage set error = %v, want missing admin token error", err)
	}
}
