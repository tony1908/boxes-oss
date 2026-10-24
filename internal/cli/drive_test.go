package cli

import (
	"reflect"
	"testing"
)

func TestParseMountFlags(t *testing.T) {
	got, err := parseMountFlags([]string{"data:/mnt/data", "cache:/mnt/cache:ro"})
	if err != nil {
		t.Fatalf("parseMountFlags() error = %v", err)
	}
	want := []map[string]any{
		{"drive": "data", "path": "/mnt/data"},
		{"drive": "cache", "path": "/mnt/cache", "readOnly": true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseMountFlags() = %#v, want %#v", got, want)
	}
	for _, value := range []string{"data", ":/mnt/data", "data:", "data:/mnt/data:rw", "data:/mnt/data:ro:extra"} {
		if _, err := parseMountFlags([]string{value}); err == nil {
			t.Errorf("parseMountFlags(%q) accepted an invalid mount", value)
		}
	}
}
