package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tony1908/boxes-oss/internal/types"
)

func TestDrivesCRUDAndBoxMountsRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "drives.db"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	now := time.Date(2026, time.October, 24, 12, 0, 0, 0, time.UTC)
	if err := s.CreateNode(ctx, NodeRecord{Node: types.Node{ID: "nd_drv", Name: "node", Status: "healthy"}}); err != nil {
		t.Fatalf("CreateNode(): %v", err)
	}
	if err := s.CreateDrive(ctx, types.Drive{Name: "data", CreatedAt: now}); err != nil {
		t.Fatalf("CreateDrive(): %v", err)
	}
	if err := s.CreateDrive(ctx, types.Drive{Name: "data", CreatedAt: now}); !errors.Is(err, ErrDriveExists) {
		t.Fatalf("second CreateDrive() = %v, want ErrDriveExists", err)
	}
	if err := s.CreateDrive(ctx, types.Drive{Name: "cache", CreatedAt: now}); err != nil {
		t.Fatalf("CreateDrive(cache): %v", err)
	}
	got, err := s.GetDrive(ctx, "data")
	if err != nil || got.Name != "data" || !got.CreatedAt.Equal(now) {
		t.Fatalf("GetDrive() = %#v, %v", got, err)
	}
	list, err := s.ListDrives(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "cache" || list[1].Name != "data" {
		t.Fatalf("ListDrives() = %#v, %v", list, err)
	}
	mounts := []types.Mount{{Drive: "data", Path: "/mnt/data"}, {Drive: "cache", Path: "/mnt/cache", ReadOnly: true}}
	if err := s.CreateBox(ctx, BoxRecord{Box: types.Box{ID: "bx_drives12345", NodeID: "nd_drv", Type: "small", State: types.StateReady, CreatedAt: now, Mounts: mounts}}); err != nil {
		t.Fatalf("CreateBox(): %v", err)
	}
	box, err := s.GetBox(ctx, "bx_drives12345")
	if err != nil || !reflect.DeepEqual(box.Mounts, mounts) {
		t.Fatalf("created mounts = %#v, %v", box.Mounts, err)
	}
	box.Mounts = nil
	if err := s.UpdateBox(ctx, box); err != nil {
		t.Fatalf("UpdateBox(): %v", err)
	}
	cleared, err := s.GetBox(ctx, "bx_drives12345")
	if err != nil || cleared.Mounts != nil {
		t.Fatalf("cleared mounts = %#v, %v", cleared.Mounts, err)
	}
	if err := s.DeleteDrive(ctx, "cache"); err != nil {
		t.Fatalf("DeleteDrive(): %v", err)
	}
	if _, err := s.GetDrive(ctx, "cache"); err == nil {
		t.Fatal("GetDrive() found a deleted drive")
	}
}
