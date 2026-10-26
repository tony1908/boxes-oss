package cli

import (
	"reflect"
	"testing"
)

func TestSourceBodyBuildsGitSourceWithTokenFromEnv(t *testing.T) {
	t.Setenv("PADDOCK_TEST_GIT_TOKEN", "ghp_from_env")
	source, err := sourceBody("https://example.test/repo.git", "v1.0", 1, "PADDOCK_TEST_GIT_TOKEN", "app", "")
	if err != nil {
		t.Fatalf("sourceBody() error = %v", err)
	}
	want := map[string]any{"type": "git", "url": "https://example.test/repo.git", "revision": "v1.0", "depth": 1, "path": "app", "token": "ghp_from_env"}
	if !reflect.DeepEqual(source, want) {
		t.Fatalf("source = %#v, want %#v", source, want)
	}
}

func TestSourceBodyBuildsTarballAndNoSource(t *testing.T) {
	source, err := sourceBody("", "", 0, "", "", "https://example.test/src.tar.gz")
	if err != nil || !reflect.DeepEqual(source, map[string]any{"type": "tarball", "url": "https://example.test/src.tar.gz"}) {
		t.Fatalf("tarball source = %#v, err %v", source, err)
	}
	source, err = sourceBody("", "", 0, "", "", "")
	if err != nil || source != nil {
		t.Fatalf("no-source = %#v, err %v; want nil", source, err)
	}
}

func TestSourceBodyRejectsInvalidFlagCombinations(t *testing.T) {
	cases := map[string][]any{
		"git and tarball":  {"https://example.test/repo.git", "", 0, "", "", "https://example.test/a.tar.gz"},
		"revision alone":   {"", "main", 0, "", "", ""},
		"depth alone":      {"", "", 1, "", "", ""},
		"path alone":       {"", "", 0, "", "app", ""},
		"tarball revision": {"", "main", 0, "", "", "https://example.test/a.tar.gz"},
	}
	for name, args := range cases {
		if _, err := sourceBody(args[0].(string), args[1].(string), args[2].(int), args[3].(string), args[4].(string), args[5].(string)); err == nil {
			t.Errorf("%s: sourceBody() accepted invalid flags", name)
		}
	}
}

func TestSourceBodyRejectsUnsetTokenEnv(t *testing.T) {
	t.Setenv("PADDOCK_TEST_EMPTY_TOKEN", "")
	if _, err := sourceBody("https://example.test/repo.git", "", 0, "PADDOCK_TEST_EMPTY_TOKEN", "", ""); err == nil {
		t.Fatal("sourceBody() accepted an empty token variable")
	}
}
