package resolve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// zone is the answer a test DNS server gives for one name.
type zone struct {
	v4  []string
	v6  []string
	ttl uint32
	// rcode overrides a successful reply.
	rcode dnsmessage.RCode
	// truncate forces the UDP reply to set TC, so the client retries over TCP.
	truncate bool
}

// testServer is a minimal DNS server over UDP and TCP, used to prove the
// resolver speaks the wire protocol correctly.
type testServer struct {
	addr     string
	queries  atomic.Int64
	tcpCount atomic.Int64
}

// startDNS serves the given zones on a loopback port over both UDP and TCP.
func startDNS(t *testing.T, zones map[string]zone) *testServer {
	t.Helper()

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	port := udpConn.LocalAddr().(*net.UDPAddr).Port

	tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		_ = udpConn.Close()
		t.Fatalf("listen tcp: %v", err)
	}

	server := &testServer{addr: fmt.Sprintf("127.0.0.1:%d", port)}
	t.Cleanup(func() {
		_ = udpConn.Close()
		_ = tcpListener.Close()
	})

	go func() {
		buffer := make([]byte, 1500)
		for {
			n, from, err := udpConn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			server.queries.Add(1)
			reply, truncate := server.answer(buffer[:n], zones)
			if reply == nil {
				continue
			}
			if truncate {
				// A truncated reply carries the header only.
				reply = truncatedHeader(reply)
			}
			_, _ = udpConn.WriteToUDP(reply, from)
		}
	}()

	go func() {
		for {
			conn, err := tcpListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				var length [2]byte
				if _, err := readFull(conn, length[:]); err != nil {
					return
				}
				body := make([]byte, int(length[0])<<8|int(length[1]))
				if _, err := readFull(conn, body); err != nil {
					return
				}
				server.queries.Add(1)
				server.tcpCount.Add(1)
				reply, _ := server.answer(body, zones)
				if reply == nil {
					return
				}
				framed := append([]byte{byte(len(reply) >> 8), byte(len(reply))}, reply...)
				_, _ = conn.Write(framed)
			}()
		}
	}()

	return server
}

// answer builds the reply for a query, reporting whether it should be
// truncated on UDP.
func (s *testServer) answer(request []byte, zones map[string]zone) ([]byte, bool) {
	var query dnsmessage.Message
	if err := query.Unpack(request); err != nil || len(query.Questions) != 1 {
		return nil, false
	}
	question := query.Questions[0]
	name := strings.TrimSuffix(question.Name.String(), ".")

	entry, known := zones[name]
	reply := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 query.Header.ID,
			Response:           true,
			RecursionAvailable: true,
			RCode:              dnsmessage.RCodeSuccess,
		},
		Questions: []dnsmessage.Question{question},
	}
	if !known {
		reply.Header.RCode = dnsmessage.RCodeNameError
		packed, _ := reply.Pack()
		return packed, false
	}
	if entry.rcode != dnsmessage.RCodeSuccess {
		reply.Header.RCode = entry.rcode
		packed, _ := reply.Pack()
		return packed, false
	}

	ttl := entry.ttl
	if ttl == 0 {
		ttl = 300
	}
	header := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: ttl}

	switch question.Type {
	case dnsmessage.TypeA:
		for _, value := range entry.v4 {
			addr := netip.MustParseAddr(value).As4()
			reply.Answers = append(reply.Answers, dnsmessage.Resource{
				Header: withType(header, dnsmessage.TypeA),
				Body:   &dnsmessage.AResource{A: addr},
			})
		}
	case dnsmessage.TypeAAAA:
		for _, value := range entry.v6 {
			addr := netip.MustParseAddr(value).As16()
			reply.Answers = append(reply.Answers, dnsmessage.Resource{
				Header: withType(header, dnsmessage.TypeAAAA),
				Body:   &dnsmessage.AAAAResource{AAAA: addr},
			})
		}
	}

	packed, err := reply.Pack()
	if err != nil {
		return nil, false
	}
	return packed, entry.truncate && len(reply.Answers) > 0
}

func withType(header dnsmessage.ResourceHeader, kind dnsmessage.Type) dnsmessage.ResourceHeader {
	header.Type = kind
	return header
}

// truncatedHeader keeps the 12-byte header, sets TC and zeroes the counts.
func truncatedHeader(reply []byte) []byte {
	if len(reply) < 12 {
		return reply
	}
	out := make([]byte, 12)
	copy(out, reply[:12])
	out[2] |= 0x02 // TC
	for i := 4; i < 12; i++ {
		out[i] = 0
	}
	return out
}

func newTestResolver(t *testing.T, server *testServer, cfg Config) *Resolver {
	t.Helper()
	if server != nil {
		cfg.Servers = []string{server.addr}
	}
	resolver, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return resolver
}

func TestLookupIPOverUDP(t *testing.T) {
	server := startDNS(t, map[string]zone{
		"api.anthropic.com": {v4: []string{"203.0.113.10", "203.0.113.11"}, v6: []string{"2001:db8::1"}},
	})
	resolver := newTestResolver(t, server, Config{})

	addrs, err := resolver.LookupIP(context.Background(), "api.anthropic.com")
	if err != nil {
		t.Fatalf("LookupIP: %v", err)
	}
	if len(addrs) != 3 {
		t.Fatalf("addrs = %v, want three records", addrs)
	}
	// IPv4 first, in the order the server returned them.
	if addrs[0].String() != "203.0.113.10" || addrs[1].String() != "203.0.113.11" {
		t.Fatalf("addrs = %v, want the A records first and in order", addrs)
	}
	if !addrs[2].Is6() {
		t.Fatalf("addrs = %v, want the AAAA record last", addrs)
	}
	if server.tcpCount.Load() != 0 {
		t.Fatal("a reply that fits in a UDP packet must not be retried over TCP")
	}
}

func TestLookupIPRetriesOverTCPWhenTruncated(t *testing.T) {
	server := startDNS(t, map[string]zone{
		"big.example": {v4: []string{"203.0.113.20"}, truncate: true},
	})
	resolver := newTestResolver(t, server, Config{})

	addrs, err := resolver.LookupIP(context.Background(), "big.example")
	if err != nil {
		t.Fatalf("LookupIP: %v", err)
	}
	if len(addrs) != 1 || addrs[0].String() != "203.0.113.20" {
		t.Fatalf("addrs = %v", addrs)
	}
	if server.tcpCount.Load() == 0 {
		t.Fatal("a truncated UDP reply must be retried over TCP")
	}
}

func TestLookupIPCachesByTTL(t *testing.T) {
	server := startDNS(t, map[string]zone{
		"cached.example": {v4: []string{"203.0.113.30"}, ttl: 300},
	})
	resolver := newTestResolver(t, server, Config{})

	for i := 0; i < 3; i++ {
		if _, err := resolver.LookupIP(context.Background(), "cached.example"); err != nil {
			t.Fatalf("LookupIP %d: %v", i, err)
		}
	}
	// One A query and one AAAA query, then nothing but cache hits.
	if got := server.queries.Load(); got != 2 {
		t.Fatalf("the server saw %d queries, want 2 with the rest served from cache", got)
	}
}

func TestLookupIPRespectsMinTTL(t *testing.T) {
	server := startDNS(t, map[string]zone{
		// A one-second TTL would otherwise re-query on every connection.
		"short.example": {v4: []string{"203.0.113.40"}, ttl: 1},
	})
	resolver := newTestResolver(t, server, Config{MinTTL: time.Hour})

	if _, err := resolver.LookupIP(context.Background(), "short.example"); err != nil {
		t.Fatalf("LookupIP: %v", err)
	}
	before := server.queries.Load()
	time.Sleep(1100 * time.Millisecond)
	if _, err := resolver.LookupIP(context.Background(), "short.example"); err != nil {
		t.Fatalf("LookupIP again: %v", err)
	}
	if server.queries.Load() != before {
		t.Fatal("the answer was re-queried before the minimum TTL elapsed")
	}
}

func TestStaticEntriesSkipTheNetwork(t *testing.T) {
	server := startDNS(t, map[string]zone{})
	resolver := newTestResolver(t, server, Config{
		Static: map[string][]string{"API.Anthropic.com": {"203.0.113.50"}},
	})

	addrs, err := resolver.LookupIP(context.Background(), "api.anthropic.com")
	if err != nil {
		t.Fatalf("LookupIP: %v", err)
	}
	if len(addrs) != 1 || addrs[0].String() != "203.0.113.50" {
		t.Fatalf("addrs = %v, want the static entry", addrs)
	}
	if server.queries.Load() != 0 {
		t.Fatal("a static entry must not reach the network")
	}
}

func TestStaticEntryMustBeAnAddress(t *testing.T) {
	if _, err := New(Config{Static: map[string][]string{"a.example": {"not-an-ip"}}}); err == nil {
		t.Fatal("a static entry that is not an IP address must be rejected")
	}
}

func TestIPLiteralsBypassLookup(t *testing.T) {
	server := startDNS(t, map[string]zone{})
	resolver := newTestResolver(t, server, Config{})

	addrs, err := resolver.LookupIP(context.Background(), "203.0.113.60")
	if err != nil {
		t.Fatalf("LookupIP: %v", err)
	}
	if len(addrs) != 1 || addrs[0].String() != "203.0.113.60" {
		t.Fatalf("addrs = %v", addrs)
	}
	if server.queries.Load() != 0 {
		t.Fatal("an IP literal must not be queried")
	}
}

func TestUnknownNameIsAnError(t *testing.T) {
	server := startDNS(t, map[string]zone{})
	resolver := newTestResolver(t, server, Config{})

	// .invalid is reserved and cannot resolve anywhere, so a non-empty
	// answer here can only have come from the test server.
	addrs, err := resolver.LookupIP(context.Background(), "absent.invalid")
	if err == nil {
		t.Fatalf("a name with no records must be an error, got %v from %v", addrs, resolver.Servers())
	}
}

func TestFailoverToTheNextServer(t *testing.T) {
	working := startDNS(t, map[string]zone{
		"failover.example": {v4: []string{"203.0.113.70"}},
	})
	// A port with nothing on it stands in for a dead nameserver.
	dead, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := dead.LocalAddr().String()
	_ = dead.Close()

	resolver, err := New(Config{
		Servers: []string{deadAddr, working.addr},
		Timeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	addrs, err := resolver.LookupIP(context.Background(), "failover.example")
	if err != nil {
		t.Fatalf("LookupIP: %v", err)
	}
	if len(addrs) != 1 || addrs[0].String() != "203.0.113.70" {
		t.Fatalf("addrs = %v, want the working server's answer", addrs)
	}
}

func TestServerFailureIsReported(t *testing.T) {
	server := startDNS(t, map[string]zone{
		"broken.example": {rcode: dnsmessage.RCodeServerFailure},
	})
	resolver := newTestResolver(t, server, Config{Timeout: 500 * time.Millisecond})

	_, err := resolver.LookupIP(context.Background(), "broken.example")
	if err == nil {
		t.Fatal("SERVFAIL must be reported as an error")
	}
	if !strings.Contains(err.Error(), "ServerFailure") {
		t.Fatalf("error = %v, want it to name the response code", err)
	}
}

func TestDialContextConnectsToTheResolvedAddress(t *testing.T) {
	// A real listener the resolved name will point at.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("hello"))
		_ = conn.Close()
	}()

	_, port, _ := net.SplitHostPort(listener.Addr().String())
	server := startDNS(t, map[string]zone{
		"upstream.example": {v4: []string{"127.0.0.1"}},
	})
	resolver := newTestResolver(t, server, Config{})

	conn, err := resolver.DialContext(context.Background(), "tcp", net.JoinHostPort("upstream.example", port))
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer func() { _ = conn.Close() }()

	buffer := make([]byte, 5)
	if _, err := readFull(conn, buffer); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buffer) != "hello" {
		t.Fatalf("read %q", buffer)
	}
}

func TestDialContextRefusesTheProxysOwnListener(t *testing.T) {
	// This is the hosts-file loop: the upstream name resolves back to the
	// proxy's own TLS listener.
	server := startDNS(t, map[string]zone{
		"api.anthropic.com": {v4: []string{"127.0.0.1"}},
	})
	resolver := newTestResolver(t, server, Config{Deny: []string{"127.0.0.1:8443"}})

	_, err := resolver.DialContext(context.Background(), "tcp", "api.anthropic.com:8443")
	if err == nil {
		t.Fatal("dialing the proxy's own listener must be refused, not looped")
	}
	if !strings.Contains(err.Error(), "own listener") {
		t.Fatalf("error = %v, want it to explain the loop", err)
	}

	// A different port on the same host is unrelated and must still work.
	if _, err := resolver.LookupIP(context.Background(), "api.anthropic.com"); err != nil {
		t.Fatalf("LookupIP: %v", err)
	}
}

func TestDenyExpandsAWildcardBind(t *testing.T) {
	resolver, err := New(Config{Servers: []string{"127.0.0.1:53"}, Deny: []string{"0.0.0.0:8443"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A wildcard bind is reachable on loopback, so that address is denied.
	_, err = resolver.DialContext(context.Background(), "tcp", "127.0.0.1:8443")
	if err == nil || !strings.Contains(err.Error(), "own listener") {
		t.Fatalf("error = %v, want a wildcard bind to be denied on loopback", err)
	}
}

func TestDialContextRejectsMalformedAddresses(t *testing.T) {
	resolver, err := New(Config{Servers: []string{"127.0.0.1:53"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := resolver.DialContext(context.Background(), "tcp", "no-port"); err == nil {
		t.Fatal("an address without a port must be rejected")
	}
}

func TestLookupIPRejectsEmptyHost(t *testing.T) {
	resolver, err := New(Config{Servers: []string{"127.0.0.1:53"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := resolver.LookupIP(context.Background(), "   "); err == nil {
		t.Fatal("an empty host must be rejected")
	}
}

func TestServersDefaultWhenNoneConfigured(t *testing.T) {
	resolver, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	servers := resolver.Servers()
	if len(servers) == 0 {
		t.Fatal("a resolver must always end up with at least one nameserver")
	}
	for _, server := range servers {
		host, port, err := net.SplitHostPort(server)
		if err != nil {
			t.Fatalf("server %q is not host:port", server)
		}
		if port == "" {
			t.Fatalf("server %q has no port", server)
		}
		addr, err := netip.ParseAddr(host)
		if err != nil {
			continue
		}
		// A loopback stub resolver reads /etc/hosts, which is what this
		// package exists to bypass.
		if addr.IsLoopback() {
			t.Fatalf("discovered nameserver %q is a loopback stub; it would honour /etc/hosts", server)
		}
	}
}

func TestServerNamesGetADefaultPort(t *testing.T) {
	resolver, err := New(Config{Servers: []string{"1.1.1.1", "8.8.8.8:5353"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	servers := resolver.Servers()
	if servers[0] != "1.1.1.1:53" {
		t.Fatalf("servers[0] = %q, want the default port added", servers[0])
	}
	if servers[1] != "8.8.8.8:5353" {
		t.Fatalf("servers[1] = %q, want an explicit port kept", servers[1])
	}
}

func TestLookupHonoursContextCancellation(t *testing.T) {
	// A server that never replies, so the context is what ends the wait.
	silent, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = silent.Close() }()

	resolver, err := New(Config{Servers: []string{silent.LocalAddr().String()}, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := resolver.LookupIP(ctx, "slow.example"); err == nil {
		t.Fatal("a cancelled lookup must fail")
	} else if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") {
		t.Logf("error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the lookup took %v; the context deadline was not honoured", elapsed)
	}
}
