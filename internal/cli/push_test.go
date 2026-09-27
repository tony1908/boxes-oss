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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPushFilesNonGitSkipsDependenciesAndAppleDouble(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"main.go": "package main", "._main.go": "junk", ".DS_Store": "junk", "node_modules/pkg/index.js": "junk", ".next/server/app.js": "junk", ".nuxt/app.js": "junk", ".svelte-kit/output.js": "junk", ".turbo/cache": "junk", ".cache/data": "junk", ".parcel-cache/data": "junk", "coverage/report": "junk", "out/app.js": "junk", "target/classes/Main.class": "junk", ".gradle/cache": "junk", ".pytest_cache/data": "junk", ".mypy_cache/data": "junk", ".tox/data": "junk", "venv/bin/python": "junk", "src/app.js": "ok", "dist/app.js": "junk"} {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := pushFiles(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(files, ","), "main.go,src/app.js"; got != want {
		t.Fatalf("files = %q, want %q", got, want)
	}
}

func TestPushFilesGitIncludesTrackedAndUnignoredUntracked(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.test")
	git("config", "user.name", "Test")
	for name, content := range map[string]string{"tracked.txt": "tracked", ".gitignore": "node_modules/\nignored.txt\n", "._metadata": "junk", ".DS_Store": "junk", "node_modules/pkg.js": "junk", "ignored.txt": "junk", "new.txt": "untracked"} {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "tracked.txt", ".gitignore")
	files, err := pushFiles(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(files, ","), ".gitignore,new.txt,tracked.txt"; got != want {
		t.Fatalf("files = %q, want %q", got, want)
	}
}

func TestPushRoundTripsStreamingArchiveToAPI(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte("echo ok\n"), 0751); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("run.sh", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	var names []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/sandboxes/bx_abcdefghijkl/archive" || r.URL.Query().Get("path") != "/config/repo" {
			t.Errorf("request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			header, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			names = append(names, header.Name)
			if header.Name == "run.sh" && (header.Mode != 0751 || header.Size != 8) {
				t.Errorf("run.sh header = %#v", header)
			}
			if header.Name == "link" && (header.Typeflag != tar.TypeSymlink || header.Linkname != "run.sh") {
				t.Errorf("link header = %#v", header)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	var output bytes.Buffer
	app := CLI{Out: &output, ConfigPath: filepath.Join(t.TempDir(), "config.json"), BaseURL: server.URL, Token: "token", HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"push", "bx_abcdefghijkl", root, "--to", "/config/repo"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names, ","); got != "link,run.sh" {
		t.Fatalf("archive entries = %q", got)
	}
	if !strings.Contains(output.String(), "Pushed 2 files (") || !strings.Contains(output.String(), "to bx_abcdefghijkl:/config/repo") {
		t.Fatalf("output = %q", output.String())
	}
}
