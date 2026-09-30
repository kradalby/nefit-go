package client

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xmpp "github.com/xmppo/go-xmpp"
)

// stalledServer accepts one XMPP client, walks it through the handshake and
// then stops reading, as a peer that vanished without closing would.
func stalledServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		_ = ln.Close()
	})

	const header = "<?xml version='1.0'?><stream:stream xmlns='jabber:client'" +
		" xmlns:stream='http://etherx.jabber.org/streams' id='s' from='localhost' version='1.0'>"
	script := []struct{ after, reply string }{
		{"<stream:stream", header + "<stream:features><mechanisms xmlns='urn:ietf:params:xml:ns:xmpp-sasl'>" +
			"<mechanism>PLAIN</mechanism></mechanisms></stream:features>"},
		{"</auth>", "<success xmlns='urn:ietf:params:xml:ns:xmpp-sasl'/>"},
		{"<stream:stream", header + "<stream:features><bind xmlns='urn:ietf:params:xml:ns:xmpp-bind'/></stream:features>"},
		{"</iq>", "<iq type='result' id='b'><bind xmlns='urn:ietf:params:xml:ns:xmpp-bind'>" +
			"<jid>u@localhost/r</jid></bind></iq>"},
	}

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		var seen strings.Builder
		buf := make([]byte, 4096)
		for _, step := range script {
			for !strings.Contains(seen.String(), step.after) {
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				seen.Write(buf[:n])
			}
			seen.Reset()
			if _, err := conn.Write([]byte(step.reply)); err != nil {
				return
			}
		}
		<-stop
	}()

	return ln.Addr().String()
}

// silentServer accepts connections and never answers, as a backend that
// stalls mid-handshake would.
func silentServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})

	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
			})
		}
	})

	return ln.Addr().String()
}

// xmppClient returns a client that logs in to addr with real go-xmpp.
func xmppClient(t *testing.T, addr string, cfg Config) *Client {
	t.Helper()

	cfg.SerialNumber = "123456789"
	cfg.AccessKey = "abcdefghijklmnop"
	cfg.Password = "secret"
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.SetLogger(slog.New(slog.DiscardHandler))
	c.dial = func(ctx context.Context) (transport, error) {
		return dialXMPP(ctx, addr, xmpp.Options{
			User:                         "u@localhost",
			Password:                     "p",
			NoTLS:                        true,
			InsecureAllowUnencryptedAuth: true,
		})
	}
	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestConnectGivesUpOnStalledHandshake(t *testing.T) {
	c := xmppClient(t, silentServer(t), Config{RetryTimeout: 200 * time.Millisecond, ConnectTimeout: time.Hour})

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(ctx) }()
	if err := wait(t, connected); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect = %v, want deadline exceeded", err)
	}

	// A request waiting on the same handshake keeps its own deadline.
	got := make(chan error, 1)
	go func() {
		_, err := c.Get(t.Context(), "/x")
		got <- err
	}()
	if err := wait(t, got); err == nil {
		t.Fatal("Get succeeded without a session")
	}

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	if err := wait(t, closed); err != nil {
		t.Fatal(err)
	}
}

func TestConnectTimeoutBoundsHandshake(t *testing.T) {
	c := xmppClient(t, silentServer(t), Config{ConnectTimeout: 200 * time.Millisecond})

	connected := make(chan error, 1)
	go func() { connected <- c.Connect(context.Background()) }()
	if err := wait(t, connected); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect = %v, want deadline exceeded", err)
	}
}

func TestCloseWithStalledPeer(t *testing.T) {
	c := xmppClient(t, stalledServer(t), Config{})
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Fill the socket buffers until a write blocks.
	cn := c.conn.Load()
	var sent atomic.Int64
	go func() {
		chat := xmpp.Chat{Type: "chat", Text: strings.Repeat("x", 1<<16)}
		for {
			if _, err := cn.xmpp.Send(chat); err != nil {
				return
			}
			sent.Add(1)
		}
	}()
	for last := int64(-1); last != sent.Load(); {
		last = sent.Load()
		time.Sleep(100 * time.Millisecond)
	}

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	if err := wait(t, closed); err != nil {
		t.Fatal(err)
	}
}
