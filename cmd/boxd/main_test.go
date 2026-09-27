package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunWithOutputDisplaysTopLevelHelp(t *testing.T) {
	var output bytes.Buffer
	if err := runWithOutput([]string{"--help"}, &output); err != nil {
		t.Fatalf("runWithOutput returned an error: %v", err)
	}
	for _, command := range []string{"serve", "mcp", "setup"} {
		if !strings.Contains(output.String(), command) {
			t.Errorf("help output %q does not mention %q", output.String(), command)
		}
	}
}
