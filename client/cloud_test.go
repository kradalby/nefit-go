package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/kradalby/nefit-go/internal/xmpp"
	"github.com/kradalby/nefit-go/protocol"
)

// cloudScript varies the fake Bosch login after TLS.
type cloudScript struct {
	mechanism string // offered SASL mechanism; PLAIN when empty
	reject    string // "auth", "bind" or "session" is refused
	session   bool   // offer session establishment
}

func tlsCloudFixture(t *testing.T, scripts ...cloudScript) (Config, *tls.Config, <-chan *cloudFixture) {
	t.Helper()
	script := cloudScript{mechanism: "PLAIN"}
	if len(scripts) > 0 {
		script = scripts[0]
		if script.mechanism == "" {
			script.mechanism = "PLAIN"
		}
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test cloud"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	cfg := Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret", Host: "127.0.0.1", Port: port, ConnectTimeout: time.Second, RetryTimeout: time.Second, PingInterval: time.Hour}
	ready := make(chan *cloudFixture, 1)
	go func() {
		socket, err := ln.Accept()
		if err != nil {
			return
		}
		_ = socket.SetDeadline(time.Now().Add(5 * time.Second))
		reader := wire.NewReader(socket)
		if _, err = reader.Next(); err != nil {
			_ = socket.Close()
			return
		}
		_, _ = writeTyped(socket, wire.Frame{Stream: &wire.Stream{From: cfg.Host, Version: "1.0"}})
		_, _ = writeTyped(socket, wire.E(wire.StreamNS, "features", wire.E(wire.TLSNS, "starttls")))
		frame, err := reader.Next()
		if err != nil || frame.Element == nil || frame.Element.Name.Local != "starttls" {
			_ = socket.Close()
			return
		}
		_, _ = writeTyped(socket, wire.E(wire.TLSNS, "proceed"))
		secure := tls.Server(socket, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}})
		if err = secure.Handshake(); err != nil {
			_ = secure.Close()
			return
		}
		reader = wire.NewReader(secure)
		if _, err = reader.Next(); err != nil {
			_ = secure.Close()
			return
		}
		_, _ = writeTyped(secure, wire.Frame{Stream: &wire.Stream{From: cfg.Host, Version: "1.0"}})
		_, _ = writeTyped(secure, wire.E(wire.StreamNS, "features", wire.E(wire.SASLNS, "mechanisms", wire.Text(wire.SASLNS, "mechanism", script.mechanism))))
		frame, err = reader.Next()
		if err != nil || frame.Element == nil {
			_ = secure.Close()
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(frame.Element.Text())
		if err != nil || string(decoded) != "\x00rrccontact_"+cfg.SerialNumber+"\x00"+cfg.AuthPassword() {
			_ = secure.Close()
			return
		}
		if script.reject == "auth" {
			_, _ = writeTyped(secure, wire.E(wire.SASLNS, "failure", wire.E(wire.SASLNS, "not-authorized")))
			return
		}
		_, _ = writeTyped(secure, wire.SASLText("success", ""))
		if _, err = reader.Next(); err != nil {
			_ = secure.Close()
			return
		}
		_, _ = writeTyped(secure, wire.Frame{Stream: &wire.Stream{From: cfg.Host, Version: "1.0"}})
		features := wire.E(wire.StreamNS, "features", wire.E(wire.BindNS, "bind"))
		if script.session {
			features.Children = append(features.Children, wire.Node{Element: ptrElement(wire.E(wire.SessionNS, "session"))})
		}
		_, _ = writeTyped(secure, features)
		frame, err = reader.Next()
		if err != nil || frame.Element == nil {
			_ = secure.Close()
			return
		}
		if script.reject == "bind" {
			_, _ = writeTyped(secure, wire.IQ{Type: "error", ID: frame.Element.Get("id")})
			return
		}
		_, _ = writeTyped(secure, wire.IQ{Type: "result", ID: frame.Element.Get("id"), Bind: &wire.Bind{JID: cfg.JID() + "/nefit00101"}})
		if script.session {
			frame, err = reader.Next()
			if err != nil || frame.Element == nil || frame.Element.Child(wire.SessionNS, "session") == nil {
				_ = secure.Close()
				return
			}
			kind := "result"
			if script.reject == "session" {
				kind = "error"
			}
			_, _ = writeTyped(secure, wire.IQ{Type: kind, ID: frame.Element.Get("id")})
			if kind == "error" {
				return
			}
		}
		// Availability must be announced before Bosch can route replies.
		frame, err = reader.Next()
		if err != nil || frame.Element == nil || frame.Element.Name.Local != "presence" {
			_ = secure.Close()
			return
		}
		_ = secure.SetDeadline(time.Now().Add(5 * time.Second))
		ready <- &cloudFixture{conn: secure, reader: reader}
	}()
	return cfg, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}, ready
}

// cloudClient logs a client in to a TLS cloud fixture.
func cloudClient(t *testing.T) (*Client, *cloudFixture) {
	t.Helper()
	cfg, tlsCfg, ready := tlsCloudFixture(t)
	c := newTestClient(t, cfg)
	c.dial = func(ctx context.Context) (transport, error) { return dialCloud(ctx, cfg, tlsCfg) }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	cloud := wait(t, ready)
	t.Cleanup(func() { _ = cloud.conn.Close() })
	return c, cloud
}

func TestTypedCloudTLSAndRequest(t *testing.T) {
	t.Parallel()
	c, cloud := cloudClient(t)
	cfg := c.config
	result := make(chan error, 1)
	go func() { _, err := c.Get(t.Context(), "/x"); result <- err }()
	e := cloud.next(t)
	body := e.Child(wire.ClientNS, "body")
	if body == nil || requestMethod(body.Text()) != "GET" {
		t.Fatal("typed request missing")
	}
	cipher, err := c.encryptor.Encrypt(`{"id":"/x","value":1}`)
	if err != nil {
		t.Fatal(err)
	}
	cloud.write(t, wire.Message{From: c.config.ResourceJID(), To: cfg.JID() + "/nefit00101", Type: "chat", Body: wire.Body{Text: "HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\n" + cipher}})
	if err = wait(t, result); err != nil {
		t.Fatal(err)
	}
}
func requestMethod(text string) string { method, _ := requestLine(text); return method }

func TestTypedCloudHandshakeDeadline(t *testing.T) {
	t.Parallel()
	address := silentServer(t)
	host, portString, _ := net.SplitHostPort(address)
	port, _ := strconv.Atoi(portString)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	cfg := Config{Host: host, Port: port}
	if _, err := dialCloud(ctx, cfg, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handshake timeout: %v", err)
	}
}

// silentServer accepts connections and never answers.
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

type cloudAdmissionSocket struct {
	net.Conn
	once     sync.Once
	started  chan struct{}
	writes   atomic.Int32
	deadline atomic.Pointer[time.Time]
}

func (s *cloudAdmissionSocket) Write(data []byte) (int, error) {
	s.writes.Add(1)
	s.once.Do(func() { close(s.started) })
	return s.Conn.Write(data)
}

func (s *cloudAdmissionSocket) SetWriteDeadline(deadline time.Time) error {
	s.deadline.Store(&deadline)
	return s.Conn.SetWriteDeadline(deadline)
}

// testCloudTransport wraps an already logged-in cloud stream.
func testCloudTransport(socket net.Conn, input io.Reader) *cloudTransport {
	cfg := Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret"}.WithDefaults()
	t := &cloudTransport{socket: socket, cfg: cfg, jid: cfg.JID() + "/test", done: make(chan struct{})}
	t.resetReader(input)
	t.w = newSocketWriter(socket, t.done, func() { _ = t.Close() }, 15*time.Second)
	return t
}

func cloudAdmissionPipe(t *testing.T) (*cloudTransport, *cloudAdmissionSocket, net.Conn) {
	t.Helper()
	socket, peer := net.Pipe()
	s := &cloudAdmissionSocket{Conn: socket, started: make(chan struct{})}
	tr := testCloudTransport(s, io.MultiReader(strings.NewReader(`<stream:stream xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams">`), s))
	t.Cleanup(func() { _ = tr.Close(); _ = peer.Close() })
	if _, err := tr.reader.Next(); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return tr, s, peer
}

// Done signals that the caller reached the admission select, without timing
// assumptions about when its goroutine starts running.
type cloudAdmissionContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (ctx *cloudAdmissionContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestCloudCancelledBeforeAdmission(t *testing.T) {
	t.Parallel()
	tr, socket, peer := cloudAdmissionPipe(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// A free slot and a done context race in select; repeat so the check
	// after taking the slot is exercised too.
	for range 20 {
		p := new(pending)
		err := tr.Send(ctx, "expired", p)
		var unsent *unsentError
		if !errors.Is(err, context.Canceled) || !errors.As(err, &unsent) || p.sent.Load() || socket.writes.Load() != 0 {
			t.Fatalf("cancelled admission = %v, marked = %v", err, p.sent.Load())
		}
	}
	after := make(chan error, 1)
	go func() { after <- tr.Send(t.Context(), "after", nil) }()
	if _, err := wire.NewReader(peer).Next(); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, after); err != nil {
		t.Fatalf("cancelled admission retired the session: %v", err)
	}
}

func TestCloudAdmissionCancellation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"send", "presence", "iq"} {
		t.Run(kind, func(t *testing.T) {
			tr, socket, peer := cloudAdmissionPipe(t)
			active := make(chan error, 1)
			go func() {
				var err error
				switch kind {
				case "send":
					err = tr.Send(t.Context(), "ordinary", nil)
				case "presence":
					err = tr.Ping()
				case "iq":
					err = tr.write(wire.IQ{Type: "result", ID: "ping"})
				}
				active <- err
			}()
			wait(t, socket.started)
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &cloudAdmissionContext{Context: base, waiting: make(chan struct{})}
			expired := new(pending)
			queued := make(chan error, 1)
			go func() {
				err := tr.Send(ctx, "expired", expired)
				queued <- err
			}()
			wait(t, ctx.waiting)
			cancel()
			var unsent *unsentError
			if err := wait(t, queued); !errors.As(err, &unsent) || !errors.Is(err, context.Canceled) {
				t.Fatalf("queued cancellation = %v", err)
			}
			if expired.sent.Load() || socket.writes.Load() != 1 {
				t.Fatal("queued cancellation marked or wrote the request")
			}
			reader := wire.NewReader(peer)
			if _, err := reader.Next(); err != nil {
				t.Fatalf("active writer interrupted: %v", err)
			}
			if err := wait(t, active); err != nil {
				t.Fatal(err)
			}
			text := "GET /after HTTP/1.1\r\n\r\n"
			after := new(pending)
			go func() {
				err := tr.Send(t.Context(), text, after)
				queued <- err
			}()
			frame, err := reader.Next()
			if err != nil {
				t.Fatal(err)
			}
			body := frame.Element.Child("", "body")
			if body == nil || body.Text() != text {
				t.Fatal("cloud body lost CR or included the cancelled request")
			}
			if err := wait(t, queued); err != nil || !after.sent.Load() {
				t.Fatalf("subsequent send = %v, marked = %v", err, after.sent.Load())
			}
		})
	}
}

func TestCloudAdmissionDeadlinePreservesClientSession(t *testing.T) {
	t.Parallel()
	tr, socket, peer := cloudAdmissionPipe(t)
	c := newTestClient(t, Config{})
	var dials atomic.Int32
	c.dial = func(context.Context) (transport, error) { dials.Add(1); return tr, nil }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	cn := c.conn.Load()
	presence := make(chan error, 1)
	go func() { presence <- tr.Ping() }()
	wait(t, socket.started)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := c.roundTrip(ctx, "/expired", protocol.PutRequest("/expired", ""), false)
	var unsent *unsentError
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &unsent) || errors.Is(err, errUnanswered) {
		t.Fatalf("unsent deadline = %v", err)
	}
	if !cn.alive() || c.conn.Load() != cn || socket.writes.Load() != 1 {
		t.Fatal("unsent deadline changed the live session")
	}
	reader := wire.NewReader(peer)
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, presence); err != nil {
		t.Fatal(err)
	}
	after := make(chan error, 1)
	go func() {
		_, err := c.roundTrip(t.Context(), "/after", protocol.PutRequest("/after", ""), false)
		after <- err
	}()
	frame, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if body := frame.Element.Child("", "body"); body == nil || !strings.Contains(body.Text(), "PUT /after ") {
		t.Fatal("expired request reached the socket")
	}
	if _, err := writeTyped(peer, wire.Message{From: tr.cfg.ResourceJID() + "/RRC-RestApi", To: tr.jid, Type: "chat", Body: wire.Body{Text: "HTTP/1.0 204 No Content\r\n\r\n"}}); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, after); err != nil {
		t.Fatal(err)
	}
	if c.conn.Load() != cn || !cn.alive() || dials.Load() != 1 {
		t.Fatal("subsequent request replaced the healthy session")
	}
}

func TestCloudActiveWriteCancellation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"cancel", "deadline", "close"} {
		t.Run(kind, func(t *testing.T) {
			tr, socket, peer := cloudAdmissionPipe(t)
			ctx, cancel := context.WithCancel(t.Context())
			if kind == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 150*time.Millisecond)
			}
			defer cancel()
			activeRequest := new(pending)
			active := make(chan error, 1)
			go func() {
				err := tr.Send(ctx, "active", activeRequest)
				active <- err
			}()
			wait(t, socket.started)
			if !activeRequest.sent.Load() {
				t.Fatal("socket I/O preceded mark-sent")
			}
			if end, ok := ctx.Deadline(); kind == "deadline" && (!ok || !socket.deadline.Load().Equal(end)) {
				t.Fatal("active write did not use the caller's deadline")
			}
			queuedCtx := &cloudAdmissionContext{Context: t.Context(), waiting: make(chan struct{})}
			queuedRequest := new(pending)
			queued := make(chan error, 1)
			go func() {
				err := tr.Send(queuedCtx, "queued", queuedRequest)
				queued <- err
			}()
			wait(t, queuedCtx.waiting)
			switch kind {
			case "cancel":
				cancel()
			case "close":
				if err := tr.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var unsent *unsentError
			err := wait(t, active)
			if err == nil || errors.As(err, &unsent) {
				t.Fatalf("active cancellation = %v", err)
			}
			expected := context.Canceled
			if kind == "deadline" {
				expected = context.DeadlineExceeded
			}
			if kind != "close" && !errors.Is(err, expected) {
				t.Fatalf("active cancellation = %v, want %v", err, expected)
			}
			if err := wait(t, queued); !errors.As(err, &unsent) || !errors.Is(err, errSessionEnded) || queuedRequest.sent.Load() {
				t.Fatalf("queued request after retirement = %v, marked = %v", err, queuedRequest.sent.Load())
			}
			if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("socket remained open: %v", err)
			}
		})
	}
}

func TestCloudEncodingFailureBeforeAdmission(t *testing.T) {
	t.Parallel()
	tr, socket, peer := cloudAdmissionPipe(t)
	active := make(chan error, 1)
	go func() { active <- tr.Ping() }()
	wait(t, socket.started)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var marked atomic.Bool
	err := tr.writeContext(ctx, make(chan int), func() { marked.Store(true) })
	var unsupported *xml.UnsupportedTypeError
	var unsent *unsentError
	if !errors.As(err, &unsupported) || !errors.As(err, &unsent) || marked.Load() {
		t.Fatalf("encoding failure = %v, marked = %v", err, marked.Load())
	}
	if _, err := wire.NewReader(peer).Next(); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, active); err != nil {
		t.Fatalf("encoding failure interrupted active writer: %v", err)
	}
}

func TestCloudSentCancellationRetiresClientSession(t *testing.T) {
	t.Parallel()
	tr, socket, _ := cloudAdmissionPipe(t)
	c := newTestClient(t, Config{})
	c.dial = func(context.Context) (transport, error) { return tr, nil }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	cn := c.conn.Load()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := c.roundTrip(ctx, "/active", protocol.PutRequest("/active", ""), false)
		result <- err
	}()
	wait(t, socket.started)
	cn.mu.Lock()
	sent := cn.inflight != nil && cn.inflight.sent.Load()
	cn.mu.Unlock()
	if !sent {
		t.Fatal("active request was not marked sent")
	}
	cancel()
	if err := wait(t, result); !errors.Is(err, context.Canceled) || !errors.Is(err, errUnanswered) {
		t.Fatalf("sent cancellation = %v", err)
	}
	wait(t, cn.ctx.Done())
	if cn.alive() {
		t.Fatal("sent cancellation kept the session alive")
	}
}

type cloudWriteTimeout struct {
	net.Conn
	requested time.Time
}

func (s *cloudWriteTimeout) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		s.requested = deadline
		deadline = time.Now().Add(20 * time.Millisecond)
	}
	return s.Conn.SetWriteDeadline(deadline)
}

func TestCloudWriteTimeoutRetiresSession(t *testing.T) {
	t.Parallel()
	socket, peer := net.Pipe()
	s := &cloudWriteTimeout{Conn: socket}
	tr := testCloudTransport(s, socket)
	t.Cleanup(func() { _ = tr.Close(); _ = peer.Close() })
	start := time.Now()
	err := tr.Send(t.Context(), "stalled", nil)
	var timeout net.Error
	var unsent *unsentError
	if !errors.As(err, &timeout) || !timeout.Timeout() || errors.As(err, &unsent) {
		t.Fatalf("active timeout = %v", err)
	}
	if end := s.requested.Sub(start); end < 14*time.Second || end > 16*time.Second {
		t.Fatalf("default write deadline = %v", end)
	}
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("timed out socket remained open: %v", err)
	}
}

func TestCloudRequiresSTARTTLSBeforeCredentials(t *testing.T) {
	t.Parallel()
	for name, reply := range map[string]func(net.Conn, *wire.Reader){
		"not offered": func(s net.Conn, _ *wire.Reader) {
			_, _ = writeTyped(s, wire.E(wire.StreamNS, "features", wire.E(wire.SASLNS, "mechanisms", wire.Text(wire.SASLNS, "mechanism", "PLAIN"))))
		},
		"refused": func(s net.Conn, r *wire.Reader) {
			_, _ = writeTyped(s, wire.E(wire.StreamNS, "features", wire.E(wire.TLSNS, "starttls")))
			if _, err := r.Next(); err == nil {
				_, _ = writeTyped(s, wire.E(wire.TLSNS, "failure"))
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Whatever the client sends after refusal arrives before its close.
			after := make(chan string, 1)
			address, _ := serve(t, func(_ int32, s net.Conn) {
				r := wire.NewReader(s)
				if _, err := r.Next(); err != nil {
					return
				}
				_, _ = writeTyped(s, wire.Frame{Stream: &wire.Stream{From: "127.0.0.1", Version: "1.0"}})
				reply(s, r)
				_ = s.SetReadDeadline(time.Now().Add(2 * time.Second))
				if f, err := r.Next(); err == nil && f.Element != nil {
					after <- f.Element.Name.Local
				} else {
					after <- ""
				}
			})
			host, port, _ := net.SplitHostPort(address)
			cfg := Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret", Host: host}.WithDefaults()
			fmt.Sscan(port, &cfg.Port) //nolint:errcheck
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if _, err := dialCloud(ctx, cfg, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err == nil {
				t.Fatal("login without STARTTLS succeeded")
			}
			if name := wait(t, after); name != "" {
				t.Fatal("sent before TLS:", name)
			}
		})
	}
}

func TestCloudLoginRejections(t *testing.T) {
	t.Parallel()
	for want, script := range map[string]cloudScript{
		"did not offer PLAIN":            {mechanism: "SCRAM-SHA-1"},
		"authentication rejected":        {reject: "auth"},
		"resource binding rejected":      {reject: "bind"},
		"session establishment rejected": {session: true, reject: "session"},
	} {
		t.Run(want, func(t *testing.T) {
			cfg, tlsCfg, _ := tlsCloudFixture(t, script)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			// Each step must refuse on its own, not leave a later one to fail.
			tr, err := dialCloud(ctx, cfg, tlsCfg)
			if err == nil {
				_ = tr.Close()
				t.Fatal("rejected login succeeded")
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatal(err)
			}
		})
	}
	cfg, tlsCfg, ready := tlsCloudFixture(t, cloudScript{session: true})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	tr, err := dialCloud(ctx, cfg, tlsCfg)
	if err != nil {
		t.Fatal("login with session establishment failed:", err)
	}
	_ = tr.Close()
	wait(t, ready)
}

func TestCloudRecvSeparatesRepliesFromOtherSessions(t *testing.T) {
	t.Parallel()
	socket, peer := net.Pipe()
	tr := testCloudTransport(socket, io.MultiReader(strings.NewReader(`<stream:stream xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams">`), socket))
	t.Cleanup(func() { _ = tr.Close(); _ = peer.Close() })
	if _, err := tr.reader.Next(); err != nil {
		t.Fatal(err)
	}
	gateway := tr.cfg.ResourceJID() + "/RRC-RestApi"
	requests := deviceReader(t, peer)
	for _, tc := range []struct {
		name, from, to, typ string
		push, error         bool
	}{
		{"own reply", gateway, tr.jid, "chat", false, false},
		{"unaddressed reply", gateway, "", "chat", false, false},
		{"error reply", gateway, tr.jid, "error", false, true},
		{"reply to an app session", gateway, tr.cfg.JID() + "/4y64o779tt", "chat", true, false},
		{"other sender", "someone@" + tr.cfg.Host, tr.jid, "chat", true, false},
	} {
		sent := make(chan error, 1)
		go func() { sent <- tr.Send(t.Context(), protocol.PutRequest("/x", ""), new(pending)) }()
		nextRequest(t, requests, "PUT /x ")
		if err := wait(t, sent); err != nil {
			t.Fatal(err)
		}
		go func() {
			_, _ = writeTyped(peer, wire.Message{From: tc.from, To: tc.to, Type: tc.typ, Body: wire.Body{Text: "HTTP/1.0 204 No Content\r\n\r\n"}})
		}()
		in, err := tr.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if in.push != tc.push || in.error != tc.error {
			t.Errorf("%s: push = %v, error = %v", tc.name, in.push, in.error)
		}
	}
	// A restart mid-session has no stanza to classify.
	go func() {
		_ = wire.WriteFrame(peer, wire.Frame{Stream: &wire.Stream{Version: "1.0"}})
	}()
	if _, err := tr.Recv(); err == nil {
		t.Fatal("stream restart accepted mid-session")
	}
}

func TestCloudReplyOwnershipAcrossReads(t *testing.T) {
	t.Parallel()
	testReplyOwnershipAcrossReads(t, func(t *testing.T, c *Client, socket net.Conn) transport {
		tr := testCloudTransport(socket, socket)
		tr.jid = c.config.JID() + "/" + localResource
		return tr
	})
}

func TestCloudSessionOutlivesLoginDeadline(t *testing.T) {
	t.Parallel()
	// The fixture bounds the login to a second.
	c, _ := cloudClient(t)
	select {
	case <-c.Done():
		t.Fatal("cloud session ended at the login deadline")
	case <-time.After(1500 * time.Millisecond):
	}
}

func TestBackendDialVerifiesCertificate(t *testing.T) {
	t.Parallel()
	cfg, _, _ := tlsCloudFixture(t)
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	// Through c.dial, so the client's wiring is checked too.
	tr, err := c.dial(ctx)
	if err == nil {
		_ = tr.Close()
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); !ok {
		t.Fatal("backend dial accepted a certificate outside the system roots:", err)
	}
}

func TestCloudAnswersIQRequests(t *testing.T) {
	t.Parallel()
	c, cloud := cloudClient(t)
	cfg := c.config
	// Results and errors are no requests: answering them could loop.
	cloud.write(t, wire.IQ{Type: "result", ID: "r", From: cfg.Host})
	cloud.write(t, wire.IQ{Type: "error", ID: "e", From: cfg.Host})
	for _, kind := range []string{"get", "set", "ping"} {
		extension := wire.E("jabber:iq:version", "query")
		requestType := kind
		if kind == "ping" {
			extension = wire.E(wire.PingNS, "ping")
			requestType = "get"
		}
		query := wire.IQ{Type: requestType, ID: kind, From: cfg.Host, To: cfg.JID() + "/nefit00101", Extensions: []wire.Element{extension}}
		cloud.write(t, query)
		response := cloud.next(t)
		expectedType := "error"
		if kind == "ping" {
			expectedType = "result"
		}
		if response.Get("id") != query.ID || response.Get("type") != expectedType || response.Get("from") != query.To || response.Get("to") != query.From {
			t.Fatal("incorrect IQ response", response)
		}
		if expectedType == "error" {
			stanzaError := response.Child(wire.ClientNS, "error")
			if stanzaError == nil || stanzaError.Get("type") != "cancel" || stanzaError.Child("urn:ietf:params:xml:ns:xmpp-stanzas", "service-unavailable") == nil {
				t.Fatal("missing service-unavailable error")
			}
		}
	}
}
