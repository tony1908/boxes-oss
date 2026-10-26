package main

import (
	"reflect"
	"testing"
)

func TestWithAutomaticJSONForNonTerminalStdout(t *testing.T) {
	if got, want := withAutomaticJSON([]string{"list"}, false), []string{"--json", "list"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("non-terminal args = %#v, want %#v", got, want)
	}
	if got, want := withAutomaticJSON([]string{"--json", "list"}, false), []string{"--json", "list"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit JSON args = %#v, want %#v", got, want)
	}
	if got, want := withAutomaticJSON([]string{"list"}, true), []string{"list"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal args = %#v, want %#v", got, want)
	}
}
