package snapshot

import "testing"

func TestBoxignoreMatchesGitignoreSubset(t *testing.T) {
	tests := []struct {
		name       string
		rules      string
		path       string
		ignorePath string
		want       bool
	}{
		{name: "star in one directory", rules: "*.log", path: "/config/debug.log", want: true},
		{name: "star matches a basename at any depth", rules: "*.log", path: "/config/nested/debug.log", want: true},
		{name: "double star crosses directories", rules: "**/*.log", path: "/config/nested/debug.log", want: true},
		{name: "question mark matches one character", rules: "file?.txt", path: "/config/file1.txt", want: true},
		{name: "trailing slash matches directory descendants", rules: "cache/", path: "/config/cache/data.bin", want: true},
		{name: "leading slash anchors to ignore file directory", rules: "/secret.txt", path: "/config/secret.txt", want: true},
		{name: "anchored rule does not match deeper basename", rules: "/secret.txt", path: "/config/nested/secret.txt", want: false},
		{name: "directory path rule excludes descendants", rules: "generated/cache", path: "/config/generated/cache/item.bin", want: true},
		{name: "negation re-includes a path", rules: "*.log\n!important.log", path: "/config/important.log", want: false},
		{name: "comments and blank lines are ignored", rules: "# note\n\n*.tmp", path: "/config/file.tmp", want: true},
		{name: "rule base is relative to ignore file", rules: "cache/", path: "/other/cache/data", want: false},
		{name: "nested ignore file uses its own directory", rules: "cache/", path: "/config/work/cache/data", ignorePath: "/config/work/.boxignore", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ignorePath := test.ignorePath
			if ignorePath == "" {
				ignorePath = "/config/.boxignore"
			}
			got, err := Ignored(test.rules, ignorePath, test.path)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("Ignored(%q, %q) = %t, want %t", test.rules, test.path, got, test.want)
			}
		})
	}
}

func TestBoxignoreNegationCanReincludeNestedPath(t *testing.T) {
	got, err := Ignored("*.log\n!important.log", "/config/.boxignore", "/config/important.log")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("negated path remained ignored")
	}
}
