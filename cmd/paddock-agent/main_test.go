package main

import "testing"

func TestValidateConfigDoesNotRequireNodeID(t *testing.T) {
	if err := validateConfig("wss://paddock.example.test/api/v1/agent/connect", "node-token", "/var/lib/paddock/restic"); err != nil {
		t.Fatalf("valid config without a node ID: %v", err)
	}
}

func TestValidateConfigRequiresServerTokenAndRepository(t *testing.T) {
	tests := []struct {
		name       string
		serverURL  string
		nodeToken  string
		repository string
	}{
		{name: "server URL", nodeToken: "token", repository: "/repo"},
		{name: "node token", serverURL: "wss://paddock.example.test/api/v1/agent/connect", repository: "/repo"},
		{name: "repository", serverURL: "wss://paddock.example.test/api/v1/agent/connect", nodeToken: "token"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateConfig(test.serverURL, test.nodeToken, test.repository); err == nil {
				t.Fatal("validateConfig succeeded with a missing required value")
			}
		})
	}
}
