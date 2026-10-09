package server

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	testHost = "takeover.example."
	testSRV  = "_xmpp-client._tcp." + testHost
)

// The hostname arrives canonical from client.Config.WithDefaults.
func testDNSConfig() DNSConfig {
	return DNSConfig{
		ListenAddress: "127.0.0.1:0", hostname: "takeover.example", XMPPPort: 5222,
		Address: netip.MustParseAddr("192.0.2.1"), deviceIP: netip.MustParseAddr("127.0.0.1"),
	}
}

func newTestDNS(t *testing.T, cfg DNSConfig) *dnsEndpoint {
	t.Helper()
	s, err := newDNS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// testPacketConn injects read errors and reports each entry into ReadFrom, so
// tests can tell when the read loop has finished with the previous datagram.
type testPacketConn struct {
	net.PacketConn
	errs    chan error
	reading chan struct{}
	writes  atomic.Int32
}

func (c *testPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case c.reading <- struct{}{}:
	default:
	}
	select {
	case err := <-c.errs:
		return 0, nil, err
	default:
	}
	return c.PacketConn.ReadFrom(b)
}

func (c *testPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	c.writes.Add(1)
	return c.PacketConn.WriteTo(b, addr)
}

// serveTestDNS starts s on loopback; a non-nil tcp replaces the TCP listener
// and the first UDP reads fail with readErrs.
func serveTestDNS(t *testing.T, s *dnsEndpoint, tcp net.Listener, readErrs ...error) *testPacketConn {
	t.Helper()
	udp, stream, err := listenDNS("127.0.0.1:0", net.ListenPacket, net.Listen)
	if err != nil {
		t.Fatal(err)
	}
	if tcp != nil {
		_ = stream.Close()
		stream = tcp
	}
	conn := &testPacketConn{PacketConn: udp, errs: make(chan error, len(readErrs)), reading: make(chan struct{}, 64)}
	for _, err := range readErrs {
		conn.errs <- err
	}
	s.start(conn, stream)
	t.Cleanup(func() { _ = s.Close() })
	return conn
}

func startDNS(t *testing.T, cfg DNSConfig) *dnsEndpoint {
	t.Helper()
	s := newTestDNS(t, cfg)
	serveTestDNS(t, s, nil)
	return s
}

func dnsQuery(id uint16, name string, typ dnsmessage.Type) dnsmessage.Message {
	return dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}},
	}
}

func dnsOPT(udpSize int, version uint32) dnsmessage.Resource {
	var h dnsmessage.ResourceHeader
	_ = h.SetEDNS0(udpSize, dnsmessage.RCodeSuccess, false)
	h.TTL |= version << 16
	return dnsmessage.Resource{Header: h, Body: &dnsmessage.OPTResource{}}
}

func dnsPack(t *testing.T, m dnsmessage.Message) []byte {
	t.Helper()
	raw, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func dnsUnpack(t *testing.T, raw []byte) dnsmessage.Message {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(raw); err != nil {
		t.Fatal(err)
	}
	return m
}

func dnsAnswer(t *testing.T, s *dnsEndpoint, m dnsmessage.Message) dnsmessage.Message {
	t.Helper()
	reply, err := s.answer(dnsPack(t, m), "udp")
	if err != nil {
		t.Fatal(err)
	}
	return dnsUnpack(t, reply)
}

func dnsDial(t *testing.T, network, address, source string) net.Conn {
	t.Helper()
	dialer := net.Dialer{Timeout: 5 * time.Second}
	if source != "" {
		if network == "udp" {
			dialer.LocalAddr = &net.UDPAddr{IP: net.ParseIP(source)}
		} else {
			dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(source)}
		}
	}
	conn, err := dialer.Dial(network, address)
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		// Only Linux routes all of 127/8 to loopback by default.
		t.Skipf("no loopback address %s on this host", source)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func dnsWrite(conn net.Conn, network string, raw []byte) error {
	if network == "tcp" {
		return writeDNSFrame(conn, raw)
	}
	_, err := conn.Write(raw)
	return err
}

func dnsRead(conn net.Conn, network string) ([]byte, error) {
	if network == "tcp" {
		return readDNSFrame(conn)
	}
	raw := make([]byte, maxDNSMessageSize)
	n, err := conn.Read(raw)
	return raw[:n], err
}

func dnsExchange(t *testing.T, conn net.Conn, network string, query []byte) dnsmessage.Message {
	t.Helper()
	if err := dnsWrite(conn, network, query); err != nil {
		t.Fatal(err)
	}
	raw, err := dnsRead(conn, network)
	if err != nil {
		t.Fatal(err)
	}
	return dnsUnpack(t, raw)
}

func dnsRecords(m dnsmessage.Message) []string {
	var records []string
	for _, rr := range m.Answers {
		switch body := rr.Body.(type) {
		case *dnsmessage.AResource:
			records = append(records, netip.AddrFrom4(body.A).String())
		case *dnsmessage.AAAAResource:
			records = append(records, netip.AddrFrom16(body.AAAA).String())
		case *dnsmessage.SRVResource:
			records = append(records, fmt.Sprintf("%d %s", body.Port, body.Target))
		default:
			records = append(records, rr.Header.Type.String())
		}
	}
	return records
}

func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !done(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

func TestDNSConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*DNSConfig)
		ok     bool
	}{
		{"valid", func(*DNSConfig) {}, true},
		{"longest label", func(c *DNSConfig) { c.hostname = strings.Repeat("a", 63) + ".example" }, true},
		{"zoned listen", func(c *DNSConfig) {
			c.ListenAddress = "[fe80::1%eth0]:53"
			c.AllowedPeers = []netip.Addr{netip.MustParseAddr("fe80::2")}
		}, true},
		{"IPv6 device", func(c *DNSConfig) {
			c.ListenAddress, c.Address, c.deviceIP = "[2001:db8::2]:53", netip.MustParseAddr("2001:db8::2"), netip.MustParseAddr("2001:db8::10")
		}, true},
		// The device would follow the answer from an address the listener refuses.
		{"address family differs", func(c *DNSConfig) { c.Address = netip.MustParseAddr("2001:db8::1") }, false},
		// Queries from the device could never reach the endpoint.
		{"listen family differs", func(c *DNSConfig) { c.ListenAddress = "[2001:db8::2]:53" }, false},
		{"forward", func(c *DNSConfig) { c.ForwardAddress = "192.0.2.53:53" }, true},
		{"missing address", func(c *DNSConfig) { c.Address = netip.Addr{} }, false},
		{"missing device", func(c *DNSConfig) { c.deviceIP = netip.Addr{} }, false},
		{"wildcard IPv4 listen", func(c *DNSConfig) { c.ListenAddress = "0.0.0.0:53" }, false},
		{"wildcard IPv6 listen", func(c *DNSConfig) { c.ListenAddress = "[::]:53" }, false},
		{"wildcard mapped listen", func(c *DNSConfig) { c.ListenAddress = "[::ffff:0.0.0.0]:53" }, false},
		{"empty listen host", func(c *DNSConfig) { c.ListenAddress = ":53" }, false},
		{"named listen host", func(c *DNSConfig) { c.ListenAddress = "localhost:53" }, false},
		{"listen without port", func(c *DNSConfig) { c.ListenAddress = "127.0.0.1" }, false},
		{"empty hostname", func(c *DNSConfig) { c.hostname = "" }, false},
		{"root hostname", func(c *DNSConfig) { c.hostname = "." }, false},
		{"empty label", func(c *DNSConfig) { c.hostname = "a..example" }, false},
		{"leading dot", func(c *DNSConfig) { c.hostname = ".example" }, false},
		{"long label", func(c *DNSConfig) { c.hostname = strings.Repeat("a", 64) + ".example" }, false},
		{"SRV name too long", func(c *DNSConfig) { c.hostname = strings.Repeat("a.", 120) + "example" }, false},
		{"named forward", func(c *DNSConfig) { c.ForwardAddress = "resolver.example:53" }, false},
		{"forward without port", func(c *DNSConfig) { c.ForwardAddress = "192.0.2.53" }, false},
		{"forward port zero", func(c *DNSConfig) { c.ForwardAddress = "192.0.2.53:0" }, false},
		{"wildcard forward", func(c *DNSConfig) { c.ForwardAddress = "0.0.0.0:53" }, false},
		{"forward to self", func(c *DNSConfig) { c.ListenAddress, c.ForwardAddress = "127.0.0.1:5353", "127.0.0.1:5353" }, false},
		{"forward to mapped self", func(c *DNSConfig) { c.ListenAddress, c.ForwardAddress = "127.0.0.1:5353", "[::ffff:127.0.0.1]:5353" }, false},
		{"forward on the same port", func(c *DNSConfig) { c.ListenAddress, c.ForwardAddress = "127.0.0.1:53", "192.0.2.53:53" }, true},
		// The proxy is IPv4 like the device; an IPv6 listener hears neither.
		{"listen family no peer has", func(c *DNSConfig) {
			c.ListenAddress, c.AllowedPeers = "[2001:db8::1]:53", []netip.Addr{netip.MustParseAddr("192.0.2.53")}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testDNSConfig()
			tc.mutate(&cfg)
			if _, err := newDNS(cfg); (err == nil) != tc.ok {
				t.Fatalf("newDNS error = %v, want ok=%t", err, tc.ok)
			}
		})
	}
}

func TestDNSAnswers(t *testing.T) {
	const marker = "203.0.113.9"
	markerReply := func(query []byte) []byte {
		var m dnsmessage.Message
		if m.Unpack(query) != nil {
			return nil
		}
		m.Response, m.RecursionAvailable = true, true
		m.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Class: dnsmessage.ClassINET, TTL: 300},
			Body:   &dnsmessage.AResource{A: netip.MustParseAddr(marker).As4()},
		}}
		raw, _ := m.Pack()
		return raw
	}
	for _, forward := range []bool{false, true} {
		cfg := testDNSConfig()
		var seen <-chan upstreamExchange
		if forward {
			cfg.ForwardAddress, seen = dnsUpstream(t, "udp", markerReply)
		}
		s := startDNS(t, cfg)
		for _, tc := range []struct {
			name, qname string
			qtype       dnsmessage.Type
			class       dnsmessage.Class
			rcode       dnsmessage.RCode
			want        []string
			unowned     bool // forwarded when configured, otherwise refused
		}{
			{name: "A", want: []string{"192.0.2.1"}},
			{name: "0x20 case", qname: "TaKeOvEr.eXaMpLe.", want: []string{"192.0.2.1"}},
			{name: "ANY", qtype: dnsmessage.TypeALL, want: []string{"192.0.2.1"}},
			{name: "AAAA without IPv6", qtype: dnsmessage.TypeAAAA},
			{name: "MX", qtype: dnsmessage.TypeMX},
			{name: "HTTPS", qtype: dnsmessage.Type(65)},
			{name: "SRV on hostname", qtype: dnsmessage.TypeSRV},
			{name: "SRV", qname: testSRV, qtype: dnsmessage.TypeSRV, want: []string{"5222 " + testHost}},
			{name: "SRV 0x20 case", qname: "_XMPP-Client._TCP.TakeOver.EXAMPLE.", qtype: dnsmessage.TypeSRV, want: []string{"5222 " + testHost}},
			{name: "A on SRV name", qname: testSRV},
			{name: "empty non-terminal", qname: "_tcp." + testHost, qtype: dnsmessage.TypeSRV},
			{name: "other service", qname: "_xmpps-client._tcp." + testHost, qtype: dnsmessage.TypeSRV, rcode: dnsmessage.RCodeNameError},
			{name: "subdomain", qname: "x." + testHost, rcode: dnsmessage.RCodeNameError},
			{name: "below SRV", qname: "x." + testSRV, qtype: dnsmessage.TypeSRV, rcode: dnsmessage.RCodeNameError},
			{name: "class ANY", class: dnsmessage.ClassANY, rcode: dnsmessage.RCodeRefused},
			{name: "class CHAOS", class: dnsmessage.ClassCHAOS, rcode: dnsmessage.RCodeRefused},
			{name: "subdomain CHAOS", qname: "x." + testHost, class: dnsmessage.ClassCHAOS, rcode: dnsmessage.RCodeRefused},
			{name: "unrelated", qname: "unrelated.example.", unowned: true},
			{name: "parent", qname: "example.", unowned: true},
			{name: "suffix", qname: testHost + "evil.", unowned: true},
			{name: "label boundary", qname: "x" + testHost, unowned: true},
			{name: "unicode fold", qname: "taKeover.example.", unowned: true},
			{name: "unrelated CHAOS", qname: "version.bind.", qtype: dnsmessage.TypeTXT, class: dnsmessage.ClassCHAOS, unowned: true},
		} {
			t.Run(fmt.Sprintf("forward=%t/%s", forward, tc.name), func(t *testing.T) {
				query := dnsQuery(42, cmp.Or(tc.qname, testHost), cmp.Or(tc.qtype, dnsmessage.TypeA))
				query.Questions[0].Class = cmp.Or(tc.class, dnsmessage.ClassINET)
				got := dnsAnswer(t, s, query)
				rcode, want := tc.rcode, tc.want
				authoritative := rcode == dnsmessage.RCodeSuccess || rcode == dnsmessage.RCodeNameError
				if tc.unowned {
					authoritative, rcode, want = false, dnsmessage.RCodeRefused, nil
					if forward {
						rcode, want = dnsmessage.RCodeSuccess, []string{marker}
					}
				}
				if got.ID != 42 || !got.Response || !got.RecursionDesired || got.RecursionAvailable != forward || got.Authoritative != authoritative || got.RCode != rcode {
					t.Fatalf("header %+v, want rcode %v aa=%t ra=%t", got.Header, rcode, authoritative, forward)
				}
				// Resolvers using 0x20 randomisation compare the echoed case.
				if len(got.Questions) != 1 || got.Questions[0] != query.Questions[0] {
					t.Fatalf("question %+v not echoed", got.Questions)
				}
				if records := dnsRecords(got); !slices.Equal(records, want) {
					t.Fatalf("records %q, want %q", records, want)
				}
				for _, rr := range got.Answers {
					if rr.Header.Name != query.Questions[0].Name || rr.Header.Class != dnsmessage.ClassINET {
						t.Fatalf("answer owner %v does not match question", rr.Header.Name)
					}
					if !tc.unowned && (rr.Header.TTL == 0 || rr.Header.TTL > 60) {
						t.Fatalf("TTL %d outlives a takeover change", rr.Header.TTL)
					}
				}
				// A forward completes before the answer, so an owned name that
				// leaked upstream is already recorded here.
				if forward {
					forwarded := 0
					if tc.unowned {
						forwarded = 1
					}
					if len(seen) != forwarded {
						t.Fatalf("upstream saw %d queries, want %d", len(seen), forwarded)
					}
					if tc.unowned {
						<-seen
					}
				}
			})
		}
	}
}

func TestDNSAddressFamilies(t *testing.T) {
	for _, tc := range []struct {
		address string
		qtype   dnsmessage.Type
		want    []string
	}{
		{"2001:db8::1", dnsmessage.TypeAAAA, []string{"2001:db8::1"}},
		{"2001:db8::1", dnsmessage.TypeALL, []string{"2001:db8::1"}},
		{"2001:db8::1", dnsmessage.TypeA, nil},
		{"::ffff:192.0.2.1", dnsmessage.TypeA, []string{"192.0.2.1"}},
		{"::ffff:192.0.2.1", dnsmessage.TypeAAAA, nil},
	} {
		t.Run(tc.address+"/"+tc.qtype.String(), func(t *testing.T) {
			cfg := testDNSConfig()
			cfg.Address = netip.MustParseAddr(tc.address)
			if cfg.Address.Is6() && !cfg.Address.Is4In6() {
				cfg.ListenAddress, cfg.deviceIP = "[2001:db8::2]:53", netip.MustParseAddr("2001:db8::10")
			}
			got := dnsAnswer(t, newTestDNS(t, cfg), dnsQuery(1, testHost, tc.qtype))
			if records := dnsRecords(got); got.RCode != dnsmessage.RCodeSuccess || !slices.Equal(records, tc.want) {
				t.Fatalf("rcode %v records %q, want %q", got.RCode, records, tc.want)
			}
		})
	}
}

func TestDNSQueryHeaders(t *testing.T) {
	s := newTestDNS(t, testDNSConfig())
	question := dnsQuery(0, testHost, dnsmessage.TypeA).Questions[0]
	for _, tc := range []struct {
		name    string
		mutate  func(*dnsmessage.Message)
		rcode   dnsmessage.RCode // extended when an OPT is returned
		answers int
		opt     bool
	}{
		{name: "plain", answers: 1},
		{name: "recursion not desired", mutate: func(m *dnsmessage.Message) { m.RecursionDesired = false }, answers: 1},
		{name: "unsupported opcode", mutate: func(m *dnsmessage.Message) { m.OpCode = 2 }, rcode: dnsmessage.RCodeNotImplemented},
		{name: "no question", mutate: func(m *dnsmessage.Message) { m.Questions = nil }, rcode: dnsmessage.RCodeFormatError},
		{name: "two questions", mutate: func(m *dnsmessage.Message) { m.Questions = append(m.Questions, question) }, rcode: dnsmessage.RCodeFormatError},
		{name: "EDNS", mutate: func(m *dnsmessage.Message) { m.Additionals = []dnsmessage.Resource{dnsOPT(4096, 0)} }, answers: 1, opt: true},
		{name: "EDNS DNSSEC OK", mutate: func(m *dnsmessage.Message) {
			m.Additionals = []dnsmessage.Resource{dnsOPT(1232, 0)}
			m.Additionals[0].Header.TTL |= 0x8000
		}, answers: 1, opt: true},
		{name: "EDNS refused", mutate: func(m *dnsmessage.Message) {
			m.Questions[0].Name = dnsmessage.MustNewName("unrelated.example.")
			m.Additionals = []dnsmessage.Resource{dnsOPT(512, 0)}
		}, rcode: dnsmessage.RCodeRefused, opt: true},
		{name: "EDNS version 1", mutate: func(m *dnsmessage.Message) { m.Additionals = []dnsmessage.Resource{dnsOPT(4096, 1)} }, rcode: dnsRCodeBadVersion, opt: true},
		{name: "two OPT records", mutate: func(m *dnsmessage.Message) { m.Additionals = []dnsmessage.Resource{dnsOPT(4096, 0), dnsOPT(4096, 0)} }, rcode: dnsmessage.RCodeFormatError, opt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := dnsQuery(42, testHost, dnsmessage.TypeA)
			if tc.mutate != nil {
				tc.mutate(&query)
			}
			got := dnsAnswer(t, s, query)
			rcode := got.RCode
			if tc.opt {
				if len(got.Additionals) != 1 || got.Additionals[0].Header.Type != dnsmessage.TypeOPT {
					t.Fatalf("additionals %+v, want one OPT", got.Additionals)
				}
				opt := got.Additionals[0].Header
				if opt.TTL&0x00ff0000 != 0 || opt.Class < 512 || opt.Name.String() != "." || opt.DNSSECAllowed() != query.Additionals[0].Header.DNSSECAllowed() {
					t.Fatalf("OPT %+v, want version 0, a UDP size of at least 512 and the query's DO bit", opt)
				}
				rcode = opt.ExtendedRCode(got.RCode)
			} else if len(got.Additionals) != 0 {
				t.Fatal("OPT returned for a query without EDNS")
			}
			if got.ID != 42 || !got.Response || got.OpCode != query.OpCode || got.RecursionDesired != query.RecursionDesired || got.CheckingDisabled {
				t.Fatalf("header %+v does not match query", got.Header)
			}
			if rcode != tc.rcode || len(got.Answers) != tc.answers || !slices.Equal(got.Questions, query.Questions) {
				t.Fatalf("rcode %v answers %d questions %v", rcode, len(got.Answers), got.Questions)
			}
		})
	}
	query := dnsQuery(42, testHost, dnsmessage.TypeA)
	query.Response = true
	if reply, err := s.answer(dnsPack(t, query), "udp"); err == nil || reply != nil {
		t.Fatal("answered a DNS response")
	}
}

func TestDNSAllowed(t *testing.T) {
	cfg := testDNSConfig()
	cfg.Address, cfg.deviceIP = netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("fe80::1")
	cfg.AllowedPeers = []netip.Addr{netip.MustParseAddr("::ffff:192.0.2.7"), netip.MustParseAddr("fe80::2%eth9")}
	s := newTestDNS(t, cfg)
	for _, tc := range []struct {
		addr net.Addr
		want bool
	}{
		{&net.UDPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0", Port: 5353}, true},
		{&net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 5353}, true},
		{&net.UDPAddr{IP: net.ParseIP("192.0.2.7"), Port: 5353}, true},
		{&net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.7"), Port: 5353}, true},
		{&net.UDPAddr{IP: net.ParseIP("fe80::2"), Zone: "eth0", Port: 5353}, true},
		{&net.UDPAddr{IP: net.ParseIP("fe80::3"), Zone: "eth0", Port: 5353}, false},
		{&net.UDPAddr{IP: net.ParseIP("192.0.2.8"), Port: 5353}, false},
		{&net.UnixAddr{Name: "pipe", Net: "unix"}, false},
	} {
		if got := s.allowed(tc.addr); got != tc.want {
			t.Errorf("allowed(%v) = %t, want %t", tc.addr, got, tc.want)
		}
	}
}

func TestDNSNegativeAnswersCarrySOA(t *testing.T) {
	s := newTestDNS(t, testDNSConfig())
	for name, q := range map[string]dnsmessage.Message{
		"NODATA":   dnsQuery(1, "takeover.example.", dnsmessage.TypeMX),
		"NXDOMAIN": dnsQuery(2, "x.takeover.example.", dnsmessage.TypeA),
	} {
		reply, err := s.answer(dnsPack(t, q), "udp")
		if err != nil {
			t.Fatal(err)
		}
		m := dnsUnpack(t, reply)
		if len(m.Answers) != 0 || len(m.Authorities) != 1 || m.Authorities[0].Header.Type != dnsmessage.TypeSOA {
			t.Errorf("%s without an SOA: %+v", name, m)
			continue
		}
		// Its minimum caps negative caching, so a takeover can be undone.
		if soa := m.Authorities[0].Body.(*dnsmessage.SOAResource); soa.MinTTL > dnsTTL || m.Authorities[0].Header.TTL > dnsTTL {
			t.Errorf("%s SOA caches negatives too long: %+v", name, m.Authorities[0])
		}
	}
	reply, err := s.answer(dnsPack(t, dnsQuery(3, "takeover.example.", dnsmessage.TypeSOA)), "udp")
	if err != nil {
		t.Fatal(err)
	}
	if m := dnsUnpack(t, reply); len(m.Answers) != 1 || m.Answers[0].Header.Type != dnsmessage.TypeSOA {
		t.Fatalf("SOA query not answered: %+v", m)
	}
}

func TestDNSLargeUDPReplyTruncates(t *testing.T) {
	cfg := testDNSConfig()
	// The longest hostname whose SRV name still fits 255 bytes; dnsmessage
	// never compresses SRV targets, so the reply exceeds 512 bytes.
	cfg.hostname = strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("b", 43)
	s := newTestDNS(t, cfg)
	name := strings.ToUpper("_xmpp-client._tcp." + cfg.hostname + ".")
	for _, tc := range []struct {
		edns    int // advertised UDP size; 0 sends no OPT
		network string
		cut     bool
	}{
		{0, "udp", true}, {512, "udp", true}, {4096, "udp", false}, {0, "tcp", false},
	} {
		query := dnsQuery(1, name, dnsmessage.TypeSRV)
		if tc.edns > 0 {
			query.Additionals = []dnsmessage.Resource{dnsOPT(tc.edns, 0)}
		}
		reply, err := s.answer(dnsPack(t, query), tc.network)
		if err != nil {
			t.Fatal(err)
		}
		m := dnsUnpack(t, reply)
		if tc.cut && (len(reply) > 512 || !m.Truncated) || !tc.cut && (m.Truncated || len(m.Answers) != 1) {
			t.Fatalf("%+v: %d bytes, truncated=%v, answers %d", tc, len(reply), m.Truncated, len(m.Answers))
		}
	}
}

type upstreamExchange struct{ query, reply []byte }

// dnsUpstream runs a loopback resolver on one network. handle builds the reply
// to each query, or nil to stay silent; every exchange is recorded before the
// reply is sent.
func dnsUpstream(t *testing.T, network string, handle func(query []byte) []byte) (string, <-chan upstreamExchange) {
	t.Helper()
	seen := make(chan upstreamExchange, 64)
	var wg sync.WaitGroup
	var address string
	var stop func() error
	if network == "udp" {
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address, stop = conn.LocalAddr().String(), conn.Close
		wg.Go(func() {
			buf := make([]byte, maxDNSMessageSize)
			for {
				n, peer, err := conn.ReadFrom(buf)
				if err != nil {
					return
				}
				query := bytes.Clone(buf[:n])
				reply := handle(query)
				seen <- upstreamExchange{query, reply}
				if reply != nil {
					_, _ = conn.WriteTo(reply, peer)
				}
			}
		})
	} else {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address, stop = listener.Addr().String(), listener.Close
		wg.Go(func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				wg.Go(func() {
					defer conn.Close() //nolint:errcheck
					_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
					for {
						query, err := readDNSFrame(conn)
						if err != nil {
							return
						}
						reply := handle(query)
						seen <- upstreamExchange{query, reply}
						if reply != nil && writeDNSFrame(conn, reply) != nil {
							return
						}
					}
				})
			}
		})
	}
	t.Cleanup(func() { _ = stop(); wg.Wait() })
	return address, seen
}

func recvExchange(t *testing.T, seen <-chan upstreamExchange) upstreamExchange {
	t.Helper()
	select {
	case exchange := <-seen:
		return exchange
	case <-time.After(5 * time.Second):
		t.Fatal("query did not reach the upstream resolver")
		return upstreamExchange{}
	}
}

// padDNS packs m with one OPT record, padded (RFC 7830) to size bytes if set.
func padDNS(m dnsmessage.Message, size int) ([]byte, error) {
	m.Additionals = []dnsmessage.Resource{dnsOPT(8192, 0)}
	raw, err := m.Pack()
	if err != nil || size == 0 {
		return raw, err
	}
	m.Additionals[0].Body = &dnsmessage.OPTResource{Options: []dnsmessage.Option{{Code: 12, Data: make([]byte, size-len(raw)-4)}}}
	if raw, err = m.Pack(); err == nil && len(raw) != size {
		err = fmt.Errorf("packed %d bytes, want %d", len(raw), size)
	}
	return raw, err
}

func TestDNSForwardReplies(t *testing.T) {
	// Larger than both the classic and the advertised EDNS UDP limits.
	text := slices.Repeat([]string{strings.Repeat("x", 250)}, 20)
	for _, network := range []string{"udp", "tcp"} {
		for _, tc := range []struct {
			name, only           string
			queryName, replyName string
			querySize, replySize int
			edit                 func(*dnsmessage.Message)
			cut                  func([]byte) []byte
			pass                 bool
		}{
			{name: "intact", pass: true},
			{name: "uppercase reply", replyName: "UNRELATED.EXAMPLE.", pass: true},
			{name: "root", queryName: ".", pass: true},
			{name: "non-ASCII bytes", queryName: "\xc9.Example.", pass: true},
			{name: "truncated mid-record", edit: func(m *dnsmessage.Message) { m.Truncated = true }, cut: func(b []byte) []byte { return b[:len(b)/2] }, pass: true},
			{name: "trailing byte cut", cut: func(b []byte) []byte { return b[:len(b)-1] }, pass: true},
			{name: "FORMERR without question", edit: func(m *dnsmessage.Message) { m.RCode, m.Questions, m.Answers = dnsmessage.RCodeFormatError, nil, nil }, pass: true},
			{name: "REFUSED without question", edit: func(m *dnsmessage.Message) { m.RCode, m.Questions, m.Answers = dnsmessage.RCodeRefused, nil, nil }, pass: true},
			{name: "padded query", querySize: 6000, pass: true},
			{name: "largest IPv4 datagram", only: "udp", querySize: 65507, pass: true},
			{name: "largest TCP messages", only: "tcp", querySize: 65535, replySize: 65535, pass: true},
			{name: "wrong name", replyName: "other.example."},
			{name: "name suffix", replyName: "unrelated.example.evil."},
			{name: "label boundary", replyName: "un.relatedexample."},
			{name: "root mismatch", queryName: ".", replyName: "example."},
			{name: "simple fold", queryName: "s.example.", replyName: "ſ.example."},
			{name: "Kelvin sign", queryName: "k.example.", replyName: "K.example."},
			{name: "non-ASCII case", queryName: "\xc3\x89.Example.", replyName: "\xc3\xa9.example."},
			{name: "wrong type", edit: func(m *dnsmessage.Message) { m.Questions[0].Type = dnsmessage.TypeA }},
			{name: "wrong class", edit: func(m *dnsmessage.Message) { m.Questions[0].Class = dnsmessage.ClassCHAOS }},
			{name: "wrong ID", edit: func(m *dnsmessage.Message) { m.ID++ }},
			{name: "query flag", edit: func(m *dnsmessage.Message) { m.Response = false }},
			{name: "wrong opcode", edit: func(m *dnsmessage.Message) { m.OpCode = 2 }},
			{name: "extra question", edit: func(m *dnsmessage.Message) { m.Questions = append(m.Questions, m.Questions[0]) }},
			{name: "NOERROR without question", edit: func(m *dnsmessage.Message) { m.Questions = nil }},
			{name: "cut in question", cut: func(b []byte) []byte { return b[:15] }},
		} {
			if tc.only != "" && tc.only != network {
				continue
			}
			t.Run(network+"/"+tc.name, func(t *testing.T) {
				if tc.querySize > 9216 && runtime.GOOS != "linux" {
					t.Skip("macOS and the BSDs cap outgoing UDP datagrams at 9216 bytes")
				}
				query := dnsQuery(42, cmp.Or(tc.queryName, "Unrelated.Example."), dnsmessage.TypeTXT)
				raw, err := padDNS(query, tc.querySize)
				if err != nil {
					t.Fatal(err)
				}
				address, seen := dnsUpstream(t, network, func(q []byte) []byte {
					var m dnsmessage.Message
					if m.Unpack(q) != nil || len(m.Questions) != 1 {
						return nil
					}
					m.Response, m.RecursionAvailable = true, true
					m.Questions[0].Name = dnsmessage.MustNewName(cmp.Or(tc.replyName, foldDNS(m.Questions[0].Name.String())))
					m.Answers = []dnsmessage.Resource{{
						Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Class: dnsmessage.ClassINET, TTL: 30},
						Body:   &dnsmessage.TXTResource{TXT: text},
					}}
					if tc.edit != nil {
						tc.edit(&m)
					}
					reply, err := padDNS(m, tc.replySize)
					if err != nil {
						return nil
					}
					if tc.cut != nil {
						reply = tc.cut(reply)
					}
					return reply
				})
				cfg := testDNSConfig()
				cfg.ForwardAddress = address
				s := newTestDNS(t, cfg)
				// Mismatched UDP replies are skipped until the forward deadline.
				s.forwardTimeout = 200 * time.Millisecond
				serveTestDNS(t, s, nil)
				conn := dnsDial(t, network, s.tcp.Addr().String(), "")
				if err := dnsWrite(conn, network, raw); err != nil {
					t.Fatal(err)
				}
				got, err := dnsRead(conn, network)
				if err != nil {
					t.Fatal(err)
				}
				exchange := recvExchange(t, seen)
				if !bytes.Equal(exchange.query[2:], raw[2:]) {
					t.Fatal("forwarded query altered beyond its ID")
				}
				if tc.pass {
					want := bytes.Clone(exchange.reply)
					binary.BigEndian.PutUint16(want, 42)
					if !bytes.Equal(got, want) {
						t.Fatalf("reply not passed through: got %d bytes, want %d", len(got), len(want))
					}
					return
				}
				m := dnsUnpack(t, got)
				if m.RCode != dnsmessage.RCodeServerFailure || m.ID != 42 || !m.Response || !m.RecursionAvailable || m.Authoritative || m.Truncated ||
					len(m.Answers)+len(m.Authorities) != 0 || len(m.Additionals) != 1 || !slices.Equal(m.Questions, query.Questions) {
					t.Fatalf("mismatched upstream reply was not replaced by SERVFAIL: %+v", m)
				}
			})
		}
	}
}

func TestDNSForwardRandomisesID(t *testing.T) {
	address, seen := dnsUpstream(t, "udp", func(q []byte) []byte {
		reply := bytes.Clone(q)
		reply[2] |= 0x80 // QR
		return reply
	})
	cfg := testDNSConfig()
	cfg.ForwardAddress = address
	s := startDNS(t, cfg)
	randomised := false
	for range 4 {
		got := dnsAnswer(t, s, dnsQuery(42, "unrelated.example.", dnsmessage.TypeA))
		if got.ID != 42 || got.RCode != dnsmessage.RCodeSuccess {
			t.Fatalf("device ID not restored: %+v", got.Header)
		}
		randomised = randomised || binary.BigEndian.Uint16(recvExchange(t, seen).query) != 42
	}
	if !randomised {
		t.Fatal("upstream queries reuse the device ID")
	}
}

func TestDNSForwardFailures(t *testing.T) {
	t.Parallel()
	for _, network := range []string{"udp", "tcp"} {
		for _, silent := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/silent=%t", network, silent), func(t *testing.T) {
				var address string
				if silent {
					address, _ = dnsUpstream(t, network, func([]byte) []byte { return nil })
				}
				cfg := testDNSConfig()
				cfg.ForwardAddress = cmp.Or(address, "192.0.2.53:53")
				s := newTestDNS(t, cfg)
				if !silent {
					// Port 0 is refused at once, and no parallel test can bind it.
					s.upstream = "127.0.0.1:0"
				}
				s.forwardTimeout = 100 * time.Millisecond
				serveTestDNS(t, s, nil)
				query := dnsQuery(42, "unrelated.example.", dnsmessage.TypeA)
				reply, err := s.answer(dnsPack(t, query), network)
				if err != nil {
					t.Fatal(err)
				}
				m := dnsUnpack(t, reply)
				if m.RCode != dnsmessage.RCodeServerFailure || m.ID != 42 || !m.RecursionAvailable || !slices.Equal(m.Questions, query.Questions) {
					t.Fatalf("upstream failure did not return SERVFAIL: %+v", m)
				}
			})
		}
	}
}

// A connected UDP socket only accepts the resolver's own address, so an
// off-path reply arriving first is ignored.
func TestDNSForwardIgnoresOtherSources(t *testing.T) {
	resolver, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resolver.Close() })
	rogue, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rogue.Close() })
	cfg := testDNSConfig()
	cfg.ForwardAddress = resolver.LocalAddr().String()
	s := startDNS(t, cfg)
	type result struct {
		reply []byte
		err   error
	}
	done := make(chan result, 1)
	query := dnsPack(t, dnsQuery(42, "unrelated.example.", dnsmessage.TypeA))
	go func() {
		reply, err := s.answer(query, "udp")
		done <- result{reply, err}
	}()
	if err := resolver.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, maxDNSMessageSize)
	n, peer, err := resolver.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	reply := func(address string) []byte {
		m := dnsUnpack(t, buf[:n])
		m.Response = true
		m.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Class: dnsmessage.ClassINET, TTL: 30},
			Body:   &dnsmessage.AResource{A: netip.MustParseAddr(address).As4()},
		}}
		return dnsPack(t, m)
	}
	if _, err := rogue.WriteTo(reply("198.51.100.66"), peer); err != nil {
		t.Fatal(err)
	}
	// A stale ID from the resolver itself must not end the wait either.
	stale := dnsUnpack(t, reply("198.51.100.67"))
	stale.ID++
	if _, err := resolver.WriteTo(dnsPack(t, stale), peer); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.WriteTo(reply("192.0.2.53"), peer); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if records := dnsRecords(dnsUnpack(t, r.reply)); !slices.Equal(records, []string{"192.0.2.53"}) {
			t.Fatalf("records %q, want the resolver's answer", records)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forwarded query did not complete")
	}
}

// pipeListener hands the server in-memory connections and injected errors.
type pipeListener struct {
	conns  chan net.Conn
	errs   chan error
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn, 4), errs: make(chan error, 1), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case err := <-l.errs:
		return nil, err
	default:
	}
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53} }

// dial returns the client end of a pipe the server sees as the device.
func (l *pipeListener) dial(t *testing.T) net.Conn {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	l.conns <- devicePipe{server}
	return client
}

type devicePipe struct{ net.Conn }

func (devicePipe) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234} }

// expectClosed requires the server to close conn rather than leave it open.
func expectClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	var b [1]byte
	n, err := conn.Read(b[:])
	var netErr net.Error
	if n != 0 || err == nil || errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("connection not closed by server: n=%d err=%v", n, err)
	}
}

func expectAnswered(t *testing.T, m dnsmessage.Message, id uint16) {
	t.Helper()
	if m.ID != id || m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 1 {
		t.Fatalf("query %d not answered: %+v", id, m)
	}
}

func TestDNSPeerScope(t *testing.T) {
	for _, network := range []string{"udp", "tcp"} {
		for _, source := range []string{"127.0.0.2", "127.0.0.3"} {
			t.Run(network+"/"+source, func(t *testing.T) {
				cfg := testDNSConfig()
				cfg.AllowedPeers = []netip.Addr{netip.MustParseAddr("127.0.0.2")}
				s := newTestDNS(t, cfg)
				udp := serveTestDNS(t, s, nil)
				conn := dnsDial(t, network, s.tcp.Addr().String(), source)
				query := dnsPack(t, dnsQuery(7, testHost, dnsmessage.TypeA))
				switch {
				case source == "127.0.0.2":
					expectAnswered(t, dnsExchange(t, conn, network, query), 7)
				case network == "tcp":
					_ = dnsWrite(conn, network, query) // may race the server's close
					expectClosed(t, conn)
				default:
					<-udp.reading
					if err := dnsWrite(conn, network, query); err != nil {
						t.Fatal(err)
					}
					// Re-entering ReadFrom means the datagram was handled; Close
					// then joins any reply it could have started.
					<-udp.reading
					_ = s.Close()
					if n := udp.writes.Load(); n != 0 {
						t.Fatalf("unrelated peer received %d replies", n)
					}
				}
			})
		}
	}
}

func TestDNSTCPPipelined(t *testing.T) {
	for _, source := range []string{"127.0.0.1", "127.0.0.2"} {
		t.Run(source, func(t *testing.T) {
			cfg := testDNSConfig()
			cfg.AllowedPeers = []netip.Addr{netip.MustParseAddr("127.0.0.2")}
			s := startDNS(t, cfg)
			conn := dnsDial(t, "tcp", s.tcp.Addr().String(), source)
			names := []string{testHost, "unrelated.example.", testHost}
			var pipeline bytes.Buffer
			for i, name := range names {
				_ = writeDNSFrame(&pipeline, dnsPack(t, dnsQuery(uint16(11+i), name, dnsmessage.TypeA)))
			}
			if _, err := conn.Write(pipeline.Bytes()); err != nil {
				t.Fatal(err)
			}
			for i, name := range names {
				raw, err := readDNSFrame(conn)
				if err != nil {
					t.Fatal(err)
				}
				m := dnsUnpack(t, raw)
				want := dnsmessage.RCodeSuccess
				if name != testHost {
					want = dnsmessage.RCodeRefused
				}
				if m.ID != uint16(11+i) || m.RCode != want {
					t.Fatalf("response %d: ID %d rcode %v, want ID %d rcode %v", i, m.ID, m.RCode, 11+i, want)
				}
			}
		})
	}
}

func TestDNSTCPRejectsInvalidFrame(t *testing.T) {
	query := dnsPack(t, dnsQuery(1, testHost, dnsmessage.TypeA))
	response := dnsQuery(1, testHost, dnsmessage.TypeA)
	response.Response = true
	for _, invalid := range [][]byte{{}, {1}, dnsPack(t, response)} {
		t.Run(strconv.Itoa(len(invalid)), func(t *testing.T) {
			s := startDNS(t, testDNSConfig())
			conn := dnsDial(t, "tcp", s.tcp.Addr().String(), "")
			if err := writeDNSFrame(conn, query); err != nil {
				t.Fatal(err)
			}
			if err := writeDNSFrame(conn, invalid); err != nil {
				t.Fatal(err)
			}
			raw, err := readDNSFrame(conn)
			if err != nil {
				t.Fatal(err)
			}
			expectAnswered(t, dnsUnpack(t, raw), 1)
			expectClosed(t, conn)
		})
	}
}

func TestDNSTCPIdleTimeout(t *testing.T) {
	t.Parallel()
	query := dnsPack(t, dnsQuery(1, testHost, dnsmessage.TypeA))
	var frame bytes.Buffer
	_ = writeDNSFrame(&frame, query)
	for _, partial := range []int{0, 1, 3} {
		t.Run(strconv.Itoa(partial), func(t *testing.T) {
			s := newTestDNS(t, testDNSConfig())
			s.tcpTimeout = 150 * time.Millisecond
			serveTestDNS(t, s, nil)
			conn := dnsDial(t, "tcp", s.tcp.Addr().String(), "")
			expectAnswered(t, dnsExchange(t, conn, "tcp", query), 1)
			if _, err := conn.Write(frame.Bytes()[:partial]); err != nil {
				t.Fatal(err)
			}
			expectClosed(t, conn)
			eventually(t, "TCP slot not released", func() bool { return len(s.tcpSlots) == 0 })
		})
	}
}

func TestDNSTCPRefreshesDeadline(t *testing.T) {
	t.Parallel()
	s := newTestDNS(t, testDNSConfig())
	s.tcpTimeout = 300 * time.Millisecond
	serveTestDNS(t, s, nil)
	conn := dnsDial(t, "tcp", s.tcp.Addr().String(), "")
	// Each request arrives within the timeout while the connection outlives it.
	for id := range uint16(5) {
		if id > 0 {
			time.Sleep(s.tcpTimeout / 2)
		}
		expectAnswered(t, dnsExchange(t, conn, "tcp", dnsPack(t, dnsQuery(id, testHost, dnsmessage.TypeA))), id)
	}
}

func TestDNSTCPCloseJoinsConnections(t *testing.T) {
	query := dnsPack(t, dnsQuery(1, testHost, dnsmessage.TypeA))
	var frame bytes.Buffer
	_ = writeDNSFrame(&frame, query)
	for _, partial := range []int{0, 1, 3} {
		t.Run(strconv.Itoa(partial), func(t *testing.T) {
			s := newTestDNS(t, testDNSConfig())
			// Only Close may end the connection within the test's bound.
			s.tcpTimeout = time.Minute
			serveTestDNS(t, s, nil)
			conn := dnsDial(t, "tcp", s.tcp.Addr().String(), "")
			expectAnswered(t, dnsExchange(t, conn, "tcp", query), 1)
			if _, err := conn.Write(frame.Bytes()[:partial]); err != nil {
				t.Fatal(err)
			}
			closeWithin(t, s)
			expectClosed(t, conn)
		})
	}
}

func closeWithin(t *testing.T, s *dnsEndpoint) {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DNS Close did not join workers")
	}
}

// A client that never reads must not hold its worker past the write deadline.
func TestDNSTCPBlockedWrite(t *testing.T) {
	t.Parallel()
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%t", shutdown), func(t *testing.T) {
			s := newTestDNS(t, testDNSConfig())
			s.tcpTimeout = 150 * time.Millisecond
			listener := newPipeListener()
			serveTestDNS(t, s, listener)
			conn := listener.dial(t)
			// The pipe is unbuffered: this returns once the server read the query.
			if err := writeDNSFrame(conn, dnsPack(t, dnsQuery(1, testHost, dnsmessage.TypeA))); err != nil {
				t.Fatal(err)
			}
			if shutdown {
				closeWithin(t, s)
			} else {
				eventually(t, "blocked write held its TCP slot", func() bool { return len(s.tcpSlots) == 0 })
			}
			expectClosed(t, conn)
		})
	}
}

// Any listener error is retried; only Close stops the listeners.
func TestDNSListenerErrorsRetried(t *testing.T) {
	s := newTestDNS(t, testDNSConfig())
	listener := newPipeListener()
	listener.errs <- errors.New("injected accept failure")
	udp := serveTestDNS(t, s, listener, errors.New("injected read failure"))
	query := dnsPack(t, dnsQuery(1, testHost, dnsmessage.TypeA))
	expectAnswered(t, dnsExchange(t, dnsDial(t, "udp", udp.LocalAddr().String(), ""), "udp", query), 1)
	expectAnswered(t, dnsExchange(t, listener.dial(t), "tcp", query), 1)
}

// Forwards hold at most half the UDP budget, so a dead resolver cannot keep
// owned names from being answered.
func TestDNSSlowForwardsKeepOwnedNamesAnswered(t *testing.T) {
	upstream, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	if err := upstream.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	cfg := testDNSConfig()
	cfg.ForwardAddress = upstream.LocalAddr().String()
	s := newTestDNS(t, cfg)
	s.forwardTimeout = time.Minute // forwards stay in flight until released
	udp := serveTestDNS(t, s, nil)
	client := dnsDial(t, "udp", udp.LocalAddr().String(), "")
	buf := make([]byte, maxDNSMessageSize)
	forwarded := func() (dnsmessage.Message, net.Addr) {
		t.Helper()
		n, peer, err := upstream.ReadFrom(buf)
		if err != nil {
			t.Fatal(err)
		}
		return dnsUnpack(t, buf[:n]), peer
	}
	exchange := func(id uint16, name string) dnsmessage.Message {
		t.Helper()
		return dnsExchange(t, client, "udp", dnsPack(t, dnsQuery(id, name, dnsmessage.TypeA)))
	}
	send := func(id uint16, name string) {
		t.Helper()
		if err := dnsWrite(client, "udp", dnsPack(t, dnsQuery(id, name, dnsmessage.TypeA))); err != nil {
			t.Fatal(err)
		}
	}
	var first dnsmessage.Message
	var firstPeer net.Addr
	for i := range cap(s.forwards) {
		send(uint16(i+1), fmt.Sprintf("q%d.example.", i))
		if m, peer := forwarded(); i == 0 {
			first, firstPeer = m, peer
		}
	}
	// Spoofed queries to a dead resolver must not keep the device from
	// finding the XMPP listener: excess forwards fail at once instead.
	if m := exchange(100, "excess.example."); m.ID != 100 || m.RCode != dnsmessage.RCodeServerFailure {
		t.Fatalf("excess forward: %+v", m.Header)
	}
	expectAnswered(t, exchange(101, testHost), 101)
	first.Response = true
	if _, err := upstream.WriteTo(dnsPack(t, first), firstPeer); err != nil {
		t.Fatal(err)
	}
	if raw, err := dnsRead(client, "udp"); err != nil || dnsUnpack(t, raw).ID != 1 {
		t.Fatalf("released query not answered: %v", err)
	}
	eventually(t, "forward slot not released", func() bool { return len(s.forwards) == cap(s.forwards)-1 })
	send(102, "after.example.")
	if m, _ := forwarded(); m.Questions[0].Name.String() != "after.example." {
		t.Fatalf("forwarded %v, want the query sent after the slot was freed", m.Questions[0].Name)
	}
}

// A burst wider than the UDP budget waits for slots rather than dropping
// queries, so the device's own lookup is not lost to a flood.
func TestDNSUDPBurstAnswered(t *testing.T) {
	s := startDNS(t, testDNSConfig())
	client := dnsDial(t, "udp", s.udp.LocalAddr().String(), "")
	// Every slot taken while the burst arrives, as under a flood.
	for range cap(s.udpSlots) {
		s.udpSlots <- struct{}{}
	}
	const burst = 8 * dnsWorkers
	for id := range uint16(burst) {
		if err := dnsWrite(client, "udp", dnsPack(t, dnsQuery(id, testHost, dnsmessage.TypeA))); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond) // a dropping loop would have read the burst by now
	for range cap(s.udpSlots) {
		<-s.udpSlots
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	answered := make(map[uint16]bool)
	for len(answered) < burst {
		raw, err := dnsRead(client, "udp")
		if err != nil {
			t.Fatalf("%d of %d queries answered: %v", len(answered), burst, err)
		}
		answered[dnsUnpack(t, raw).ID] = true
	}
}

// Idle TCP connections exhaust only the TCP budget; UDP keeps answering.
func TestDNSTCPBudgetSeparate(t *testing.T) {
	s := startDNS(t, testDNSConfig())
	query := dnsPack(t, dnsQuery(1, testHost, dnsmessage.TypeA))
	for range cap(s.tcpSlots) {
		expectAnswered(t, dnsExchange(t, dnsDial(t, "tcp", s.tcp.Addr().String(), ""), "tcp", query), 1)
	}
	excess := dnsDial(t, "tcp", s.tcp.Addr().String(), "")
	_ = dnsWrite(excess, "tcp", query) // may race the server's close
	expectClosed(t, excess)
	expectAnswered(t, dnsExchange(t, dnsDial(t, "udp", s.tcp.Addr().String(), ""), "udp", query), 1)
}

func TestDNSEphemeralPortCollision(t *testing.T) {
	var packets []net.PacketConn
	var occupant net.Listener
	calls := 0
	packet := func(network, address string) (net.PacketConn, error) {
		p, err := net.ListenPacket(network, address)
		if err == nil {
			packets = append(packets, p)
		}
		return p, err
	}
	stream := func(network, address string) (net.Listener, error) {
		calls++
		if calls == 1 {
			var err error
			if occupant, err = net.Listen(network, address); err != nil {
				return nil, err
			}
		}
		return net.Listen(network, address)
	}
	udp, tcp, err := listenDNS("127.0.0.1:0", packet, stream)
	if occupant != nil {
		defer occupant.Close() //nolint:errcheck
	}
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close() //nolint:errcheck
	defer tcp.Close() //nolint:errcheck
	if calls < 2 || udp.LocalAddr().String() != tcp.Addr().String() {
		t.Fatal("did not retry onto a shared UDP/TCP port")
	}
	for _, abandoned := range packets[:len(packets)-1] {
		if _, err := abandoned.WriteTo([]byte("closed"), udp.LocalAddr()); !errors.Is(err, net.ErrClosed) {
			t.Fatal("abandoned UDP listener remains open")
		}
	}
}

func TestDNSPortAllocationErrors(t *testing.T) {
	for _, tc := range []struct {
		name, address string
		failure       error
		attempts      int
	}{
		{"explicit", "127.0.0.1:53", syscall.EADDRINUSE, 1},
		{"permission", "127.0.0.1:0", syscall.EACCES, 1},
		{"exhausted", "127.0.0.1:0", syscall.EADDRINUSE, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var packets []net.PacketConn
			packet := func(network, _ string) (net.PacketConn, error) {
				p, err := net.ListenPacket(network, "127.0.0.1:0")
				if err == nil {
					packets = append(packets, p)
				}
				return p, err
			}
			stream := func(string, string) (net.Listener, error) { return nil, tc.failure }
			if _, _, err := listenDNS(tc.address, packet, stream); !errors.Is(err, tc.failure) || len(packets) != tc.attempts {
				t.Fatalf("error %v, attempts %d", err, len(packets))
			}
			for _, p := range packets {
				if _, err := p.WriteTo([]byte("closed"), p.LocalAddr()); !errors.Is(err, net.ErrClosed) {
					t.Fatal("failed listener remains open")
				}
			}
		})
	}
}

func TestDNSTCPServesAfterDeniedPeer(t *testing.T) {
	s := startDNS(t, testDNSConfig())
	query := dnsPack(t, dnsQuery(1, testHost, dnsmessage.TypeA))
	denied := dnsDial(t, "tcp", s.tcp.Addr().String(), "127.0.0.2")
	_ = dnsWrite(denied, "tcp", query)
	if _, err := dnsRead(denied, "tcp"); err == nil {
		t.Fatal("denied peer answered")
	}
	// The refusal must not stop the listener for everyone else.
	expectAnswered(t, dnsExchange(t, dnsDial(t, "tcp", s.tcp.Addr().String(), "127.0.0.1"), "tcp", query), 1)
}
