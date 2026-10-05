package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"testing"
	"time"

	wire "github.com/kradalby/nefit-go/xmpp"
)

func tlsCloudFixture(t *testing.T) (Config, *tls.Config, <-chan *cloudFixture) {
	t.Helper()
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
		_, _ = writeTyped(secure, wire.E(wire.StreamNS, "features", wire.E(wire.SASLNS, "mechanisms", wire.Text(wire.SASLNS, "mechanism", "PLAIN"))))
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
		_, _ = writeTyped(secure, wire.SASLText("success", ""))
		if _, err = reader.Next(); err != nil {
			_ = secure.Close()
			return
		}
		_, _ = writeTyped(secure, wire.Frame{Stream: &wire.Stream{From: cfg.Host, Version: "1.0"}})
		_, _ = writeTyped(secure, wire.E(wire.StreamNS, "features", wire.E(wire.BindNS, "bind")))
		frame, err = reader.Next()
		if err != nil || frame.Element == nil {
			_ = secure.Close()
			return
		}
		_, _ = writeTyped(secure, wire.IQ{Type: "result", ID: frame.Element.Get("id"), Bind: &wire.Bind{JID: cfg.JID() + "/nefit00101"}})
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

func TestTypedCloudTLSAndRequest(t *testing.T) {
	cfg, tlsCfg, ready := tlsCloudFixture(t)
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.SetLogger(slog.New(slog.DiscardHandler))
	c.dial = func(ctx context.Context) (transport, error) { return dialCloud(ctx, cfg, tlsCfg) }
	t.Cleanup(func() { _ = c.Close() })
	if err = c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	cloud := wait(t, ready)
	t.Cleanup(func() { _ = cloud.conn.Close() })
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
func TestTypedCloudRejectsUntrustedTLS(t *testing.T) {
	cfg, tlsCfg, _ := tlsCloudFixture(t)
	tlsCfg.RootCAs = x509.NewCertPool()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := dialCloud(ctx, cfg, tlsCfg); err == nil {
		t.Fatal("untrusted TLS accepted")
	}
}

func TestTypedCloudHandshakeDeadline(t *testing.T) {
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
