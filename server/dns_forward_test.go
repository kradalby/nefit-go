package server

import (
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDNSForwardTCPReturnsCompleteResponse(t *testing.T) {
	resolver, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resolver.Close() }()
	tcp, err := net.Listen("tcp", resolver.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tcp.Close() }()
	var tcpSeen atomic.Bool
	go func() {
		conn, err := tcp.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }() //nolint:errcheck
		tcpSeen.Store(true)
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		var size [2]byte
		if _, err = io.ReadFull(conn, size[:]); err != nil {
			return
		}
		raw := make([]byte, int(binary.BigEndian.Uint16(size[:])))
		if _, err = io.ReadFull(conn, raw); err != nil {
			return
		}
		var query dnsmessage.Message
		if query.Unpack(raw) != nil {
			return
		}
		query.Response = true
		query.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: query.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}, Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}}}}
		reply, err := query.Pack()
		if err != nil {
			return
		}
		binary.BigEndian.PutUint16(size[:], uint16(len(reply)))
		_, _ = conn.Write(append(size[:], reply...))
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, a, e := resolver.ReadFrom(buf)
			if e != nil {
				return
			}
			var q dnsmessage.Message
			if q.Unpack(buf[:n]) != nil {
				return
			}
			q.Response = true
			q.Truncated = true
			raw, _ := q.Pack()
			_, _ = resolver.WriteTo(raw, a)
		}
	}()
	s, err := NewDNS(DNSConfig{ListenAddress: "127.0.0.1:0", Hostname: "original.example", Address: netip.MustParseAddr("127.0.0.1"), DeviceIP: netip.MustParseAddr("127.0.0.1"), ForwardAddress: resolver.LocalAddr().String()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	name, _ := dnsmessage.NewName("unrelated.example.")
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: 42, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	raw, _ := query.Pack()
	for _, network := range []string{"udp", "tcp"} {
		c, err := net.Dial(network, s.Address().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(time.Second))
		var size [2]byte
		if network == "tcp" {
			binary.BigEndian.PutUint16(size[:], uint16(len(raw)))
			_, _ = c.Write(size[:])
		}
		_, _ = c.Write(raw)
		b := make([]byte, 4096)
		var n int
		if network == "tcp" {
			_, err = io.ReadFull(c, size[:])
			if err != nil {
				t.Fatal(err)
			}
			n = int(binary.BigEndian.Uint16(size[:]))
			_, err = io.ReadFull(c, b[:n])
		} else {
			n, err = c.Read(b)
		}
		_ = c.Close()
		if err != nil {
			t.Fatal(err)
		}
		var response dnsmessage.Message
		if err = response.Unpack(b[:n]); err != nil {
			t.Fatal(err)
		}
		if network == "udp" && !response.Truncated {
			t.Fatal("UDP lost upstream truncation signal")
		}
		if network == "tcp" && (response.Truncated || len(response.Answers) != 1 || !tcpSeen.Load()) {
			t.Fatal("TCP retry did not return full answer", response)
		}
	}
}
