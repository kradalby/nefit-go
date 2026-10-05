package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/kradalby/nefit-go/client"
)

func TestRunStopsAndReleasesListeners(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		name := "close"
		if cancelContext {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			s, err := New(Config{
				Device:        client.Config{SerialNumber: "123", AccessKey: "key", Password: "secret"},
				ListenAddress: "127.0.0.1:0", DeviceIP: net.ParseIP("127.0.0.1"),
				DNS: &DNSConfig{
					ListenAddress: "127.0.0.1:0", Hostname: client.DefaultHost,
					Address: netip.MustParseAddr("127.0.0.1"), DeviceIP: netip.MustParseAddr("127.0.0.1"),
				},
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
			wantErr := ErrClosed
			if cancelContext {
				cancel()
				wantErr = context.Canceled
			} else if err := s.Close(); err != nil {
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
			for _, address := range []string{s.LocalAddress().String(), s.dns.Address().String()} {
				conn, err := net.DialTimeout("tcp", address, time.Second)
				if err == nil {
					_ = conn.Close()
					t.Fatalf("listener at %s remained open", address)
				}
			}
		})
	}
}
