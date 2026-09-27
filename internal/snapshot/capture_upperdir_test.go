package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureUsesUpperDirAndWritesNULManifest(t *testing.T) {
	root := t.TempDir()
	upperDir := filepath.Join(root, "upper")
	if err := os.Mkdir(upperDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upperDir, "marker-x"), []byte("present"), 0o644); err != nil {
		t.Fatal(err)
	}
	procDir := filepath.Join(root, "proc", "123")
	if err := os.MkdirAll(procDir, 0o755); err != nil {
		t.Fatal(err)
	}
	upperOption := strings.ReplaceAll(upperDir, " ", "\\040")
	mountInfo := "51 40 0:45 / / rw - overlay overlay rw,lowerdir=/lower,upperdir=" + upperOption + ",workdir=/work\n"
	if err := os.WriteFile(filepath.Join(procDir, "mountinfo"), []byte(mountInfo), 0o600); err != nil {
		t.Fatal(err)
	}

	docker := &fakeExecutor{pid: "123", replies: []executorReply{
		{stdout: "{}"}, // image-declared volumes
		{},             // .boxignore discovery
		{},             // create /.boxes
		{},             // write deletions
		{},             // write manifest
		{},             // write opaque metadata
		{stdout: "tar-data"},
	}}
	restic := &fakeExecutor{replies: []executorReply{{stdout: `{"snapshot_id":"snapshot-upper","total_bytes_processed":7}`}}}
	engine := New(docker, restic, "/repo", "secret")
	t.Setenv("BOXES_HOST_PROC", filepath.Join(root, "proc"))
	engine.mountInfo = newProcMountInfo()
	if _, err := engine.Capture(context.Background(), "bx_abcdefghijkl"); err != nil {
		t.Fatal(err)
	}

	for _, call := range docker.calls {
		if contains(call.argv, "diff") {
			t.Fatalf("Capture() used docker diff despite a readable overlay upperdir: %#v", call.argv)
		}
	}
	manifestCall, tarCall := -1, -1
	for i, call := range docker.calls {
		if contains(call.argv, "/.boxes/manifest") {
			manifestCall = i
		}
		if contains(call.argv, "--files-from") {
			tarCall = i
		}
	}
	if manifestCall < 0 || docker.calls[manifestCall].stdin != "/marker-x\x00" {
		t.Fatalf("manifest write = %#v, want NUL-separated /marker-x", docker.calls)
	}
	const wantTarPaths = "/.boxes/deletions\x00/.boxes/manifest\x00/.boxes/opaque\x00/marker-x\x00"
	if tarCall < 0 || docker.calls[tarCall].stdin != wantTarPaths {
		t.Fatalf("tar paths = %q, want %q", docker.calls[tarCall].stdin, wantTarPaths)
	}
}

func TestCaptureFallsBackToDockerDiffWhenUpperDirIsUnavailable(t *testing.T) {
	root := t.TempDir()
	procDir := filepath.Join(root, "proc", "123")
	if err := os.MkdirAll(procDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "mountinfo"), []byte("not an overlay mountinfo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	docker := &fakeExecutor{pid: "123", replies: []executorReply{
		{stdout: "A /from-diff\nD /removed-by-diff\n"},
		{stdout: "{}"}, // image-declared volumes
		{},             // .boxignore discovery
		{},             // create /.boxes
		{},             // write deletions
		{},             // write manifest
		{},             // write opaque metadata
		{stdout: "/.boxes/deletions\x00/.boxes/manifest\x00/.boxes/opaque\x00/from-diff\x00"}, // filter fallback paths
		{stdout: "tar-data"},
	}}
	restic := &fakeExecutor{replies: []executorReply{{stdout: `{"snapshot_id":"snapshot-fallback","total_bytes_processed":1}`}}}
	engine := New(docker, restic, "/repo", "secret")
	engine.mountInfo = procMountInfo{root: filepath.Join(root, "proc")}
	if _, err := engine.Capture(context.Background(), "bx_abcdefghijkl"); err != nil {
		t.Fatal(err)
	}

	var foundDeletion, foundManifest, foundTar bool
	for _, call := range docker.calls {
		if strings.HasPrefix(call.stdin, "/removed-by-diff\n") {
			foundDeletion = true
		}
		if contains(call.argv, "/.boxes/manifest") && call.stdin == "/from-diff\x00" {
			foundManifest = true
		}
		if contains(call.argv, "--files-from") && call.stdin == "/.boxes/deletions\x00/.boxes/manifest\x00/.boxes/opaque\x00/from-diff\x00" {
			foundTar = true
		}
	}
	if !foundDeletion || !foundManifest || !foundTar {
		t.Fatalf("fallback did not preserve docker diff paths and metadata: %#v", docker.calls)
	}
}
