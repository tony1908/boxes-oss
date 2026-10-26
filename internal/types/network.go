package types

import (
	"fmt"
	"net"
	"regexp"
)

// Network policy modes.
const (
	NetworkAllowAll  = "allow-all"
	NetworkDenyAll   = "deny-all"
	NetworkAllowlist = "allowlist"
)

// MaxNetworkEntries bounds each of the domain and CIDR lists in a policy.
const MaxNetworkEntries = 64

// MetadataCIDR is the cloud instance metadata address; it is always blocked.
const MetadataCIDR = "169.254.169.254/32"

// NetworkPolicy controls a box's outbound network access.
// DenyCIDRs wins over every other rule in every mode.
type NetworkPolicy struct {
	Mode         string   `json:"mode"`
	AllowDomains []string `json:"allowDomains,omitempty"`
	AllowCIDRs   []string `json:"allowCidrs,omitempty"`
	DenyCIDRs    []string `json:"denyCidrs,omitempty"`
}

var domainPattern = regexp.MustCompile(`^(\*\.)?([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)*[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// ValidateNetworkPolicy checks mode, list sizes, domain syntax and IPv4 CIDRs.
// An empty mode means allow-all.
// ponytail: IPv6 is rejected, not filtered: paddock-net is IPv4-only, so v6 traffic would bypass the rules.
func ValidateNetworkPolicy(policy NetworkPolicy) error {
	switch policy.Mode {
	case "", NetworkAllowAll, NetworkDenyAll, NetworkAllowlist:
	default:
		return fmt.Errorf("network policy mode must be %s, %s, or %s", NetworkAllowAll, NetworkDenyAll, NetworkAllowlist)
	}
	if len(policy.AllowDomains) > MaxNetworkEntries {
		return fmt.Errorf("allowDomains must not exceed %d entries", MaxNetworkEntries)
	}
	if len(policy.AllowCIDRs) > MaxNetworkEntries {
		return fmt.Errorf("allowCidrs must not exceed %d entries", MaxNetworkEntries)
	}
	if len(policy.DenyCIDRs) > MaxNetworkEntries {
		return fmt.Errorf("denyCidrs must not exceed %d entries", MaxNetworkEntries)
	}
	for _, domain := range policy.AllowDomains {
		if len(domain) > 253 || !domainPattern.MatchString(domain) {
			return fmt.Errorf("allowDomains entry %q must be a hostname or *.example.com", domain)
		}
	}
	for field, cidrs := range map[string][]string{"allowCidrs": policy.AllowCIDRs, "denyCidrs": policy.DenyCIDRs} {
		for _, cidr := range cidrs {
			if err := validateIPv4CIDR(cidr); err != nil {
				return fmt.Errorf("%s entry %q: %w", field, cidr, err)
			}
		}
	}
	return nil
}

func validateIPv4CIDR(cidr string) error {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("must be a CIDR such as 10.0.0.0/8")
	}
	if ip.To4() == nil {
		return fmt.Errorf("must be IPv4")
	}
	return nil
}
