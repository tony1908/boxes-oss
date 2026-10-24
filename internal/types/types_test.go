package types

import (
	"regexp"
	"testing"
)

func TestSizeFor(t *testing.T) {
	tests := []struct {
		name      string
		cpus      int
		memoryGiB int64
		diskGiB   int64
	}{
		{name: "xsmall", cpus: 1, memoryGiB: 2, diskGiB: 12},
		{name: "small", cpus: 2, memoryGiB: 4, diskGiB: 12},
		{name: "default", cpus: 4, memoryGiB: 8, diskGiB: 50},
		{name: "large", cpus: 8, memoryGiB: 16, diskGiB: 125},
		{name: "xlarge", cpus: 16, memoryGiB: 32, diskGiB: 251},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size, err := SizeFor(tt.name)
			if err != nil {
				t.Fatalf("SizeFor(%q): %v", tt.name, err)
			}
			if size.CPUs != tt.cpus {
				t.Errorf("CPUs = %d, want %d", size.CPUs, tt.cpus)
			}
			if size.MemoryBytes != tt.memoryGiB<<30 {
				t.Errorf("MemoryBytes = %d, want %d", size.MemoryBytes, tt.memoryGiB<<30)
			}
			if size.DiskBytes != tt.diskGiB<<30 {
				t.Errorf("DiskBytes = %d, want %d", size.DiskBytes, tt.diskGiB<<30)
			}
			wantReservationBytes := map[string]int64{"xsmall": 1 << 27, "small": 1 << 28, "default": 1 << 29, "large": 1 << 30, "xlarge": 2 << 30}[tt.name]
			if size.MemoryReservationBytes != wantReservationBytes {
				t.Errorf("MemoryReservationBytes = %d, want %d", size.MemoryReservationBytes, wantReservationBytes)
			}
		})
	}

	if _, err := SizeFor("unknown"); err == nil {
		t.Fatal("SizeFor(unknown) returned no error")
	}
}

func TestBoxStates(t *testing.T) {
	states := []BoxState{
		StateProvisioning,
		StateReady,
		StateIdle,
		StateStopping,
		StateStopped,
		StateResuming,
		StateError,
		StateDeleting,
		StateUnreachable,
	}
	want := []string{"provisioning", "ready", "idle", "stopping", "stopped", "resuming", "error", "deleting", "unreachable"}
	for i, state := range states {
		if string(state) != want[i] {
			t.Errorf("state %d = %q, want %q", i, state, want[i])
		}
	}
}

func TestNewID(t *testing.T) {
	pattern := regexp.MustCompile(`^bx_[a-z2-7]{12}$`)
	for i := 0; i < 20; i++ {
		id := NewID("bx_")
		if !pattern.MatchString(id) {
			t.Fatalf("NewID returned malformed ID %q", id)
		}
	}
}

func TestAPIError(t *testing.T) {
	err := APIError{Code: "not_found", Message: "box not found"}
	if err.Error() != "box not found" {
		t.Fatalf("Error() = %q, want %q", err.Error(), "box not found")
	}
}

func TestValidateTags(t *testing.T) {
	tooMany := map[string]string{}
	for i := 0; i <= MaxTags; i++ {
		tooMany[string(rune('a'+i))] = "v"
	}
	tests := []struct {
		name    string
		tags    map[string]string
		wantErr bool
	}{
		{"nil ok", nil, false},
		{"valid", map[string]string{"env.name-1_x": "prod", "empty": ""}, false},
		{"ten entries ok", map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5", "f": "6", "g": "7", "h": "8", "i": "9", "j": "10"}, false},
		{"eleven entries", tooMany, true},
		{"uppercase key", map[string]string{"Env": "x"}, true},
		{"leading dash key", map[string]string{"-env": "x"}, true},
		{"space in key", map[string]string{"a b": "x"}, true},
		{"key 63 chars ok", map[string]string{strRepeat('a', 63): "x"}, false},
		{"key 64 chars", map[string]string{strRepeat('a', 64): "x"}, true},
		{"value too long", map[string]string{"k": strRepeat('v', 256)}, true},
		{"value 255 ok", map[string]string{"k": strRepeat('v', 255)}, false},
		{"newline in value", map[string]string{"k": "a\nb"}, true},
		{"tab in value", map[string]string{"k": "a\tb"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTags(tt.tags)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateTags() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func strRepeat(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
