package client

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xmpp "github.com/xmppo/go-xmpp"

	wire "github.com/kradalby/nefit-go/xmpp"
)

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
	raw := d.readThrough(t, "</message>")
	if !strings.Contains(raw, "PUT /local ") {
		t.Fatal("unexpected request", raw)
	}
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBothCloudLossPreservesOutstandingRequest(t *testing.T) {
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	cloud.write(t, wire.Message{From: c.config.JID() + "/phone", To: c.config.ResourceJID(), Body: wire.Body{Text: "GET /cloud HTTP/1.1\r\n\r\n"}})
	d.readThrough(t, "</message>")
	_ = cloud.conn.Close()
	waitCloudLoss(t, c)
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/local", map[string]int{"value": 1}) }()
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
	raw := d.readThrough(t, "</message>")
	if !strings.Contains(raw, "PUT /local ") {
		t.Fatal(raw)
	}
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBothQueuedTimeoutPreservesSession(t *testing.T) {
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
	for _, mode := range []ServerMode{ModeOffline, ModeBoth} {
		t.Run(string(mode), func(t *testing.T) {
			var c *Client
			var d *deviceFixture
			if mode == ModeBoth {
				c, d, _ = bothTestClient(t, UpdatesBlock)
			} else {
				var err error
				c, err = NewLocalClient(Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret", ConnectTimeout: time.Second, RetryTimeout: time.Second, PingInterval: time.Hour}, LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: net.ParseIP("127.0.0.1"), UpdatePolicy: UpdatesBlock})
				if err != nil {
					t.Fatal(err)
				}
				c.SetLogger(slog.New(slog.DiscardHandler))
				t.Cleanup(func() { _ = c.Close() })
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

func TestBothConsumesCloudStreamError(t *testing.T) {
	c, d, cloud := bothTestClient(t, UpdatesAllow)
	cloud.write(t, wire.E(wire.StreamNS, "error", wire.E("urn:ietf:params:xml:ns:xmpp-streams", "system-shutdown")))
	waitCloudLoss(t, c)
	localWrite(t, c, d)
}

type failingCloudWrite struct{ net.Conn }

func (failingCloudWrite) Write([]byte) (int, error) { return 0, errors.New("upstream write failed") }

func TestBothNonMessageWriteFailurePreservesSession(t *testing.T) {
	for _, name := range []string{"presence", "iq"} {
		t.Run(name, func(t *testing.T) {
			c, err := NewClient(Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret", PingInterval: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			c.SetLogger(slog.New(slog.DiscardHandler))
			t.Cleanup(func() { _ = c.Close() })
			socket, device := net.Pipe()
			up, cloud := net.Pipe()
			t.Cleanup(func() { _ = device.Close(); _ = cloud.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			tr := &localTransport{socket: socket, config: c.config, options: LocalOptions{Mode: ModeBoth, RequestTimeout: time.Second, ReconnectInterval: time.Hour}, bridgeState: bridgeState{ctx: ctx, cancel: cancel, upstream: failingCloudWrite{up}, inputs: make(chan frameEvent, 32), requests: make(chan bridgeRequest, 32), deliveries: make(chan bridgeDelivery, 32), recovery: make(chan struct{}, 1)}}
			tr.upstreamConnected.Store(true)
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

func TestBothBlocksUpdatesThroughoutHandshake(t *testing.T) {
	var cloudControls, deviceControls atomic.Int32
	cloudHook := func(cloud *cloudFixture, _ wire.Frame) {
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
}

type recordedDeviceWrites struct {
	net.Conn
	data bytes.Buffer
}

func (c *recordedDeviceWrites) Write(p []byte) (int, error) { return c.data.Write(p) }

func TestOfflineServiceResponsesRespectUpdatePolicy(t *testing.T) {
	for _, policy := range []UpdatePolicy{UpdatesBlock, UpdatesAllow} {
		t.Run(string(policy), func(t *testing.T) {
			socket, peer := net.Pipe()
			t.Cleanup(func() { _ = socket.Close(); _ = peer.Close() })
			recorder := &recordedDeviceWrites{Conn: socket}
			calls := 0
			tr := &localTransport{socket: recorder, options: LocalOptions{UpdatePolicy: policy, RequestTimeout: time.Second, Service: func(context.Context, wire.Message) (*wire.Message, error) {
				calls++
				return &wire.Message{From: "gservice_time@example.test", To: "device@example.test", Body: wire.Body{Text: "ordinary service reply"}, Extensions: []wire.Element{wire.Text(wire.ClientNS, "body", "PUT /gateway/update/strategy HTTP/1.1\r\n\r\n")}}, nil
			}}}
			request := wire.Message{From: "device@example.test", To: "gservice_time@example.test"}
			if err := tr.service(request); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("ordinary time service blocked")
			}
			if policy == UpdatesBlock && recorder.data.Len() != 0 {
				t.Fatal("firmware service reply escaped policy")
			}
			if policy == UpdatesAllow && recorder.data.Len() == 0 {
				t.Fatal("allow policy blocked service reply")
			}
			request.To = "gservice_update@example.test"
			if err := tr.service(request); err != nil {
				t.Fatal(err)
			}
			if policy == UpdatesBlock && calls != 1 {
				t.Fatal("blocked service reached handler")
			}
		})
	}
}

func TestBothAllowsFirmwareWrites(t *testing.T) {
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
	for _, mode := range []ServerMode{ModeOffline, ModeBoth} {
		t.Run(string(mode), func(t *testing.T) {
			c, err := NewClient(Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret", RetryTimeout: time.Second, MaxRetries: 1, PingInterval: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			c.SetLogger(slog.New(slog.DiscardHandler))
			t.Cleanup(func() { _ = c.Close() })
			socket, peer := net.Pipe()
			t.Cleanup(func() { _ = peer.Close() })
			announced := &announcedWrite{Conn: socket, started: make(chan struct{})}
			tr := &localTransport{socket: announced, reader: wire.NewReader(socket), config: c.config, options: LocalOptions{Mode: mode, RequestTimeout: time.Second, ReconnectInterval: time.Hour}}
			if mode == ModeBoth {
				tr.initBridge(nil)
			}
			c.dial = func(context.Context) (transport, error) { return tr, nil }
			if err := c.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			presence := make(chan error, 1)
			go func() { _, err := tr.SendPresence(xmpp.Presence{}); presence <- err }()
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

func TestBothBlocksAllMessageBodies(t *testing.T) {
	c, d, cloud := bothTestClient(t, UpdatesBlock)
	blocked := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "PUT /gateway/update/strategy HTTP/1.1\r\n\r\n"), wire.Text(wire.ClientNS, "body", "ordinary text"))
	blocked.Set("from", c.config.JID()+"/phone")
	blocked.Set("to", c.config.ResourceJID())
	blocked.Children[1].Element.Attr = []xml.Attr{{Name: xml.Name{Space: wire.XMLNS, Local: "lang"}, Value: "nl"}}
	cloud.write(t, blocked)
	if text := cloud.next(t).Child(wire.ClientNS, "body").Text(); !strings.Contains(text, "403 Forbidden") {
		t.Fatal("multi-body update not rejected", text)
	}
	localWrite(t, c, d)
}

func TestBothQueuesRequestsReceivedDuringLogin(t *testing.T) {
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
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/local", map[string]int{"value": 1}) }()
	for i := 1; i <= 2; i++ {
		expected := fmt.Sprintf("/early-%d", i)
		if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "GET "+expected+" ") {
			t.Fatal("login requests lost order", raw)
		}
		_ = d.socket.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
		if _, err := d.reader.ReadByte(); err == nil {
			t.Fatal("second request sent before first replied")
		} else {
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatal(err)
			}
		}
		_ = d.socket.SetReadDeadline(time.Now().Add(2 * time.Second))
		if i == 2 {
			_ = cloud.conn.Close()
			waitCloudLoss(t, c)
		}
		cipher, err := c.encryptor.Encrypt(fmt.Sprintf(`{"id":%q,"value":1}`, expected))
		if err != nil {
			t.Fatal(err)
		}
		d.write(t, marshalTest(t, wire.Message{To: c.config.JID() + "/phone", Type: "chat", Body: wire.Body{Text: "HTTP/1.0 200 OK\r\n\r\n" + cipher}}))
		if i == 1 {
			cloud.next(t)
		}
	}
	if raw := d.readThrough(t, "</message>"); !strings.Contains(raw, "PUT /local ") {
		t.Fatal(raw)
	}
	d.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBridgeRejectsAmbiguousAPIBodies(t *testing.T) {
	ordinary := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "English"), wire.Text(wire.ClientNS, "body", "Nederlands"))
	if _, err := bridgeMessage(&ordinary); err != nil {
		t.Fatal("ordinary language alternatives rejected", err)
	}
	for _, body := range []string{"GET /first HTTP/1.1\r\n\r\n", "PUT /first HTTP/1.1\r\n\r\n"} {
		ambiguous := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", body), wire.Text(wire.ClientNS, "body", "ordinary text"))
		if _, err := bridgeMessage(&ambiguous); err == nil {
			t.Fatal("ambiguous API bodies accepted")
		}
	}
}
