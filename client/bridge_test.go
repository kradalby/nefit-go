package client

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	wire "github.com/kradalby/nefit-go/xmpp"
)

type cloudFixture struct {
	conn   net.Conn
	reader *wire.Reader
	mu     sync.Mutex
}

func (c *cloudFixture) write(t *testing.T, v any) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := writeTyped(c.conn, v); err != nil {
		t.Fatal(err)
	}
}

func (c *cloudFixture) next(t *testing.T) *wire.Element {
	t.Helper()
	for {
		f, err := c.reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if f.Element != nil {
			return f.Element
		}
	}
}

func bothTestClient(t *testing.T, policy UpdatePolicy) (*Client, *deviceFixture, *cloudFixture) {
	return bothHandshakeClient(t, policy, nil, nil)
}

func bothHandshakeClient(t *testing.T, policy UpdatePolicy, cloudHook func(*cloudFixture, wire.Frame), deviceHook func(*deviceFixture)) (*Client, *deviceFixture, *cloudFixture) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cloudReady := make(chan *cloudFixture, 1)
	handshakeErr := make(chan error, 1)
	go func() {
		s, err := ln.Accept()
		if err != nil {
			handshakeErr <- err
			return
		}
		cloud := &cloudFixture{conn: s, reader: wire.NewReader(s)}
		_ = s.SetDeadline(time.Now().Add(5 * time.Second))
		stage := 0
		for {
			f, err := cloud.reader.Next()
			if err != nil {
				handshakeErr <- err
				return
			}
			if f.Element != nil && f.Element.Get("id") == "service-control" {
				continue
			}
			if f.Element != nil && f.Element.Get("id") == "blocked-handshake" {
				handshakeErr <- errors.New("device update passed handshake policy")
				return
			}
			var response any
			if f.Stream != nil {
				if _, err = writeTyped(s, wire.Frame{Stream: &wire.Stream{From: DefaultHost, Version: "1.0"}}); err != nil {
					handshakeErr <- err
					return
				}
				if stage == 0 {
					response = wire.E(wire.StreamNS, "features", wire.E(wire.SASLNS, "mechanisms", wire.Text(wire.SASLNS, "mechanism", "DIGEST-MD5")))
				} else {
					response = wire.E(wire.StreamNS, "features", wire.E(wire.BindNS, "bind"), wire.E(wire.SessionNS, "session"))
				}
			} else if f.Element != nil {
				e := f.Element
				switch e.Name.Local {
				case "auth":
					response = wire.SASLText("challenge", "Zml4dHVyZQ==")
				case "response":
					response = wire.SASLText("success", "cnNwYXV0aD0wMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMA==")
					stage = 1
				case "iq":
					if e.Child(wire.BindNS, "bind") != nil {
						response = wire.IQ{Type: "result", ID: e.Get("id"), Bind: &wire.Bind{JID: "rrcgateway_123456789@" + DefaultHost + "/RRC-RestApi"}}
					} else {
						response = wire.IQ{Type: "result", ID: e.Get("id")}
					}
				case "presence":
					cloudReady <- cloud
					return
				default:
					handshakeErr <- io.ErrUnexpectedEOF
					return
				}
			}
			if _, err = writeTyped(s, response); err != nil {
				handshakeErr <- err
				return
			}
			if cloudHook != nil {
				cloudHook(cloud, f)
			}
		}
	}()
	c, err := NewLocalClient(Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret", RetryTimeout: 3 * time.Second, ConnectTimeout: 3 * time.Second, PingInterval: time.Hour}, LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: net.ParseIP("127.0.0.1"), Mode: ModeBoth, UpstreamAddress: ln.Addr().String(), UpdatePolicy: policy, RequestTimeout: 3 * time.Second, ReconnectInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = c.Close() })
	d := connectHandshakeFixture(t, c, deviceHook)
	select {
	case cloud := <-cloudReady:
		t.Cleanup(func() { _ = cloud.conn.Close() })
		return c, d, cloud
	case err := <-handshakeErr:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("cloud handshake stalled")
	}
	return nil, nil, nil
}

func TestBothSerializesCloudAndLocalRequests(t *testing.T) {
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	localDone := make(chan error, 1)
	go func() { _, err := c.Get(t.Context(), "/local"); localDone <- err }()
	request := d.readThrough(t, "</message>")
	if !strings.Contains(request, "GET /local ") {
		t.Fatal("wrong first request")
	}
	cloud.write(t, wire.Message{From: c.config.JID() + "/phone", To: c.config.ResourceJID(), Type: "chat", Body: wire.Body{Text: "GET /cloud HTTP/1.1\r\n\r\n"}})
	encrypted, err := c.encryptor.Encrypt(`{"id":"/local","value":1}`)
	if err != nil {
		t.Fatal(err)
	}
	d.reply(t, c, "HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\n"+encrypted)
	if err := wait(t, localDone); err != nil {
		t.Fatal(err)
	}
	request = d.readThrough(t, "</message>")
	var m wire.Message
	var stream bytes.Buffer
	if err := wire.WriteFrame(&stream, wire.Frame{Stream: &wire.Stream{Version: "1.0"}}); err != nil {
		t.Fatal(err)
	}
	stream.WriteString(request)
	reader := wire.NewReader(&stream)
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	frame, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	value, err := frame.Element.Typed()
	if err != nil {
		t.Fatal(err)
	}
	m = *value.(*wire.Message)
	if m.From != c.config.JID()+"/phone" || !strings.Contains(m.Body.Text, "GET /cloud ") {
		t.Fatal("cloud request lost or interleaved")
	}
	encrypted, err = c.encryptor.Encrypt(`{"id":"/cloud","value":2}`)
	if err != nil {
		t.Fatal(err)
	}
	d.write(t, marshalTest(t, wire.Message{To: c.config.JID() + "/phone", Type: "chat", Body: wire.Body{Text: "HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\n" + encrypted}}))
	e := cloud.next(t)
	if e.Name.Local != "message" || e.Get("to") != c.config.JID()+"/phone" {
		t.Fatal("cloud response not forwarded")
	}
}

func TestBothPreservesQueuedRequestMetadata(t *testing.T) {
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	body := wire.Text(wire.ClientNS, "body", "GET /cloud HTTP/1.1\r\n\r\n")
	body.Attr = []xml.Attr{{Name: xml.Name{Space: wire.XMLNS, Local: "lang"}, Value: "nl"}}
	extra := wire.Text("urn:example", "extra", "request metadata")
	m := wire.E(wire.ClientNS, "message", extra, body)
	m.Set("from", c.config.JID()+"/phone")
	m.Set("to", c.config.ResourceJID())
	m.Set("custom", "keep me")
	cloud.write(t, m)
	raw := d.readThrough(t, "</message>")
	var decoded wire.Element
	if err := xml.Unmarshal([]byte(raw), &decoded); err != nil {
		// Device stanzas inherit jabber:client from the open stream.
		t.Fatal(err)
	}
	if decoded.Get("custom") != "keep me" || len(decoded.Children) != 2 || decoded.Children[0].Element.Name.Space != "urn:example" {
		t.Fatal("queued relay lost metadata or extension order")
	}
	received := decoded.Children[1].Element
	if len(received.Attr) != 1 || received.Attr[0] != body.Attr[0] {
		t.Fatal("queued relay lost body attributes")
	}
}

func marshalTest(t *testing.T, v any) string {
	t.Helper()
	raw, err := xml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBothCloudLossKeepsLocalControl(t *testing.T) {
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	_ = cloud.conn.Close()
	deadline := time.Now().Add(time.Second)
	for c.UpstreamConnected() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.UpstreamConnected() {
		t.Fatal("cloud failure not detected")
	}
	result := make(chan error, 1)
	go func() { result <- c.Put(t.Context(), "/local", map[string]any{"value": 1}) }()
	d.readThrough(t, "</message>")
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, result); err != nil {
		t.Fatal(err)
	}
	if !c.IsConnected() {
		t.Fatal("cloud loss retired local device")
	}
}

func TestBothBlocksUpdateWrites(t *testing.T) {
	c, d, cloud := bothTestClient(t, UpdatesBlock)
	update := wire.IQ{Type: "get", ID: "update-service", From: "gservice_update@" + DefaultHost, To: c.config.ResourceJID()}
	cloud.write(t, update)
	cloud.write(t, wire.Message{From: c.config.JID() + "/phone", To: c.config.ResourceJID(), Body: wire.Body{Text: "PUT /gateway/update/strategy HTTP/1.1\r\n\r\n"}})
	e := cloud.next(t)
	value, err := e.Typed()
	if err != nil {
		t.Fatal(err)
	}
	m := value.(*wire.Message)
	if !strings.Contains(m.Body.Text, "403 Forbidden") {
		t.Fatal("update request not rejected")
	}
	cloud.write(t, wire.Message{From: c.config.JID() + "/phone", To: c.config.ResourceJID(), Body: wire.Body{Text: "GET /gateway/versionFirmware HTTP/1.1\r\n\r\n"}})
	read := d.readThrough(t, "</message>")
	if !strings.Contains(read, "GET /gateway/versionFirmware ") {
		t.Fatal("ordinary read blocked")
	}
	if strings.Contains(read, "update-service") {
		t.Fatal("non-message firmware service traffic passed through policy")
	}
}

func TestBothOfflineFallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	_ = ln.Close()
	c, err := NewLocalClient(Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret", ConnectTimeout: time.Second, RetryTimeout: time.Second, PingInterval: time.Hour}, LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: net.ParseIP("127.0.0.1"), Mode: ModeBoth, UpstreamAddress: address, RequestTimeout: time.Second, ReconnectInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = c.Close() })
	d := connectFixture(t, c)
	if c.UpstreamConnected() {
		t.Fatal("unavailable cloud marked connected")
	}
	result := make(chan error, 1)
	go func() { result <- c.Put(context.Background(), "/local", map[string]int{"value": 1}) }()
	d.readThrough(t, "</message>")
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, result); err != nil {
		t.Fatal(err)
	}
}
