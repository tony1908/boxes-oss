package types

import (
	"strings"
	"testing"
)

func TestValidateNetworkPolicy(t *testing.T) {
	many := make([]string, MaxNetworkEntries+1)
	for i := range many {
		many[i] = "10.0.0.0/8"
	}
	manyDomains := make([]string, MaxNetworkEntries+1)
	for i := range manyDomains {
		manyDomains[i] = "example.com"
	}
	tests := []struct {
		name    string
		policy  NetworkPolicy
		wantErr string
	}{
		{name: "empty mode is allow-all", policy: NetworkPolicy{}},
		{name: "deny-all", policy: NetworkPolicy{Mode: NetworkDenyAll}},
		{name: "allowlist with domains and cidrs", policy: NetworkPolicy{Mode: NetworkAllowlist,
			AllowDomains: []string{"api.github.com", "*.example.com"}, AllowCIDRs: []string{"10.0.0.0/8"}, DenyCIDRs: []string{"10.1.0.0/16"}}},
		{name: "unknown mode", policy: NetworkPolicy{Mode: "block-some"}, wantErr: "mode"},
		{name: "too many domains", policy: NetworkPolicy{Mode: NetworkAllowlist, AllowDomains: manyDomains}, wantErr: "allowDomains must not exceed"},
		{name: "too many allow cidrs", policy: NetworkPolicy{Mode: NetworkAllowlist, AllowCIDRs: many}, wantErr: "allowCidrs must not exceed"},
		{name: "too many deny cidrs", policy: NetworkPolicy{Mode: NetworkAllowAll, DenyCIDRs: many}, wantErr: "denyCidrs must not exceed"},
		{name: "bad domain", policy: NetworkPolicy{Mode: NetworkAllowlist, AllowDomains: []string{"bad_domain!"}}, wantErr: "allowDomains entry"},
		{name: "wildcard not leading", policy: NetworkPolicy{Mode: NetworkAllowlist, AllowDomains: []string{"a.*.com"}}, wantErr: "allowDomains entry"},
		{name: "bad cidr", policy: NetworkPolicy{Mode: NetworkAllowlist, AllowCIDRs: []string{"10.0.0.1"}}, wantErr: "allowCidrs entry"},
		{name: "ipv6 cidr rejected", policy: NetworkPolicy{Mode: NetworkAllowlist, DenyCIDRs: []string{"2001:db8::/32"}}, wantErr: "must be IPv4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNetworkPolicy(tc.policy)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
