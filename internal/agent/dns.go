package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tony1908/paddock/internal/types"
	"golang.org/x/net/dns/dnsmessage"
)

const (
	dnsPort             = "53"
	dnsUpstreamTimeout  = 3 * time.Second
	dnsConnIdleTimeout  = 10 * time.Second
	dnsStubResolver     = "127.0.0.53" // systemd-resolved; its real upstreams live in the systemd resolv.conf
	dnsFallbackResolver = "1.1.1.1"
)

// dnsSource is the network policy of the box that owns a container IP.
type dnsSource struct {
	boxID  string
	policy types.NetworkPolicy
}

// StartDNS serves DNS on the paddock-net gateway. Boxes are created with --dns pointing here, so every lookup
// passes through the agent, which opens exactly the IPv4 addresses a box is allowed to reach before the box
// receives them. On error the agent keeps running and new boxes get Docker's default resolver.
func (a *Agent) StartDNS(ctx context.Context, gateway string) error {
	if net.ParseIP(gateway) == nil {
		return fmt.Errorf("invalid DNS gateway %q", gateway)
	}
	a.dnsUpstream = hostDNSUpstreams() // set before serving so the query goroutines see it
	addr, err := a.serveDNS(ctx, net.JoinHostPort(gateway, dnsPort))
	if err != nil {
		return err
	}
	a.networkMu.Lock()
	a.dnsGateway = gateway
	a.networkMu.Unlock()
	log.Printf("DNS forwarder listening on %s upstream=%v", addr, a.dnsUpstream)
	return nil
}

// serveDNS binds UDP and TCP on addr (TCP on the same port) and serves until ctx ends. It returns the bound UDP address.
func (a *Agent) serveDNS(ctx context.Context, addr string) (string, error) {
	udp, err := net.ListenPacket("udp", addr)
	if err != nil {
		return "", err
	}
	tcp, err := net.Listen("tcp", udp.LocalAddr().String())
	if err != nil {
		_ = udp.Close()
		return "", err
	}
	go func() {
		<-ctx.Done()
		_ = udp.Close()
		_ = tcp.Close()
	}()
	go a.serveDNSUDP(udp)
	go a.serveDNSTCP(tcp)
	return udp.LocalAddr().String(), nil
}

func (a *Agent) serveDNSUDP(conn net.PacketConn) {
	buf := make([]byte, 65535)
	for {
		n, src, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		query := append([]byte(nil), buf[:n]...)
		go func() {
			if resp := a.answerDNS(query, addrIP(src), "udp"); resp != nil {
				_, _ = conn.WriteTo(resp, src)
			}
		}()
	}
}

func (a *Agent) serveDNSTCP(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go a.serveDNSConn(conn)
	}
}

func (a *Agent) serveDNSConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	src := addrIP(conn.RemoteAddr())
	for {
		_ = conn.SetDeadline(time.Now().Add(dnsConnIdleTimeout))
		query, err := readDNSTCP(conn)
		if err != nil {
			return
		}
		resp := a.answerDNS(query, src, "tcp")
		if resp == nil {
			return
		}
		if err := writeDNSTCP(conn, resp); err != nil {
			return
		}
	}
}

// answerDNS returns the reply for one query from the box at src, or nil when the query cannot be answered.
// Allowed answers are returned only after their IPv4 addresses are open in the box's host firewall.
// ponytail: forwarded over UDP (or TCP for TCP queries) with no caching; add a cache if query volume matters.
func (a *Agent) answerDNS(query []byte, src, proto string) []byte {
	a.networkMu.Lock()
	source, known := a.dnsSources[src]
	a.networkMu.Unlock()
	if !known || source.policy.Mode == "" || source.policy.Mode == types.NetworkAllowAll {
		resp, err := a.forwardDNS(query, proto)
		if err != nil {
			return dnsReply(query, dnsmessage.RCodeServerFailure)
		}
		return resp
	}
	if source.policy.Mode == types.NetworkDenyAll {
		return dnsReply(query, dnsmessage.RCodeRefused)
	}
	var msg dnsmessage.Message
	if err := msg.Unpack(query); err != nil || len(msg.Questions) != 1 {
		return nil
	}
	pattern, ok := matchDNSDomain(source.policy.AllowDomains, msg.Questions[0].Name.String())
	if !ok {
		return dnsReply(query, dnsmessage.RCodeRefused)
	}
	raw, err := a.forwardDNS(query, proto)
	if err != nil {
		return dnsReply(query, dnsmessage.RCodeServerFailure)
	}
	var resp dnsmessage.Message
	if err := resp.Unpack(raw); err != nil {
		return dnsReply(query, dnsmessage.RCodeServerFailure)
	}
	// IPv6 is not filtered, so AAAA answers are dropped rather than handed to the box.
	var ips []net.IP
	var answers []dnsmessage.Resource
	for _, rr := range resp.Answers {
		switch body := rr.Body.(type) {
		case *dnsmessage.AResource:
			ips = append(ips, net.IP(body.A[:]))
			answers = append(answers, rr)
		case *dnsmessage.AAAAResource:
		default:
			answers = append(answers, rr)
		}
	}
	resp.Answers = answers
	out, err := resp.Pack()
	if err != nil {
		return dnsReply(query, dnsmessage.RCodeServerFailure)
	}
	if err := a.allowDNSAnswer(src, pattern, ips); err != nil {
		log.Printf("dns allow box=%s domain=%s error=%q", source.boxID, pattern, err)
		return dnsReply(query, dnsmessage.RCodeServerFailure)
	}
	return out
}

// allowDNSAnswer opens each IPv4 address for the box at src: it adds the address to the box's history and
// inserts a RETURN rule into its chain. The rule goes after the conntrack rule, the deny CIDRs and the metadata
// drop, so a deny always wins. Caller does not hold networkMu.
// ponytail: one iptables insert per new address per query; upgrade to an ipset per box if query rates get high.
func (a *Agent) allowDNSAnswer(src, domain string, ips []net.IP) error {
	a.networkMu.Lock()
	defer a.networkMu.Unlock()
	source, ok := a.dnsSources[src]
	if !ok {
		return errors.New("box network policy changed during lookup")
	}
	chain := networkChain(source.boxID)
	position := strconv.Itoa(len(source.policy.DenyCIDRs) + 3)
	now := time.Now()
	for _, ip := range ips {
		v4 := ip.To4()
		if v4 == nil {
			continue
		}
		addr := v4.String() + "/32"
		if _, code, err := a.runIptables(context.Background(), iptablesArgv("-C", chain, "-d", addr, "-j", "RETURN")); err != nil || code != 0 {
			_, code, err := a.runIptables(context.Background(), iptablesArgv("-I", chain, position, "-d", addr, "-j", "RETURN"))
			if err != nil {
				return err
			}
			if code != 0 {
				return fmt.Errorf("iptables -I %s failed with exit code %d", chain, code)
			}
		}
		if a.dnsHistory == nil {
			a.dnsHistory = map[string]map[string]map[string]time.Time{}
		}
		if a.dnsHistory[source.boxID] == nil {
			a.dnsHistory[source.boxID] = map[string]map[string]time.Time{}
		}
		if a.dnsHistory[source.boxID][domain] == nil {
			a.dnsHistory[source.boxID][domain] = map[string]time.Time{}
		}
		a.dnsHistory[source.boxID][domain][v4.String()] = now
	}
	return nil
}

// forwardDNS sends the raw query to each upstream in turn and returns the first reply.
func (a *Agent) forwardDNS(query []byte, proto string) ([]byte, error) {
	err := errors.New("no DNS upstream configured")
	for _, upstream := range a.dnsUpstream {
		var resp []byte
		if resp, err = exchangeDNS(upstream, query, proto); err == nil {
			return resp, nil
		}
	}
	return nil, err
}

func exchangeDNS(upstream string, query []byte, proto string) ([]byte, error) {
	conn, err := net.DialTimeout(proto, upstream, dnsUpstreamTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(dnsUpstreamTimeout))
	if proto == "tcp" {
		if err := writeDNSTCP(conn, query); err != nil {
			return nil, err
		}
		return readDNSTCP(conn)
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// dnsReply answers query with rcode and no records. It returns nil when query does not parse.
func dnsReply(query []byte, rcode dnsmessage.RCode) []byte {
	var msg dnsmessage.Message
	if err := msg.Unpack(query); err != nil {
		return nil
	}
	msg.Response = true
	msg.RCode = rcode
	msg.Answers, msg.Authorities, msg.Additionals = nil, nil, nil
	out, err := msg.Pack()
	if err != nil {
		return nil
	}
	return out
}

// matchDNSDomain returns the allowlist entry that name falls under. "*.x.com" covers x.com and every subdomain.
// Matching ignores case and a trailing dot.
func matchDNSDomain(domains []string, name string) (string, bool) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for _, domain := range domains {
		entry := strings.TrimSuffix(strings.ToLower(domain), ".")
		if base, ok := strings.CutPrefix(entry, "*."); ok {
			if name == base || strings.HasSuffix(name, "."+base) {
				return domain, true
			}
		} else if name == entry {
			return domain, true
		}
	}
	return "", false
}

// boxDNSGateway returns the forwarder address when a box was created with --dns pointing at it, and "" otherwise.
// Boxes created before the forwarder keep the unrestricted DNS rules, since their resolver cannot reach the forwarder.
// Caller holds networkMu.
func (a *Agent) boxDNSGateway(dns []string) string {
	if a.dnsGateway == "" {
		return ""
	}
	for _, server := range dns {
		if server == a.dnsGateway {
			return a.dnsGateway
		}
	}
	return ""
}

// setDNSSource records that containerIP belongs to boxID under policy. Caller holds networkMu.
func (a *Agent) setDNSSource(boxID, containerIP string, policy types.NetworkPolicy) {
	a.forgetDNSSource(boxID)
	if a.dnsSources == nil {
		a.dnsSources = map[string]dnsSource{}
	}
	a.dnsSources[containerIP] = dnsSource{boxID: boxID, policy: policy}
}

// forgetDNSSource drops every container IP of boxID. Caller holds networkMu.
func (a *Agent) forgetDNSSource(boxID string) {
	for ip, source := range a.dnsSources {
		if source.boxID == boxID {
			delete(a.dnsSources, ip)
		}
	}
}

// hostDNSUpstreams returns the host's resolvers, reading the systemd-resolved list when only its stub is configured.
func hostDNSUpstreams() []string {
	servers := resolvConfNameservers("/etc/resolv.conf")
	if len(servers) == 0 || (len(servers) == 1 && servers[0] == dnsStubResolver) {
		if upstreams := resolvConfNameservers("/run/systemd/resolve/resolv.conf"); len(upstreams) > 0 {
			servers = upstreams
		}
	}
	if len(servers) == 0 {
		servers = []string{dnsFallbackResolver}
	}
	out := make([]string, 0, len(servers))
	for _, server := range servers {
		out = append(out, net.JoinHostPort(server, dnsPort))
	}
	return out
}

func resolvConfNameservers(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var servers []string
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "nameserver" {
			servers = append(servers, fields[1])
		}
	}
	return servers
}

// addrIP returns the IP of a network address, or nil.
func addrIP(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return ""
}

func readDNSTCP(r io.Reader) ([]byte, error) {
	var length [2]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func writeDNSTCP(w io.Writer, msg []byte) error {
	if len(msg) > 65535 {
		return errors.New("DNS message too large")
	}
	out := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(out, uint16(len(msg)))
	copy(out[2:], msg)
	_, err := w.Write(out)
	return err
}
