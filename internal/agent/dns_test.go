package agent

import (
	"bytes"
	"context"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tony1908/boxes-oss/internal/dockercli"
	"github.com/tony1908/boxes-oss/internal/snapshot"
	boxtypes "github.com/tony1908/boxes-oss/internal/types"
	"golang.org/x/net/dns/dnsmessage"
)

const dnsTestBoxID = "bx_dnsabcdefgh2"

// dnsTestFirewall records every iptables call and reports chain checks as absent.
type dnsTestFirewall struct {
	mu     sync.Mutex
	calls  [][]string
	exists bool // report chain checks as present
}

func (f *dnsTestFirewall) Run(_ context.Context, argv []string, _ io.Reader, _, _ io.Writer) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), argv...))
	if len(argv) > 3 && argv[3] == "-C" && !f.exists {
		return 1, nil
	}
	return 0, nil
}

// argvs returns the joined argv of every call so far.
func (f *dnsTestFirewall) argvs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

// inserts returns the joined argv of every -I call made so far.
func (f *dnsTestFirewall) inserts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, argv := range f.calls {
		if len(argv) > 3 && argv[3] == "-I" {
			out = append(out, strings.Join(argv, " "))
		}
	}
	return out
}

func (f *dnsTestFirewall) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// startDNSTestUpstream serves every query with A 192.0.2.10, A 192.0.2.11 and AAAA 2001:db8::1, and counts queries.
func startDNSTestUpstream(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var queries atomic.Int32
	go func() {
		buf := make([]byte, 512)
		for {
			n, src, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			queries.Add(1)
			var query dnsmessage.Message
			if query.Unpack(buf[:n]) != nil {
				continue
			}
			name := query.Questions[0].Name
			resp := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: query.ID, Response: true},
				Questions: query.Questions,
				Answers: []dnsmessage.Resource{
					dnsTestRR(name, dnsmessage.TypeA, &dnsmessage.AResource{A: [4]byte{192, 0, 2, 10}}),
					dnsTestRR(name, dnsmessage.TypeA, &dnsmessage.AResource{A: [4]byte{192, 0, 2, 11}}),
					dnsTestRR(name, dnsmessage.TypeAAAA, &dnsmessage.AAAAResource{AAAA: [16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 1}}),
				},
			}
			if out, err := resp.Pack(); err == nil {
				_, _ = conn.WriteTo(out, src)
			}
		}
	}()
	return conn.LocalAddr().String(), &queries
}

func dnsTestRR(name dnsmessage.Name, typ dnsmessage.Type, body dnsmessage.ResourceBody) dnsmessage.Resource {
	return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: name, Type: typ, Class: dnsmessage.ClassINET, TTL: 60}, Body: body}
}

func dnsTestQuery(t *testing.T, name string, qtype dnsmessage.Type) []byte {
	t.Helper()
	out, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x4242, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: qtype, Class: dnsmessage.ClassINET}},
	}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// newDNSTestAgent serves DNS on loopback. The box is the loopback source 127.0.0.1 with the given policy;
// a nil policy leaves the source unknown, which means allow-all.
func newDNSTestAgent(t *testing.T, upstream string, policy *boxtypes.NetworkPolicy) (*Agent, *dnsTestFirewall, string) {
	t.Helper()
	service := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir(), NetworkDir: t.TempDir()}, dockercli.New(testExecutor{}), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	firewall := &dnsTestFirewall{}
	service.firewall = firewall
	service.dnsUpstream = []string{upstream}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	addr, err := service.serveDNS(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if policy != nil {
		service.networkMu.Lock()
		service.setDNSSource(dnsTestBoxID, "127.0.0.1", *policy)
		service.networkMu.Unlock()
	}
	return service, firewall, addr
}

func dnsTestExchange(t *testing.T, server string, query []byte) dnsmessage.Message {
	t.Helper()
	conn, err := net.Dial("udp", server)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(query); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no DNS reply: %v", err)
	}
	var msg dnsmessage.Message
	if err := msg.Unpack(buf[:n]); err != nil {
		t.Fatal(err)
	}
	return msg
}

func dnsTestAnswerIPs(msg dnsmessage.Message) (ips []string, aaaa int) {
	for _, rr := range msg.Answers {
		switch body := rr.Body.(type) {
		case *dnsmessage.AResource:
			ips = append(ips, net.IP(body.A[:]).String())
		case *dnsmessage.AAAAResource:
			aaaa++
		}
	}
	return ips, aaaa
}

func TestDNSAllowedNameOpensAnswerIPsBeforeReply(t *testing.T) {
	upstream, queries := startDNSTestUpstream(t)
	policy := boxtypes.NetworkPolicy{Mode: boxtypes.NetworkAllowlist, AllowDomains: []string{"api.github.com"}, DenyCIDRs: []string{"10.1.0.0/16"}}
	service, firewall, addr := newDNSTestAgent(t, upstream, &policy)

	msg := dnsTestExchange(t, addr, dnsTestQuery(t, "api.github.com.", dnsmessage.TypeA))

	if msg.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("rcode = %v, want success", msg.RCode)
	}
	ips, aaaa := dnsTestAnswerIPs(msg)
	if want := []string{"192.0.2.10", "192.0.2.11"}; !reflect.DeepEqual(ips, want) {
		t.Fatalf("answer IPs = %v, want %v", ips, want)
	}
	if aaaa != 0 {
		t.Fatalf("AAAA records reached the box: %d", aaaa)
	}
	// One deny CIDR puts the conntrack rule, the deny rule and the metadata drop before position 4.
	chain := networkChain(dnsTestBoxID)
	want := []string{
		"iptables -w 5 -I " + chain + " 4 -d 192.0.2.10/32 -j RETURN",
		"iptables -w 5 -I " + chain + " 4 -d 192.0.2.11/32 -j RETURN",
	}
	if got := firewall.inserts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("firewall inserts = %q, want %q", got, want)
	}
	service.networkMu.Lock()
	seen := service.dnsHistory[dnsTestBoxID]["api.github.com"]
	service.networkMu.Unlock()
	if _, ok := seen["192.0.2.10"]; !ok || len(seen) != 2 {
		t.Fatalf("dns history = %v, want both answer IPs", seen)
	}
	if queries.Load() != 1 {
		t.Fatalf("upstream queries = %d, want 1", queries.Load())
	}
}

func TestDNSAllowlistWildcardAndCaseInsensitiveMatch(t *testing.T) {
	upstream, queries := startDNSTestUpstream(t)
	policy := boxtypes.NetworkPolicy{Mode: boxtypes.NetworkAllowlist, AllowDomains: []string{"*.github.com"}}
	_, _, addr := newDNSTestAgent(t, upstream, &policy)

	for _, name := range []string{"API.GitHub.com.", "github.com.", "a.b.github.com."} {
		if msg := dnsTestExchange(t, addr, dnsTestQuery(t, name, dnsmessage.TypeA)); msg.RCode != dnsmessage.RCodeSuccess {
			t.Fatalf("%s: rcode = %v, want success", name, msg.RCode)
		}
	}
	if msg := dnsTestExchange(t, addr, dnsTestQuery(t, "notgithub.com.", dnsmessage.TypeA)); msg.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("notgithub.com: rcode = %v, want refused", msg.RCode)
	}
	if queries.Load() != 3 {
		t.Fatalf("upstream queries = %d, want 3 (refused name must not be forwarded)", queries.Load())
	}
}

func TestDNSAllowlistRefusesDisallowedNameWithoutForwarding(t *testing.T) {
	upstream, queries := startDNSTestUpstream(t)
	policy := boxtypes.NetworkPolicy{Mode: boxtypes.NetworkAllowlist, AllowDomains: []string{"api.github.com"}}
	_, firewall, addr := newDNSTestAgent(t, upstream, &policy)

	msg := dnsTestExchange(t, addr, dnsTestQuery(t, "exfil.example.com.", dnsmessage.TypeA))
	if msg.RCode != dnsmessage.RCodeRefused || len(msg.Answers) != 0 {
		t.Fatalf("reply = rcode %v with %d answers, want refused and empty", msg.RCode, len(msg.Answers))
	}
	if queries.Load() != 0 || firewall.count() != 0 {
		t.Fatalf("upstream queries = %d, firewall calls = %d, want none", queries.Load(), firewall.count())
	}
}

func TestDNSDenyAllRefusesWithoutForwarding(t *testing.T) {
	upstream, queries := startDNSTestUpstream(t)
	policy := boxtypes.NetworkPolicy{Mode: boxtypes.NetworkDenyAll}
	_, _, addr := newDNSTestAgent(t, upstream, &policy)

	if msg := dnsTestExchange(t, addr, dnsTestQuery(t, "api.github.com.", dnsmessage.TypeA)); msg.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("rcode = %v, want refused", msg.RCode)
	}
	if queries.Load() != 0 {
		t.Fatalf("upstream queries = %d, want 0", queries.Load())
	}
}

func TestDNSAllowlistStripsAAAAAnswers(t *testing.T) {
	upstream, _ := startDNSTestUpstream(t)
	policy := boxtypes.NetworkPolicy{Mode: boxtypes.NetworkAllowlist, AllowDomains: []string{"api.github.com"}}
	_, _, addr := newDNSTestAgent(t, upstream, &policy)

	msg := dnsTestExchange(t, addr, dnsTestQuery(t, "api.github.com.", dnsmessage.TypeA))
	ips, aaaa := dnsTestAnswerIPs(msg)
	if len(ips) != 2 || aaaa != 0 {
		t.Fatalf("answers: %d A, %d AAAA; want 2 A and no AAAA", len(ips), aaaa)
	}
}

func TestDNSAllowAllForwardsWithoutFirewallChanges(t *testing.T) {
	upstream, queries := startDNSTestUpstream(t)
	_, firewall, addr := newDNSTestAgent(t, upstream, nil)

	msg := dnsTestExchange(t, addr, dnsTestQuery(t, "anything.example.", dnsmessage.TypeA))
	if ips, _ := dnsTestAnswerIPs(msg); len(ips) != 2 {
		t.Fatalf("A answers = %v, want the upstream answer", ips)
	}
	if queries.Load() != 1 || firewall.count() != 0 {
		t.Fatalf("upstream queries = %d, firewall calls = %d, want 1 and 0", queries.Load(), firewall.count())
	}
}

func TestMatchDNSDomain(t *testing.T) {
	domains := []string{"api.github.com", "*.npmjs.org"}
	cases := map[string]bool{
		"api.github.com.":    true,
		"API.GitHub.com":     true,
		"github.com":         false,
		"x.api.github.com":   false,
		"registry.npmjs.org": true,
		"npmjs.org.":         true,
		"npmjs.org.evil.com": false,
		"evilnpmjs.org":      false,
	}
	for name, want := range cases {
		if _, got := matchDNSDomain(domains, name); got != want {
			t.Errorf("matchDNSDomain(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDNSTCPFramingRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeDNSTCP(&buf, []byte("query")); err != nil {
		t.Fatal(err)
	}
	got, err := readDNSTCP(&buf)
	if err != nil || string(got) != "query" {
		t.Fatalf("readDNSTCP = %q, %v; want query", got, err)
	}
}

func TestSetNetworkPolicyGatesDNSRulesOnCreateTimeResolver(t *testing.T) {
	const gateway = "172.18.0.1"
	chain := networkChain(dnsTestBoxID)
	cases := map[string]struct {
		dns  string
		want [][]string
	}{
		// Created before the forwarder: its resolver cannot reach the agent, so DNS stays unrestricted.
		"old box": {dns: `[]`, want: [][]string{
			ipt("-A", chain, "-p", "udp", "--dport", "53", "-j", "RETURN"),
			ipt("-A", chain, "-p", "tcp", "--dport", "53", "-j", "RETURN"),
		}},
		"box with forwarder": {dns: `["` + gateway + `"]`, want: [][]string{
			ipt("-A", chain, "-p", "udp", "-d", gateway+"/32", "--dport", "53", "-j", "RETURN"),
			ipt("-A", chain, "-p", "tcp", "-d", gateway+"/32", "--dport", "53", "-j", "RETURN"),
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			inspect := `[{"Id":"container-id","Name":"/box-` + dnsTestBoxID + `","State":{"Status":"running"},"HostConfig":{"Dns":` + tc.dns + `},"NetworkSettings":{"Networks":{"boxes-net":{"IPAddress":"172.18.0.7"}}}}]`
			dockerExec := testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
				if len(argv) > 1 && argv[1] == "inspect" {
					_, _ = io.WriteString(stdout, inspect)
				}
				return 0, nil
			}}
			firewall := &dnsTestFirewall{}
			service := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir(), NetworkDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
			service.firewall = firewall
			service.dnsGateway = gateway
			policy := boxtypes.NetworkPolicy{Mode: boxtypes.NetworkAllowlist, AllowCIDRs: []string{"10.0.0.0/8"}} // no domains: no live lookups
			if err := service.setNetworkPolicy(context.Background(), dnsTestBoxID, &policy); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !containsArgv(firewall.argvs(), want) {
					t.Fatalf("firewall calls lack %q", want)
				}
			}
		})
	}
}

func TestEnsureDNSInputAcceptsForwarderOnceAndNoOpWithoutForwarder(t *testing.T) {
	const gateway = "172.18.0.1"
	dockerExec := testExecutor{command: func(argv []string, _ []byte, stdout, _ io.Writer) (int, error) {
		if len(argv) > 1 && argv[1] == "network" {
			_, _ = io.WriteString(stdout, "172.18.0.0/16\n")
		}
		return 0, nil
	}}
	service := New(Config{NodeID: "nd_test", Runtime: "runc", TraefikDir: t.TempDir(), NetworkDir: t.TempDir()}, dockercli.New(dockerExec), snapshot.New(testExecutor{}, testExecutor{}, "/repo", "pw"))
	firewall := &dnsTestFirewall{}
	service.firewall = firewall

	if err := service.EnsureDNSInput(context.Background()); err != nil || len(firewall.argvs()) != 0 {
		t.Fatalf("without forwarder: err=%v calls=%q, want no calls", err, firewall.argvs())
	}
	service.dnsGateway = gateway
	if err := service.EnsureDNSInput(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"iptables -w 5 -I INPUT -s 172.18.0.0/16 -d 172.18.0.1/32 -p udp --dport 53 -j ACCEPT",
		"iptables -w 5 -I INPUT -s 172.18.0.0/16 -d 172.18.0.1/32 -p tcp --dport 53 -j ACCEPT",
	}
	if got := firewall.inserts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("INPUT inserts = %q, want %q", got, want)
	}
	// Rules already present: the -C checks succeed and nothing is inserted again.
	present := &dnsTestFirewall{exists: true}
	service.firewall = present
	if err := service.EnsureDNSInput(context.Background()); err != nil || len(present.inserts()) != 0 {
		t.Fatalf("second run: err=%v inserts=%q, want none", err, present.inserts())
	}
}
