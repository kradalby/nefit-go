package server

import (
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDNSTakeoverUDPAndTCP(t *testing.T) {
	cfg := DNSConfig{ListenAddress: "127.0.0.1:0", Hostname: "original.example", Address: netip.MustParseAddr("10.65.0.27"), DeviceIP: netip.MustParseAddr("127.0.0.1")}
	s, err := NewDNS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, network := range []string{"udp", "tcp"} {
		for _, kind := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA, dnsmessage.TypeSRV} {
			t.Run(network+kind.String(), func(t *testing.T) {
				name := "original.example."
				if kind == dnsmessage.TypeSRV {
					name = "_xmpp-client._tcp." + name
				}
				questionName, _ := dnsmessage.NewName(name)
				query := dnsmessage.Message{Header: dnsmessage.Header{ID: 42, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: questionName, Type: kind, Class: dnsmessage.ClassINET}}}
				raw, err := query.Pack()
				if err != nil {
					t.Fatal(err)
				}
				conn, err := net.Dial(network, s.Address().String())
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close() //nolint:errcheck
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				var length [2]byte
				if network == "tcp" {
					binary.BigEndian.PutUint16(length[:], uint16(len(raw)))
					if _, err := conn.Write(length[:]); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := conn.Write(raw); err != nil {
					t.Fatal(err)
				}
				response := make([]byte, 4096)
				var n int
				if network == "tcp" {
					if _, err := io.ReadFull(conn, length[:]); err != nil {
						t.Fatal(err)
					}
					n = int(binary.BigEndian.Uint16(length[:]))
					if _, err := io.ReadFull(conn, response[:n]); err != nil {
						t.Fatal(err)
					}
				} else {
					n, err = conn.Read(response)
					if err != nil {
						t.Fatal(err)
					}
				}
				var message dnsmessage.Message
				if err := message.Unpack(response[:n]); err != nil {
					t.Fatal(err)
				}
				if message.ID != 42 || !message.Authoritative || !message.Response {
					t.Fatal("incorrect response header")
				}
				switch kind {
				case dnsmessage.TypeA:
					if len(message.Answers) != 1 || message.Answers[0].Body.(*dnsmessage.AResource).A != cfg.Address.As4() {
						t.Fatal("incorrect redirected address")
					}
				case dnsmessage.TypeAAAA:
					if len(message.Answers) != 0 {
						t.Fatal("IPv6 would bypass takeover")
					}
				case dnsmessage.TypeSRV:
					if len(message.Answers) != 1 || message.Answers[0].Body.(*dnsmessage.SRVResource).Port != 5222 {
						t.Fatal("incorrect SRV record")
					}
				}
			})
		}
	}
}

func TestDNSIsScoped(t *testing.T) {
	s := &DNS{cfg: DNSConfig{Hostname: "original.example.", DeviceIP: netip.MustParseAddr("192.0.2.1"), Address: netip.MustParseAddr("10.65.0.27")}}
	if s.allowed(&net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 53}) {
		t.Fatal("unrelated peer admitted")
	}
	name, _ := dnsmessage.NewName("other.example.")
	raw, _ := (&dnsmessage.Message{Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}).Pack()
	reply, err := s.answer(raw)
	if err != nil {
		t.Fatal(err)
	}
	var response dnsmessage.Message
	if err := response.Unpack(reply); err != nil {
		t.Fatal(err)
	}
	if response.RCode != dnsmessage.RCodeRefused {
		t.Fatal("unrelated DNS name answered")
	}
}
