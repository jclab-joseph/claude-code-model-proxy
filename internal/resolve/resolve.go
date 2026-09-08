// Package resolve is the proxy's own DNS client.
//
// It exists because the proxy must not be subject to the redirection it
// installs. In the hosts-file deployment, api.anthropic.com is pointed at this
// proxy's TLS listener; if the proxy then used the system resolver to reach the
// real API it would resolve its own address and loop. Querying nameservers
// directly bypasses /etc/hosts, nsswitch and any local stub resolver that
// honours them, so "the upstream" always means the real upstream.
//
// It answers A and AAAA queries over UDP, retries over TCP when a response is
// truncated, caches by TTL, and refuses to dial the proxy's own listeners.
package resolve

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	defaultTimeout = 5 * time.Second
	// defaultMinTTL keeps a hot cache from re-querying on every connection
	// when a zone publishes very short TTLs.
	defaultMinTTL = 30 * time.Second
	defaultMaxTTL = 1 * time.Hour
	// negativeTTL is how long a lookup failure is remembered. It is short so
	// a transient outage does not linger.
	negativeTTL = 5 * time.Second
	// udpResponseLimit is the largest response accepted without EDNS(0).
	// A larger answer arrives truncated and is retried over TCP.
	udpResponseLimit = 512
)

// publicFallbackServers are used when no usable nameserver can be discovered.
var publicFallbackServers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// Config configures the resolver.
type Config struct {
	// Servers are the nameservers to query, as host or host:port. When empty
	// they are discovered from /etc/resolv.conf, skipping loopback stubs.
	Servers []string
	// Timeout bounds a single query attempt.
	Timeout time.Duration
	// Static pins hostnames to addresses, answered without any query.
	Static map[string][]string
	// MinTTL and MaxTTL clamp the cache lifetime of an answer.
	MinTTL time.Duration
	MaxTTL time.Duration
	// Deny lists addresses the dialer refuses to connect to. The proxy's own
	// listeners go here, so a hosts-file redirect cannot make it call itself.
	Deny []string
	// DialTimeout bounds one connection attempt to a resolved address.
	DialTimeout time.Duration
}

// Resolver resolves names and dials connections.
type Resolver struct {
	servers     []string
	timeout     time.Duration
	dialTimeout time.Duration
	minTTL      time.Duration
	maxTTL      time.Duration
	static      map[string][]netip.Addr
	deny        map[netip.AddrPort]struct{}

	mu    sync.RWMutex
	cache map[string]*entry
}

// entry is one cached answer. A nil-free empty address list with an error
// records a negative result.
type entry struct {
	addrs   []netip.Addr
	err     error
	expires time.Time
}

// New builds a resolver.
func New(cfg Config) (*Resolver, error) {
	servers := normalizeServers(cfg.Servers)
	if len(servers) == 0 {
		servers = normalizeServers(systemServers())
	}
	if len(servers) == 0 {
		servers = publicFallbackServers
	}

	static := make(map[string][]netip.Addr, len(cfg.Static))
	for host, values := range cfg.Static {
		name := canonical(host)
		if name == "" {
			continue
		}
		for _, value := range values {
			addr, err := netip.ParseAddr(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("resolve: static entry %q for %q is not an IP address", value, host)
			}
			static[name] = append(static[name], addr)
		}
	}

	deny := make(map[netip.AddrPort]struct{}, len(cfg.Deny))
	for _, value := range cfg.Deny {
		for _, addrPort := range expandListenAddr(value) {
			deny[addrPort] = struct{}{}
		}
	}

	resolver := &Resolver{
		servers:     servers,
		timeout:     orDuration(cfg.Timeout, defaultTimeout),
		dialTimeout: orDuration(cfg.DialTimeout, 30*time.Second),
		minTTL:      orDuration(cfg.MinTTL, defaultMinTTL),
		maxTTL:      orDuration(cfg.MaxTTL, defaultMaxTTL),
		static:      static,
		deny:        deny,
		cache:       map[string]*entry{},
	}
	if resolver.maxTTL < resolver.minTTL {
		resolver.maxTTL = resolver.minTTL
	}
	return resolver, nil
}

// Servers reports the nameservers in use.
func (r *Resolver) Servers() []string {
	out := make([]string, len(r.servers))
	copy(out, r.servers)
	return out
}

// LookupIP resolves host to addresses, consulting the static table and the
// cache first.
func (r *Resolver) LookupIP(ctx context.Context, host string) ([]netip.Addr, error) {
	name := canonical(host)
	if name == "" {
		return nil, fmt.Errorf("resolve: empty host")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	if addrs, ok := r.static[name]; ok && len(addrs) > 0 {
		return addrs, nil
	}
	if addrs, err, ok := r.cached(name); ok {
		return addrs, err
	}

	addrs, ttl, err := r.query(ctx, name)
	r.store(name, addrs, ttl, err)
	return addrs, err
}

func (r *Resolver) cached(name string) ([]netip.Addr, error, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cached, ok := r.cache[name]
	if !ok || time.Now().After(cached.expires) {
		return nil, nil, false
	}
	return cached.addrs, cached.err, true
}

func (r *Resolver) store(name string, addrs []netip.Addr, ttl time.Duration, err error) {
	lifetime := negativeTTL
	if err == nil && len(addrs) > 0 {
		lifetime = ttl
		if lifetime < r.minTTL {
			lifetime = r.minTTL
		}
		if lifetime > r.maxTTL {
			lifetime = r.maxTTL
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[name] = &entry{addrs: addrs, err: err, expires: time.Now().Add(lifetime)}
}

// query asks each nameserver in turn until one answers. It returns the
// addresses and the smallest TTL among the records that produced them.
func (r *Resolver) query(ctx context.Context, name string) ([]netip.Addr, time.Duration, error) {
	dnsName, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return nil, 0, fmt.Errorf("resolve: %q is not a valid DNS name: %w", name, err)
	}

	var errs []error
	for _, server := range r.servers {
		// IPv4 first: it is the more reliable path out of a typical
		// developer machine, and the order is preserved when dialing.
		v4, ttl4, err4 := r.ask(ctx, server, dnsName, dnsmessage.TypeA)
		v6, ttl6, err6 := r.ask(ctx, server, dnsName, dnsmessage.TypeAAAA)

		addrs := append(append([]netip.Addr(nil), v4...), v6...)
		if len(addrs) > 0 {
			return addrs, smallestTTL(ttl4, ttl6), nil
		}
		if err4 != nil {
			errs = append(errs, fmt.Errorf("%s A: %w", server, err4))
		}
		if err6 != nil {
			errs = append(errs, fmt.Errorf("%s AAAA: %w", server, err6))
		}
		// A server that answered with no records has given a real answer;
		// asking the next server would not change it.
		if err4 == nil && err6 == nil {
			return nil, 0, fmt.Errorf("resolve: no address records for %q", name)
		}
	}
	if len(errs) == 0 {
		return nil, 0, fmt.Errorf("resolve: no address records for %q", name)
	}
	return nil, 0, fmt.Errorf("resolve: lookup %q failed: %w", name, errors.Join(errs...))
}

// smallestTTL picks the shorter of two lifetimes, ignoring zeroes that mean
// "no records of this type".
func smallestTTL(a, b time.Duration) time.Duration {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// ask performs one query, retrying over TCP when the UDP answer is truncated.
func (r *Resolver) ask(ctx context.Context, server string, name dnsmessage.Name, kind dnsmessage.Type) ([]netip.Addr, time.Duration, error) {
	id := uint16(rand.UintN(1 << 16))
	question := dnsmessage.Question{Name: name, Type: kind, Class: dnsmessage.ClassINET}

	request, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{question},
	}).Pack()
	if err != nil {
		return nil, 0, fmt.Errorf("pack query: %w", err)
	}

	response, err := r.exchangeUDP(ctx, server, request)
	if err != nil {
		return nil, 0, err
	}
	if response.Header.Truncated {
		if response, err = r.exchangeTCP(ctx, server, request); err != nil {
			return nil, 0, err
		}
	}

	// Matching the id and the question is what makes an off-path forged
	// answer fail rather than be believed.
	if response.Header.ID != id {
		return nil, 0, fmt.Errorf("response id %d does not match query id %d", response.Header.ID, id)
	}
	if len(response.Questions) != 1 || response.Questions[0].Name.String() != name.String() || response.Questions[0].Type != kind {
		return nil, 0, fmt.Errorf("response does not answer the question that was asked")
	}
	switch response.Header.RCode {
	case dnsmessage.RCodeSuccess, dnsmessage.RCodeNameError:
	default:
		return nil, 0, fmt.Errorf("server returned %s", response.Header.RCode)
	}

	// A CNAME chain files the addresses under a different owner name, so
	// every address record in the answer counts.
	var addrs []netip.Addr
	var ttl time.Duration
	for _, answer := range response.Answers {
		var addr netip.Addr
		switch body := answer.Body.(type) {
		case *dnsmessage.AResource:
			addr = netip.AddrFrom4(body.A)
		case *dnsmessage.AAAAResource:
			addr = netip.AddrFrom16(body.AAAA)
		default:
			continue
		}
		addrs = append(addrs, addr)
		ttl = smallestTTL(ttl, time.Duration(answer.Header.TTL)*time.Second)
	}
	return addrs, ttl, nil
}

func (r *Resolver) exchangeUDP(ctx context.Context, server string, request []byte) (*dnsmessage.Message, error) {
	queryCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	// A plain dialer: the resolver must never resolve through itself.
	conn, err := (&net.Dialer{}).DialContext(queryCtx, "udp", server)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := queryCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(request); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	buffer := make([]byte, udpResponseLimit)
	n, err := conn.Read(buffer)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	var response dnsmessage.Message
	if err := response.Unpack(buffer[:n]); err != nil {
		return nil, fmt.Errorf("unpack: %w", err)
	}
	return &response, nil
}

func (r *Resolver) exchangeTCP(ctx context.Context, server string, request []byte) (*dnsmessage.Message, error) {
	queryCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(queryCtx, "tcp", server)
	if err != nil {
		return nil, fmt.Errorf("dial tcp: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := queryCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// DNS over TCP prefixes each message with its 16-bit length.
	framed := make([]byte, 2+len(request))
	framed[0] = byte(len(request) >> 8)
	framed[1] = byte(len(request))
	copy(framed[2:], request)
	if _, err := conn.Write(framed); err != nil {
		return nil, fmt.Errorf("write tcp: %w", err)
	}

	var length [2]byte
	if _, err := readFull(conn, length[:]); err != nil {
		return nil, fmt.Errorf("read tcp length: %w", err)
	}
	size := int(length[0])<<8 | int(length[1])
	if size == 0 || size > 65535 {
		return nil, fmt.Errorf("read tcp: implausible response length %d", size)
	}
	body := make([]byte, size)
	if _, err := readFull(conn, body); err != nil {
		return nil, fmt.Errorf("read tcp body: %w", err)
	}

	var response dnsmessage.Message
	if err := response.Unpack(body); err != nil {
		return nil, fmt.Errorf("unpack tcp: %w", err)
	}
	return &response, nil
}

// DialContext resolves addr with this resolver and connects to it. It is the
// DialContext of every outbound http.Transport in the proxy.
func (r *Resolver) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("resolve: %q is not host:port: %w", addr, err)
	}

	addrs, err := r.LookupIP(ctx, host)
	if err != nil {
		return nil, err
	}

	portNumber, err := net.LookupPort(network, port)
	if err != nil {
		return nil, fmt.Errorf("resolve: bad port in %q: %w", addr, err)
	}

	dialer := &net.Dialer{Timeout: r.dialTimeout}
	var errs []error
	for _, candidate := range addrs {
		target := netip.AddrPortFrom(candidate.Unmap(), uint16(portNumber))
		if _, denied := r.deny[target]; denied {
			// Almost always a hosts-file entry pointing the upstream name at
			// this proxy; connecting would be an infinite loop.
			return nil, fmt.Errorf("resolve: %s resolves to this proxy's own listener %s; "+
				"the internal resolver was bypassed or a static entry points back here", host, target)
		}
		conn, err := dialer.DialContext(ctx, network, target.String())
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("resolve: no usable address for %q", host)
	}
	return nil, fmt.Errorf("resolve: could not connect to %q: %w", host, errors.Join(errs...))
}

func readFull(conn net.Conn, buffer []byte) (int, error) {
	read := 0
	for read < len(buffer) {
		n, err := conn.Read(buffer[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

// systemServers reads the nameservers from /etc/resolv.conf, skipping loopback
// addresses. A local stub resolver (systemd-resolved on 127.0.0.53, dnsmasq)
// consults /etc/hosts itself, which is exactly what this package must avoid.
func systemServers() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}

	var servers []string
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		addr, err := netip.ParseAddr(fields[1])
		if err != nil || addr.IsLoopback() {
			continue
		}
		servers = append(servers, fields[1])
	}
	return servers
}

func normalizeServers(servers []string) []string {
	out := make([]string, 0, len(servers))
	for _, server := range servers {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(server); err != nil {
			server = net.JoinHostPort(server, "53")
		}
		out = append(out, server)
	}
	return out
}

// expandListenAddr turns a listen address into the concrete addresses a client
// could reach it at, so a wildcard bind is denied on loopback too.
func expandListenAddr(value string) []netip.AddrPort {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return nil
	}
	portNumber, err := net.LookupPort("tcp", port)
	if err != nil {
		return nil
	}

	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return []netip.AddrPort{
			netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(portNumber)),
			netip.AddrPortFrom(netip.MustParseAddr("::1"), uint16(portNumber)),
		}
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return nil
	}
	return []netip.AddrPort{netip.AddrPortFrom(addr.Unmap(), uint16(portNumber))}
}

func canonical(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

func orDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}
