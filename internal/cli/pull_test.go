package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPullWritesFileAndExtractsDirectory(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "./nested/file.txt", Mode: 0644, Size: 5, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hello"))
	_ = tw.Close()
	_ = gz.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/sandboxes/bx_abcdefghijkl/artifacts" && r.URL.Query().Get("path") == "/tmp/file.txt" {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "data")
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive.Bytes())
	}))
	defer server.Close()
	root := t.TempDir()
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "boxes.json"), BaseURL: server.URL, Token: "token", HTTPClient: server.Client()}
	file := filepath.Join(root, "file.txt")
	if err := app.Run(context.Background(), []string{"pull", "bx_abcdefghijkl", "/tmp/file.txt", "-o", file}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "data" {
		t.Fatalf("file = %q, %v", data, err)
	}
	dir := filepath.Join(root, "dir")
	if err := app.Run(context.Background(), []string{"pull", "bx_abcdefghijkl", "/tmp/tree", "-o", dir}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(dir, "nested", "file.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("extracted file = %q, %v", data, err)
	}
}
