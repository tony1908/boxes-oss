package types

import "testing"

func TestValidateMounts(t *testing.T) {
	valid := []Mount{{Drive: "data", Path: "/mnt/data"}, {Drive: "cache", Path: "/mnt/cache", ReadOnly: true}}
	if err := ValidateMounts(valid); err != nil {
		t.Fatalf("ValidateMounts(valid) = %v", err)
	}
	invalid := map[string][]Mount{
		"too many":       {{Drive: "a", Path: "/mnt/a"}, {Drive: "b", Path: "/mnt/b"}, {Drive: "c", Path: "/mnt/c"}, {Drive: "d", Path: "/mnt/d"}, {Drive: "e", Path: "/mnt/e"}},
		"bad drive":      {{Drive: "Bad_Name", Path: "/mnt/x"}},
		"relative":       {{Drive: "a", Path: "mnt/a"}},
		"mnt root":       {{Drive: "a", Path: "/mnt"}},
		"outside /mnt":   {{Drive: "a", Path: "/home/a"}},
		"not clean":      {{Drive: "a", Path: "/mnt/a/../b"}},
		"trailing slash": {{Drive: "a", Path: "/mnt/a/"}},
		"comma":          {{Drive: "a", Path: "/mnt/a,ro"}},
		"equals":         {{Drive: "a", Path: "/mnt/a=b"}},
		"duplicate":      {{Drive: "a", Path: "/mnt/a"}, {Drive: "b", Path: "/mnt/a"}},
		"nested":         {{Drive: "a", Path: "/mnt/a"}, {Drive: "b", Path: "/mnt/a/b"}},
	}
	for name, mounts := range invalid {
		if err := ValidateMounts(mounts); err == nil {
			t.Errorf("ValidateMounts(%s) accepted %#v", name, mounts)
		}
	}
	if err := ValidateMounts([]Mount{{Drive: "a", Path: "/mnt/ab"}, {Drive: "b", Path: "/mnt/a"}}); err != nil {
		t.Errorf("sibling prefix paths rejected: %v", err)
	}
}
