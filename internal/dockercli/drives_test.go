package dockercli

import (
	"context"
	"reflect"
	"testing"

	boxtypes "github.com/tony1908/paddock/internal/types"
)

func TestCreateArgsMountsDrivesReadWriteAndReadOnly(t *testing.T) {
	got, err := CreateArgs(Spec{BoxID: "bx_abcdefghijkl", Image: "paddock/box:dev", Type: "xsmall", Runtime: "runc",
		Mounts: []boxtypes.Mount{{Drive: "data", Path: "/mnt/data"}, {Drive: "cache", Path: "/mnt/cache", ReadOnly: true}}})
	if err != nil {
		t.Fatalf("CreateArgs() error = %v", err)
	}
	var mounts []string
	for i := 0; i < len(got)-1; i++ {
		if got[i] == "--mount" {
			mounts = append(mounts, got[i+1])
		}
	}
	want := []string{"type=volume,source=paddock-drive-data,target=/mnt/data", "type=volume,source=paddock-drive-cache,target=/mnt/cache,readonly"}
	if !reflect.DeepEqual(mounts, want) {
		t.Fatalf("mounts = %#v, want %#v", mounts, want)
	}
	if _, err := CreateArgs(Spec{BoxID: "bx_abcdefghijkl", Image: "paddock/box:dev", Type: "xsmall", Runtime: "runc",
		Mounts: []boxtypes.Mount{{Drive: "data", Path: "/mnt/a,readonly"}}}); err == nil {
		t.Fatal("CreateArgs accepted a mount path containing a comma")
	}
}

func TestDriveVolumeCommandsBuildArgv(t *testing.T) {
	exec := &scriptedExecutor{}
	docker := New(exec)
	ctx := context.Background()
	if err := docker.CreateDriveVolume(ctx, "data"); err != nil {
		t.Fatalf("CreateDriveVolume(): %v", err)
	}
	if err := docker.RemoveDriveVolume(ctx, "data"); err != nil {
		t.Fatalf("RemoveDriveVolume(): %v", err)
	}
	if err := docker.CopyDriveVolume(ctx, "data", "copy", "paddock/box:dev"); err != nil {
		t.Fatalf("CopyDriveVolume(): %v", err)
	}
	want := [][]string{
		{"docker", "volume", "create", "--label", "paddock.drive=data", "paddock-drive-data"},
		{"docker", "volume", "rm", "paddock-drive-data"},
		{"docker", "volume", "create", "--label", "paddock.drive=copy", "paddock-drive-copy"},
		{"docker", "run", "--rm", "--network", "none", "--runtime", "runc", "--entrypoint", "cp",
			"-v", "paddock-drive-data:/from:ro", "-v", "paddock-drive-copy:/to", "paddock/box:dev", "-a", "/from/.", "/to/"},
	}
	if len(exec.calls) != len(want) {
		t.Fatalf("made %d calls, want %d: %#v", len(exec.calls), len(want), exec.calls)
	}
	for i := range want {
		if !reflect.DeepEqual(exec.calls[i].argv, want[i]) {
			t.Errorf("call %d = %#v, want %#v", i, exec.calls[i].argv, want[i])
		}
	}
}

func TestCopyDriveVolumeRemovesTargetWhenCopyFails(t *testing.T) {
	exec := &scriptedExecutor{responses: []struct {
		code   int
		err    error
		stdout string
		stderr string
	}{{}, {code: 1, stderr: "copy failed"}, {}}}
	if err := New(exec).CopyDriveVolume(context.Background(), "data", "copy", "paddock/box:dev"); err == nil {
		t.Fatal("CopyDriveVolume() succeeded after a failed copy")
	}
	last := exec.calls[len(exec.calls)-1].argv
	if !reflect.DeepEqual(last, []string{"docker", "volume", "rm", "paddock-drive-copy"}) {
		t.Fatalf("last call = %#v, want removal of the target volume", last)
	}
}
