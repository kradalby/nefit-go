package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// EDNS UDP sizes and TCP message lengths are both unsigned 16-bit values.
	maxDNSMessageSize = 1<<16 - 1
	// Advertised EDNS size; small enough to avoid IP fragmentation.
	dnsEDNSSize = 1232
	// Short so a takeover can be undone without waiting out device caches.
	dnsTTL = 30
	// Per-protocol worker budget, so idle TCP connections cannot starve UDP.
	dnsWorkers = 16

	dnsRCodeBadVersion dnsmessage.RCode = 16
)

var errDNSMismatch = errors.New("DNS upstream reply does not match query")

// DNSConfig optionally supplies a device-scoped DNS takeover endpoint. Configure
// the thermostat/DHCP resolver to use it, or prefer a scoped router DNS record.
// It answers the device's XMPP host and its _xmpp-client._tcp SRV name with
// Address, owns every name below the host, and answers only the device IP
// unless AllowedPeers adds more. Other names are refused unless ForwardAddress
// is set. It is not a LAN resolver.
type DNSConfig struct {
	// ListenAddress is a specific IP:port, of the family of the device IP or
	// an AllowedPeers entry; wildcard binds are rejected because UDP replies
	// could leave from a different source address than queried.
	ListenAddress string
	// Address is the A or AAAA answer, of the device IP's family: an address
	// the device reaches the XMPP listener on.
	Address netip.Addr
	// AllowedPeers permits a known DNS proxy in addition to the thermostat.
	AllowedPeers []netip.Addr
	// ForwardAddress is an upstream resolver IP:port for names not owned here.
	ForwardAddress string
	// XMPPPort overrides the SRV port, which defaults to the XMPP listener's.
	XMPPPort uint16

	// New takes these from the device session, so DNS sends the device only
	// where the XMPP listener admits it.
	hostname string
	deviceIP netip.Addr
}

// dnsEndpoint serves DNSConfig over UDP and TCP on one address.
type dnsEndpoint struct {
	host, srv      string // ASCII-lowercase FQDNs
	address        netip.Addr
	xmppPort       uint16
	allow          []netip.Addr
	upstream       string
	tcpTimeout     time.Duration
	forwardTimeout time.Duration

	udp      net.PacketConn
	tcp      net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	once     sync.Once
	udpSlots chan struct{}
	tcpSlots chan struct{}
	// forwards caps forwards in flight below the worker budget, so spoofed
	// queries to a slow resolver cannot crowd out the owned names.
	forwards chan struct{}
	logger   atomic.Pointer[slog.Logger] // nil until Server.SetLogger
}

// openDNS validates cfg and starts the UDP and TCP listeners on the same port.
func openDNS(cfg DNSConfig) (*dnsEndpoint, error) {
	s, err := newDNS(cfg)
	if err != nil {
		return nil, err
	}
	udp, tcp, err := listenDNS(cfg.ListenAddress, net.ListenPacket, net.Listen)
	if err != nil {
		return nil, err
	}
	s.start(udp, tcp)
	return s, nil
}

func newDNS(cfg DNSConfig) (*dnsEndpoint, error) {
	if !cfg.Address.IsValid() || !cfg.deviceIP.IsValid() {
		return nil, fmt.Errorf("DNS address and device IP are required")
	}
	listen, err := netip.ParseAddrPort(cfg.ListenAddress)
	if err != nil || peerAddr(listen.Addr()).IsUnspecified() {
		return nil, fmt.Errorf("DNS listen address %q must be a specific IP:port", cfg.ListenAddress)
	}
	host := cfg.hostname + "." // canonical, from client.Config.WithDefaults
	srv := "_xmpp-client._tcp." + host
	// NewName checks only total length; packing also rejects empty (including
	// an empty or root hostname) and overlong labels.
	name, err := dnsmessage.NewName(srv)
	if err == nil {
		_, err = (&dnsmessage.Message{Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET}}}).Pack()
	}
	if err != nil {
		return nil, fmt.Errorf("DNS hostname %q is invalid", cfg.hostname)
	}
	if cfg.ForwardAddress != "" {
		// A literal avoids a host lookup per forwarded query, which could recurse here.
		forward, err := netip.ParseAddrPort(cfg.ForwardAddress)
		if err != nil || peerAddr(forward.Addr()).IsUnspecified() || forward.Port() == 0 {
			return nil, fmt.Errorf("DNS forward address %q must be an IP:port", cfg.ForwardAddress)
		}
		if peerAddr(forward.Addr()) == peerAddr(listen.Addr()) && forward.Port() == listen.Port() {
			return nil, fmt.Errorf("DNS forward address %q is the listen address", cfg.ForwardAddress)
		}
	}
	allow := []netip.Addr{peerAddr(cfg.deviceIP)}
	for _, peer := range cfg.AllowedPeers {
		allow = append(allow, peerAddr(peer))
	}
	// The device connects to Address from its IP, which the XMPP listener
	// admits; and queries reach the listener only from a peer of its family.
	if peerAddr(cfg.Address).Is4() != peerAddr(cfg.deviceIP).Is4() ||
		!slices.ContainsFunc(allow, func(a netip.Addr) bool { return a.Is4() == peerAddr(listen.Addr()).Is4() }) {
		return nil, fmt.Errorf("DNS address must share the device IP's family, and the listen address an allowed peer's")
	}
	return &dnsEndpoint{
		host: host, srv: srv, address: cfg.Address.Unmap(), xmppPort: cfg.XMPPPort, allow: allow,
		upstream: cfg.ForwardAddress, tcpTimeout: 3 * time.Second, forwardTimeout: 2 * time.Second,
		udpSlots: make(chan struct{}, dnsWorkers), tcpSlots: make(chan struct{}, dnsWorkers),
		forwards: make(chan struct{}, dnsWorkers/2),
	}, nil
}

func (s *dnsEndpoint) start(udp net.PacketConn, tcp net.Listener) {
	s.udp, s.tcp = udp, tcp
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.wg.Go(s.serveUDP)
	s.wg.Go(s.serveTCP)
}

// UDP and TCP allocate ephemeral ports independently. Retry only when the TCP
// port selected by UDP has already been claimed; explicit ports remain exact.
func listenDNS(address string, packet func(string, string) (net.PacketConn, error), stream func(string, string) (net.Listener, error)) (net.PacketConn, net.Listener, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, nil, err
	}
	for attempt := 0; attempt < 32; attempt++ {
		udp, err := packet("udp", address)
		if err != nil {
			return nil, nil, err
		}
		tcp, err := stream("tcp", udp.LocalAddr().String())
		if err == nil {
			return udp, tcp, nil
		}
		_ = udp.Close()
		if port != "0" || !errors.Is(err, syscall.EADDRINUSE) {
			return nil, nil, err
		}
	}
	return nil, nil, fmt.Errorf("could not allocate shared UDP/TCP DNS port: %w", syscall.EADDRINUSE)
}

func (s *dnsEndpoint) Close() error {
	s.once.Do(func() { s.cancel(); _ = s.udp.Close(); _ = s.tcp.Close() })
	s.wg.Wait()
	return nil
}

// Remote addresses carry their interface zone while configured ones usually do
// not; the listener is bound to one address, so the zone adds nothing.
func peerAddr(a netip.Addr) netip.Addr { return a.Unmap().WithZone("") }

func (s *dnsEndpoint) allowed(addr net.Addr) bool {
	peer, err := netip.ParseAddrPort(addr.String())
	return err == nil && slices.Contains(s.allow, peerAddr(peer.Addr()))
}

func (s *dnsEndpoint) serveUDP() {
	buf := make([]byte, maxDNSMessageSize)
	var delay time.Duration
	for {
		n, addr, err := s.udp.ReadFrom(buf)
		if err != nil {
			if !s.backoff(&delay, "udp", err) {
				return
			}
			continue
		}
		delay = 0
		if !s.allowed(addr) {
			continue
		}
		raw := bytes.Clone(buf[:n])
		// Waiting rather than dropping keeps a flood from losing the device's
		// own query: forwards hold at most half the slots, the rest free up
		// in microseconds, and the kernel buffers what arrives meanwhile.
		select {
		case s.udpSlots <- struct{}{}:
		case <-s.ctx.Done():
			return
		}
		s.wg.Go(func() {
			defer func() { <-s.udpSlots }()
			if reply, err := s.answer(raw, "udp"); err == nil {
				_, _ = s.udp.WriteTo(reply, addr)
			}
		})
	}
}

func (s *dnsEndpoint) serveTCP() {
	var delay time.Duration
	for {
		conn, err := s.tcp.Accept()
		if err != nil {
			if !s.backoff(&delay, "tcp", err) {
				return
			}
			continue
		}
		delay = 0
		if !s.allowed(conn.RemoteAddr()) {
			_ = conn.Close()
			continue
		}
		select {
		case s.tcpSlots <- struct{}{}:
			s.wg.Go(func() {
				defer func() { <-s.tcpSlots }()
				s.serveConn(conn)
			})
		default:
			_ = conn.Close()
		}
	}
}

func (s *dnsEndpoint) serveConn(conn net.Conn) {
	defer conn.Close() //nolint:errcheck
	stop := context.AfterFunc(s.ctx, func() { _ = conn.Close() })
	defer stop()
	for {
		// One deadline covers the length and body to bound slow senders.
		if conn.SetReadDeadline(time.Now().Add(s.tcpTimeout)) != nil {
			return
		}
		raw, err := readDNSFrame(conn)
		if err != nil {
			return
		}
		reply, err := s.answer(raw, "tcp")
		if err != nil || conn.SetWriteDeadline(time.Now().Add(s.tcpTimeout)) != nil || writeDNSFrame(conn, reply) != nil {
			return
		}
	}
}

// backoff retries every listener error with a capped delay, so a protocol
// never stops silently; only Close ends a listener.
func (s *dnsEndpoint) backoff(delay *time.Duration, network string, err error) bool {
	if s.ctx.Err() != nil {
		return false
	}
	*delay = min(max(2**delay, 5*time.Millisecond), time.Second)
	logger := s.logger.Load()
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("DNS listener retrying", "network", network, "error", err, "delay", *delay)
	select {
	case <-s.ctx.Done():
		return false
	case <-time.After(*delay):
		return true
	}
}

func readDNSFrame(r io.Reader) ([]byte, error) {
	var length [2]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return nil, err
	}
	raw := make([]byte, binary.BigEndian.Uint16(length[:]))
	_, err := io.ReadFull(r, raw)
	return raw, err
}

func writeDNSFrame(w io.Writer, raw []byte) error {
	_, err := w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(raw))), raw...))
	return err
}

func (s *dnsEndpoint) answer(raw []byte, network string) ([]byte, error) {
	var query dnsmessage.Message
	if err := query.Unpack(raw); err != nil {
		return nil, err
	}
	if query.Response {
		return nil, errors.New("DNS response is not a query")
	}
	response := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: query.ID, OpCode: query.OpCode, Response: true, RecursionDesired: query.RecursionDesired, RecursionAvailable: s.upstream != ""},
		Questions: query.Questions,
	}
	var opts int
	var version uint32
	var dnssecOK bool
	// RFC 1035: without EDNS a UDP reply fits 512 bytes; RFC 6891 lets the
	// query raise that, never lower it.
	udpLimit := 512
	for _, rr := range query.Additionals {
		if rr.Header.Type == dnsmessage.TypeOPT {
			opts++
			version = rr.Header.TTL >> 16 & 0xff
			dnssecOK = rr.Header.TTL&0x8000 != 0
			udpLimit = max(512, int(rr.Header.Class))
		}
	}
	var rcode dnsmessage.RCode
	switch {
	case query.OpCode != 0:
		rcode = dnsmessage.RCodeNotImplemented
	case len(query.Questions) != 1 || opts > 1:
		rcode = dnsmessage.RCodeFormatError
	case version != 0:
		rcode = dnsRCodeBadVersion
	default:
		q := query.Questions[0]
		name := foldDNS(q.Name.String())
		switch {
		case name != s.host && !strings.HasSuffix(name, "."+s.host):
			if s.upstream == "" {
				rcode = dnsmessage.RCodeRefused
				break
			}
			if reply, err := s.forward(raw, network, q); err == nil {
				return reply, nil
			}
			rcode = dnsmessage.RCodeServerFailure
		case q.Class != dnsmessage.ClassINET:
			rcode = dnsmessage.RCodeRefused
		default:
			response.Authoritative = true
			response.Answers, rcode = s.records(name, q)
			if len(response.Answers) == 0 {
				// RFC 2308: negative answers carry the zone's SOA so caches can keep them.
				response.Authorities = []dnsmessage.Resource{s.soa()}
			}
		}
	}
	if opts > 0 {
		// RFC 6891 requires an OPT in replies to queries carrying one.
		var opt dnsmessage.ResourceHeader
		// RFC 3225: the DO bit is copied from the query.
		_ = opt.SetEDNS0(dnsEDNSSize, rcode, dnssecOK)
		response.Additionals = []dnsmessage.Resource{{Header: opt, Body: &dnsmessage.OPTResource{}}}
	}
	response.RCode = rcode & 0xf
	packed, err := response.Pack()
	if err == nil && network == "udp" && len(packed) > udpLimit {
		// Too large for the client: say so, so it retries over TCP.
		response.Truncated = true
		response.Answers, response.Authorities = nil, nil
		packed, err = response.Pack()
	}
	return packed, err
}

// records answers a name in the owned subtree. Only the hostname, the SRV name
// and the empty non-terminal between them exist. Anything else is NXDOMAIN,
// which per RFC 8020 also denies every name below it; that holds here.
func (s *dnsEndpoint) records(name string, q dnsmessage.Question) ([]dnsmessage.Resource, dnsmessage.RCode) {
	if name != s.srv && !strings.HasSuffix(s.srv, "."+name) {
		return nil, dnsmessage.RCodeNameError
	}
	want := func(t dnsmessage.Type) bool { return q.Type == t || q.Type == dnsmessage.TypeALL }
	header := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: dnsTTL}
	var body dnsmessage.ResourceBody
	switch {
	case name == s.host && s.address.Is4() && want(dnsmessage.TypeA):
		body = &dnsmessage.AResource{A: s.address.As4()}
	case name == s.host && s.address.Is6() && want(dnsmessage.TypeAAAA):
		body = &dnsmessage.AAAAResource{AAAA: s.address.As16()}
	case name == s.srv && want(dnsmessage.TypeSRV):
		body = &dnsmessage.SRVResource{Port: s.xmppPort, Target: dnsmessage.MustNewName(s.host)}
	case name == s.host && want(dnsmessage.TypeSOA):
		return []dnsmessage.Resource{s.soa()}, dnsmessage.RCodeSuccess
	default:
		return nil, dnsmessage.RCodeSuccess
	}
	return []dnsmessage.Resource{{Header: header, Body: body}}, dnsmessage.RCodeSuccess
}

// soa describes the owned zone; its minimum bounds how long negatives are cached.
func (s *dnsEndpoint) soa() dnsmessage.Resource {
	host := dnsmessage.MustNewName(s.host)
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: host, Class: dnsmessage.ClassINET, TTL: dnsTTL},
		Body:   &dnsmessage.SOAResource{NS: host, MBox: dnsmessage.MustNewName("hostmaster." + s.host), Serial: 1, Refresh: dnsTTL, Retry: dnsTTL, Expire: dnsTTL, MinTTL: dnsTTL},
	}
}

// DNS names compare case-insensitively for ASCII only (RFC 4343); Unicode
// folding would equate names such as the Kelvin sign with "k".
func foldDNS(name string) string {
	b := []byte(name)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// forward relays a query this endpoint does not own.
func (s *dnsEndpoint) forward(raw []byte, network string, q dnsmessage.Question) ([]byte, error) {
	select {
	case s.forwards <- struct{}{}:
		defer func() { <-s.forwards }()
	default:
		return nil, errors.New("too many forwarded queries in flight")
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.forwardTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, s.upstream)
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	// Device IDs may be predictable; a fresh one makes spoofed replies guess it.
	id := uint16(rand.Uint32())
	query := bytes.Clone(raw)
	binary.BigEndian.PutUint16(query, id)
	var reply []byte
	if network == "tcp" {
		if err := writeDNSFrame(conn, query); err != nil {
			return nil, err
		}
		reply, err = readDNSFrame(conn)
		if err != nil {
			return nil, err
		}
		if err := validReply(reply, id, q); err != nil {
			return nil, err
		}
	} else {
		if _, err := conn.Write(query); err != nil {
			return nil, err
		}
		// A stale or spoofed datagram must not end the wait for the real reply.
		buf := make([]byte, maxDNSMessageSize)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return nil, err
			}
			if validReply(buf[:n], id, q) == nil {
				reply = buf[:n]
				break
			}
		}
	}
	binary.BigEndian.PutUint16(reply, binary.BigEndian.Uint16(raw))
	return reply, nil
}

// validReply checks a forwarded reply's header and question only, so truncated
// or question-less error replies pass through for the client to act on.
func validReply(reply []byte, id uint16, q dnsmessage.Question) error {
	var p dnsmessage.Parser
	h, err := p.Start(reply)
	if err != nil {
		return err
	}
	if !h.Response || h.ID != id || h.OpCode != 0 {
		return errDNSMismatch
	}
	got, err := p.Question()
	switch {
	case errors.Is(err, dnsmessage.ErrSectionDone) && h.RCode != dnsmessage.RCodeSuccess:
		// Errors such as FORMERR may omit the question.
		return nil
	case err != nil:
		return err
	case got.Type != q.Type || got.Class != q.Class || foldDNS(got.Name.String()) != foldDNS(q.Name.String()):
		return errDNSMismatch
	}
	if _, err := p.Question(); !errors.Is(err, dnsmessage.ErrSectionDone) {
		return errDNSMismatch
	}
	return nil
}
