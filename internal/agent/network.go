package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tony1908/boxes-oss/internal/types"
)

const (
	defaultNetworkDir      = "/var/lib/boxes/network"
	networkRefreshInterval = 60 * time.Second
	networkResolveTimeout  = 10 * time.Second
	// dnsHistoryWindow keeps an IP allowed this long after a domain last resolved to it.
	dnsHistoryWindow = 30 * time.Minute
	// dnsHistoryCap bounds the IPs kept per domain per box; the most recently seen win.
	dnsHistoryCap = 64
)

// networkChain returns the per-box iptables chain. Box IDs are 15 characters, so the name is 21 characters (limit 28).
func networkChain(boxID string) string { return "BOXES-" + boxID }

// iptablesArgv builds an iptables command. -w waits for the xtables lock shared with Docker.
func iptablesArgv(args ...string) []string {
	return append([]string{"iptables", "-w", "5"}, args...)
}

// networkRules returns the argv lists that (re)build the box's chain and hook it into DOCKER-USER
// (and INPUT unless allow-all). The chain is flushed first, so re-running is safe.
// ponytail: flush-then-rebuild leaves a few-millisecond window where the chain is empty; swap to iptables-restore if that matters.
func networkRules(boxID, containerIP string, policy types.NetworkPolicy, resolved map[string][]net.IP) [][]string {
	if containerIP == "" {
		return nil
	}
	chain := networkChain(boxID)
	mode := policy.Mode
	if mode == "" {
		mode = types.NetworkAllowAll
	}
	rule := func(args ...string) []string { return iptablesArgv(append([]string{"-A", chain}, args...)...) }
	rules := [][]string{
		iptablesArgv("-N", chain), // fails harmlessly when the chain already exists
		iptablesArgv("-F", chain),
		rule("-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"),
	}
	for _, cidr := range policy.DenyCIDRs {
		rules = append(rules, rule("-d", cidr, "-j", "DROP"))
	}
	rules = append(rules, rule("-d", types.MetadataCIDR, "-j", "DROP"))
	switch mode {
	case types.NetworkDenyAll:
		rules = append(rules, rule("-j", "DROP"))
	case types.NetworkAllowlist:
		for _, cidr := range policy.AllowCIDRs {
			rules = append(rules, rule("-d", cidr, "-j", "RETURN"))
		}
		// ponytail: DNS port 53 is left open so names can resolve; any resolver is reachable, so DNS is an exfiltration channel.
		rules = append(rules, rule("-p", "udp", "--dport", "53", "-j", "RETURN"), rule("-p", "tcp", "--dport", "53", "-j", "RETURN"))
		for _, ip := range resolvedIPs(resolved) {
			rules = append(rules, rule("-d", ip+"/32", "-j", "RETURN"))
		}
		rules = append(rules, rule("-j", "DROP"))
	default:
		rules = append(rules, rule("-j", "RETURN"))
	}
	rules = append(rules, iptablesArgv("-I", "DOCKER-USER", "-s", containerIP+"/32", "-j", chain))
	if mode != types.NetworkAllowAll {
		rules = append(rules, iptablesArgv("-I", "INPUT", "-s", containerIP+"/32", "-j", chain))
	}
	return rules
}

// resolvedIPs returns the sorted, de-duplicated IPv4 addresses from resolved.
func resolvedIPs(resolved map[string][]net.IP) []string {
	seen := map[string]bool{}
	var out []string
	for _, ips := range resolved {
		for _, ip := range ips {
			if v4 := ip.To4(); v4 != nil && !seen[v4.String()] {
				seen[v4.String()] = true
				out = append(out, v4.String())
			}
		}
	}
	sort.Strings(out)
	return out
}

// jumpDeleteRules returns the -D argv for every jump into chain listed in iptables -S output,
// except jumps from keepIP (pass "" to delete all).
func jumpDeleteRules(chain, listing, keepIP string) [][]string {
	var out [][]string
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "-A" || !containsJump(fields, chain) {
			continue
		}
		if keepIP != "" && containsSource(fields, keepIP+"/32") {
			continue
		}
		args := append([]string{"-D"}, fields[1:]...)
		out = append(out, iptablesArgv(args...))
	}
	return out
}

func containsJump(fields []string, chain string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "-j" && fields[i+1] == chain {
			return true
		}
	}
	return false
}

func containsSource(fields []string, source string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "-s" && fields[i+1] == source {
			return true
		}
	}
	return false
}

// teardownRules returns the argv that empties and deletes a box's chain. Jumps are removed first via jumpDeleteRules.
func teardownRules(boxID string) [][]string {
	chain := networkChain(boxID)
	return [][]string{iptablesArgv("-F", chain), iptablesArgv("-X", chain)}
}

// runIptables runs argv and returns its stdout and exit code. Exec failures are returned as errors.
func (a *Agent) runIptables(ctx context.Context, argv []string) (string, int, error) {
	var stdout, stderr bytes.Buffer
	code, err := a.firewall.Run(ctx, argv, nil, &stdout, &stderr)
	if err != nil {
		return "", code, fmt.Errorf("%s: %w", argv[3], err)
	}
	return stdout.String(), code, nil
}

// listJumps returns the rules currently in DOCKER-USER and INPUT.
func (a *Agent) listJumps(ctx context.Context) string {
	var listing strings.Builder
	for _, chain := range []string{"DOCKER-USER", "INPUT"} {
		out, code, err := a.runIptables(ctx, iptablesArgv("-S", chain))
		if err != nil || code != 0 {
			continue
		}
		listing.WriteString(out)
		listing.WriteString("\n")
	}
	return listing.String()
}

// applyNetworkRules installs the box's rules. Caller holds networkMu.
// It hooks the chain first and removes stale jumps afterwards, so the box is never unhooked during a change.
func (a *Agent) applyNetworkRules(ctx context.Context, boxID, containerIP string, policy types.NetworkPolicy, resolved map[string][]net.IP) error {
	for _, argv := range networkRules(boxID, containerIP, policy, resolved) {
		op := argv[3]
		if op == "-I" {
			check := append([]string(nil), argv...)
			check[3] = "-C"
			if _, code, err := a.runIptables(ctx, check); err == nil && code == 0 {
				continue
			}
		}
		_, code, err := a.runIptables(ctx, argv)
		if err != nil {
			return err
		}
		if code != 0 && op != "-N" {
			return fmt.Errorf("iptables %s failed with exit code %d", op, code)
		}
	}
	for _, argv := range jumpDeleteRules(networkChain(boxID), a.listJumps(ctx), containerIP) {
		_, _, _ = a.runIptables(ctx, argv)
	}
	return nil
}

// teardownNetworkRules removes every jump to the box's chain, then the chain. Caller holds networkMu.
// Missing rules are not errors, so this is idempotent.
func (a *Agent) teardownNetworkRules(ctx context.Context, boxID string) {
	for _, argv := range jumpDeleteRules(networkChain(boxID), a.listJumps(ctx), "") {
		_, _, _ = a.runIptables(ctx, argv)
	}
	for _, argv := range teardownRules(boxID) {
		if _, _, err := a.runIptables(ctx, argv); err != nil {
			log.Printf("network teardown box=%s error=%q", boxID, err)
		}
	}
}

// dropNetworkRules removes a box's rules but keeps its saved policy (used on stop).
// Boxes without a saved policy have no rules, so the host firewall is not touched for them.
func (a *Agent) dropNetworkRules(ctx context.Context, boxID string) {
	a.networkMu.Lock()
	defer a.networkMu.Unlock()
	if _, err := os.Stat(a.networkPolicyPath(boxID)); errors.Is(err, os.ErrNotExist) {
		return
	}
	a.teardownNetworkRules(ctx, boxID)
}

// resolveAllowedDomains resolves IPv4 addresses for allowlisted domains. "*.x.com" resolves x.com only.
// ponytail: wildcard subdomains cannot be enumerated without an SNI proxy; upgrade path is a transparent SNI proxy.
func resolveAllowedDomains(ctx context.Context, policy types.NetworkPolicy) (map[string][]net.IP, error) {
	if policy.Mode != types.NetworkAllowlist {
		return nil, nil
	}
	resolved := make(map[string][]net.IP, len(policy.AllowDomains))
	for _, domain := range policy.AllowDomains {
		lookupCtx, cancel := context.WithTimeout(ctx, networkResolveTimeout)
		ips, err := net.DefaultResolver.LookupIP(lookupCtx, "ip4", strings.TrimPrefix(domain, "*."))
		cancel()
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", domain, err)
		}
		resolved[domain] = ips
	}
	return resolved, nil
}

// setNetworkPolicy stores policy and, when the box is running, applies it. A nil policy clears it.
// A stopped box only gets the policy saved; it is applied when the box starts.
// ponytail: one global lock serializes all box rule changes; per-box locks if many boxes change at once.
func (a *Agent) setNetworkPolicy(ctx context.Context, boxID string, policy *types.NetworkPolicy) error {
	a.networkMu.Lock()
	defer a.networkMu.Unlock()
	if policy == nil {
		a.teardownNetworkRules(ctx, boxID)
		delete(a.dnsHistory, boxID)
		return a.deleteNetworkPolicyFile(boxID)
	}
	container, err := a.docker.Inspect(ctx, boxName(boxID))
	if err != nil {
		return err
	}
	if container.State == "running" {
		if container.IP == "" {
			return errors.New("running box has no network address")
		}
		fresh, err := resolveAllowedDomains(ctx, *policy)
		if err != nil {
			return err
		}
		resolved := a.mergeDNSHistory(boxID, fresh, time.Now())
		if err := a.applyNetworkRules(ctx, boxID, container.IP, *policy, resolved); err != nil {
			// Fail closed: a half-built chain can fall through to ACCEPT, so stop the box.
			a.teardownNetworkRules(ctx, boxID)
			_ = a.docker.Stop(context.Background(), boxName(boxID), 10*time.Second)
			return err
		}
	}
	if err := a.saveNetworkPolicy(boxID, *policy); err != nil {
		if container.State == "running" {
			a.teardownNetworkRules(ctx, boxID)
			_ = a.docker.Stop(context.Background(), boxName(boxID), 10*time.Second)
		}
		return err
	}
	return nil
}

// mergeDNSHistory records the IPv4s just resolved for each domain of a box and returns, per domain, every IP
// seen within dnsHistoryWindow (capped at dnsHistoryCap). Entries are pruned here on every refresh, and domains
// no longer in the policy are dropped. Caller holds networkMu.
// ponytail: in-memory, so an agent restart forgets history. Ceiling: an IP that rotates out and is needed again
// after 30 minutes, or a domain with more than 64 live IPs, is still refused until the next refresh resolves it.
func (a *Agent) mergeDNSHistory(boxID string, fresh map[string][]net.IP, now time.Time) map[string][]net.IP {
	if a.dnsHistory == nil {
		a.dnsHistory = map[string]map[string]map[string]time.Time{}
	}
	history := a.dnsHistory[boxID]
	if history == nil {
		history = map[string]map[string]time.Time{}
		a.dnsHistory[boxID] = history
	}
	for domain := range history {
		if _, ok := fresh[domain]; !ok {
			delete(history, domain)
		}
	}
	union := make(map[string][]net.IP, len(fresh))
	for domain, ips := range fresh {
		seen := history[domain]
		if seen == nil {
			seen = map[string]time.Time{}
			history[domain] = seen
		}
		for _, ip := range ips {
			if v4 := ip.To4(); v4 != nil {
				seen[v4.String()] = now
			}
		}
		for ip, at := range seen {
			if now.Sub(at) > dnsHistoryWindow {
				delete(seen, ip)
			}
		}
		if len(seen) > dnsHistoryCap {
			keys := make([]string, 0, len(seen))
			for ip := range seen {
				keys = append(keys, ip)
			}
			sort.Slice(keys, func(i, j int) bool { return seen[keys[i]].After(seen[keys[j]]) })
			for _, ip := range keys[dnsHistoryCap:] {
				delete(seen, ip)
			}
		}
		for ip := range seen {
			union[domain] = append(union[domain], net.ParseIP(ip))
		}
	}
	return union
}

func (a *Agent) networkPolicyPath(boxID string) string {
	return filepath.Join(a.config.NetworkDir, boxID+".json")
}

func (a *Agent) saveNetworkPolicy(boxID string, policy types.NetworkPolicy) error {
	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.config.NetworkDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(a.networkPolicyPath(boxID), data, 0o600)
}

func (a *Agent) loadNetworkPolicy(boxID string) (*types.NetworkPolicy, error) {
	data, err := os.ReadFile(a.networkPolicyPath(boxID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var policy types.NetworkPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return nil, fmt.Errorf("decode network policy for %s: %w", boxID, err)
	}
	return &policy, nil
}

func (a *Agent) deleteNetworkPolicyFile(boxID string) error {
	if err := os.Remove(a.networkPolicyPath(boxID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RestoreNetworkPolicies re-applies saved policies for running boxes after an agent restart or reboot.
func (a *Agent) RestoreNetworkPolicies(ctx context.Context) {
	a.eachNetworkPolicy(ctx, func(boxID string, policy types.NetworkPolicy) {
		if err := a.setNetworkPolicy(ctx, boxID, &policy); err != nil {
			log.Printf("network policy restore box=%s error=%q", boxID, err)
		}
	})
}

// RunNetworkRefresh re-resolves allowlisted domains every minute and re-applies their rules.
// A failed refresh keeps the previous rules in place.
func (a *Agent) RunNetworkRefresh(ctx context.Context) {
	ticker := time.NewTicker(networkRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.eachNetworkPolicy(ctx, func(boxID string, policy types.NetworkPolicy) {
				if policy.Mode != types.NetworkAllowlist {
					return
				}
				if err := a.setNetworkPolicy(ctx, boxID, &policy); err != nil {
					log.Printf("network policy refresh box=%s error=%q", boxID, err)
				}
			})
		}
	}
}

func (a *Agent) eachNetworkPolicy(ctx context.Context, fn func(boxID string, policy types.NetworkPolicy)) {
	entries, err := os.ReadDir(a.config.NetworkDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		boxID := strings.TrimSuffix(entry.Name(), ".json")
		if ctx.Err() != nil || !boxIDPattern.MatchString(boxID) || entry.IsDir() {
			continue
		}
		policy, err := a.loadNetworkPolicy(boxID)
		if err != nil || policy == nil {
			log.Printf("network policy load box=%s error=%q", boxID, err)
			continue
		}
		fn(boxID, *policy)
	}
}

func (a *Agent) putNetwork(w http.ResponseWriter, r *http.Request) {
	var policy *types.NetworkPolicy
	if err := decodeJSON(w, r, &policy); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid network policy request")
		return
	}
	if policy != nil {
		if err := types.ValidateNetworkPolicy(*policy); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_network_policy", err.Error())
			return
		}
	}
	if err := a.setNetworkPolicy(r.Context(), r.PathValue("id"), policy); err != nil {
		a.writeOperationError(w, r, http.StatusBadGateway, "network_policy_failed", "cannot apply box network policy", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// EnsureMetadataBlock installs one DOCKER-USER rule that drops traffic from the boxes-net subnet to the
// cloud metadata address, so every box is blocked, not only boxes with a network policy. Idempotent.
func (a *Agent) EnsureMetadataBlock(ctx context.Context) error {
	subnet, err := a.docker.NetworkSubnet(ctx, "boxes-net")
	if err != nil {
		return fmt.Errorf("read boxes-net subnet: %w", err)
	}
	a.networkMu.Lock()
	defer a.networkMu.Unlock()
	rule := []string{"DOCKER-USER", "-s", subnet, "-d", types.MetadataCIDR, "-j", "DROP"}
	if _, code, err := a.runIptables(ctx, iptablesArgv(append([]string{"-C"}, rule...)...)); err == nil && code == 0 {
		return nil
	}
	_, code, err := a.runIptables(ctx, iptablesArgv(append([]string{"-I"}, rule...)...))
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("iptables -I DOCKER-USER failed with exit code %d", code)
	}
	return nil
}
