package agent

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tony1908/boxes-oss/internal/dockercli"
	"github.com/tony1908/boxes-oss/internal/snapshot"
	boxtypes "github.com/tony1908/boxes-oss/internal/types"
)

const netBoxID = "bx_netabcdefgh2"

func ipt(args ...string) []string { return append([]string{"iptables", "-w", "5"}, args...) }

func TestNetworkRulesDenyAll(t *testing.T) {
	got := networkRules(netBoxID, "172.18.0.7", boxtypes.NetworkPolicy{Mode: boxtypes.NetworkDenyAll}, nil)
	want := [][]string{
		ipt("-N", "BOXES-"+netBoxID),
		ipt("-F", "BOXES-"+netBoxID),
		ipt("-A", "BOXES-"+netBoxID, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"),
		ipt("-A", "BOXES-"+netBoxID, "-d", "169.254.169.254/32", "-j", "DROP"),
		ipt("-A", "BOXES-"+netBoxID, "-j", "DROP"),
		ipt("-I", "DOCKER-USER", "-s", "172.18.0.7/32", "-j", "BOXES-"+netBoxID),
		ipt("-I", "INPUT", "-s", "172.18.0.7/32", "-j", "BOXES-"+netBoxID),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deny-all rules:\n got %q\nwant %q", got, want)
	}
}

func TestNetworkRulesAllowlistResolvesDomainsAndDenyWins(t *testing.T) {
	policy := boxtypes.NetworkPolicy{Mode: boxtypes.NetworkAllowlist, AllowDomains: []string{"api.github.com"},
		AllowCIDRs: []string{"10.0.0.0/8"}, DenyCIDRs: []string{"10.1.0.0/16"}}
	resolved := map[string][]net.IP{"api.github.com": {net.ParseIP("140.82.112.5")}}
	got := networkRules(netBoxID, "172.18.0.7", policy, resolved)
	chain := "BOXES-" + netBoxID
	want := [][]string{
		ipt("-N", chain),
		ipt("-F", chain),
		ipt("-A", chain, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"),
		ipt("-A", chain, "-d", "10.1.0.0/16", "-j", "DROP"),
		ipt("-A", chain, "-d", "169.254.169.254/32", "-j", "DROP"),
		ipt("-A", chain, "-d", "10.0.0.0/8", "-j", "RETURN"),
		ipt("-A", chain, "-p", "udp", "--dport", "53", "-j", "RETURN"),
		ipt("-A", chain, "-p", "tcp", "--dport", "53", "-j", "RETURN"),
		ipt("-A", chain, "-d", "140.82.112.5/32", "-j", "RETURN"),
		ipt("-A", chain, "-j", "DROP"),
		ipt("-I", "DOCKER-USER", "-s", "172.18.0.7/32", "-j", chain),
		ipt("-I", "INPUT", "-s", "172.18.0.7/32", "-j", chain),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("allowlist rules:\n got %q\nwant %q", got, want)
	}
}

func TestNetworkRulesAllowAllSkipsInputJumpAndDefaultsEmptyMode(t *testing.T) {
	for _, mode := range []string{boxtypes.NetworkAllowAll, ""} {
		got := networkRules(netBoxID, "172.18.0.7", boxtypes.NetworkPolicy{Mode: mode}, nil)
		last := got[len(got)-1]
		if !reflect.DeepEqual(last, ipt("-I", "DOCKER-USER", "-s", "172.18.0.7/32", "-j", "BOXES-"+netBoxID)) {
			t.Fatalf("mode %q: last rule = %q, want only the DOCKER-USER jump", mode, last)
		}
		if rule := got[len(got)-2]; !reflect.DeepEqual(rule, ipt("-A", "BOXES-"+netBoxID, "-j", "RETURN")) {
			t.Fatalf("mode %q: final chain rule = %q, want RETURN", mode, rule)
		}
	}
}

func TestNetworkRulesWithoutIPProducesNothing(t *testing.T) {
	if got := networkRules(netBoxID, "", boxtypes.NetworkPolicy{Mode: boxtypes.NetworkDenyAll}, nil); got != nil {
		t.Fatalf("rules without container IP = %q, want nil", got)
	}
}

func TestJumpDeleteRulesAndTeardown(t *testing.T) {
	chain := "BOXES-" + netBoxID
	listing := "-N DOCKER-USER\n-A DOCKER-USER -s 172.18.0.7/32 -j " + chain + "\n-A DOCKER-USER -s 172.18.0.9/32 -j " + chain + "\n-A DOCKER-USER -j RETURN\n"
	if got, want := jumpDeleteRules(chain, listing, ""), [][]string{
		ipt("-D", "DOCKER-USER", "-s", "172.18.0.7/32", "-j", chain),
		ipt("-D", "DOCKER-USER", "-s", "172.18.0.9/32", "-j", chain),
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delete all jumps = %q, want %q", got, want)
	}
	if got, want := jumpDeleteRules(chain, listing, "172.18.0.9"), [][]string{
		ipt("-D", "DOCKER-USER", "-s", "172.18.0.7/32", "-j", chain),
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delete stale jumps = %q, want %q", got, want)
	}
	if got, want := teardownRules(netBoxID), [][]string{ipt("-F", chain), ipt("-X", chain)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("teardown = %q, want %q", got, want)
	}
}

func TestResolvedIPsAreSortedAndUnique(t *testing.T) {
	got := resolvedIPs(map[string][]net.IP{
		"a.example.com": {net.ParseIP("192.0.2.9"), net.ParseIP("192.0.2.1")},
		"b.example.com": {net.ParseIP("192.0.2.1"), net.ParseIP("2001:db8::1")},
	})
	if want := []string{"192.0.2.1", "192.0.2.9"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("resolvedIPs = %q, want %q", got, want)
	}
}

func TestCreateBoxWithDenyAllPolicyAppliesFirewallRules(t *testing.T) {
	t.Setenv("BOXES_TOOLS_IMAGE", "none")
	startBoxdTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	var dockerCalls, firewallCalls []callRecord
	inspectCount := 0
	dockerExec := testExecutor{calls: &dockerCalls, command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		switch {
		case len(argv) > 2 && argv[1] == "inspect" && argv[2] == "box-"+netBoxID:
			inspectCount++
			if inspectCount == 1 {
				return 1, nil
			}
			_, _ = io.WriteString(stdout, `[{"Id":"container-id","Name":"/box-`+netBoxID+`","State":{"Status":"running"},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"127.0.0.1"}}}}]`)
			return 0, nil
		case len(argv) > 1 && (argv[1] == "create" || argv[1] == "start"):
			if argv[1] == "create" {
				_, _ = io.WriteString(stdout, "container-id\n")
			}
			return 0, nil
		case len(argv) > 2 && argv[1] == "image" && argv[2] == "inspect":
			return 0, nil
		}
		return 1, nil
	}}
	firewall := testExecutor{calls: &firewallCalls, command: func(argv []string, _ []byte, _, _ io.Writer) (int, error) {
		if len(argv) > 3 && argv[3] == "-C" {
			return 1, nil // chain jump not present yet
		}
		return 0, nil
	}}
	service := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir(), NetworkDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	service.firewall = firewall

	body := `{"boxId":"` + netBoxID + `","image":"images.example.test/boxes/box:dev","type":"default","networkPolicy":{"mode":"deny-all"}}`
	recorder := httptest.NewRecorder()
	service.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/boxes", strings.NewReader(body)))
	if recorder.Code != 201 {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var got []string
	for _, call := range firewallCalls {
		if len(call.argv) > 3 && (call.argv[3] == "-C" || call.argv[3] == "-S") {
			continue // existence checks and listings, not rule changes
		}
		got = append(got, strings.Join(call.argv, " "))
	}
	want := []string{}
	for _, argv := range networkRules(netBoxID, "127.0.0.1", boxtypes.NetworkPolicy{Mode: boxtypes.NetworkDenyAll}, nil) {
		want = append(want, strings.Join(argv, " "))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("firewall commands:\n got %q\nwant %q", got, want)
	}
}

func TestCreateBoxRejectsInvalidNetworkPolicy(t *testing.T) {
	service := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir(), NetworkDir: t.TempDir()}, dockercli.New(testExecutor{}), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	body := `{"boxId":"` + netBoxID + `","image":"img","networkPolicy":{"mode":"allowlist","allowCidrs":["not-a-cidr"]}}`
	recorder := httptest.NewRecorder()
	service.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/boxes", strings.NewReader(body)))
	if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), "invalid_network_policy") {
		t.Fatalf("status = %d body=%s, want 400 invalid_network_policy", recorder.Code, recorder.Body.String())
	}
}
