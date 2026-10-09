package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
		// Error, not Fatal: hooks call this from the fixture's goroutine.
		t.Error(err)
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

func bothHandshakeClient(t *testing.T, policy UpdatePolicy, cloudHook func(*cloudFixture, wire.Frame), deviceHook func(*deviceFixture), configure ...func(*Config, *LocalOptions)) (*Client, *deviceFixture, *cloudFixture) {
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
		if err := fakeBosch(cloud, nil, cloudHook); err != nil {
			handshakeErr <- err
			return
		}
		cloudReady <- cloud
	}()
	options := LocalOptions{Mode: ModeBoth, UpstreamAddress: ln.Addr().String(), UpdatePolicy: policy, RequestTimeout: 3 * time.Second, ReconnectInterval: time.Hour}
	config := Config{RetryTimeout: 3 * time.Second, ConnectTimeout: 3 * time.Second}
	for _, f := range configure {
		f(&config, &options)
	}
	c := newLocalTestClient(t, config, options)
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
	t.Parallel()
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
	if frame.Element.Get("from") != c.config.JID()+"/phone" || !strings.Contains(frame.Element.Child(wire.ClientNS, "body").Text(), "GET /cloud ") {
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
	t.Parallel()
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

// testBridge serves an already logged-in device on socket, optionally
// relaying to upstream as if its Bosch login had succeeded.
func testBridge(t *testing.T, c *Client, socket, upstream net.Conn, options LocalOptions) *localTransport {
	t.Helper()
	options.logf = c.log
	tr := newLocalTransport(socket, c.config, options)
	tr.encryptor, tr.health = c.encryptor, new(upstreamHealth)
	tr.initBridge(upstream)
	tr.upstreamConnected.Store(upstream != nil)
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func marshalTest(t *testing.T, v any) string {
	t.Helper()
	raw, err := xml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBothBlocksUpdateWrites(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesBlock)
	update := wire.IQ{Type: "get", ID: "update-service", From: "gservice_update@" + DefaultHost, To: c.config.ResourceJID()}
	cloud.write(t, update)
	cloud.write(t, wire.Message{From: c.config.JID() + "/phone", To: c.config.ResourceJID(), Body: wire.Body{Text: "PUT /gateway/update/strategy HTTP/1.1\r\n\r\n"}})
	if !strings.Contains(cloud.next(t).Child(wire.ClientNS, "body").Text(), "403 Forbidden") {
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

// unreachable refuses connections at once: a privileged port nothing listens
// on, which no parallel test can take over the way it could a closed
// ephemeral one.
const unreachable = "127.0.0.1:1"

func TestBothOfflineFallback(t *testing.T) {
	t.Parallel()
	address := unreachable
	c := newLocalTestClient(t, Config{ConnectTimeout: time.Second, RetryTimeout: time.Second}, LocalOptions{Mode: ModeBoth, UpstreamAddress: address, RequestTimeout: time.Second, ReconnectInterval: time.Hour})
	d := connectFixture(t, c)
	if c.UpstreamConnected() {
		t.Fatal("unavailable cloud marked connected")
	}
	if c.conn.Load().xmpp.(*localTransport).health.ready() {
		t.Fatal("unreachable Bosch did not back off relaying")
	}
	result := make(chan error, 1)
	go func() { result <- c.Put(context.Background(), "/local", map[string]int{"value": 1}) }()
	d.readThrough(t, "</message>")
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, result); err != nil {
		t.Fatal(err)
	}
}

func waitCloudLoss(t *testing.T, c *Client) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for c.UpstreamConnected() && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if c.UpstreamConnected() || !c.IsConnected() {
		t.Fatal("cloud loss did not preserve device session")
	}
}

func cloudReply(t *testing.T, c *Client, d *deviceFixture) {
	t.Helper()
	cipher, err := c.encryptor.Encrypt(`{"id":"/cloud","value":1}`)
	if err != nil {
		t.Fatal(err)
	}
	d.write(t, marshalTest(t, wire.Message{To: c.config.JID() + "/phone", Type: "chat", Body: wire.Body{Text: "HTTP/1.0 200 OK\r\n\r\n" + cipher}}))
}

func localWrite(t *testing.T, c *Client, d *deviceFixture) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/local", map[string]int{"value": 1}) }()
	// Callers rely on this being the device's next stanza: nothing before it
	// was relayed.
	raw := d.readThrough(t, "</message>")
	if !strings.HasPrefix(strings.TrimSpace(raw), "<message") || strings.Count(raw, "<message") != 1 || !strings.Contains(raw, "PUT /local ") {
		t.Fatal("unexpected device input", raw)
	}
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBothCloudLossPreservesOutstandingRequest(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	cloud.write(t, wire.Message{From: c.config.JID() + "/phone", To: c.config.ResourceJID(), Body: wire.Body{Text: "GET /cloud HTTP/1.1\r\n\r\n"}})
	d.readThrough(t, "</message>")
	// Queued behind the active request; its sender is gone after the loss.
	cloud.write(t, wire.Message{From: c.config.JID() + "/phone", To: c.config.ResourceJID(), Body: wire.Body{Text: "GET /queued HTTP/1.1\r\n\r\n"}})
	// Queued before the loss, the local request must survive it.
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/local", map[string]int{"value": 1}) }()
	time.Sleep(20 * time.Millisecond)
	_ = cloud.conn.Close()
	waitCloudLoss(t, c)
	_ = d.socket.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := d.reader.ReadByte(); err == nil {
		t.Fatal("local request sent while cloud reply outstanding")
	} else {
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatal(err)
		}
	}
	_ = d.socket.SetReadDeadline(time.Now().Add(3 * time.Second))
	cloudReply(t, c, d)
	// The local request is next; /queued must never follow.
	raw := d.readThrough(t, "</message>")
	if !strings.Contains(raw, "PUT /local ") {
		t.Fatal(raw)
	}
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	_ = d.socket.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if raw, err := d.reader.ReadString('>'); err == nil {
		t.Fatal("queued app request sent after Bosch loss:", raw)
	}
	_ = d.socket.SetReadDeadline(time.Now().Add(3 * time.Second))
	done = make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/local", map[string]int{"value": 1}) }()
	raw = d.readThrough(t, "</message>")
	if !strings.Contains(raw, "PUT /local ") {
		t.Fatal(raw)
	}
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBothQueuedTimeoutPreservesSession(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	cloud.write(t, wire.Message{From: c.config.JID() + "/phone", To: c.config.ResourceJID(), Body: wire.Body{Text: "GET /cloud HTTP/1.1\r\n\r\n"}})
	d.readThrough(t, "</message>")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := c.Put(ctx, "/expired", map[string]int{"value": 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if !c.IsConnected() || !c.UpstreamConnected() {
		t.Fatal("unsent deadline retired shared session")
	}
	cloudReply(t, c, d)
	cloud.next(t)
	localWrite(t, c, d)
}

func TestBlockedLocalWritePreservesSession(t *testing.T) {
	t.Parallel()
	for _, mode := range []ServerMode{ModeOffline, ModeBoth} {
		t.Run(string(mode), func(t *testing.T) {
			var c *Client
			var d *deviceFixture
			if mode == ModeBoth {
				c, d, _ = bothTestClient(t, UpdatesBlock)
			} else {
				c = newLocalTestClient(t, Config{ConnectTimeout: time.Second, RetryTimeout: time.Second}, LocalOptions{UpdatePolicy: UpdatesBlock})
				d = connectFixture(t, c)
			}
			if err := c.Put(t.Context(), "/gateway/update/strategy", map[string]int{"value": 1}); err == nil || !strings.Contains(err.Error(), "blocked by policy") {
				t.Fatal(err)
			}
			if !c.IsConnected() {
				t.Fatal("policy rejection retired session")
			}
			localWrite(t, c, d)
		})
	}
}

type failingCloudWrite struct{ net.Conn }

func (failingCloudWrite) Write([]byte) (int, error) { return 0, errors.New("upstream write failed") }

func TestBothNonMessageWriteFailurePreservesSession(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"presence", "iq", "message"} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, Config{})
			socket, device := net.Pipe()
			up, cloud := net.Pipe()
			t.Cleanup(func() { _ = device.Close(); _ = cloud.Close() })
			tr := testBridge(t, c, socket, failingCloudWrite{up}, LocalOptions{Mode: ModeBoth, RequestTimeout: time.Second, ReconnectInterval: time.Hour})
			c.dial = func(context.Context) (transport, error) { return tr, nil }
			if err := c.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			element := wire.E(wire.ClientNS, name)
			if name == "iq" {
				element.Set("type", "get")
				element.Set("id", "ping")
				element.Children = []wire.Node{{Element: ptrElement(wire.E(wire.PingNS, "ping"))}}
			}
			if name == "message" {
				// An app's push, relayed like any device message.
				element = wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "HTTP/1.0 200 OK\r\n\r\n"))
				element.Set("to", c.config.JID()+"/phone")
			}
			tr.inputs <- frameEvent{frame: wire.Frame{Element: &element}}
			if name == "iq" {
				f, err := wire.NewReader(device).Next()
				if err != nil {
					t.Fatal(err)
				}
				if f.Element.Get("type") != "result" {
					t.Fatal("offline ping not answered")
				}
			}
			waitCloudLoss(t, c)
			done := make(chan error, 1)
			go func() { done <- c.Put(t.Context(), "/local", map[string]int{"value": 1}) }()
			if _, err := wire.NewReader(device).Next(); err != nil {
				t.Fatal(err)
			}
			response := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "HTTP/1.0 204 No Content\r\n\r\n"))
			response.Set("to", c.config.JID()+"/localprobe")
			tr.inputs <- frameEvent{frame: wire.Frame{Element: &response}}
			if err := wait(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func ptrElement(e wire.Element) *wire.Element { return &e }

func TestBothRefusalWriteFailureDropsRelay(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, Config{})
	socket, device := net.Pipe()
	up, cloud := net.Pipe()
	t.Cleanup(func() { _ = device.Close(); _ = cloud.Close() })
	go func() { _, _ = io.Copy(io.Discard, device) }() // the device never answers
	// RequestTimeout also bounds device writes; expiry runs on a 100ms tick anyway.
	tr := testBridge(t, c, socket, failingCloudWrite{up}, LocalOptions{Mode: ModeBoth, RequestTimeout: 50 * time.Millisecond, ReconnectInterval: time.Hour})
	c.dial = func(context.Context) (transport, error) { return tr, nil }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Its 504 cannot reach Bosch: the relay is dropped rather than kept
	// stalled with a half-written stanza.
	request := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "GET /x HTTP/1.1\r\n\r\n"))
	request.Set("from", c.config.JID()+"/phone")
	request.Set("to", c.config.ResourceJID())
	tr.inputs <- frameEvent{cloud: true, frame: wire.Frame{Element: &request}}
	waitCloudLoss(t, c)
}

func TestBothBlocksUpdatesThroughoutHandshake(t *testing.T) {
	t.Parallel()
	var cloudControls, deviceControls, rejected atomic.Int32
	cloudHook := func(cloud *cloudFixture, f wire.Frame) {
		if e := f.Element; e != nil {
			switch {
			case e.Get("id") == "blocked-handshake":
				t.Error("device update passed handshake policy")
			case e.Get("id") == "service-control":
				return
			case e.Name.Local == "message":
				if strings.Contains(e.Child(wire.ClientNS, "body").Text(), "403 Forbidden") {
					rejected.Add(1)
				}
				return
			}
		}
		cloud.write(t, wire.IQ{Type: "get", ID: "blocked-handshake", From: "gservice_update@" + DefaultHost})
		cloud.write(t, wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "PUT /gateway/update/strategy HTTP/1.1\r\n\r\n"), wire.Text(wire.ClientNS, "body", "ordinary text")))
		cloud.write(t, wire.IQ{Type: "get", ID: "service-control", From: "gservice_weather@" + DefaultHost})
	}
	deviceHook := func(d *deviceFixture) {
		d.checkRead = func(raw string) {
			if strings.Contains(raw, "blocked-handshake") || strings.Contains(raw, "PUT /gateway/update/") {
				t.Fatal("cloud update passed handshake policy", raw)
			}
			if strings.Contains(raw, "service-control") {
				cloudControls.Add(1)
			}
		}
		d.afterWrite = func(raw string) {
			if strings.HasPrefix(raw, "<presence") {
				return
			}
			values := []any{wire.IQ{Type: "get", ID: "blocked-handshake", To: "gservice_firmware@" + DefaultHost}, wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "PUT /gateway/update/strategy HTTP/1.1\r\n\r\n"), wire.Text(wire.ClientNS, "body", "ordinary text")), wire.IQ{Type: "get", ID: "service-control", To: "gservice_time@" + DefaultHost}}
			for _, value := range values {
				if _, err := writeTyped(d.socket, value); err != nil {
					t.Fatal(err)
				}
			}
			deviceControls.Add(1)
		}
	}
	c, _, _ := bothHandshakeClient(t, UpdatesBlock, cloudHook, deviceHook)
	if !c.IsConnected() || cloudControls.Load() < 4 || deviceControls.Load() < 4 {
		t.Fatal("handshake did not cover login phases", cloudControls.Load(), deviceControls.Load())
	}
	if rejected.Load() < 3 {
		t.Fatal("blocked login requests not answered", rejected.Load())
	}
}

func TestBothAllowsFirmwareWrites(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	cloud.write(t, wire.IQ{Type: "get", ID: "update-service", From: "gservice_update@" + DefaultHost})
	if raw := d.readThrough(t, "/>"); !strings.Contains(raw, "update-service") {
		t.Fatal("allow policy dropped update service", raw)
	}
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/gateway/update/strategy", map[string]int{"value": 1}) }()
	if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "PUT /gateway/update/strategy ") {
		t.Fatal("allow policy blocked update write", raw)
	}
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

type announcedWrite struct {
	net.Conn
	once    sync.Once
	started chan struct{}
}

func (c *announcedWrite) Write(data []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(data)
}

func TestUnsentDeadlineWhileWaitingForWriter(t *testing.T) {
	t.Parallel()
	for _, mode := range []ServerMode{ModeOffline, ModeBoth} {
		t.Run(string(mode), func(t *testing.T) {
			c := newTestClient(t, Config{RetryTimeout: time.Second, MaxRetries: 1})
			socket, peer := net.Pipe()
			t.Cleanup(func() { _ = peer.Close() })
			announced := &announcedWrite{Conn: socket, started: make(chan struct{})}
			tr := testBridge(t, c, announced, nil, LocalOptions{Mode: mode, RequestTimeout: time.Second, ReconnectInterval: time.Hour})
			c.dial = func(context.Context) (transport, error) { return tr, nil }
			if err := c.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			presence := make(chan error, 1)
			go func() { presence <- tr.Ping() }()
			select {
			case <-announced.started:
			case <-time.After(time.Second):
				t.Fatal("presence did not claim writer")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			if err := c.Put(ctx, "/expired", map[string]int{"value": 1}); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if !c.IsConnected() {
				t.Fatal("unsent cancellation retired session")
			}
			select {
			case err := <-presence:
				t.Fatal("unsent cancellation interrupted presence", err)
			default:
			}
			_ = peer.SetDeadline(time.Now().Add(2 * time.Second))
			reader := wire.NewReader(peer)
			if _, err := reader.Next(); err != nil {
				t.Fatal(err)
			}
			if err := wait(t, presence); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- c.Put(t.Context(), "/after", map[string]int{"value": 1}) }()
			frame, err := reader.Next()
			if err != nil {
				t.Fatal(err)
			}
			if frame.Element.Child("", "body") == nil || !strings.Contains(frame.Element.Child("", "body").Text(), "PUT /after ") {
				t.Fatal("expired request reached writer", frame.Element)
			}
			response := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "HTTP/1.0 204 No Content\r\n\r\n"))
			response.Set("to", c.config.JID()+"/localprobe")
			if err := wire.WriteFrame(peer, wire.Frame{Element: &response}); err != nil {
				t.Fatal(err)
			}
			if err := wait(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBothQueuesRequestsReceivedDuringLogin(t *testing.T) {
	t.Parallel()
	cloudHook := func(cloud *cloudFixture, frame wire.Frame) {
		if frame.Element == nil || frame.Element.Child(wire.SessionNS, "session") == nil {
			return
		}
		for i := 1; i <= 2; i++ {
			cloud.write(t, wire.Message{From: "rrccontact_123456789@" + DefaultHost + "/phone", To: "rrcgateway_123456789@" + DefaultHost + "/RRC-RestApi", Body: wire.Body{Text: fmt.Sprintf("GET /early-%d HTTP/1.1\r\n\r\n", i)}})
		}
		cloud.write(t, wire.IQ{Type: "get", ID: "queued-login-control"})
	}
	deviceHook := func(d *deviceFixture) {
		d.beforeWrite = func(raw string) {
			if !strings.HasPrefix(raw, "<presence") {
				return
			}
			for {
				raw := d.readThrough(t, "/>")
				if strings.Contains(raw, "GET /early-") {
					t.Fatal("API request forwarded before login completed", raw)
				}
				if strings.Contains(raw, "queued-login-control") {
					break
				}
			}
		}
	}
	c, d, cloud := bothHandshakeClient(t, UpdatesAllow, cloudHook, deviceHook)
	expect := func(want string) {
		t.Helper()
		if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, want) {
			t.Fatalf("want %q, device got %s", want, raw)
		}
	}
	answer := func(uri string) {
		t.Helper()
		d.write(t, marshalTest(t, wire.Message{To: c.config.JID() + "/phone", Type: "chat", Body: wire.Body{Text: encryptedReply(t, c, "200 OK", fmt.Sprintf(`{"id":%q,"value":1}`, uri))}}))
	}
	expect("GET /early-1 ")
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/local", map[string]int{"value": 1}) }()
	_ = d.socket.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	if _, err := d.reader.ReadByte(); err == nil {
		t.Fatal("second request sent before the first was answered")
	}
	_ = d.socket.SetReadDeadline(time.Now().Add(2 * time.Second))
	answer("/early-1")
	cloud.next(t)
	// Local requests go ahead of queued app requests.
	expect("PUT /local ")
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	expect("GET /early-2 ")
	_ = cloud.conn.Close()
	waitCloudLoss(t, c)
	answer("/early-2")
	localWrite(t, c, d)
}

func TestBridgeRejectsAmbiguousAPIBodies(t *testing.T) {
	t.Parallel()
	ordinary := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "English"), wire.Text(wire.ClientNS, "body", "Nederlands"))
	if _, err := bridgeMessage(&ordinary); err != nil {
		t.Fatal("ordinary language alternatives rejected", err)
	}
	for _, ending := range []string{"\r", "\n", "\r\n"} {
		for _, method := range []string{"GET", "PUT"} {
			body := method + " /resource HTTP/1.1" + ending + "User-Agent: NefitEasy" + ending + ending
			if got, uri := requestLine(body); got != method || uri != "/resource" {
				t.Fatalf("line ending %q missed request: %q %q", ending, got, uri)
			}
			message := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", body), wire.Text(wire.ClientNS, "body", "ordinary text"))
			if _, err := bridgeMessage(&message); err == nil {
				t.Fatalf("%s with line ending %q bypassed multiple-body rejection", method, ending)
			}
		}
	}
}

func TestBothCloudErrorsKeepDeviceSession(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	ambiguous := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "GET /a HTTP/1.1\r\n\r\n"), wire.Text(wire.ClientNS, "body", "GET /b HTTP/1.1\r\n\r\n"))
	ambiguous.Set("from", c.config.JID()+"/phone")
	ambiguous.Set("to", c.config.ResourceJID())
	cloud.write(t, ambiguous)
	if status := cloudStatus(t, cloud); !strings.Contains(status, "400 Bad Request") {
		t.Fatal(status)
	}
	// One request occupies the device and maxCloudRequests wait: the next is refused.
	for i := range maxCloudRequests + 2 {
		cloud.write(t, cloudRequest(c, "phone", fmt.Sprintf("GET /q%d HTTP/1.1\r\n\r\n", i)))
	}
	if !strings.Contains(d.readThrough(t, "</message>"), "GET /q0 ") {
		t.Fatal("first cloud request not sent")
	}
	if status := cloudStatus(t, cloud); !strings.Contains(status, "503 Service Unavailable") {
		t.Fatal(status)
	}
	if !c.IsConnected() || !c.UpstreamConnected() {
		t.Fatal("cloud error retired the device session")
	}
}

func TestBothCloudQueueLeavesLocalRequests(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	for i := range maxCloudRequests + 3 {
		cloud.write(t, cloudRequest(c, "phone", fmt.Sprintf("GET /q%d HTTP/1.1\r\n\r\n", i)))
	}
	if !strings.Contains(d.readThrough(t, "</message>"), "GET /q0 ") {
		t.Fatal("first cloud request not sent")
	}
	// One is on the device and maxCloudRequests wait; the rest are refused.
	for range 2 {
		if status := cloudStatus(t, cloud); !strings.Contains(status, "503 Service Unavailable") {
			t.Fatal(status)
		}
	}
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/local", 1) }()
	for range maxCloudRequests + 1 {
		cloudReply(t, c, d)
		if strings.Contains(d.readThrough(t, "</message>"), "PUT /local ") {
			d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
			if err := wait(t, done); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("local request found no room beside queued cloud requests:", wait(t, done))
}

func TestBothOversizedRequestRefused(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	// Escaping grows each quote fivefold, past what the encoder writes.
	cloud.mu.Lock()
	_, err := fmt.Fprintf(cloud.conn, `<message xmlns="jabber:client" from="%s/phone" to="%s" type="chat"><body>GET /x HTTP/1.1&#13;&#10;&#13;&#10;%s</body></message>`,
		c.config.JID(), c.config.ResourceJID(), strings.Repeat(`"`, wire.MaxStanzaBytes/2))
	cloud.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if status := cloudStatus(t, cloud); !strings.Contains(status, "400 Bad Request") {
		t.Fatal(status)
	}
	localWrite(t, c, d)
}

func TestBothStalledSendersAreBounded(t *testing.T) {
	t.Parallel()
	c := replayClient(t)
	socket, device := net.Pipe()
	upstream, bosch := net.Pipe()
	t.Cleanup(func() { _ = device.Close(); _ = bosch.Close() })
	go func() { _, _ = io.Copy(io.Discard, device) }() // the device never answers
	cloud := &cloudFixture{conn: bosch, reader: wire.NewReader(bosch)}
	// RequestTimeout also bounds device writes; expiry runs on a 100ms tick anyway.
	tr := testBridge(t, c, socket, upstream, LocalOptions{Mode: ModeBoth, UpstreamAddress: "127.0.0.1:1", RequestTimeout: 50 * time.Millisecond, ReconnectInterval: time.Hour})
	// Set before the bridge runs: one more stalled sender reaches the cap.
	for i := range maxStalledSenders - 1 {
		tr.stalled[fmt.Sprintf("app%d", i)] = &activeRequest{}
	}
	c.dial = func(context.Context) (transport, error) { return tr, nil }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	// No stream header on this pipe: the namespace is spelled out, and the
	// reply carries none.
	_, err := fmt.Fprintf(bosch, `<message xmlns="jabber:client" from="%s/phone" to="%s" type="chat"><body>GET /slow HTTP/1.1&#13;&#10;&#13;&#10;</body></message>`, c.config.JID(), c.config.ResourceJID())
	if err != nil {
		t.Fatal(err)
	}
	if status := fullText(cloud.next(t)); !strings.Contains(status, "504 Gateway Timeout") {
		t.Fatal(status)
	}
	for deadline := time.Now().Add(2 * time.Second); c.UpstreamConnected(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("relay kept with every stalled-sender slot taken")
		}
	}
	if !c.IsConnected() {
		t.Fatal("dropping the relay ended local control")
	}
}

func TestFailedReplacementStillCountsBoschDrops(t *testing.T) {
	t.Parallel()
	sessions := make(chan net.Conn, 4)
	address, accepts := serve(t, func(_ int32, s net.Conn) {
		if fakeBosch(&cloudFixture{conn: s, reader: wire.NewReader(s)}, nil, nil) == nil {
			sessions <- s
			_, _ = io.Copy(io.Discard, s)
		}
	})
	c := bothClientFor(t, address, time.Hour)
	s, err := deviceLogin(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck
	relay := wait(t, sessions)
	// The login completes before its session is published.
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	tr := c.conn.Load().xmpp.(*localTransport)
	// A replacement login the device abandons is no Bosch failure, and the
	// relay it would have replaced is current again.
	d := reconnectCandidate(t, c)
	d.write(t, loginSteps(c)[0].send)
	d.readThrough(t, loginSteps(c)[0].end)
	_ = d.socket.Close()
	for deadline := time.Now().Add(2 * time.Second); accepts.Load() < 2 || tr.superseded.Load(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("relay still marked replaced after the replacement failed")
		}
	}
	// So Bosch dropping it backs off like any drop.
	_ = relay.Close()
	for deadline := time.Now().Add(2 * time.Second); c.UpstreamConnected(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("Bosch drop not noticed")
		}
	}
	next, err := deviceLogin(c)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close() //nolint:errcheck
	if c.UpstreamConnected() || accepts.Load() != 2 {
		t.Fatal("Bosch drop after a failed replacement did not back off", accepts.Load())
	}
}

func TestUpdateServiceSpellingsMatch(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothHandshakeClient(t, UpdatesBlock, nil, nil, func(_ *Config, o *LocalOptions) {
		o.UpdateServices = []string{" Extra_Update@Example./resource "}
	})
	cloud.write(t, wire.Message{From: "extra_update@example/other", To: c.config.ResourceJID(), Type: "chat", Body: wire.Body{Text: "GET /x HTTP/1.1\r\n\r\n"}})
	if status := cloudStatus(t, cloud); !strings.Contains(status, "403 Forbidden") {
		t.Fatal(status)
	}
	localWrite(t, c, d)
}

func TestBothReservesLocalResource(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	cloud.write(t, cloudRequest(c, localResource, "GET /spoofed HTTP/1.1\r\n\r\n"))
	cloud.write(t, cloudRequest(c, "phone", "GET /real HTTP/1.1\r\n\r\n"))
	if request := d.readThrough(t, "</message>"); !strings.Contains(request, "GET /real ") {
		t.Fatal("cloud stanza using the local resource reached the device", request)
	}
}

func TestBothQueuedLocalRequestStillReceivesPushes(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	pushes := make(chan string, 1)
	c.Subscribe(func(uri string, _ any) { pushes <- uri })
	cloud.write(t, cloudRequest(c, "phone", "GET /cloud HTTP/1.1\r\n\r\n"))
	d.readThrough(t, "</message>")
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/local", 1) }()
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	d.reply(t, c, encryptedReply(t, c, "200 OK", `{"id":"/push","value":1}`))
	if uri := wait(t, pushes); uri != "/push" {
		t.Fatal("legitimate push lost while request waited", uri)
	}
	select {
	case err := <-done:
		t.Fatal("unsent local request consumed another reply", err)
	default:
	}
	cloudReply(t, c, d)
	cloud.next(t)
	if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "PUT /local ") {
		t.Fatal("local request did not follow cloud reply", raw)
	}
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBothUnansweredCloudRequestQuarantinesSender(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothHandshakeClient(t, UpdatesAllow, nil, nil, func(_ *Config, o *LocalOptions) { o.RequestTimeout = 150 * time.Millisecond })
	cloud.write(t, cloudRequest(c, "phone", "PUT /slow HTTP/1.1\r\n\r\n"))
	d.readThrough(t, "</message>")
	if status := cloudStatus(t, cloud); !strings.Contains(status, "504 Gateway Timeout") {
		t.Fatal("unanswered cloud request did not time out", status)
	}
	// A push naming a resource cannot be the PUT's bodiless ack.
	d.write(t, marshalTest(t, wire.Message{To: c.config.JID() + "/phone", Type: "chat", Body: wire.Body{Text: encryptedReply(t, c, "200 OK", `{"id":"/other","value":1}`)}}))
	d.write(t, marshalTest(t, wire.IQ{Type: "result", ID: "marker", To: c.config.JID() + "/phone"}))
	if id := cloudMarker(t, cloud); id != "marker" {
		t.Fatal(id)
	}
	// Nor can a second deadline prove that the device abandoned the PUT.
	time.Sleep(400 * time.Millisecond)
	cloud.write(t, cloudRequest(c, "phone", "PUT /refused HTTP/1.1\r\n\r\n"))
	if status := cloudStatus(t, cloud); !strings.Contains(status, "504 Gateway Timeout") {
		t.Fatal("sender reused an ambiguous outstanding request", status)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Get(t.Context(), "/local"); done <- err }()
	if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "GET /local ") {
		t.Fatal("cloud timeout blocked local control", raw)
	}
	d.write(t, marshalTest(t, wire.Message{To: c.config.JID() + "/phone", Type: "chat", Body: wire.Body{Text: "HTTP/1.0 200 OK\r\n\r\n"}}))
	d.write(t, marshalTest(t, wire.IQ{Type: "result", ID: "after-late-ack", To: c.config.JID() + "/phone"}))
	if e := cloud.next(t); e.Name.Local != "iq" || e.Get("id") != "after-late-ack" {
		t.Fatal("expired acknowledgement reached cloud sender", e.Name)
	}
	d.reply(t, c, encryptedReply(t, c, "200 OK", `{"id":"/local","value":1}`))
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	cloud.write(t, cloudRequest(c, "phone", "GET /next HTTP/1.1\r\n\r\n"))
	if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "GET /next ") {
		t.Fatal("sender did not recover after its old reply was discarded", raw)
	}
	d.write(t, marshalTest(t, wire.Message{To: c.config.JID() + "/phone", Type: "chat", Body: wire.Body{Text: encryptedReply(t, c, "200 OK", `{"id":"/next","value":2}`)}}))
	if status := cloudStatus(t, cloud); !strings.Contains(status, "200 OK") {
		t.Fatal("fresh cloud request was not answered", status)
	}
	if !c.IsConnected() || !c.UpstreamConnected() {
		t.Fatal("unanswered cloud request retired the device session")
	}
}

func cloudRequest(c *Client, resource, body string) wire.Message {
	return wire.Message{From: c.config.JID() + "/" + resource, To: c.config.ResourceJID(), Type: "chat", Body: wire.Body{Text: body}}
}

func cloudStatus(t *testing.T, cloud *cloudFixture) string {
	t.Helper()
	e := cloud.next(t)
	body := e.Child(wire.ClientNS, "body")
	if body == nil {
		t.Fatal("cloud received no reply body")
	}
	return body.Text()
}

func TestProbeRequiresXMPPFeatures(t *testing.T) {
	t.Parallel()
	tcpOnly, _ := serve(t, func(_ int32, s net.Conn) { _, _ = wire.NewReader(s).Next() })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := probeUpstream(ctx, tcpOnly, DefaultHost); err == nil {
		t.Fatal("open TCP port counted as XMPP recovery")
	}
	noMechanisms, _ := serve(t, func(_ int32, s net.Conn) {
		if _, err := wire.NewReader(s).Next(); err == nil {
			_, _ = writeTyped(s, wire.Frame{Stream: &wire.Stream{From: DefaultHost, Version: "1.0"}})
			_, _ = writeTyped(s, wire.E(wire.StreamNS, "features"))
			_, _ = io.Copy(io.Discard, s)
		}
	})
	if err := probeUpstream(ctx, noMechanisms, DefaultHost); err == nil {
		t.Fatal("server offering no login counted as recovery")
	}
	xmppServer, _ := serve(t, func(_ int32, s net.Conn) { xmppGreeter(s) })
	if err := probeUpstream(ctx, xmppServer, DefaultHost); err != nil {
		t.Fatal(err)
	}
	// A silent server must not hold the probe, or recovery would never run.
	silent, _ := serve(t, func(_ int32, s net.Conn) { _, _ = io.Copy(io.Discard, s) })
	short, cancelShort := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancelShort()
	probed := make(chan error, 1)
	go func() { probed <- probeUpstream(short, silent, DefaultHost) }()
	if err := wait(t, probed); err == nil {
		t.Fatal("silent server counted as recovery")
	}
}

func TestRelayOutlivingIntervalResetsBackoff(t *testing.T) {
	t.Parallel()
	c := replayClient(t)
	socket, device := net.Pipe()
	upstream, cloud := net.Pipe()
	t.Cleanup(func() { _ = device.Close(); _ = cloud.Close() })
	go func() { _, _ = io.Copy(io.Discard, device) }()
	go func() { _, _ = io.Copy(io.Discard, cloud) }()
	tr := testBridge(t, c, socket, upstream, LocalOptions{Mode: ModeBoth, RequestTimeout: time.Second, ReconnectInterval: 20 * time.Millisecond})
	for range 5 {
		tr.health.fail(time.Hour)
	}
	go func() { _, _ = tr.Recv() }()
	// Without a reset, failures would add up for the life of the process.
	deadline := time.Now().Add(2 * time.Second)
	for !tr.health.ready() {
		if time.Now().After(deadline) {
			t.Fatal("a relay that stayed up kept its backoff")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUpstreamBackoffGrows(t *testing.T) {
	t.Parallel()
	h := new(upstreamHealth)
	if !h.ready() {
		t.Fatal("fresh health not ready")
	}
	for n := 1; n <= 8; n++ {
		before := time.Now()
		h.fail(time.Minute)
		// Doubling from one interval, capped at 32 of them.
		if want := time.Minute << min(n-1, 5); h.until.Sub(before) < want || h.until.Sub(before) > want+time.Second || h.ready() {
			t.Fatalf("failure %d backs off %v, want %v", n, h.until.Sub(before), want)
		}
	}
	h.ok()
	if !h.ready() {
		t.Fatal("successful login kept backoff")
	}
	// The count resets too: the next failure starts from one interval again.
	before := time.Now()
	if h.fail(time.Minute); h.until.Sub(before) > time.Minute+time.Second {
		t.Fatal("backoff resumed at its doubled level", h.until.Sub(before))
	}
}

func TestBothRecoveryNeedsXMPPAndBacksOffFailedLogins(t *testing.T) {
	t.Parallel()
	// The first probe finds no XMPP; a later one must still run.
	var failed atomic.Bool
	address, probes := rejectingBosch(t, func(s net.Conn) {
		if failed.CompareAndSwap(false, true) {
			return
		}
		greetXMPP(s)
	})
	c := newLocalTestClient(t, Config{ConnectTimeout: 5 * time.Second, RetryTimeout: time.Second},
		LocalOptions{Mode: ModeBoth, UpstreamAddress: address, RequestTimeout: time.Second, ReconnectInterval: 300 * time.Millisecond})
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(t.Context()) }()
	if s, err := deviceLogin(c); err == nil {
		_ = s.Close()
		t.Fatal("login rejected by Bosch still succeeded")
	}
	s := offlineLogin(t, c)
	defer s.Close() //nolint:errcheck
	if err := wait(t, connected); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("XMPP recovery not detected")
	}
	if probes.Load() < 2 {
		t.Fatal("recovery did not come from a later probe", probes.Load())
	}
}

// rejectingBosch closes every relayed login, whose header names the gateway,
// and hands probes, which name nobody, to probe once their header is read.
func rejectingBosch(t *testing.T, probe func(net.Conn)) (string, *atomic.Int32) {
	t.Helper()
	probes := new(atomic.Int32)
	address, _ := serve(t, func(_ int32, s net.Conn) {
		if f, err := wire.NewReader(s).Next(); err == nil && f.Stream != nil && f.Stream.From == "" {
			probes.Add(1)
			probe(s)
		}
	})
	return address, probes
}

// offlineLogin retries the device login until the server serves it
// offline: one that outlives the backoff is relayed and rejected again.
func offlineLogin(t *testing.T, c *Client) net.Conn {
	t.Helper()
	for range 8 {
		if s, err := deviceLogin(c); err == nil {
			if c.UpstreamConnected() {
				t.Fatal("rejected login relayed")
			}
			return s
		}
	}
	t.Fatal("device not served offline after failed relayed logins")
	return nil
}

// xmppGreeter answers a stream open like an XMPP server offering SASL.
func xmppGreeter(s net.Conn) {
	if _, err := wire.NewReader(s).Next(); err != nil {
		return
	}
	greetXMPP(s)
}

func greetXMPP(s net.Conn) {
	_, _ = writeTyped(s, wire.Frame{Stream: &wire.Stream{From: DefaultHost, Version: "1.0"}})
	_, _ = writeTyped(s, wire.E(wire.StreamNS, "features", wire.E(wire.SASLNS, "mechanisms", wire.Text(wire.SASLNS, "mechanism", "DIGEST-MD5"))))
	_, _ = io.Copy(io.Discard, s)
}

// abandonScenario gives up on a sent local request, then checks the session
// survives and the late reply answers nothing.
func abandonScenario(t *testing.T, c *Client, d *deviceFixture) {
	t.Helper()
	pushes := make(chan string, 4)
	c.Subscribe(func(uri string, _ any) { pushes <- uri })
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Get(ctx, "/first"); !errors.Is(err, errUnanswered) || retryable(err) {
		t.Fatalf("abandoned request = %v, want unanswered", err)
	}
	if !strings.Contains(d.readThrough(t, "</message>"), "GET /first ") {
		t.Fatal("abandoned request never reached the device")
	}
	if !c.IsConnected() {
		t.Fatal("a caller giving up retired the device session")
	}
	result := make(chan error, 1)
	go func() { _, err := c.Get(t.Context(), "/next"); result <- err }()
	// The abandoned request keeps the single device slot until it is answered.
	_ = d.socket.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, err := d.reader.ReadByte(); err == nil {
		t.Fatal("request sent while an abandoned one was unanswered")
	}
	_ = d.socket.SetReadDeadline(time.Now().Add(3 * time.Second))
	d.reply(t, c, encryptedReply(t, c, "200 OK", `{"id":"/first","value":1}`))
	if !strings.Contains(d.readThrough(t, "</message>"), "GET /next ") {
		t.Fatal("queue did not resume after the late reply")
	}
	d.reply(t, c, encryptedReply(t, c, "200 OK", `{"id":"/next","value":2}`))
	if err := wait(t, result); err != nil {
		t.Fatal(err)
	}
	select {
	case uri := <-pushes:
		t.Fatal("late reply delivered as a push:", uri)
	default:
	}
}

func TestLocalAbandonKeepsSession(t *testing.T) {
	t.Parallel()
	t.Run("offline", func(t *testing.T) {
		c := localTestClient(t, time.Second)
		abandonScenario(t, c, connectFixture(t, c))
	})
	t.Run("both", func(t *testing.T) {
		c, d, _ := bothTestClient(t, UpdatesAllow)
		abandonScenario(t, c, d)
	})
}

// cloudMarker returns the id of the next IQ the cloud receives.
func cloudMarker(t *testing.T, cloud *cloudFixture) string {
	t.Helper()
	for {
		e := cloud.next(t)
		if e.Name.Local == "iq" {
			return e.Get("id")
		}
	}
}

func TestBothDeviceTrafficFilters(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesBlock)
	local := c.config.JID() + "/" + localResource
	for _, raw := range []string{
		// Update traffic from the device stays blocked after login.
		marshalTest(t, wire.IQ{Type: "get", ID: "firmware", To: "gservice_firmware@" + DefaultHost}),
		// Replies to the local keepalive belong to no one upstream.
		marshalTest(t, wire.IQ{Type: "result", ID: "keepalive", To: local}),
		marshalTest(t, wire.Presence{To: local}),
		marshalTest(t, wire.IQ{Type: "get", ID: "marker", To: "gservice_time@" + DefaultHost}),
	} {
		d.write(t, raw)
	}
	if id := cloudMarker(t, cloud); id != "marker" {
		t.Fatal("device stanza leaked upstream:", id)
	}
}

func TestBothErrorReplyEndsCloudRequest(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	cloud.write(t, cloudRequest(c, "phone", "GET /a HTTP/1.1\r\n\r\n"))
	d.readThrough(t, "</message>")
	cloud.write(t, cloudRequest(c, "phone", "GET /b HTTP/1.1\r\n\r\n"))
	// An error stanza echoes the request; only its type marks it a reply.
	d.write(t, marshalTest(t, wire.Message{To: c.config.JID() + "/phone", Type: "error", Body: wire.Body{Text: "GET /a HTTP/1.1\r\n\r\n"}}))
	if e := cloud.next(t); e.Get("type") != "error" {
		t.Fatal("error reply not forwarded")
	}
	if !strings.Contains(d.readThrough(t, "</message>"), "GET /b ") {
		t.Fatal("error reply did not end the cloud request")
	}
}

func TestBothStalledSenderQueuedRequestRefused(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothHandshakeClient(t, UpdatesAllow, nil, nil, func(_ *Config, o *LocalOptions) { o.RequestTimeout = 150 * time.Millisecond })
	cloud.write(t, cloudRequest(c, "phone", "GET /slow HTTP/1.1\r\n\r\n"))
	d.readThrough(t, "</message>")
	cloud.write(t, cloudRequest(c, "phone", "GET /queued HTTP/1.1\r\n\r\n"))
	for range 2 {
		if status := cloudStatus(t, cloud); !strings.Contains(status, "504 Gateway Timeout") {
			t.Fatal(status)
		}
	}
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/local", 1) }()
	if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "PUT /local ") {
		t.Fatal("stalled sender's queued request reached the device:", raw)
	}
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBothLoginReservesLocalResource(t *testing.T) {
	t.Parallel()
	cloudHook := func(cloud *cloudFixture, frame wire.Frame) {
		if frame.Element != nil && frame.Element.Child(wire.SessionNS, "session") != nil {
			cloud.write(t, wire.Message{From: "rrccontact_123456789@" + DefaultHost + "/" + localResource, To: "rrcgateway_123456789@" + DefaultHost + "/RRC-RestApi", Body: wire.Body{Text: "GET /spoofed HTTP/1.1\r\n\r\n"}})
		}
	}
	c, d, _ := bothHandshakeClient(t, UpdatesAllow, cloudHook, nil)
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/local", 1) }()
	if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "PUT /local ") {
		t.Fatal("cloud stanza using the local resource reached the device during login:", raw)
	}
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBothUnrelayableCloudStanzaKeepsSession(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, Config{})
	socket, device := net.Pipe()
	upstream, cloud := net.Pipe()
	t.Cleanup(func() { _ = device.Close(); _ = cloud.Close() })
	tr := testBridge(t, c, socket, upstream, LocalOptions{Mode: ModeBoth, RequestTimeout: time.Second, ReconnectInterval: time.Hour})
	c.dial = func(context.Context) (transport, error) { return tr, nil }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	// No XML encoder can write this name.
	invalid := wire.E(wire.ClientNS, "0")
	tr.inputs <- frameEvent{cloud: true, frame: wire.Frame{Element: &invalid}}
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/after", 1) }()
	nextRequest(t, deviceReader(t, device), "PUT /after ")
	response := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "HTTP/1.0 204 No Content\r\n\r\n"))
	response.Set("to", c.config.JID()+"/"+localResource)
	tr.inputs <- frameEvent{frame: wire.Frame{Element: &response}}
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineAnswersEveryIQRequest(t *testing.T) {
	t.Parallel()
	c := localTestClient(t, time.Second)
	d := connectFixture(t, c)
	// Results and errors are no requests: answering them could loop.
	d.write(t, `<iq type="result" id="r"/><iq type="error" id="e"/><iq type="get" id="ping"><ping xmlns="urn:xmpp:ping"/></iq>`)
	if raw := d.readThrough(t, "/>"); !strings.Contains(raw, `type="result"`) || !strings.Contains(raw, `id="ping"`) {
		t.Fatal("ping not answered", raw)
	}
	for _, iq := range []string{`<iq type="get" id="version"><query xmlns="jabber:iq:version"/></iq>`, `<iq type="set" id="set"><query xmlns="jabber:iq:private"/></iq>`} {
		d.write(t, iq)
		if raw := d.readThrough(t, "</iq>"); !strings.Contains(raw, `type="error"`) || !strings.Contains(raw, "service-unavailable") {
			t.Fatal("unsupported request not refused", raw)
		}
	}
}

func TestOfflineNeverProbesBosch(t *testing.T) {
	t.Parallel()
	address, accepts := serve(t, func(_ int32, s net.Conn) { xmppGreeter(s) })
	// The Bosch host itself is the greeter, as an upstream address needs ModeBoth.
	host, port, _ := net.SplitHostPort(address)
	portNumber, _ := strconv.Atoi(port)
	c := newLocalTestClient(t, Config{Host: host, Port: portNumber, ConnectTimeout: time.Second, RetryTimeout: time.Second},
		LocalOptions{Mode: ModeOffline, ReconnectInterval: 20 * time.Millisecond})
	d := connectFixture(t, c)
	// Probe ticks would have fired many times by now.
	localWrite(t, c, d)
	time.Sleep(100 * time.Millisecond)
	localWrite(t, c, d)
	if accepts.Load() != 0 || !c.IsConnected() {
		t.Fatal("offline mode probed Bosch", accepts.Load())
	}
}

func TestBridgeReportsEveryQueuedRequest(t *testing.T) {
	t.Parallel()
	c := replayClient(t)
	socket, device := net.Pipe()
	t.Cleanup(func() { _ = device.Close() })
	tr := testBridge(t, c, socket, nil, LocalOptions{Mode: ModeOffline, RequestTimeout: time.Second, ReconnectInterval: time.Hour})
	go func() { _, _ = tr.Recv() }()
	// Expired before the bridge picks it up: the bridge must still answer submit.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := bridgeRequest{message: wire.Message{Body: wire.Body{Text: "PUT /x HTTP/1.1\r\n\r\n"}}, sent: make(chan error, 1), ctx: ctx, state: new(atomic.Int32)}
	tr.requests <- r
	var unsent *unsentError
	if err := wait(t, r.sent); !errors.As(err, &unsent) {
		t.Fatalf("expired request reported %v, want unsent", err)
	}
}

func TestReplyOwnedByAbandonedRequestAnswersNothing(t *testing.T) {
	t.Parallel()
	c := replayClient(t)
	pushes := make(chan string, 1)
	c.Subscribe(func(uri string, _ any) { pushes <- uri })
	cn := &conn{}
	abandoned := &pending{uri: "/a", get: true}
	abandoned.markSent()
	next := &pending{uri: "/a", get: true, reply: make(chan reply, 1)}
	next.markSent()
	cn.begin(next)
	// One push() would dispatch, so misrouting it shows.
	ack := reply{resp: &protocol.HTTPResponse{StatusCode: 200}, id: "/a", data: map[string]any{"value": 1}}
	c.route(cn, ack, abandoned)
	select {
	case <-next.reply:
		t.Fatal("reply assigned to an abandoned request answered the next one")
	default:
	}
	c.route(cn, ack, next)
	if r := wait(t, next.reply); r.resp.StatusCode != 200 {
		t.Fatal("owned reply not delivered")
	}
	c.wg.Wait()
	select {
	case uri := <-pushes:
		t.Fatal("abandoned reply dispatched as a push:", uri)
	default:
	}
}

func TestBothRecoveryWaitsForActiveRequest(t *testing.T) {
	t.Parallel()
	release, probed := make(chan struct{}), make(chan struct{}, 1)
	address, probes := rejectingBosch(t, func(s net.Conn) {
		<-release
		greetXMPP(s)
		select {
		case probed <- struct{}{}:
		default:
		}
	})
	c := newLocalTestClient(t, Config{ConnectTimeout: 5 * time.Second, RetryTimeout: 5 * time.Second},
		LocalOptions{Mode: ModeBoth, UpstreamAddress: address, RequestTimeout: 5 * time.Second, ReconnectInterval: 50 * time.Millisecond})
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(t.Context()) }()
	if s, err := deviceLogin(c); err == nil {
		_ = s.Close()
		t.Fatal("rejected relayed login succeeded")
	}
	s := offlineLogin(t, c)
	defer s.Close() //nolint:errcheck
	if err := wait(t, connected); err != nil {
		t.Fatal(err)
	}
	done := c.Done()
	d := &deviceFixture{socket: s, reader: bufio.NewReader(s)}
	_ = s.SetDeadline(time.Now().Add(5 * time.Second))
	result := make(chan error, 1)
	go func() { result <- c.Put(t.Context(), "/active", 1) }()
	d.readThrough(t, "</message>")
	close(release)
	wait(t, probed)
	select {
	case <-done:
		t.Fatal("recovery cut off a request the device was answering")
	case <-time.After(150 * time.Millisecond):
	}
	// Bosch was found: waiting for the request needs no further probes.
	if n := probes.Load(); n > 2 {
		t.Fatal("probed Bosch while recovering", n)
	}
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, result); err != nil {
		t.Fatal(err)
	}
	wait(t, done)
}

// A request queued behind the active one, as after a caller abandons it,
// must not keep the device from reconnecting to Bosch.
func TestRecoveryNotPostponedByQueuedRequest(t *testing.T) {
	t.Parallel()
	c := replayClient(t)
	socket, device := net.Pipe()
	t.Cleanup(func() { _ = device.Close() })
	tr := testBridge(t, c, socket, nil, LocalOptions{Mode: ModeBoth, UpstreamAddress: "127.0.0.1:1", RequestTimeout: time.Hour, ReconnectInterval: time.Hour})
	ended := make(chan error, 1)
	go func() {
		for {
			if _, err := tr.Recv(); err != nil {
				ended <- err
				return
			}
		}
	}()
	sent := make(chan error, 2)
	go func() { sent <- tr.Send(t.Context(), "GET /a HTTP/1.1\r\n\r\n", new(pending)) }()
	nextRequest(t, deviceReader(t, device), "GET /a ")
	if err := wait(t, sent); err != nil {
		t.Fatal(err)
	}
	go func() { sent <- tr.Send(t.Context(), "GET /b HTTP/1.1\r\n\r\n", new(pending)) }()
	// The buffer holds one signal: the second is taken once the first latched.
	tr.recovery <- struct{}{}
	tr.recovery <- struct{}{}
	// Lets /b reach the queue; were it late, both outcomes would pass.
	time.Sleep(50 * time.Millisecond)
	if _, err := fmt.Fprintf(device, `<message xmlns="jabber:client" to="%s/%s" type="chat"><body>HTTP/1.0 404 Not Found&#13;&#10;&#13;&#10;</body></message>`, c.config.JID(), localResource); err != nil {
		t.Fatal(err)
	}
	// Sending /b instead would block on the unread pipe.
	if err := wait(t, ended); err == nil || !strings.Contains(err.Error(), "reconnecting device") {
		t.Fatal("recovery did not follow the active request:", err)
	}
}

func TestBothUnscheduledShapesRefused(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	request := "GET /hidden HTTP/1.1\r\n\r\n"
	// Only messages are scheduled, so a request in an IQ is not relayed.
	// Written first: each refusal below proves it was handled.
	iq := wire.E(wire.ClientNS, "iq", wire.Text(wire.ClientNS, "body", "FOO /hidden\u00a0HTTP/1.1\r\n\r\n"))
	iq.Set("type", "set")
	iq.Set("from", c.config.JID()+"/phone")
	iq.Set("to", c.config.ResourceJID())
	cloud.write(t, iq)
	shapes := map[string]wire.Element{
		"server namespace":   wire.E("jabber:server", "message", wire.Text("jabber:server", "body", request)),
		"foreign body":       wire.E(wire.ClientNS, "message", wire.Text("urn:x", "body", request)),
		"nested body":        wire.E(wire.ClientNS, "message", wire.E("urn:x", "x", wire.Text(wire.ClientNS, "body", request))),
		"capital body":       wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "BODY", request)),
		"junk first line":    wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "x\r\n"+request)),
		"text outside":       wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "")),
		"request in subject": wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "subject", request), wire.Text(wire.ClientNS, "body", "GET /x HTTP/1.1\r\n\r\n")),
		// Markup inside a body would hide text from the scheduler and policy.
		"markup in body": wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", request)),
		// A lenient parser might read either as a request.
		"capital message": wire.E(wire.ClientNS, "Message", wire.Text(wire.ClientNS, "body", request)),
		"split in extension": wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "hello"),
			wire.E("urn:x", "x", wire.Text("urn:x", "a", "x"), wire.Text("urn:x", "b", "PUT /x HTTP/1.1"), wire.Text("urn:x", "c", " y"))),
		"split across elements": wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "hello"),
			wire.E("urn:x", "x", wire.Text("urn:x", "a", "PU"), wire.Text("urn:x", "b", "T /x HT"), wire.Text("urn:x", "c", "TP/1.1"))),
	}
	for _, line := range []string{"\r\nGET /leading HTTP/1.1", "PUT /x extra HTTP/1.1", "put /x extra HTTP/1.1", "GET /x junk", "GET /x HTTP/1.1 extra", "x\r\nFOO /x\u00a0HTTP/1.1"} {
		shapes[line] = wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", line+"\r\n\r\n"))
	}
	markup := shapes["markup in body"]
	markup.Children[0].Element.Children = append(markup.Children[0].Element.Children, wire.Node{Element: ptrElement(wire.E("", "x"))})
	outside := shapes["text outside"]
	outside.Children = append(outside.Children, wire.Node{Text: request})
	shapes["text outside"] = outside
	for name, e := range shapes {
		e.Set("from", c.config.JID()+"/phone")
		e.Set("to", c.config.ResourceJID())
		cloud.write(t, e)
		if status := cloudStatus(t, cloud); !strings.Contains(status, "400 Bad Request") {
			t.Fatalf("%s answered %q", name, status)
		}
	}
	localWrite(t, c, d)
}

func TestConnectedBridgeNeverProbes(t *testing.T) {
	t.Parallel()
	address, accepts := serve(t, func(_ int32, s net.Conn) { xmppGreeter(s) })
	c := newTestClient(t, Config{})
	socket, device := net.Pipe()
	upstream, cloud := net.Pipe()
	t.Cleanup(func() { _ = device.Close(); _ = cloud.Close() })
	go func() { _, _ = io.Copy(io.Discard, device) }()
	go func() { _, _ = io.Copy(io.Discard, cloud) }()
	tr := testBridge(t, c, socket, upstream, LocalOptions{Mode: ModeBoth, UpstreamAddress: address, RequestTimeout: time.Second, ReconnectInterval: 20 * time.Millisecond})
	c.dial = func(context.Context) (transport, error) { return tr, nil }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
		t.Fatal("live relay reconnected for a probe")
	case <-time.After(200 * time.Millisecond):
	}
	if accepts.Load() != 0 {
		t.Fatal("probed Bosch while relaying", accepts.Load())
	}
}

func TestBothRefusesDeviceSTARTTLS(t *testing.T) {
	t.Parallel()
	relayed := make(chan string, 8)
	address, _ := serve(t, func(_ int32, s net.Conn) {
		r := wire.NewReader(s)
		for {
			f, err := r.Next()
			if err != nil {
				return
			}
			if f.Stream != nil {
				_, _ = writeTyped(s, wire.Frame{Stream: &wire.Stream{From: DefaultHost, Version: "1.0"}})
				_, _ = writeTyped(s, wire.E(wire.StreamNS, "features", wire.E(wire.TLSNS, "starttls")))
			} else if f.Element != nil {
				relayed <- f.Element.Name.Local
			}
		}
	})
	c := newLocalTestClient(t, Config{ConnectTimeout: time.Second},
		LocalOptions{Mode: ModeBoth, UpstreamAddress: address, ReconnectInterval: time.Hour})
	d := reconnectCandidate(t, c)
	d.write(t, `<stream:stream from="rrcgateway_123456789" to="`+DefaultHost+`" xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" version="1.0">`)
	d.readThrough(t, "</stream:features>")
	d.write(t, `<starttls xmlns="urn:ietf:params:xml:ns:xmpp-tls"/>`)
	reconnectSocketClosed(t, d.socket)
	select {
	case name := <-relayed:
		t.Fatal("device STARTTLS relayed to Bosch:", name)
	default:
	}
}

func TestBoschSASLFailureBacksOff(t *testing.T) {
	t.Parallel()
	address, accepts := serve(t, func(_ int32, s net.Conn) {
		_ = fakeBosch(&cloudFixture{conn: s, reader: wire.NewReader(s)}, func(e *wire.Element) bool { return e.Name.Local == "auth" }, nil)
	})
	c := bothClientFor(t, address, time.Hour)
	d := reconnectCandidate(t, c)
	d.write(t, `<stream:stream from="rrcgateway_123456789" to="`+DefaultHost+`" xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" version="1.0">`)
	d.readThrough(t, "</stream:features>")
	d.write(t, `<auth xmlns="urn:ietf:params:xml:ns:xmpp-sasl" mechanism="DIGEST-MD5"/>`)
	d.readThrough(t, "</failure>")
	_ = d.socket.Close()
	s, err := deviceLogin(c)
	if err != nil {
		t.Fatal("device not served offline after Bosch rejected its login:", err)
	}
	defer s.Close() //nolint:errcheck
	if c.UpstreamConnected() || accepts.Load() != 1 {
		t.Fatal("rejected login did not back off", accepts.Load())
	}
}

func TestSilentBoschBacksOff(t *testing.T) {
	t.Parallel()
	address, accepts := serve(t, func(_ int32, s net.Conn) { _, _ = io.Copy(io.Discard, s) })
	c := newLocalTestClient(t, Config{ConnectTimeout: 200 * time.Millisecond, RetryTimeout: time.Second},
		LocalOptions{Mode: ModeBoth, UpstreamAddress: address, ReconnectInterval: time.Hour})
	d := reconnectCandidate(t, c)
	d.write(t, `<stream:stream from="rrcgateway_123456789" to="`+DefaultHost+`" xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" version="1.0">`)
	reconnectSocketClosed(t, d.socket)
	s, err := deviceLogin(c)
	if err != nil {
		t.Fatal("silent Bosch: next login not served offline:", err, accepts.Load())
	}
	_ = s.Close()
	if accepts.Load() != 1 {
		t.Fatal("silent Bosch retried without backoff", accepts.Load())
	}
}

// hiddenDeadline expires like its context but reports no deadline, so the
// login sets none on its sockets.
type hiddenDeadline struct{ context.Context }

func (hiddenDeadline) Deadline() (time.Time, bool) { return time.Time{}, false }

// fixedDeadline keeps the deadline set before the login.
type fixedDeadline struct{ net.Conn }

func (fixedDeadline) SetDeadline(time.Time) error { return nil }

// Whichever login deadline fires first, a stalled login counts against Bosch.
func TestLoginDeadlinesBackOff(t *testing.T) {
	t.Parallel()
	address, _ := serve(t, func(_ int32, s net.Conn) { _, _ = io.Copy(io.Discard, s) }) // Bosch stays silent
	cfg := Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret"}.WithDefaults()
	options := LocalOptions{Mode: ModeBoth, UpstreamAddress: address, logf: func() *slog.Logger { return slog.New(slog.DiscardHandler) }}
	for _, socketFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("socket first ", socketFirst), func(t *testing.T) {
			t.Parallel()
			socket, device := net.Pipe()
			t.Cleanup(func() { _ = device.Close() })
			ctx := t.Context()
			conn := socket
			if socketFirst {
				_ = socket.SetDeadline(time.Now().Add(100 * time.Millisecond))
				conn = fixedDeadline{socket}
			} else {
				timeout, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				ctx = hiddenDeadline{timeout}
			}
			_, err := acceptBoth(ctx, conn, cfg, options)
			var upstream *upstreamError
			if !errors.As(err, &upstream) {
				t.Fatal("stalled login not counted against Bosch:", err)
			}
		})
	}
}

// fakeBosch answers a probe or a relayed gateway login on cloud and returns
// at the device's presence. refuse, if set, sees each auth, bind and session
// and may refuse it; hook, if set, sees every device frame after its answer.
func fakeBosch(cloud *cloudFixture, refuse func(iq *wire.Element) bool, hook func(*cloudFixture, wire.Frame)) error {
	_ = cloud.conn.SetDeadline(time.Now().Add(5 * time.Second))
	features := wire.E(wire.StreamNS, "features", wire.E(wire.SASLNS, "mechanisms", wire.Text(wire.SASLNS, "mechanism", "DIGEST-MD5")))
	for {
		f, err := cloud.reader.Next()
		if err != nil {
			return err
		}
		var response any
		if e := f.Element; f.Stream != nil {
			if _, err := writeTyped(cloud.conn, wire.Frame{Stream: &wire.Stream{From: DefaultHost, Version: "1.0"}}); err != nil {
				return err
			}
			response = features
		} else if e != nil {
			switch {
			case e.Name.Local == "presence":
				return nil
			case e.Name.Local == "auth" && refuse != nil && refuse(e):
				response = wire.E(wire.SASLNS, "failure", wire.E(wire.SASLNS, "not-authorized"))
			case e.Name.Local == "auth":
				response = wire.SASLText("challenge", "Zml4dHVyZQ==")
			case e.Name.Local == "response":
				response = wire.SASLText("success", "cnNwYXV0aD0w")
				features = wire.E(wire.StreamNS, "features", wire.E(wire.BindNS, "bind"), wire.E(wire.SessionNS, "session"))
			case (e.Child(wire.BindNS, "bind") != nil || e.Child(wire.SessionNS, "session") != nil) && refuse != nil && refuse(e):
				conflict := wire.E(wire.ClientNS, "error", wire.E("urn:ietf:params:xml:ns:xmpp-stanzas", "conflict"))
				conflict.Set("type", "cancel")
				response = wire.IQ{Type: "error", ID: e.Get("id"), Extensions: []wire.Element{conflict}}
			case e.Child(wire.BindNS, "bind") != nil:
				response = wire.IQ{Type: "result", ID: e.Get("id"), Bind: &wire.Bind{JID: "rrcgateway_123456789@" + DefaultHost + "/RRC-RestApi"}}
			case e.Child(wire.SessionNS, "session") != nil:
				response = wire.IQ{Type: "result", ID: e.Get("id")}
			}
		}
		if response != nil {
			if _, err := writeTyped(cloud.conn, response); err != nil {
				return err
			}
		}
		if hook != nil {
			hook(cloud, f)
		}
	}
}

func bothClientFor(t *testing.T, upstream string, reconnect time.Duration) *Client {
	t.Helper()
	return newLocalTestClient(t, Config{ConnectTimeout: time.Second, RetryTimeout: time.Second},
		LocalOptions{Mode: ModeBoth, UpstreamAddress: upstream, ReconnectInterval: reconnect})
}

func TestBoschDroppingAfterLoginBacksOff(t *testing.T) {
	t.Parallel()
	var logins atomic.Int32
	address, _ := serve(t, func(_ int32, s net.Conn) {
		if fakeBosch(&cloudFixture{conn: s, reader: wire.NewReader(s)}, nil, nil) == nil {
			logins.Add(1)
		}
	})
	interval := 50 * time.Millisecond
	c := bothClientFor(t, address, interval)
	// A device that logs straight back in whenever it is dropped.
	go func() {
		for c.ctx.Err() == nil {
			s, err := deviceLogin(c)
			if err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			_, _ = io.Copy(io.Discard, s)
			_ = s.Close()
		}
	}()
	time.Sleep(20 * interval)
	// Without backoff each probe interval costs the device a reconnect.
	if n := logins.Load(); n < 2 || n > 7 {
		t.Fatalf("%d relayed logins in %v", n, 20*interval)
	}
}

func TestReplacedRelayIsNoBoschFailure(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var current net.Conn
	address, _ := serve(t, func(_ int32, s net.Conn) {
		if fakeBosch(&cloudFixture{conn: s, reader: wire.NewReader(s)}, func(iq *wire.Element) bool {
			if iq.Child(wire.BindNS, "bind") == nil {
				return false
			}
			mu.Lock()
			defer mu.Unlock()
			// Like most XMPP servers, Bosch ends the older session once the
			// resource is bound again, before the new login completes.
			if current != nil {
				_, _ = writeTyped(current, wire.E(wire.StreamNS, "error", wire.E("urn:ietf:params:xml:ns:xmpp-streams", "conflict")))
				_ = current.Close()
			}
			current = s
			return false
		}, nil) == nil {
			_, _ = io.Copy(io.Discard, s)
		}
	})
	c := bothClientFor(t, address, time.Hour)
	// The device reboots twice, leaving each old socket half-open.
	for login := range 3 {
		previous := c.conn.Load()
		s, err := deviceLogin(c)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		deadline := time.Now().Add(2 * time.Second)
		for c.conn.Load() == previous || !c.UpstreamConnected() {
			if time.Now().After(deadline) {
				t.Fatalf("login %d not relayed: Bosch ending the replaced session backed off relaying", login)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestBlockedNonRequestsAreNotAnswered(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesBlock)
	service := "gservice_update@" + DefaultHost
	// RFC 6120 8.3.1: answering an error invites a bounce loop.
	cloud.write(t, wire.Message{From: service + "/bounce", To: c.config.ResourceJID(), Type: "error", Body: wire.Body{Text: "PUT /gateway/update HTTP/1.1\r\n\r\n"}})
	// Services answer every message, so one that is no request stays unanswered too.
	cloud.write(t, wire.Message{From: service + "/opaque", To: c.config.ResourceJID(), Type: "chat", Body: wire.Body{Text: "AAAAAAAAAAAAAAAAAAAAAA=="}})
	cloud.write(t, wire.Message{From: service + "/request", To: c.config.ResourceJID(), Type: "chat", Body: wire.Body{Text: "GET /gateway/versionFirmware HTTP/1.1\r\n\r\n"}})
	if e := cloud.next(t); e.Get("to") != service+"/request" || !strings.Contains(e.Child(wire.ClientNS, "body").Text(), "403") {
		t.Fatalf("first reply went to %q", e.Get("to"))
	}
	// Markup a lenient parser reads as a line break still sets off a request,
	// though the joined text reads as none.
	split := wire.E(wire.ClientNS, "message", wire.E(wire.ClientNS, "body"))
	split.Children[0].Element.Children = []wire.Node{{Text: "x"}, {Element: ptrElement(wire.E(wire.ClientNS, "br"))}, {Text: "PUT /gateway/update\r\n\r\n"}}
	split.Set("from", service+"/split")
	split.Set("to", c.config.ResourceJID())
	cloud.write(t, split)
	if e := cloud.next(t); e.Get("to") != service+"/split" || !strings.Contains(e.Child(wire.ClientNS, "body").Text(), "403") {
		t.Fatalf("split request: reply went to %q", e.Get("to"))
	}
	// Nor is a blocked error relayed: the device's next stanza is ours.
	localWrite(t, c, d)
}

func TestBothNonRequestsAreNotAnswered(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	// Bosch services answer every gateway message: refusing one that holds
	// no request could ping-pong with them.
	xhtml := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "hi"),
		wire.E("http://jabber.org/protocol/xhtml-im", "html", wire.Text("http://www.w3.org/1999/xhtml", "body", "hi")))
	outside := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "hi"))
	outside.Children = append(outside.Children, wire.Node{Text: "stray"})
	foreign := wire.E("jabber:server", "message", wire.Text("jabber:server", "body", "hi"))
	ambiguous := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "GET /x HTTP/1.1\r\n\r\n"), wire.Text(wire.ClientNS, "body", "x"))
	service := "gservice_time@" + DefaultHost
	for i, e := range []wire.Element{xhtml, outside, foreign, ambiguous} {
		e.Set("from", fmt.Sprintf("%s/%d", service, i))
		e.Set("to", c.config.ResourceJID())
		cloud.write(t, e)
	}
	if e := cloud.next(t); e.Get("to") != service+"/3" || !strings.Contains(e.Child(wire.ClientNS, "body").Text(), "400 Bad Request") {
		t.Fatalf("first reply went to %q", e.Get("to"))
	}
	// Nothing else was answered or relayed: the device's next stanza is ours.
	localWrite(t, c, d)
}

func TestLocalPushIsNoReply(t *testing.T) {
	t.Parallel()
	c := replayClient(t)
	socket, device := net.Pipe()
	t.Cleanup(func() { _ = device.Close() })
	tr := testBridge(t, c, socket, nil, LocalOptions{Mode: ModeOffline, RequestTimeout: time.Hour, ReconnectInterval: time.Hour})
	push := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "HTTP/1.0 200 OK\r\n\r\n"))
	push.Set("to", c.config.JID()+"/"+localResource)
	go func() { tr.inputs <- frameEvent{frame: wire.Frame{Element: &push}} }()
	// Unowned, it must stay a push, or the client could take it for the
	// reply to a request written before it is routed.
	if in, err := tr.Recv(); err != nil || !in.push || in.owner != nil {
		t.Fatalf("push delivered as %+v, %v", in, err)
	}
}

func TestErrorQuotingRequestIsNotScheduled(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	bounce := cloudRequest(c, "phone", "PUT /x HTTP/1.1\r\n\r\n")
	bounce.Type = "error"
	cloud.write(t, bounce)
	// The next device frame must be the local write, not the bounce.
	localWrite(t, c, d)
	cloud.write(t, cloudRequest(c, "phone", "GET /after HTTP/1.1\r\n\r\n"))
	if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "GET /after ") {
		t.Fatal("the bounce's sender was held back:", raw)
	}
}

func TestSelfEndedRelayIsNoBoschFailure(t *testing.T) {
	t.Parallel()
	c := replayClient(t)
	socket, device := net.Pipe()
	upstream, cloud := net.Pipe()
	t.Cleanup(func() { _ = device.Close(); _ = cloud.Close() })
	tr := testBridge(t, c, socket, upstream, LocalOptions{Mode: ModeBoth, RequestTimeout: time.Second, ReconnectInterval: time.Hour})
	go func() { _, _ = tr.Recv() }()
	// Bosch reads nothing, so relaying this presence blocks until abort.
	if _, err := device.Write([]byte(`<presence xmlns="jabber:client"/>`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	tr.abort()
	tr.workers.Wait()
	if !tr.health.ready() {
		t.Fatal("ending the session counted as a Bosch failure")
	}
}

func TestBoschRefusingLoginStepBacksOff(t *testing.T) {
	t.Parallel()
	for _, refused := range []struct {
		name    xml.Name
		step    int  // index into loginSteps
		carryOn bool // the device ignores the refusal and finishes its login
	}{
		{xml.Name{Space: wire.BindNS, Local: "bind"}, 4, false},
		{xml.Name{Space: wire.SessionNS, Local: "session"}, 5, false},
		{xml.Name{Space: wire.BindNS, Local: "bind"}, 4, true},
		{xml.Name{Space: wire.SessionNS, Local: "session"}, 5, true},
	} {
		t.Run(fmt.Sprintf("%s carry on %v", refused.name.Local, refused.carryOn), func(t *testing.T) {
			address, accepts := serve(t, func(_ int32, s net.Conn) {
				_ = fakeBosch(&cloudFixture{conn: s, reader: wire.NewReader(s)}, func(iq *wire.Element) bool { return iq.Child(refused.name.Space, refused.name.Local) != nil }, nil)
				// Bosch keeps the stream: only the refusal can fail the login.
				_, _ = io.Copy(io.Discard, s)
			})
			c := bothClientFor(t, address, time.Hour)
			d := reconnectCandidate(t, c)
			steps := loginSteps(c)
			if !refused.carryOn {
				steps = steps[:refused.step+1]
			}
			for i, step := range steps {
				d.write(t, step.send)
				switch {
				case i == refused.step:
					d.readThrough(t, "</iq>")
				case step.end != "":
					d.readThrough(t, step.end)
				}
			}
			if !refused.carryOn {
				// The device gives up on the refusal; only the refusal can
				// count against Bosch before the login deadline.
				_ = d.socket.Close()
			}
			start := time.Now()
			s, err := deviceLogin(c)
			if err != nil {
				t.Fatalf("device not served offline after Bosch refused its %s: %v", refused.name.Local, err)
			}
			defer s.Close() //nolint:errcheck
			if c.UpstreamConnected() || accepts.Load() != 1 || time.Since(start) > 500*time.Millisecond {
				t.Fatal("refusal did not back off", accepts.Load(), time.Since(start))
			}
		})
	}
}

func TestBothRequiresHeaderFirst(t *testing.T) {
	t.Parallel()
	relayed := make(chan int64, 1)
	address, _ := serve(t, func(_ int32, s net.Conn) {
		n, _ := io.Copy(io.Discard, s)
		relayed <- n
	})
	c := bothClientFor(t, address, time.Hour)
	d := reconnectCandidate(t, c)
	d.write(t, `<presence xmlns="jabber:client"/>`)
	reconnectSocketClosed(t, d.socket)
	if n := wait(t, relayed); n != 0 {
		t.Fatalf("%d bytes relayed before the device's stream header", n)
	}
}

func TestBoschStreamErrorAtLoginBacksOff(t *testing.T) {
	t.Parallel()
	address, accepts := serve(t, func(_ int32, s net.Conn) {
		_ = fakeBosch(&cloudFixture{conn: s, reader: wire.NewReader(s)}, nil, func(cloud *cloudFixture, f wire.Frame) {
			if f.Element != nil && f.Element.Child(wire.BindNS, "bind") != nil {
				cloud.write(t, wire.E(wire.StreamNS, "error", wire.E("urn:ietf:params:xml:ns:xmpp-streams", "conflict")))
			}
		})
		// Bosch keeps the socket: only its error can fail the login.
		_, _ = io.Copy(io.Discard, s)
	})
	c := bothClientFor(t, address, time.Hour)
	d := reconnectCandidate(t, c)
	for _, step := range loginSteps(c)[:5] {
		d.write(t, step.send)
		d.readThrough(t, step.end)
	}
	d.readThrough(t, "</stream:error>")
	_ = d.socket.Close()
	s, err := deviceLogin(c)
	if err != nil {
		t.Fatal("device not served offline after a Bosch stream error:", err)
	}
	defer s.Close() //nolint:errcheck
	if c.UpstreamConnected() || accepts.Load() != 1 {
		t.Fatal("stream error did not back off", accepts.Load())
	}
}

func TestBothOutlivesLoginDeadline(t *testing.T) {
	t.Parallel()
	c, d, _ := bothHandshakeClient(t, UpdatesAllow, nil, nil, func(c *Config, _ *LocalOptions) { c.ConnectTimeout = time.Second })
	time.Sleep(1300 * time.Millisecond)
	if !c.UpstreamConnected() {
		t.Fatal("relay lost at the login deadline")
	}
	localWrite(t, c, d)
}

func TestBothProbeWaitsForBackoff(t *testing.T) {
	t.Parallel()
	address, accepts := serve(t, func(_ int32, s net.Conn) { xmppGreeter(s) })
	c := replayClient(t)
	socket, device := net.Pipe()
	t.Cleanup(func() { _ = device.Close() })
	go func() { _, _ = io.Copy(io.Discard, device) }()
	tr := testBridge(t, c, socket, nil, LocalOptions{Mode: ModeBoth, UpstreamAddress: address, RequestTimeout: time.Second, ReconnectInterval: 20 * time.Millisecond})
	tr.health.fail(time.Hour)
	c.dial = func(context.Context) (transport, error) { return tr, nil }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Bosch answers XMPP, but rejected the gateway: recovering would only
	// cost the device a reconnect.
	select {
	case <-c.Done():
		t.Fatal("device reconnected during backoff")
	case <-time.After(200 * time.Millisecond):
	}
	if accepts.Load() != 0 {
		t.Fatal("Bosch probed during backoff", accepts.Load())
	}
}

func TestBothRelaysServiceMessagesPastActiveRequest(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/local", map[string]int{"value": 1}) }()
	if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "PUT /local ") {
		t.Fatal(raw)
	}
	// Time and weather pushes are not requests; queuing them would delay them.
	cloud.write(t, wire.Message{From: "gservice_time@" + DefaultHost + "/x", To: c.config.ResourceJID(), Type: "chat", Body: wire.Body{Text: "opaque service payload"}})
	if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "opaque service payload") {
		t.Fatal("service message held behind the active request", raw)
	}
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestEncoderBoundsOutput(t *testing.T) {
	t.Parallel()
	e := wire.Text(wire.ClientNS, "body", strings.Repeat("x", 2*wire.MaxStanzaBytes+1))
	if _, err := wire.Marshal(wire.Frame{Element: &e}, true); err == nil {
		t.Fatal("oversized stanza encoded")
	}
}

func TestCloudLossClosesUpstream(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	cloud.write(t, wire.E(wire.StreamNS, "error", wire.E("urn:ietf:params:xml:ns:xmpp-streams", "system-shutdown")))
	waitCloudLoss(t, c)
	_ = cloud.conn.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, err := cloud.reader.Next(); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("Bosch socket left open after stream error")
			}
			break
		}
	}
	localWrite(t, c, d)
}

func TestStalledBoschDoesNotBlockLocalControl(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, Config{RetryTimeout: 2 * time.Second})
	socket, device := net.Pipe()
	upstream, cloud := net.Pipe() // cloud never reads
	t.Cleanup(func() { _ = device.Close(); _ = cloud.Close() })
	tr := testBridge(t, c, socket, upstream, LocalOptions{Mode: ModeBoth, RequestTimeout: 200 * time.Millisecond, ReconnectInterval: time.Hour})
	c.dial = func(context.Context) (transport, error) { return tr, nil }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	presence := wire.E(wire.ClientNS, "presence")
	tr.inputs <- frameEvent{frame: wire.Frame{Element: &presence}}
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/after", 1) }()
	nextRequest(t, deviceReader(t, device), "PUT /after ")
	response := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "HTTP/1.0 204 No Content\r\n\r\n"))
	response.Set("to", c.config.JID()+"/"+localResource)
	tr.inputs <- frameEvent{frame: wire.Frame{Element: &response}}
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestAbandonedRequestsLeaveQueue(t *testing.T) {
	t.Parallel()
	c := replayClient(t)
	socket, device := net.Pipe()
	t.Cleanup(func() { _ = device.Close() })
	go func() { _, _ = io.Copy(io.Discard, device) }() // the device never answers
	tr := testBridge(t, c, socket, nil, LocalOptions{Mode: ModeOffline, RequestTimeout: time.Hour, ReconnectInterval: time.Hour})
	go func() {
		for {
			if _, err := tr.Recv(); err != nil {
				return
			}
		}
	}()
	if err := tr.Send(t.Context(), "GET /busy HTTP/1.1\r\n\r\n", new(pending)); err != nil {
		t.Fatal(err)
	}
	// Each caller gives up while the device is busy, then the next arrives.
	for range 100 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		err := tr.Send(ctx, "GET /x HTTP/1.1\r\n\r\n", new(pending))
		cancel()
		var unsent *unsentError
		if !errors.As(err, &unsent) {
			t.Fatal("abandoned request:", err)
		}
	}
	_ = tr.Close() // the bridge has stopped: its queue is safe to read
	if n := len(tr.queue); n > 2 {
		t.Fatalf("%d entries queued after their callers gave up", n)
	}
}

func TestBothSurvivesStrayDeviceAndStreamEnds(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	// A device push with nothing in flight, then Bosch closing its stream.
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if _, err := cloud.conn.Write([]byte("</stream:stream>")); err != nil {
		t.Fatal(err)
	}
	waitCloudLoss(t, c)
	localWrite(t, c, d)
	// The device closing its stream ends the session cleanly.
	done := c.Done()
	d.write(t, "</stream:stream>")
	wait(t, done)
}

func TestBothSchedulesWrappedRequests(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/local", map[string]int{"value": 1}) }()
	if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "PUT /local ") {
		t.Fatal(raw)
	}
	// Whitespace and an extension before the body: still a request, which
	// must wait for the active one.
	m := wire.E(wire.ClientNS, "message", wire.E("urn:example", "x"), wire.Text(wire.ClientNS, "body", "GET /cloud HTTP/1.1\r\n\r\n"))
	m.Children = append([]wire.Node{{Text: "\n  "}}, m.Children...)
	m.Set("from", c.config.JID()+"/phone")
	m.Set("to", c.config.ResourceJID())
	cloud.write(t, m)
	_ = d.socket.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if raw, err := d.reader.ReadString('>'); err == nil {
		t.Fatal("request sent while another was active:", raw)
	}
	_ = d.socket.SetReadDeadline(time.Now().Add(3 * time.Second))
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "GET /cloud ") {
		t.Fatal(raw)
	}
}

func TestBothDropsRequestsBehindStreamError(t *testing.T) {
	t.Parallel()
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	// Both in one read: the request after the error must not be scheduled.
	streamError := marshalTest(t, wire.E(wire.StreamNS, "error", wire.E("urn:ietf:params:xml:ns:xmpp-streams", "system-shutdown")))
	late := marshalTest(t, cloudRequest(c, "phone", "GET /late HTTP/1.1\r\n\r\n"))
	if _, err := cloud.conn.Write([]byte(streamError + late)); err != nil {
		t.Fatal(err)
	}
	waitCloudLoss(t, c)
	localWrite(t, c, d)
}

func TestFailedProbeKeepsDevice(t *testing.T) {
	t.Parallel()
	tcpOnly, _ := serve(t, func(_ int32, s net.Conn) { _, _ = wire.NewReader(s).Next() })
	c := replayClient(t)
	socket, device := net.Pipe()
	t.Cleanup(func() { _ = device.Close() })
	go func() { _, _ = io.Copy(io.Discard, device) }()
	tr := testBridge(t, c, socket, nil, LocalOptions{Mode: ModeBoth, UpstreamAddress: tcpOnly, RequestTimeout: time.Second, ReconnectInterval: 20 * time.Millisecond})
	c.dial = func(context.Context) (transport, error) { return tr, nil }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Recovering only helps once Bosch answers XMPP; until then each probe
	// would cost the device a reconnect.
	select {
	case <-c.Done():
		t.Fatal("device reconnected after a failed probe")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestReaderStopsAfterError(t *testing.T) {
	t.Parallel()
	c := replayClient(t)
	socket, device := net.Pipe()
	upstream, cloud := net.Pipe()
	t.Cleanup(func() { _ = device.Close() })
	tr := testBridge(t, c, socket, upstream, LocalOptions{Mode: ModeBoth, RequestTimeout: time.Second, ReconnectInterval: time.Hour})
	_ = cloud.Close()
	// One error, then silence: a reader that kept going would spin.
	if event := wait(t, tr.inputs); event.err == nil {
		t.Fatal("no error after Bosch closed")
	}
	select {
	case event := <-tr.inputs:
		t.Fatal("reader kept reading:", event)
	case <-time.After(50 * time.Millisecond):
	}
}

// The next socket read proves the decoder consumed an incomplete stanza;
// observing Next's entry alone would confuse an idle read with old input.
type replyReadSocket struct {
	net.Conn
	reads chan int
	count int
}

func (s *replyReadSocket) Read(p []byte) (int, error) {
	s.count++
	s.reads <- s.count
	return s.Conn.Read(p)
}

// Hold the first decoded reply so a complete older stanza remains buffered
// while the public request starts. Local requests also signal queue admission.
type heldReplyTransport struct {
	transport
	first, release, queued chan struct{}
	holdOnce, releaseOnce  sync.Once
	queuedOnce             sync.Once
}

func (tr *heldReplyTransport) Recv() (inbound, error) {
	in, err := tr.transport.Recv()
	if err == nil {
		tr.holdOnce.Do(func() {
			close(tr.first)
			<-tr.release
		})
	}
	return in, err
}

func (tr *heldReplyTransport) Send(ctx context.Context, body string, p *pending) error {
	if tr.queued != nil {
		ctx = &replyQueuedContext{Context: ctx, admitted: func() { tr.queuedOnce.Do(func() { close(tr.queued) }) }}
	}
	return tr.transport.Send(ctx, body, p)
}

func (tr *heldReplyTransport) unblock() { tr.releaseOnce.Do(func() { close(tr.release) }) }

func (tr *heldReplyTransport) Close() error {
	tr.unblock()
	return tr.transport.Close()
}

type replyQueuedContext struct {
	context.Context
	calls    atomic.Int32
	admitted func()
}

func (ctx *replyQueuedContext) Done() <-chan struct{} {
	// submit evaluates Done before enqueuing and again while waiting for its
	// write. An unbuffered request channel makes the latter prove admission.
	if ctx.calls.Add(1) == 2 {
		ctx.admitted()
	}
	return ctx.Context.Done()
}

func testReplyOwnershipAcrossReads(t *testing.T, build func(*testing.T, *Client, net.Conn) transport) {
	t.Helper()
	for _, scenario := range []string{"partial push", "complete push in read-ahead", "idle read before request"} {
		t.Run(scenario, func(t *testing.T) {
			c := newTestClient(t, Config{RetryTimeout: 3 * time.Second})
			socket, peer := net.Pipe()
			t.Cleanup(func() { _ = peer.Close() })
			observed := &replyReadSocket{Conn: socket, reads: make(chan int, 32)}
			base := build(t, c, observed)
			tr := base
			local, bridged := base.(*localTransport)
			var held *heldReplyTransport
			if scenario == "complete push in read-ahead" {
				held = &heldReplyTransport{transport: base, first: make(chan struct{}), release: make(chan struct{})}
				if bridged {
					// Make the queued public GET observable before the first reply
					// frees the device, without reading bridge state concurrently.
					local.requests = make(chan bridgeRequest)
					held.queued = make(chan struct{})
				}
				tr = held
			}
			c.dial = func(context.Context) (transport, error) { return tr, nil }
			pushes := make(chan PushNotification, 8)
			c.Subscribe(func(uri string, data any) { pushes <- PushNotification{URI: uri, Data: data} })
			if err := c.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			if n := wait(t, observed.reads); n != 1 {
				t.Fatal("initial socket read", n)
			}
			reader := deviceReader(t, peer)
			h := &harness{c: c}
			var result <-chan getResult
			replyXML := func(uri, value string) string {
				return marshalTest(t, wire.Message{
					From: c.config.ResourceJID() + "/RRC-RestApi", To: c.config.JID() + "/" + localResource, Type: "chat",
					Body: wire.Body{Text: encryptedReply(t, c, "200 OK", fmt.Sprintf(`{"id":%q,"value":%q}`, uri, value))},
				})
			}
			write := func(raw string) {
				t.Helper()
				if _, err := io.WriteString(peer, raw); err != nil {
					t.Fatal(err)
				}
			}
			older := replyXML("/same", "earlierpush")
			switch scenario {
			case "partial push":
				split := len(older) - 3
				write(older[:split])
				if n := wait(t, observed.reads); n != 2 {
					t.Fatal("decoder did not request the closing-tag suffix", n)
				}
				result = h.get(t.Context(), "/same")
				nextRequest(t, reader, "GET /same ")
				write(older[split:])
			case "complete push in read-ahead":
				firstSent := make(chan error, 1)
				go func() {
					firstSent <- base.Send(t.Context(), protocol.GetRequest("/first"), &pending{uri: "/first", get: true})
				}()
				nextRequest(t, reader, "GET /first ")
				if err := wait(t, firstSent); err != nil {
					t.Fatal(err)
				}
				if bridged {
					result = h.get(t.Context(), "/same")
					wait(t, held.queued)
				}
				// net.Pipe copies this whole batch in one read into the decoder's
				// 4096-byte buffer; both stanzas arrived before GET /same.
				batch := replyXML("/first", "firstreply") + "\n  " + older
				if len(batch) >= 4096 {
					t.Fatal("batch no longer fits one decoder read")
				}
				write(batch)
				wait(t, held.first)
				if !bridged {
					result = h.get(t.Context(), "/same")
				}
				nextRequest(t, reader, "GET /same ")
				held.unblock()
			case "idle read before request":
				// The initial socket read is blocked, but has received no byte
				// from this reply. It must not inherit the earlier empty window.
				result = h.get(t.Context(), "/same")
				nextRequest(t, reader, "GET /same ")
			}
			if scenario != "idle read before request" {
				select {
				case got := <-result:
					t.Fatalf("pre-request stanza answered GET: value=%v", valueOf(t, got))
				case notification := <-pushes:
					if notification.URI != "/same" || notification.Data.(map[string]any)["value"] != "earlierpush" {
						t.Fatal("earlier push lost", notification)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("earlier stanza was not delivered as a push")
				}
				select {
				case got := <-result:
					t.Fatalf("GET completed before actual reply: value=%v", valueOf(t, got))
				default:
				}
			}
			write(replyXML("/other", "inflightpush"))
			if notification := wait(t, pushes); notification.URI != "/other" || notification.Data.(map[string]any)["value"] != "inflightpush" {
				t.Fatal("subscription lost a push during GET", notification)
			}
			select {
			case got := <-result:
				t.Fatalf("unrelated push answered GET: value=%v", valueOf(t, got))
			default:
			}
			if bridged && scenario != "idle read before request" {
				// A push must not release the device slot even if the original
				// public GET is still waiting in the client's own queue.
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
				p := &pending{uri: "/next", get: true}
				err := base.Send(ctx, protocol.GetRequest("/next"), p)
				cancel()
				var unsent *unsentError
				if !errors.As(err, &unsent) || !errors.Is(err, context.DeadlineExceeded) || p.sent.Load() {
					t.Fatalf("push freed the device slot: sent=%v, error=%v", p.sent.Load(), err)
				}
			}
			write(replyXML("/same", "actualreply"))
			if value := valueOf(t, wait(t, result)); value != "actualreply" {
				t.Fatal("GET returned an earlier push", value)
			}
			// Keeping an ownership window must not swallow subsequent pushes.
			write(replyXML("/same", "samepathpush"))
			if notification := wait(t, pushes); notification.URI != "/same" || notification.Data.(map[string]any)["value"] != "samepathpush" {
				t.Fatal("subscription lost a push matching the completed GET", notification)
			}
			write(replyXML("/push", "freshpush"))
			if notification := wait(t, pushes); notification.URI != "/push" || notification.Data.(map[string]any)["value"] != "freshpush" {
				t.Fatal("subscription stopped after GET", notification)
			}
			if !c.IsConnected() {
				t.Fatal("reply ownership retired the healthy session")
			}
		})
	}
}

func TestLocalReplyOwnershipAcrossReads(t *testing.T) {
	t.Parallel()
	for _, mode := range []ServerMode{ModeOffline, ModeBoth} {
		t.Run(string(mode), func(t *testing.T) {
			testReplyOwnershipAcrossReads(t, func(t *testing.T, c *Client, socket net.Conn) transport {
				var upstream net.Conn
				if mode == ModeBoth {
					var cloud net.Conn
					upstream, cloud = net.Pipe()
					t.Cleanup(func() { _ = cloud.Close() })
					go func() { _, _ = io.Copy(io.Discard, cloud) }()
				}
				return testBridge(t, c, socket, upstream, LocalOptions{Mode: mode, RequestTimeout: 5 * time.Second, ReconnectInterval: time.Hour})
			})
		})
	}
}
