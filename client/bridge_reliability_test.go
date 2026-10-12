package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kradalby/nefit-go/internal/xmpp"
)

// observedCloudWrite makes upstream backpressure visible without changing
// the connection's normal deadline or close behavior.
type observedCloudWrite struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (w *observedCloudWrite) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	return w.Conn.Write(data)
}

func TestBothProcessesLocalReplyDuringCloudBackpressure(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, Config{RetryTimeout: 500 * time.Millisecond, PingInterval: time.Hour})
	socket, device := net.Pipe()
	upstream, cloud := net.Pipe()
	t.Cleanup(func() { _ = device.Close(); _ = cloud.Close() })
	observed := &observedCloudWrite{Conn: upstream, entered: make(chan struct{})}
	tr := testBridge(t, c, socket, observed, LocalOptions{Mode: ModeBoth, RequestTimeout: 2 * time.Second, ReconnectInterval: time.Hour})
	c.dial = func(context.Context) (transport, error) { return tr, nil }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.Get(t.Context(), "/ecus/rrc/uiStatus")
		done <- err
	}()
	nextRequest(t, deviceReader(t, device), "GET /ecus/rrc/uiStatus ")
	// Bosch stops reading while a local request is outstanding. Forwarding
	// this presence must not stop the bridge receiving the device's answer.
	presence := xmpp.E(xmpp.ClientNS, "presence")
	tr.inputs <- frameEvent{frame: xmpp.Frame{Element: &presence}}
	wait(t, observed.entered)
	response := xmpp.Message{
		To: c.config.JID() + "/" + localResource, Type: "chat",
		Body: xmpp.Body{Text: encryptedReply(t, c, "200 OK", `{"id":"/ecus/rrc/uiStatus","value":1}`)},
	}
	if _, err := io.WriteString(device, marshalTest(t, response)); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, done); err != nil {
		t.Fatal("a blocked Bosch write delayed a valid local reply:", err)
	}
	if !c.IsConnected() || !c.UpstreamConnected() {
		t.Fatal("backpressure retired an otherwise healthy connection")
	}
	// Let the same upstream write complete successfully, before its own
	// deadline, so the test does not rely on disconnecting Bosch to recover.
	frame, err := deviceReader(t, cloud).Next()
	if err != nil || frame.Element == nil || frame.Element.Name.Local != "presence" {
		t.Fatalf("queued presence was not delivered: frame=%v err=%v", frame, err)
	}
}

func blockedRelayClient(t *testing.T, timeout time.Duration) (*Client, *localTransport, net.Conn, net.Conn, *observedCloudWrite) {
	t.Helper()
	c := newTestClient(t, Config{RetryTimeout: 3 * time.Second, PingInterval: time.Hour})
	socket, device := net.Pipe()
	upstream, cloud := net.Pipe()
	t.Cleanup(func() { _ = device.Close(); _ = cloud.Close() })
	observed := &observedCloudWrite{Conn: upstream, entered: make(chan struct{})}
	tr := testBridge(t, c, socket, observed, LocalOptions{Mode: ModeBoth, RequestTimeout: timeout, ReconnectInterval: time.Hour})
	c.dial = func(context.Context) (transport, error) { return tr, nil }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c, tr, device, cloud, observed
}

func blockCloud(t *testing.T, tr *localTransport, observed *observedCloudWrite) {
	t.Helper()
	presence := xmpp.E(xmpp.ClientNS, "presence")
	tr.inputs <- frameEvent{frame: xmpp.Frame{Element: &presence}}
	wait(t, observed.entered)
}

func writeDeviceStanza(t *testing.T, device net.Conn, stanza any) {
	t.Helper()
	if _, err := io.WriteString(device, marshalTest(t, stanza)); err != nil {
		t.Fatal(err)
	}
}

func replyLocalValue(t *testing.T, c *Client, device net.Conn, value string) {
	t.Helper()
	writeDeviceStanza(t, device, xmpp.Message{
		To: c.config.JID() + "/" + localResource, Type: "chat",
		Body: xmpp.Body{Text: encryptedReply(t, c, "200 OK", fmt.Sprintf(`{"id":"/ecus/rrc/uiStatus","value":%q}`, value))},
	})
}

func TestBothCloudWriteTimeoutAnswersQueuedDevicePings(t *testing.T) {
	t.Parallel()
	c, tr, device, _, observed := blockedRelayClient(t, 500*time.Millisecond)
	h := &harness{c: c}
	done := h.get(t.Context(), "/ecus/rrc/uiStatus")
	reader := deviceReader(t, device)
	nextRequest(t, reader, "GET /ecus/rrc/uiStatus ")
	blockCloud(t, tr, observed)
	for _, id := range []string{"first", "second"} {
		writeDeviceStanza(t, device, xmpp.IQ{Type: "get", ID: id, Extensions: []xmpp.Element{xmpp.E(xmpp.PingNS, "ping")}})
	}
	replyLocalValue(t, c, device, "current")
	if v := valueOf(t, wait(t, done)); v != "current" || !c.UpstreamConnected() {
		t.Fatal("local response waited for the upstream deadline", v)
	}
	waitCloudLoss(t, c)
	for _, id := range []string{"first", "second"} {
		frame, err := reader.Next()
		if err != nil || frame.Element == nil || frame.Element.Get("type") != "result" || frame.Element.Get("id") != id {
			t.Fatalf("queued device ping %s was not answered in order: frame=%v err=%v", id, frame, err)
		}
	}
	done = h.get(t.Context(), "/ecus/rrc/uiStatus")
	nextRequest(t, reader, "GET /ecus/rrc/uiStatus ")
	replyLocalValue(t, c, device, "offline")
	if v := valueOf(t, wait(t, done)); v != "offline" {
		t.Fatal("local control failed after the cloud write deadline", v)
	}
}

func TestBothCloudOutputQueueIsBounded(t *testing.T) {
	t.Parallel()
	c, tr, device, _, observed := blockedRelayClient(t, 5*time.Second)
	blockCloud(t, tr, observed)
	for i := range maxCloudRequests + 1 {
		writeDeviceStanza(t, device, xmpp.IQ{Type: "result", ID: strconv.Itoa(i)})
	}
	waitCloudLoss(t, c)
	h := &harness{c: c}
	done := h.get(t.Context(), "/ecus/rrc/uiStatus")
	nextRequest(t, deviceReader(t, device), "GET /ecus/rrc/uiStatus ")
	replyLocalValue(t, c, device, "offline")
	if v := valueOf(t, wait(t, done)); v != "offline" {
		t.Fatal("full cloud output queue interrupted local control", v)
	}
}

func TestBothCloudOutputPreservesOrder(t *testing.T) {
	t.Parallel()
	c, tr, device, cloud, observed := blockedRelayClient(t, 5*time.Second)
	blockCloud(t, tr, observed)
	for _, id := range []string{"first", "second", "third"} {
		writeDeviceStanza(t, device, xmpp.IQ{Type: "result", ID: id})
	}
	reader := deviceReader(t, cloud)
	frame, err := reader.Next()
	if err != nil || frame.Element == nil || frame.Element.Name.Local != "presence" {
		t.Fatal("initial queued presence was not delivered", err)
	}
	for _, id := range []string{"first", "second", "third"} {
		frame, err := reader.Next()
		if err != nil || frame.Element == nil || frame.Element.Get("id") != id {
			t.Fatalf("cloud output order lost at %s: frame=%v err=%v", id, frame, err)
		}
	}
	if !c.IsConnected() || !c.UpstreamConnected() {
		t.Fatal("successful queued writes lost a connection")
	}
}

func TestBothCloseUnblocksCloudOutput(t *testing.T) {
	t.Parallel()
	c, tr, _, _, observed := blockedRelayClient(t, time.Hour)
	blockCloud(t, tr, observed)
	done := make(chan error, 1)
	go func() { done <- c.Close() }()
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBothStopsAppRequestsBeforeFinishingRelay(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, Config{})
	socket, device := net.Pipe()
	upstream, cloud := net.Pipe()
	t.Cleanup(func() { _ = device.Close(); _ = cloud.Close() })
	tr := testBridge(t, c, socket, upstream, LocalOptions{Mode: ModeBoth, RequestTimeout: time.Second, ReconnectInterval: time.Hour})
	// Model a writer held behind earlier output: the terminal reply and close
	// marker can be queued before the actual upstream socket closes.
	tr.upstreamWrites = make(chan upstreamWrite, maxCloudRequests)
	tr.queue = []bridgeRequest{{message: cloudRequest(c, "phone", "PUT /already-queued HTTP/1.1\r\n\r\n"), ctx: tr.ctx}}
	tr.finishUpstream()
	request := xmpp.E(xmpp.ClientNS, "message", xmpp.Text(xmpp.ClientNS, "body", "PUT /new HTTP/1.1\r\n\r\n"))
	request.Set("from", c.config.JID()+"/other-phone")
	request.Set("to", c.config.ResourceJID())
	if relay, err := tr.admitCloud(&request); relay || err != nil {
		t.Fatal("new app request was not discarded while finishing", relay, err)
	}
	done := make(chan error, 1)
	go func() { done <- tr.sendNext() }()
	_ = device.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var unexpected [1]byte
	if _, err := device.Read(unexpected[:]); err == nil {
		t.Fatal("app PUT reached the device after relay termination was decided")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal("checking device dispatch while finishing:", err)
	}
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBothHeartbeatsDoNotInterruptRelayFinish(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, Config{PingInterval: 20 * time.Millisecond})
	socket, device := net.Pipe()
	upstream, cloud := net.Pipe()
	t.Cleanup(func() { _ = device.Close(); _ = cloud.Close() })
	go func() { _, _ = io.Copy(io.Discard, device) }()
	tr := testBridge(t, c, socket, upstream, LocalOptions{Mode: ModeBoth, RequestTimeout: time.Second, ReconnectInterval: time.Hour})
	// Before starting the owner, model a terminal response still waiting to
	// drain: the relay remains connected, but accepts no further output.
	tr.upstreamFinishing = true
	c.dial = func(context.Context) (transport, error) { return tr, nil }
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-time.After(5 * c.config.PingInterval)
	if !c.IsConnected() || !c.UpstreamConnected() {
		t.Fatal("a heartbeat interrupted the relay before terminal output drained")
	}
}

func TestBothBackpressurePreservesAbandonedReplyOwnership(t *testing.T) {
	t.Parallel()
	c, tr, device, _, observed := blockedRelayClient(t, 3*time.Second)
	h := &harness{c: c}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := h.get(ctx, "/ecus/rrc/uiStatus")
	reader := deviceReader(t, device)
	nextRequest(t, reader, "GET /ecus/rrc/uiStatus ")
	blockCloud(t, tr, observed)
	cancel()
	if r := wait(t, first); !errors.Is(r.err, context.Canceled) {
		t.Fatal("first caller did not abandon its sent request", r.err)
	}
	second := h.get(t.Context(), "/ecus/rrc/uiStatus")
	_ = device.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var unexpected [1]byte
	if _, err := device.Read(unexpected[:]); err == nil {
		t.Fatal("second request reached a device still answering the first")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal("checking the outstanding device request:", err)
	}
	_ = device.SetReadDeadline(time.Now().Add(5 * time.Second))
	replyLocalValue(t, c, device, "abandoned")
	nextRequest(t, reader, "GET /ecus/rrc/uiStatus ")
	replyLocalValue(t, c, device, "current")
	if v := valueOf(t, wait(t, second)); v != "current" {
		t.Fatal("late reply answered the newer request", v)
	}
}
