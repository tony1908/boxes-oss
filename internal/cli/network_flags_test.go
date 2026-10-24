package cli

import (
	"flag"
	"reflect"
	"testing"
)

func parseNetworkArgs(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var network networkFlags
	network.register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %q: %v", args, err)
	}
	return network.policy()
}

func TestNetworkFlagsBuildPolicy(t *testing.T) {
	policy, err := parseNetworkArgs(t, "--network", "allowlist",
		"--allow-domain", "api.github.com", "--allow-domain", "*.example.com",
		"--allow-cidr", "10.0.0.0/8", "--deny-cidr", "10.1.0.0/16", "--deny-cidr", "192.168.0.0/16")
	if err != nil {
		t.Fatalf("policy() error = %v", err)
	}
	want := map[string]any{
		"mode":         "allowlist",
		"allowDomains": []string{"api.github.com", "*.example.com"},
		"allowCidrs":   []string{"10.0.0.0/8"},
		"denyCidrs":    []string{"10.1.0.0/16", "192.168.0.0/16"},
	}
	if !reflect.DeepEqual(policy, want) {
		t.Fatalf("policy = %#v, want %#v", policy, want)
	}
}

func TestNetworkFlagsModesAndDefaults(t *testing.T) {
	policy, err := parseNetworkArgs(t, "--network", "deny-all")
	if err != nil || !reflect.DeepEqual(policy, map[string]any{"mode": "deny-all"}) {
		t.Fatalf("deny-all policy = %#v, err %v", policy, err)
	}
	policy, err = parseNetworkArgs(t, "--deny-cidr", "10.0.0.0/8", "--network", "allow-all")
	if err != nil || !reflect.DeepEqual(policy, map[string]any{"mode": "allow-all", "denyCidrs": []string{"10.0.0.0/8"}}) {
		t.Fatalf("allow-all with deny-cidr = %#v, err %v", policy, err)
	}
	policy, err = parseNetworkArgs(t)
	if err != nil || policy != nil {
		t.Fatalf("no flags policy = %#v, err %v, want nil", policy, err)
	}
}

func TestNetworkFlagsRejectInvalidCombinations(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown mode":              {"--network", "block"},
		"allow-domain without list": {"--network", "deny-all", "--allow-domain", "example.com"},
		"allow-cidr without mode":   {"--allow-cidr", "10.0.0.0/8"},
		"deny-cidr without mode":    {"--deny-cidr", "10.0.0.0/8"},
	} {
		if policy, err := parseNetworkArgs(t, args...); err == nil {
			t.Fatalf("%s: policy = %#v, want error", name, policy)
		}
	}
}
