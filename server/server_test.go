package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/kradalby/nefit-go/client"
)

func TestSetLoggerReachesDNS(t *testing.T) {
	s, err := New(Config{
		Device:       testDevice,
		LocalOptions: client.LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1")},
		DNS:          &DNSConfig{ListenAddress: "127.0.0.1:0", Address: netip.MustParseAddr("127.0.0.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	logger := slog.New(slog.DiscardHandler)
	s.SetLogger(logger)
	if s.dns.logger.Load() != logger {
		t.Fatal("DNS endpoint kept its logger")
	}
}

func TestRunStopsAndReleasesListeners(t *testing.T) {
	for _, stop := range []string{"close", "cancel", "client close"} {
		for _, device := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/device=%t", stop, device), func(t *testing.T) {
				s, err := New(Config{
					Device:       testDevice,
					LocalOptions: client.LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1")},
					DNS:          &DNSConfig{ListenAddress: "127.0.0.1:0", Address: netip.MustParseAddr("127.0.0.1")},
				})
				if err != nil {
					t.Fatal(err)
				}
				s.SetLogger(slog.New(slog.DiscardHandler))
				t.Cleanup(func() { _ = s.Close() })
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				go func() { result <- s.Run(ctx) }()
				if device {
					loginFakeDevice(t, s.Client, testDevice)
				}
				wantErr := client.ErrClosed
				switch stop {
				case "close":
					err = s.Close()
				case "cancel":
					cancel()
					wantErr = context.Canceled
				case "client close":
					// The embedded client bypasses Server.Close; Run must still
					// notice and release the DNS endpoint.
					err = s.Client.Close()
				}
				if err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-result:
					if !errors.Is(err, wantErr) {
						t.Fatalf("Run returned %v, want %v", err, wantErr)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Run did not stop")
				}
				for _, address := range []net.Addr{s.LocalAddress(), s.dns.tcp.Addr()} {
					if listenerFD(address.(*net.TCPAddr).Port) >= 0 {
						t.Fatalf("listener at %s remained open", address)
					}
				}
			})
		}
	}
}

func TestDNSSRVMatchesListener(t *testing.T) {
	for _, override := range []uint16{0, 15222} {
		t.Run(fmt.Sprint(override), func(t *testing.T) {
			dnsConfig := &DNSConfig{ListenAddress: "127.0.0.1:0", Address: netip.MustParseAddr("127.0.0.1"), XMPPPort: override}
			s, err := New(Config{Device: client.Config{SerialNumber: "123", AccessKey: "key", Password: "secret", Host: "original.example"}, LocalOptions: client.LocalOptions{DeviceIP: netip.MustParseAddr("127.0.0.1"), ListenAddress: "127.0.0.1:0"}, DNS: dnsConfig})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			msg := dnsAnswer(t, s.dns, dnsQuery(1, "_xmpp-client._tcp.original.example.", dnsmessage.TypeSRV))
			expected := cmp.Or(override, uint16(s.LocalAddress().(*net.TCPAddr).Port))
			if len(msg.Answers) != 1 || msg.Answers[0].Body.(*dnsmessage.SRVResource).Port != expected {
				t.Fatalf("wrong SRV port, expected %d: %+v", expected, msg)
			}
			if dnsConfig.XMPPPort != override {
				t.Fatal("caller DNS configuration mutated")
			}
		})
	}
}

func TestRunEndsOnListenerFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("relies on Linux failing accept after shutdown(SHUT_RD)")
	}
	s, err := New(Config{
		Device:       client.Config{SerialNumber: "123", AccessKey: "key", Password: "secret"},
		LocalOptions: client.LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SetLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = s.Close() })
	result := make(chan error, 1)
	go func() { result <- s.Run(t.Context()) }()
	shutdownListener(t, s.LocalAddress().(*net.TCPAddr))
	// Retrying would spin forever, and the supervisor would never restart it.
	select {
	case err := <-result:
		if !errors.Is(err, client.ErrListenerFailed) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run kept going on a failed listener")
	}
}

func TestNewReleasesListenerOnDNSFailure(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address, port := probe.Addr().String(), probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	for name, tc := range map[string]struct {
		device, listen, address string
	}{
		"wildcard DNS listen": {"127.0.0.1", "0.0.0.0:0", "127.0.0.1"},
		// The DNS answer must be an address the XMPP listener accepts on.
		"unreachable answer": {"127.0.0.1", "127.0.0.1:0", "192.0.2.1"},
		"unspecified answer": {"127.0.0.1", "127.0.0.1:0", "0.0.0.0"},
		// Loopback is only an answer for a device on this host.
		"loopback answer": {"192.0.2.10", "127.0.0.1:0", "127.0.0.1"},
	} {
		_, err = New(Config{
			Device:       client.Config{SerialNumber: "123", AccessKey: "key", Password: "secret"},
			LocalOptions: client.LocalOptions{ListenAddress: address, DeviceIP: netip.MustParseAddr(tc.device)},
			DNS:          &DNSConfig{ListenAddress: tc.listen, Address: netip.MustParseAddr(tc.address)},
		})
		if err == nil {
			t.Fatal(name, "accepted")
		}
		// A retried New must be able to bind again.
		if listenerFD(port) >= 0 {
			t.Fatal(name, "leaked the XMPP listener")
		}
	}
}

func TestRunRetriesConnectTimeouts(t *testing.T) {
	s, err := New(Config{
		Device:       client.Config{SerialNumber: "123", AccessKey: "key", Password: "secret", ConnectTimeout: 20 * time.Millisecond},
		LocalOptions: client.LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SetLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = s.Close() })
	// No device logs in; each Connect times out and Run keeps waiting.
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := s.Run(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) < 1400*time.Millisecond {
		t.Fatalf("Run returned %v after %v", err, time.Since(start))
	}
}

func TestDNSMatchesDeviceSpellings(t *testing.T) {
	// A mapped device IP must match the IPv4 peer, and any spelling of the
	// domain must answer the device's own lower-case query.
	s, err := New(Config{
		Device:       client.Config{SerialNumber: "123", AccessKey: "key", Password: "secret", Host: strings.ToUpper(client.DefaultHost) + "."},
		LocalOptions: client.LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("::ffff:127.0.0.1")},
		DNS:          &DNSConfig{ListenAddress: "127.0.0.1:0", Address: netip.MustParseAddr("127.0.0.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SetLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = s.Close() })
	conn := dnsDial(t, "udp", s.dns.udp.LocalAddr().String(), "127.0.0.1")
	reply := dnsExchange(t, conn, "udp", dnsPack(t, dnsQuery(1, client.DefaultHost+".", dnsmessage.TypeA)))
	if len(reply.Answers) != 1 {
		t.Fatalf("device query unanswered: %+v", reply)
	}
}

func TestDNSWithWildcardXMPPListener(t *testing.T) {
	for address, ok := range map[string]bool{"192.0.2.1": true, "0.0.0.0": false} {
		t.Run(address, func(t *testing.T) {
			// A remote device, as in every real takeover.
			s, err := New(Config{
				Device:       client.Config{SerialNumber: "123", AccessKey: "key", Password: "secret"},
				LocalOptions: client.LocalOptions{ListenAddress: "0.0.0.0:0", DeviceIP: netip.MustParseAddr("192.0.2.10")},
				DNS:          &DNSConfig{ListenAddress: "127.0.0.1:0", Address: netip.MustParseAddr(address)},
			})
			if err == nil {
				_ = s.Close()
			}
			if (err == nil) != ok {
				t.Fatalf("New = %v, want ok=%t", err, ok)
			}
		})
	}
}
