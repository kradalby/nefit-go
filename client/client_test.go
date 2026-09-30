package client

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xmpp "github.com/xmppo/go-xmpp"

	"github.com/kradalby/nefit-go/types"
)

// fakeTransport stands in for the XMPP stream. Values pushed into in are
// returned by Recv; an error is sticky, as it is for the xml.Decoder behind
// the real stream.
type fakeTransport struct {
	in     chan any
	sent   chan xmpp.Chat
	closed chan struct{}
	once   sync.Once
	recvs  atomic.Int64

	mu  sync.Mutex
	err error
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		in:     make(chan any, 16),
		sent:   make(chan xmpp.Chat, 16),
		closed: make(chan struct{}),
	}
}

func (f *fakeTransport) Recv() (any, error) {
	f.recvs.Add(1)

	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}

	var v any
	select {
	case v = <-f.in:
	case <-f.closed:
		v = io.EOF
	}

	if err, ok := v.(error); ok {
		f.mu.Lock()
		f.err = err
		f.mu.Unlock()
		return nil, err
	}
	return v, nil
}

func (f *fakeTransport) Send(chat xmpp.Chat) (int, error) {
	select {
	case <-f.closed:
		return 0, net.ErrClosed
	case f.sent <- chat:
		return len(chat.Text), nil
	}
}

func (f *fakeTransport) SendPresence(xmpp.Presence) (int, error) { return 0, nil }

func (f *fakeTransport) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// request waits for the client to send a request and returns its first line.
func (f *fakeTransport) request(t *testing.T) string {
	t.Helper()
	select {
	case chat := <-f.sent:
		line, _, _ := strings.Cut(chat.Text, "\r")
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("no request sent")
		return ""
	}
}

type harness struct {
	c     *Client
	dials chan *fakeTransport
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()

	cfg.SerialNumber = "123456789"
	cfg.AccessKey = "abcdefghijklmnop"
	cfg.Password = "secret"

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.SetLogger(slog.New(slog.DiscardHandler))

	h := &harness{c: c, dials: make(chan *fakeTransport, 4)}
	c.dial = func(context.Context) (transport, error) {
		f := newFakeTransport()
		h.dials <- f
		return f, nil
	}
	t.Cleanup(func() { _ = c.Close() })

	return h
}

func (h *harness) connect(t *testing.T) *fakeTransport {
	t.Helper()
	if err := h.c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	return wait(t, h.dials)
}

// reply builds a gateway answer for uri: encrypted JSON carrying the
// resource path as its id.
func (h *harness) reply(t *testing.T, uri string, value any) xmpp.Chat {
	t.Helper()
	body, err := json.Marshal(map[string]any{"id": uri, "value": value})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := h.c.encryptor.Encrypt(string(body))
	if err != nil {
		t.Fatal(err)
	}
	return xmpp.Chat{
		Type: "chat",
		Text: "HTTP/1.1 200 OK\nContent-Type: application/json\n\n" + enc,
	}
}

type getResult struct {
	v   any
	err error
}

func (h *harness) get(ctx context.Context, uri string) <-chan getResult {
	ch := make(chan getResult, 1)
	go func() {
		v, err := h.c.Get(ctx, uri)
		ch <- getResult{v, err}
	}()
	return ch
}

func valueOf(t *testing.T, r getResult) any {
	t.Helper()
	if r.err != nil {
		t.Fatalf("Get: %v", r.err)
	}
	m, ok := r.v.(map[string]any)
	if !ok {
		t.Fatalf("Get returned %T, want map", r.v)
	}
	return m["value"]
}

func wait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}

func TestGetRoundTrip(t *testing.T) {
	h := newHarness(t, Config{})
	f := h.connect(t)

	res := h.get(t.Context(), types.URIOutdoorTemp)
	if got, want := f.request(t), "GET "+types.URIOutdoorTemp+" HTTP/1.1"; got != want {
		t.Fatalf("request = %q, want %q", got, want)
	}
	f.in <- h.reply(t, types.URIOutdoorTemp, 7.5)

	if got := valueOf(t, wait(t, res)); got != 7.5 {
		t.Errorf("value = %v, want 7.5", got)
	}
}

func TestCloseTwice(t *testing.T) {
	h := newHarness(t, Config{})
	h.connect(t)

	if err := h.c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseWaitsForPushHandlers(t *testing.T) {
	h := newHarness(t, Config{})
	started := make(chan string, 1)
	release := make(chan struct{})
	h.c.Subscribe(func(uri string, _ any) {
		started <- uri
		<-release
	})
	f := h.connect(t)

	f.in <- h.reply(t, types.URIStatus, "push")
	if got := wait(t, started); got != types.URIStatus {
		t.Fatalf("push uri = %q, want %q", got, types.URIStatus)
	}

	closed := make(chan error, 1)
	go func() { closed <- h.c.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned while a push handler was running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if err := wait(t, closed); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionLossClosesDone(t *testing.T) {
	h := newHarness(t, Config{})
	f := h.connect(t)

	done := h.c.Done()
	select {
	case <-done:
		t.Fatal("Done closed while connected")
	default:
	}
	if !h.c.IsConnected() {
		t.Fatal("not connected after Connect")
	}

	f.in <- io.ErrUnexpectedEOF
	wait(t, done)

	if h.c.IsConnected() {
		t.Error("IsConnected after connection loss")
	}
	select {
	case <-f.closed:
	case <-time.After(5 * time.Second):
		t.Error("dead transport not closed")
	}
	// The stream error is sticky; reading again would only spin.
	time.Sleep(300 * time.Millisecond)
	if n := f.recvs.Load(); n != 1 {
		t.Errorf("Recv called %d times after loss, want 1", n)
	}
}

func TestReconnectAfterLoss(t *testing.T) {
	h := newHarness(t, Config{})
	f1 := h.connect(t)
	f1.in <- io.ErrUnexpectedEOF
	wait(t, h.c.Done())

	f2 := h.connect(t)
	if !h.c.IsConnected() {
		t.Fatal("not connected after reconnect")
	}
	select {
	case <-h.c.Done():
		t.Fatal("Done closed on fresh connection")
	default:
	}

	res := h.get(t.Context(), types.URIOutdoorTemp)
	f2.request(t)
	f2.in <- h.reply(t, types.URIOutdoorTemp, 3.0)
	if got := valueOf(t, wait(t, res)); got != 3.0 {
		t.Errorf("value = %v, want 3", got)
	}
}

func TestConnectWhileConnectedKeepsSession(t *testing.T) {
	h := newHarness(t, Config{})
	f := h.connect(t)

	if err := h.c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.dials:
		t.Fatal("Connect dialed a second session over a live one")
	default:
	}

	res := h.get(t.Context(), types.URIOutdoorTemp)
	f.request(t)
	f.in <- h.reply(t, types.URIOutdoorTemp, 1.0)
	valueOf(t, wait(t, res))
}

func TestConnectAfterClose(t *testing.T) {
	h := newHarness(t, Config{})
	_ = h.c.Close()

	if err := h.c.Connect(t.Context()); err == nil {
		t.Fatal("Connect after Close succeeded")
	}
	select {
	case <-h.c.Done():
	default:
		t.Error("Done open after Close")
	}
}

func TestConnectionLossFailsPendingRequest(t *testing.T) {
	h := newHarness(t, Config{RetryTimeout: time.Minute})
	f := h.connect(t)

	res := h.get(t.Context(), types.URIStatus)
	f.request(t)
	f.in <- io.ErrUnexpectedEOF

	if r := wait(t, res); r.err == nil {
		t.Fatal("Get succeeded on a lost connection")
	}
}

func rawReply(text string) xmpp.Chat {
	return xmpp.Chat{Type: "chat", Text: text}
}

func TestReplyForOtherResourceIsNotTheAnswer(t *testing.T) {
	h := newHarness(t, Config{})
	pushes := make(chan string, 4)
	h.c.Subscribe(func(uri string, _ any) { pushes <- uri })
	f := h.connect(t)

	res := h.get(t.Context(), types.URIOutdoorTemp)
	f.request(t)
	f.in <- h.reply(t, types.URIStatus, "status")
	f.in <- h.reply(t, types.URIOutdoorTemp, 7.5)

	if got := valueOf(t, wait(t, res)); got != 7.5 {
		t.Errorf("value = %v, want 7.5", got)
	}
	if got := wait(t, pushes); got != types.URIStatus {
		t.Errorf("push uri = %q, want %q", got, types.URIStatus)
	}
}

func TestLateReplyIsNotAPush(t *testing.T) {
	h := newHarness(t, Config{RetryTimeout: 200 * time.Millisecond, MaxRetries: 1})
	pushes := make(chan any, 8)
	h.c.Subscribe(func(_ string, data any) {
		pushes <- data.(map[string]any)["value"]
	})
	f1 := h.connect(t)

	res := h.get(t.Context(), types.URIStatus)
	f1.request(t)
	f2 := wait(t, h.dials)
	f2.request(t)
	if r := wait(t, res); r.err == nil {
		t.Fatal("Get succeeded without a reply")
	}

	// Both attempts are answered after their callers gave up.
	f1.in <- h.reply(t, types.URIStatus, "late")
	f2.in <- h.reply(t, types.URIStatus, "late")

	// Get can return before the worker retires the session.
	wait(t, h.c.Done())
	f3 := h.connect(t)
	f3.in <- h.reply(t, types.URIStatus, "push")
	if got := wait(t, pushes); got != "push" {
		t.Errorf("push = %v, want the real push", got)
	}
	_ = h.c.Close()
	if n := len(pushes); n != 0 {
		t.Errorf("%d late replies dispatched as pushes", n)
	}
}

func TestRetryIgnoresLateReply(t *testing.T) {
	h := newHarness(t, Config{RetryTimeout: 200 * time.Millisecond, MaxRetries: 1})
	f1 := h.connect(t)

	res := h.get(t.Context(), types.URIStatus)
	f1.request(t)

	// The retry may go out on a fresh session.
	f2 := f1
	select {
	case <-f1.sent:
	case f2 = <-h.dials:
		f2.request(t)
	case <-time.After(5 * time.Second):
		t.Fatal("no retry sent")
	}

	// The first attempt's reply overtakes the retry's; both name the
	// resource, and nothing else tells them apart.
	f1.in <- h.reply(t, types.URIStatus, "stale")
	f2.in <- h.reply(t, types.URIStatus, "fresh")

	if got := valueOf(t, wait(t, res)); got != "fresh" {
		t.Errorf("value = %v, want fresh", got)
	}
}

func TestReplyIDOmitsQuery(t *testing.T) {
	h := newHarness(t, Config{})
	f := h.connect(t)

	res := h.get(t.Context(), types.URIGasUsage+"?page=1")
	f.request(t)
	f.in <- h.reply(t, types.URIGasUsage, "page one")

	if got := valueOf(t, wait(t, res)); got != "page one" {
		t.Errorf("value = %v, want page one", got)
	}
}

func TestBodilessSuccessDoesNotAnswerGet(t *testing.T) {
	h := newHarness(t, Config{})
	f := h.connect(t)

	res := h.get(t.Context(), types.URIStatus)
	f.request(t)
	f.in <- rawReply("HTTP/1.1 204 No Content\n\n") // a late PUT ack
	f.in <- h.reply(t, types.URIStatus, "ok")

	if got := valueOf(t, wait(t, res)); got != "ok" {
		t.Errorf("value = %v, want ok", got)
	}
}

func TestGetErrorStatus(t *testing.T) {
	h := newHarness(t, Config{})
	f := h.connect(t)

	res := h.get(t.Context(), types.URIStatus)
	f.request(t)
	f.in <- rawReply("HTTP/1.1 404 Not Found\n\n")

	r := wait(t, res)
	if r.err == nil || !strings.Contains(r.err.Error(), "404") {
		t.Errorf("Get error = %v, want HTTP 404", r.err)
	}
}

func TestPutRoundTrip(t *testing.T) {
	h := newHarness(t, Config{})
	pushes := make(chan string, 4)
	h.c.Subscribe(func(uri string, _ any) { pushes <- uri })
	f := h.connect(t)

	done := make(chan error, 1)
	go func() { done <- h.c.Put(t.Context(), types.URIUserMode, map[string]string{"value": "clock"}) }()
	if got, want := f.request(t), "PUT "+types.URIUserMode+" HTTP/1.1"; got != want {
		t.Fatalf("request = %q, want %q", got, want)
	}

	// A push naming the same resource is not the PUT's ack.
	f.in <- h.reply(t, types.URIUserMode, "clock")
	if got := wait(t, pushes); got != types.URIUserMode {
		t.Fatalf("push uri = %q, want %q", got, types.URIUserMode)
	}
	select {
	case err := <-done:
		t.Fatalf("Put returned %v before its ack", err)
	default:
	}

	f.in <- rawReply("HTTP/1.1 204 No Content\n\n")

	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}
