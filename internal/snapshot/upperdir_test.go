package snapshot

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseOverlayUpperDirDecodesMountInfoEscapes(t *testing.T) {
	mountInfo := "" +
		"31 22 0:28 / / rw,relatime - ext4 /dev/root rw\n" +
		"44 31 0:41 / / rw,lowerdir=/lower,upperdir=/var/lib/containerd/a\\040b\\054c/fs,workdir=/work rw - overlay overlay rw,lowerdir=/lower,upperdir=/var/lib/containerd/a\\040b\\054c/fs,workdir=/work\n"

	got, err := parseOverlayUpperDir(mountInfo)
	if err != nil {
		t.Fatal(err)
	}
	const want = "/var/lib/containerd/a b,c/fs"
	if got != want {
		t.Fatalf("parseOverlayUpperDir() = %q, want %q", got, want)
	}
}

func TestParseOverlayUpperDirRequiresAbsoluteUpperDirOnRootOverlay(t *testing.T) {
	for name, mountInfo := range map[string]string{
		"no root overlay":   "44 31 0:41 / /data rw - overlay overlay rw,upperdir=/somewhere\n",
		"missing upperdir":  "44 31 0:41 / / rw - overlay overlay rw,lowerdir=/lower\n",
		"relative upperdir": "44 31 0:41 / / rw - overlay overlay rw,upperdir=relative\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseOverlayUpperDir(mountInfo); err == nil {
				t.Fatal("parseOverlayUpperDir() succeeded, want an error")
			}
		})
	}
}

func TestWalkUpperDirClassifiesChangesAndOpaqueDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "var", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "issue.net"), []byte("updated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "var", "lib", "marker"), []byte("upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "whiteout-fixture"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("issue.net", filepath.Join(root, "etc", "issue-link")); err != nil {
		t.Fatal(err)
	}

	changes, err := walkUpperDirWith(root, func(hostPath string, name string) (string, error) {
		if filepath.Clean(hostPath) == filepath.Join(root, "var", "lib") {
			if name == "trusted.overlay.opaque" {
				return "", fs.ErrPermission
			}
			if name == "user.overlay.opaque" {
				return "y", nil
			}
		}
		return "", fs.ErrNotExist
	}, func(hostPath string, mode fs.FileMode, rdev uint64) upperEntryKind {
		if filepath.Base(hostPath) == "whiteout-fixture" {
			return classifyUpperEntry(os.ModeDevice|os.ModeCharDevice, 0)
		}
		return classifyUpperEntry(mode, rdev)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := changes.files, []string{"/etc", "/etc/issue-link", "/etc/issue.net", "/var", "/var/lib", "/var/lib/marker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("upper files = %#v, want %#v", got, want)
	}
	if got, want := changes.deleted, []string{"/etc/whiteout-fixture"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deleted upper paths = %#v, want %#v", got, want)
	}
	if got, want := changes.opaque, []string{"/var/lib"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("opaque directories = %#v, want %#v", got, want)
	}
}

func TestClassifyUpperDeviceRecognizesOverlayWhiteout(t *testing.T) {
	if got := classifyUpperEntry(os.ModeDevice|os.ModeCharDevice, 0); got != upperEntryDeleted {
		t.Fatalf("classifyUpperEntry(0/0 char device) = %v, want deleted", got)
	}
	if got := classifyUpperEntry(os.ModeDevice|os.ModeCharDevice, 1); got != upperEntryOther {
		t.Fatalf("classifyUpperEntry(nonzero char device) = %v, want other", got)
	}
}
