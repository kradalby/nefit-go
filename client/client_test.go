package client

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	wire "github.com/kradalby/nefit-go/internal/xmpp"
	"github.com/kradalby/nefit-go/protocol"
	"github.com/kradalby/nefit-go/types"
)

// fakeTransport stands in for the cloud stream. Values pushed into in are
// returned by Recv; an error is sticky, as it is for the xml.Decoder behind
// the real stream.
type fakeTransport struct {
	in     chan any
	sent   chan string
	closed chan struct{}
	once   sync.Once
	recvs  atomic.Int64

	// stall blocks sends until Close, as a peer that stopped reading would.
	stall atomic.Bool
	pings atomic.Int32
	// failNext delivers the next send, then reports it failed.
	failNext atomic.Bool

	mu  sync.Mutex
	err error
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		in:     make(chan any, 16),
		sent:   make(chan string, 16),
		closed: make(chan struct{}),
	}
}

func (f *fakeTransport) Recv() (inbound, error) {
	f.recvs.Add(1)

	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return inbound{}, err
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
		return inbound{}, err
	}
	return v.(inbound), nil
}

func (f *fakeTransport) Send(ctx context.Context, body string, p *pending) error {
	p.markSent()
	if f.stall.Load() {
		// As the socket writer does: cancellation mid-write retires the stream.
		select {
		case <-f.closed:
			return net.ErrClosed
		case <-ctx.Done():
			_ = f.Close()
			return ctx.Err()
		}
	}
	select {
	case <-f.closed:
		return net.ErrClosed
	case f.sent <- body:
	}
	if f.failNext.Swap(false) {
		return syscall.EPIPE
	}
	return nil
}

func (f *fakeTransport) Ping() error { f.pings.Add(1); return nil }

func (f *fakeTransport) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// request waits for the client to send a request and returns its first line.
func (f *fakeTransport) request(t *testing.T) string {
	t.Helper()
	select {
	case body := <-f.sent:
		line, _, _ := strings.Cut(body, "\r")
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("no request sent")
		return ""
	}
}

// testConfig fills test credentials and an hourly ping into what cfg leaves unset.
func testConfig(cfg Config) Config {
	cfg.SerialNumber = cmp.Or(cfg.SerialNumber, "123456789")
	cfg.AccessKey = cmp.Or(cfg.AccessKey, "abcdefghijklmnop")
	cfg.Password = cmp.Or(cfg.Password, "secret")
	cfg.PingInterval = cmp.Or(cfg.PingInterval, time.Hour)
	return cfg
}

// testClient discards c's logs and closes it with the test.
func testClient(t *testing.T, c *Client, err error) *Client {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	c.SetLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := NewClient(testConfig(cfg))
	return testClient(t, c, err)
}

type harness struct {
	c     *Client
	dials chan *fakeTransport
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()

	c := newTestClient(t, cfg)
	h := &harness{c: c, dials: make(chan *fakeTransport, 4)}
	c.dial = func(ctx context.Context) (transport, error) {
		f := newFakeTransport()
		select {
		case h.dials <- f:
			return f, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
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
func (h *harness) reply(t *testing.T, uri string, value any) inbound {
	t.Helper()
	body, err := json.Marshal(map[string]any{"id": uri, "value": value})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := h.c.encryptor.Encrypt(string(body))
	if err != nil {
		t.Fatal(err)
	}
	return inbound{text: "HTTP/1.1 200 OK\nContent-Type: application/json\n\n" + enc}
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
	t.Parallel()
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

func TestCloseWaitsForPushHandlers(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestConnectWhileConnectedKeepsSession(t *testing.T) {
	t.Parallel()
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

func TestConnectionLossFailsPendingRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{RetryTimeout: time.Minute})
	f := h.connect(t)

	res := h.get(t.Context(), types.URIStatus)
	f.request(t)
	f.in <- io.ErrUnexpectedEOF

	if r := wait(t, res); r.err == nil {
		t.Fatal("Get succeeded on a lost connection")
	}
}

func rawReply(text string) inbound {
	return inbound{text: text}
}

func TestReplyForOtherResourceIsNotTheAnswer(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	h := newHarness(t, Config{RetryTimeout: 200 * time.Millisecond})
	pushes := make(chan any, 8)
	h.c.Subscribe(func(_ string, data any) {
		pushes <- data.(map[string]any)["value"]
	})
	f1 := h.connect(t)

	res := h.get(t.Context(), types.URIStatus)
	f1.request(t)
	if r := wait(t, res); r.err == nil {
		t.Fatal("Get succeeded without a reply")
	}

	// Answered after the caller gave up.
	f1.in <- h.reply(t, types.URIStatus, "late")

	wait(t, h.c.Done())
	f2 := h.connect(t)
	f2.in <- h.reply(t, types.URIStatus, "push")
	if got := wait(t, pushes); got != "push" {
		t.Errorf("push = %v, want the real push", got)
	}
	_ = h.c.Close()
	if n := len(pushes); n != 0 {
		t.Errorf("%d late replies dispatched as pushes", n)
	}
}

func TestNextRequestIgnoresLateReply(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{RetryTimeout: 5 * time.Second})
	f1 := h.connect(t)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if r := wait(t, h.get(ctx, types.URIStatus)); r.err == nil {
		t.Fatal("Get succeeded without a reply")
	}
	f1.request(t)

	// The first request's reply overtakes the second's; both name the
	// resource, and nothing else tells them apart.
	res := h.get(t.Context(), types.URIStatus)
	f1.in <- h.reply(t, types.URIStatus, "stale")
	f2 := wait(t, h.dials)
	f2.request(t)
	f2.in <- h.reply(t, types.URIStatus, "fresh")

	if got := valueOf(t, wait(t, res)); got != "fresh" {
		t.Errorf("value = %v, want fresh", got)
	}
}

func TestFailedSendRetiresSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	f1 := h.connect(t)

	// The write errors after the request reached the peer.
	f1.failNext.Store(true)
	if r := wait(t, h.get(t.Context(), types.URIStatus)); r.err == nil {
		t.Fatal("Get succeeded on a failed send")
	}
	f1.request(t)

	res := h.get(t.Context(), types.URIStatus)
	f1.in <- h.reply(t, types.URIStatus, "stale")
	f2 := wait(t, h.dials)
	f2.request(t)
	f2.in <- h.reply(t, types.URIStatus, "fresh")

	if got := valueOf(t, wait(t, res)); got != "fresh" {
		t.Errorf("value = %v, want fresh", got)
	}
}

func TestStalledSendDoesNotBlockLaterRequests(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	f1 := h.connect(t)

	f1.stall.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if r := wait(t, h.get(ctx, types.URIStatus)); r.err == nil {
		t.Fatal("Get succeeded with a stalled write")
	}

	res := h.get(t.Context(), types.URIOutdoorTemp)
	f2 := wait(t, h.dials)
	f2.request(t)
	f2.in <- h.reply(t, types.URIOutdoorTemp, 1.0)
	valueOf(t, wait(t, res))
}

func TestRequestReconnectsAfterLoss(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	f1 := h.connect(t)
	f1.in <- io.ErrUnexpectedEOF
	wait(t, h.c.Done())

	res := h.get(t.Context(), types.URIOutdoorTemp)
	f2 := wait(t, h.dials)
	f2.request(t)
	f2.in <- h.reply(t, types.URIOutdoorTemp, 2.0)
	if got := valueOf(t, wait(t, res)); got != 2.0 {
		t.Errorf("value = %v, want 2", got)
	}
}

func TestUnansweredRequestIsNotRetried(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{RetryTimeout: 100 * time.Millisecond, MaxRetries: 3})
	f := h.connect(t)

	r := wait(t, h.get(t.Context(), types.URIStatus))
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("Get error = %v, want deadline exceeded", r.err)
	}
	f.request(t)

	// Every retry would cost a fresh login, and fail the same way while the
	// gateway is silent.
	select {
	case <-h.dials:
		t.Error("unanswered request retried on a new session")
	default:
	}
}

func TestConcurrentConnectsShareDial(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	dialing := make(chan struct{}, 4)
	release := make(chan struct{})
	dial := h.c.dial
	h.c.dial = func(ctx context.Context) (transport, error) {
		dialing <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return dial(ctx)
	}

	errs := make(chan error, 2)
	go func() { errs <- h.c.Connect(t.Context()) }()
	wait(t, dialing)
	// A request needing a session meanwhile joins the same dial.
	res := h.get(t.Context(), types.URIOutdoorTemp)
	go func() { errs <- h.c.Connect(t.Context()) }()
	select {
	case <-dialing:
		t.Fatal("second dial while one was in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	f := wait(t, h.dials)
	for range 2 {
		if err := wait(t, errs); err != nil {
			t.Fatal(err)
		}
	}
	f.request(t)
	f.in <- h.reply(t, types.URIOutdoorTemp, 4.0)
	valueOf(t, wait(t, res))
	if len(dialing) != 0 {
		t.Error("dialed more than once")
	}
}

func TestPutRoundTrip(t *testing.T) {
	t.Parallel()
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

func TestQueueSkipsRequestExpiredWhileQueued(t *testing.T) {
	t.Parallel()
	q := NewRequestQueue()
	defer q.Close()

	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = q.Submit(context.Background(), func() (any, error) {
			close(started)
			<-release
			return nil, nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	var ran atomic.Bool
	submitted := make(chan error, 1)
	go func() {
		_, err := q.Submit(ctx, func() (any, error) {
			ran.Store(true)
			return nil, nil
		})
		submitted <- err
	}()
	waitQueued(q)
	cancel()
	if err := wait(t, submitted); !errors.Is(err, context.Canceled) {
		t.Fatalf("Submit = %v, want context.Canceled", err)
	}

	close(release)
	// FIFO: once this runs, the expired request has been dequeued.
	if _, err := q.Submit(context.Background(), func() (any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Error("request expired while queued was executed")
	}
}

// waitQueued blocks until a request is queued behind the running one.
func waitQueued(q *RequestQueue) {
	for len(q.requestCh) == 0 {
		runtime.Gosched()
	}
}

func TestQueueSubmitReturnsWhenClosed(t *testing.T) {
	t.Parallel()
	q := NewRequestQueue()

	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	go func() {
		_, _ = q.Submit(context.Background(), func() (any, error) {
			close(started)
			<-release
			return nil, nil
		})
	}()
	<-started

	queued := make(chan error, 1)
	go func() {
		_, err := q.Submit(context.Background(), func() (any, error) { return nil, nil })
		queued <- err
	}()
	waitQueued(q)

	go q.Close()
	if err := wait(t, queued); err == nil {
		t.Error("request queued on a closed queue succeeded")
	}
}

func TestQueueReportsOutcomeOfStartedRequest(t *testing.T) {
	t.Parallel()
	q := NewRequestQueue()
	defer q.Close()

	errSent := errors.New("sent")
	ctx, cancel := context.WithCancel(t.Context())
	got := make(chan error, 1)
	go func() {
		_, err := q.Submit(ctx, func() (any, error) {
			cancel()
			// Winding down after cancellation, as a request aborting its write does.
			time.Sleep(50 * time.Millisecond)
			return nil, errSent
		})
		got <- err
	}()

	if err := wait(t, got); !errors.Is(err, errSent) {
		t.Errorf("Submit = %v, want the request's own error", err)
	}
}

// heldWriteReturn holds the first Write's return after its bytes reached the
// peer, until resumed or closed.
type heldWriteReturn struct {
	net.Conn
	first   sync.Once
	onClose sync.Once
	written chan struct{}
	resume  chan struct{}
	closed  chan struct{}
}

func newHeldWriteReturn(c net.Conn) *heldWriteReturn {
	return &heldWriteReturn{Conn: c, written: make(chan struct{}), resume: make(chan struct{}), closed: make(chan struct{})}
}

func (c *heldWriteReturn) Write(p []byte) (int, error) {
	first := false
	c.first.Do(func() { first = true })
	n, err := c.Conn.Write(p)
	if first {
		close(c.written)
		select {
		case <-c.resume:
		case <-c.closed:
		}
	}
	return n, err
}

func (c *heldWriteReturn) Close() error {
	c.onClose.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func replayClient(t *testing.T) *Client {
	t.Helper()
	c := newTestClient(t, Config{RetryTimeout: time.Second, MaxRetries: 2})
	return c
}

// nextRequest reads device-bound stanzas until one carries want.
func nextRequest(t *testing.T, r *wire.Reader, want string) {
	t.Helper()
	for {
		f, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if f.Element != nil && strings.Contains(f.Element.Child(wire.ClientNS, "body").Text(), want) {
			return
		}
	}
}

// deviceReader reads device-bound stanzas, which inherit the stream header.
func deviceReader(t *testing.T, device net.Conn) *wire.Reader {
	t.Helper()
	_ = device.SetDeadline(time.Now().Add(5 * time.Second))
	r := wire.NewReader(io.MultiReader(strings.NewReader(`<stream:stream xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams">`), device))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSubmitSessionEndDistinguishesUnsentAndWritten(t *testing.T) {
	t.Parallel()
	c := replayClient(t)
	t.Run("queued", func(t *testing.T) {
		socket, device := net.Pipe()
		t.Cleanup(func() { _ = device.Close() })
		tr := testBridge(t, c, socket, nil, LocalOptions{Mode: ModeOffline, RequestTimeout: 5 * time.Second, ReconnectInterval: time.Hour})
		p := new(pending)
		result := make(chan error, 1)
		go func() {
			result <- tr.submit(t.Context(), wire.Message{Body: wire.Body{Text: "PUT /queued HTTP/1.1\r\n\r\n"}}, p)
		}()
		request := wait(t, tr.requests)
		tr.abort()
		err := wait(t, result)
		var unsent *unsentError
		if !errors.As(err, &unsent) || !errors.Is(err, errSessionEnded) || !retryable(err) {
			t.Fatalf("queued session end = %v, want retryable unsent session end", err)
		}
		if p.sent.Load() || request.state.Load() != abandoned {
			t.Fatal("queued request admitted after session end")
		}
	})
	t.Run("written", func(t *testing.T) {
		socket, device := net.Pipe()
		t.Cleanup(func() { _ = device.Close() })
		gate := newHeldWriteReturn(socket)
		tr := testBridge(t, c, gate, nil, LocalOptions{Mode: ModeOffline, RequestTimeout: 5 * time.Second, ReconnectInterval: time.Hour})
		go func() { _, _ = tr.Recv() }()
		p := new(pending)
		result := make(chan error, 1)
		go func() {
			result <- tr.submit(t.Context(), wire.Message{
				From: c.config.JID() + "/" + localResource, To: c.config.ResourceJID(),
				Type: "chat", Body: wire.Body{Text: "PUT /written HTTP/1.1\r\n\r\n"},
			}, p)
		}()
		nextRequest(t, deviceReader(t, device), "PUT /written ")
		wait(t, gate.written)
		if !p.sent.Load() {
			t.Fatal("PUT written before it was marked sent")
		}
		tr.abort()
		err := wait(t, result)
		var unsent *unsentError
		if errors.As(err, &unsent) || retryable(err) {
			t.Fatalf("written session end = %v, must not permit replay", err)
		}
	})
}

func TestWrittenPutIsNotReplayedAfterSessionEnd(t *testing.T) {
	t.Parallel()
	c := replayClient(t)
	socket, device := net.Pipe()
	t.Cleanup(func() { _ = device.Close() })
	gate := newHeldWriteReturn(socket)
	tr := testBridge(t, c, gate, nil, LocalOptions{Mode: ModeOffline, RequestTimeout: 5 * time.Second, ReconnectInterval: time.Hour})
	var dials atomic.Int32
	c.dial = func(context.Context) (transport, error) {
		if dials.Add(1) != 1 {
			return nil, errors.New("unexpected reconnect after a written PUT")
		}
		return tr, nil
	}
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- c.Put(t.Context(), "/applied", map[string]int{"value": 1}) }()
	nextRequest(t, deviceReader(t, device), "PUT /applied ")
	wait(t, gate.written)
	tr.abort()
	if err := wait(t, result); err == nil || retryable(err) {
		t.Fatalf("written PUT after session end = %v, want a non-retryable error", err)
	}
	if n := dials.Load(); n != 1 {
		t.Fatalf("written PUT retried through %d sessions", n)
	}
}

func encryptedReply(t *testing.T, c *Client, status, plain string) string {
	t.Helper()
	cipher, err := c.encryptor.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	return "HTTP/1.0 " + status + "\r\nContent-Type: application/json\r\n\r\n" + cipher
}

func TestBridgeAndClientAgreeOnReplies(t *testing.T) {
	t.Parallel()
	c, err := NewClient(Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	tr := &localTransport{config: c.config}
	tr.encryptor = c.encryptor
	local := c.config.JID() + "/" + localResource
	for _, tc := range []struct {
		name, method, uri, to, body string
		want                        bool
	}{
		{"other destination", "GET", "/a", c.config.JID() + "/phone", encryptedReply(t, c, "200 OK", `{"id":"/a"}`), false},
		{"echoed id", "GET", "/a", local, encryptedReply(t, c, "200 OK", `{"id":"/a"}`), true},
		{"echoed id ignores query", "GET", "/a?x=1", local, encryptedReply(t, c, "200 OK", `{"id":"/a"}`), true},
		{"other echoed id", "GET", "/a", local, encryptedReply(t, c, "200 OK", `{"id":"/b"}`), false},
		{"GET without body", "GET", "/a", local, "HTTP/1.0 200 OK\r\n\r\n", false},
		{"GET failure", "GET", "/a", local, "HTTP/1.0 404 Not Found\r\n\r\n", true},
		{"PUT ack", "PUT", "/a", local, "HTTP/1.0 204 No Content\r\n\r\n", true},
		{"PUT ack with body", "PUT", "/a", local, encryptedReply(t, c, "200 OK", `{"value":1}`), true},
		{"push during PUT", "PUT", "/a", local, encryptedReply(t, c, "200 OK", `{"id":"/push"}`), false},
		{"not HTTP", "PUT", "/a", local, "opaque", false},
		{"HEAD non-HTTP push", "HEAD", "/a", local, "opaque", false},
		{"HEAD other endpoint push", "HEAD", "/a", local, encryptedReply(t, c, "200 OK", `{"id":"/push"}`), false},
		{"HEAD reply", "HEAD", "/a", local, "HTTP/1.0 200 OK\r\n\r\n", true},
		{"DELETE non-HTTP push", "DELETE", "/a", local, "opaque", false},
		{"POST other endpoint push", "POST", "/a", local, encryptedReply(t, c, "200 OK", `{"id":"/push"}`), false},
		{"POST ack", "POST", "/a", local, "HTTP/1.0 204 No Content\r\n\r\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			active := &activeRequest{request: bridgeRequest{message: wire.Message{From: local}}, method: tc.method, uri: tc.uri}
			m := &wire.Message{To: tc.to, Body: wire.Body{Text: tc.body}}
			if got := tr.requestMatches(active, *m); got != tc.want {
				t.Fatalf("bridge match = %v, want %v", got, tc.want)
			}
			if tc.to != local {
				return
			}
			resp, err := protocol.ParseHTTPResponse(tc.body)
			if err != nil {
				return
			}
			p := &pending{uri: tc.uri, get: tc.method == "GET"}
			if got := p.answeredBy(decodeReply(c.encryptor, resp)); got != tc.want {
				t.Fatalf("client match = %v, bridge %v", got, tc.want)
			}
		})
	}
}

func TestUnsentRequestCannotTakeReply(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, Config{})
	cn := &conn{}
	p := &pending{reply: make(chan reply, 1)}
	cn.begin(p)
	r := reply{resp: &protocol.HTTPResponse{StatusCode: 204}}
	if cn.match(r, nil) != nil {
		t.Fatal("queued request matched a reply before it was sent")
	}
	c.route(cn, r, p)
	if p.replied.Load() {
		t.Fatal("queued request retired its reply window before it was sent")
	}
	p.markSent()
	c.route(cn, r, p)
	if !p.replied.Load() || wait(t, p.reply).resp != r.resp {
		t.Fatal("sent request did not receive its reply")
	}
}

func TestUnsentErrorsRetryOnlyWhenSessionEnded(t *testing.T) {
	t.Parallel()
	if !retryable(fmt.Errorf("request not sent: %w", &unsentError{errSessionEnded})) {
		t.Fatal("request cut by session end not retried")
	}
	if retryable(fmt.Errorf("request not sent: %w", &unsentError{errors.New("firmware update write blocked by policy")})) {
		t.Fatal("policy rejection retried")
	}
	if retryable(fmt.Errorf("failed send: %w", errSessionEnded)) {
		t.Fatal("session loss retried without proof that request was unsent")
	}
	for _, cause := range []error{errSessionEnded, context.DeadlineExceeded} {
		if retryable(errors.Join(errUnanswered, cause)) {
			t.Fatal("possibly sent request retried", cause)
		}
	}
}

func TestInvalidInputNeverSent(t *testing.T) {
	t.Parallel()
	c, err := NewClient(Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.dial = func(context.Context) (transport, error) { t.Fatal("invalid request dialled"); return nil, nil }
	if _, err := c.Get(t.Context(), "/gateway/update HTTP/1.1"); !errors.Is(err, protocol.ErrInvalidURI) {
		t.Fatal(err)
	}
	if err := c.Put(t.Context(), "/x\r\nPUT /gateway/update", 1); !errors.Is(err, protocol.ErrInvalidURI) {
		t.Fatal(err)
	}
	for _, temperature := range []float64{MinTemperature - 0.1, MaxTemperature + 0.1} {
		if err := c.SetTemperature(t.Context(), temperature); !errors.Is(err, ErrInvalidValue) {
			t.Fatal(temperature, err)
		}
	}
	if err := c.SetUserMode(t.Context(), "off"); !errors.Is(err, ErrInvalidValue) {
		t.Fatal(err)
	}
	// Before sending, an expired attempt is retried while time remains.
	if !retryable(context.DeadlineExceeded) {
		t.Fatal("unsent deadline not retryable")
	}
}

func TestSetHotWaterUsesModeEndpoint(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	f := h.connect(t)
	done := make(chan error, 1)
	go func() { done <- h.c.SetHotWaterSupply(t.Context(), true) }()
	if got := f.request(t); !strings.HasPrefix(got, "GET "+types.URIStatus+" ") {
		t.Fatal(got)
	}
	f.in <- h.reply(t, types.URIStatus, map[string]any{"UMD": "clock"})
	var body string
	select {
	case body = <-f.sent:
	case err := <-done:
		t.Fatal(err)
	}
	if line, _, _ := strings.Cut(body, "\r"); line != "PUT "+types.URIHotWaterClockMode+" HTTP/1.1" {
		t.Fatal(line)
	}
	plain, err := h.c.encryptor.DecryptAndStrip(body[strings.LastIndex(body, "\n")+1:])
	if err != nil || plain != `{"value":"on"}` {
		t.Fatal(plain, err)
	}
	f.in <- rawReply("HTTP/1.1 204 No Content\n\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestSpawnRefusedAfterClose(t *testing.T) {
	t.Parallel()
	socket, peer := net.Pipe()
	defer peer.Close() //nolint:errcheck
	tr := newLocalTransport(socket, Config{}, LocalOptions{})
	_ = tr.Close()
	if tr.spawn(func() {}) {
		t.Fatal("worker registered after Close")
	}
}

func TestPushHandlersAreBounded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	release := make(chan struct{})
	var running atomic.Int32
	h.c.Subscribe(func(string, any) {
		running.Add(1)
		<-release
	})
	f := h.connect(t)
	for range maxPushHandlers + 10 {
		f.in <- h.reply(t, types.URIStatus, "push")
	}
	deadline := time.Now().Add(2 * time.Second)
	for running.Load() < maxPushHandlers && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Let the receive worker drain the rest, which must be dropped, not queued.
	for len(f.in) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if n := running.Load(); n != maxPushHandlers || len(f.in) > 0 {
		t.Fatalf("%d push handlers running, %d pushes waiting; want the bound %d and none waiting", n, len(f.in), maxPushHandlers)
	}
	close(release)
	// Finished handlers free their slots for later pushes.
	for running.Load() <= maxPushHandlers && time.Now().Before(deadline.Add(time.Second)) {
		f.in <- h.reply(t, types.URIStatus, "later")
		time.Sleep(10 * time.Millisecond)
	}
	if running.Load() <= maxPushHandlers {
		t.Fatal("push slots not released")
	}
}

func TestTransportClassificationReachesCaller(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	pushes := make(chan string, 4)
	h.c.Subscribe(func(uri string, _ any) { pushes <- uri })
	f := h.connect(t)
	res := h.get(t.Context(), types.URIStatus)
	f.request(t)
	f.in <- inbound{error: true, push: true, text: "other session"}
	app := h.reply(t, types.URIStatus, "app")
	app.push = true
	f.in <- app
	if got := wait(t, pushes); got != types.URIStatus {
		t.Fatal(got)
	}
	f.in <- inbound{error: true, text: "recipient-unavailable"}
	if r := wait(t, res); r.err == nil || !strings.Contains(r.err.Error(), "recipient-unavailable") {
		t.Fatal(r.err)
	}
}

func TestUnmatchedAckIsNotAPush(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	pushes := make(chan string, 4)
	h.c.Subscribe(func(uri string, _ any) { pushes <- uri })
	f := h.connect(t)
	res := h.get(t.Context(), types.URIStatus)
	f.request(t)
	f.in <- rawReply("HTTP/1.1 204 No Content\n\n")
	f.in <- h.reply(t, types.URIStatus, "ok")
	if got := valueOf(t, wait(t, res)); got != "ok" {
		t.Fatal(got)
	}
	_ = h.c.Close()
	if len(pushes) != 0 {
		t.Fatal("unmatched ack dispatched as a push")
	}
}

// lagging has passed its deadline before its timer fired, as every deadline
// context briefly has.
type lagging struct {
	context.Context
	end time.Time
}

func (l lagging) Deadline() (time.Time, bool) { return l.end, true }

func TestWriteAtElapsedDeadlineKeepsSession(t *testing.T) {
	t.Parallel()
	socket, peer := net.Pipe()
	t.Cleanup(func() { _ = socket.Close(); _ = peer.Close() })
	var aborted atomic.Bool
	w := newSocketWriter(socket, make(chan struct{}), func() { aborted.Store(true) }, time.Second)
	var state atomic.Int32
	err := w.write(lagging{t.Context(), time.Now().Add(-time.Millisecond)}, []byte("<message/>"), &state, nil)
	var unsent *unsentError
	if !errors.As(err, &unsent) || !errors.Is(err, context.DeadlineExceeded) || aborted.Load() || state.Load() != queued {
		t.Fatalf("write at an elapsed deadline = %v, aborted = %v", err, aborted.Load())
	}
}

// gatedSend holds Send's return, so a reply and the end of its wait are both
// ready when roundTrip starts waiting.
type gatedSend struct {
	*fakeTransport
	gate chan struct{}
}

func (g *gatedSend) Send(ctx context.Context, body string, p *pending) error {
	if err := g.fakeTransport.Send(ctx, body, p); err != nil {
		return err
	}
	<-g.gate
	return nil
}

func TestRoutedReplyWinsOverConcurrentEnd(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"stream end", "caller cancel"} {
		t.Run(end, func(t *testing.T) {
			// select picks a ready case at random; repeat so the wrong pick shows.
			for range 20 {
				c, err := NewClient(Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret", RetryTimeout: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				c.SetLogger(slog.New(slog.DiscardHandler))
				g := &gatedSend{fakeTransport: newFakeTransport(), gate: make(chan struct{})}
				c.dial = func(context.Context) (transport, error) { return g, nil }
				if err := c.Connect(t.Context()); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- c.Put(ctx, "/x", 1) }()
				<-g.sent
				g.in <- rawReply("HTTP/1.1 204 No Content\n\n")
				if end == "stream end" {
					g.in <- io.EOF
					<-g.closed
				} else {
					cn := c.conn.Load()
					// abandon(nil) holds once inflight is cleared: the reply was routed.
					for !cn.abandon(nil) {
						time.Sleep(time.Millisecond)
					}
					cancel()
				}
				close(g.gate)
				if err := wait(t, done); err != nil {
					t.Fatalf("answered write reported failed: %v", err)
				}
				cancel()
				_ = c.Close()
			}
		})
	}
}

func TestRequestsAfterCloseReturnErrClosed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	f := h.connect(t)
	// One request in flight when Close runs, one after.
	inflight := h.get(t.Context(), types.URIStatus)
	f.request(t)
	if err := h.c.Close(); err != nil {
		t.Fatal(err)
	}
	if r := wait(t, inflight); !errors.Is(r.err, ErrClosed) {
		t.Fatal("in-flight request:", r.err)
	}
	if _, err := h.c.Get(t.Context(), types.URIStatus); !errors.Is(err, ErrClosed) {
		t.Fatal("Get after Close:", err)
	}
	if err := h.c.Put(t.Context(), types.URIUserMode, "clock"); !errors.Is(err, ErrClosed) {
		t.Fatal("Put after Close:", err)
	}
	if err := h.c.Connect(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatal("Connect after Close:", err)
	}
	select {
	case <-h.c.Done():
	default:
		t.Error("Done open after Close")
	}
}

func TestDeviceRefusalIsHTTPError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	f := h.connect(t)
	result := h.get(t.Context(), types.URIStatus)
	f.request(t)
	f.in <- rawReply("HTTP/1.1 400 Bad Request\n\n")
	var refused *HTTPError
	if r := wait(t, result); !errors.As(r.err, &refused) || refused.StatusCode != 400 || retryable(r.err) {
		t.Fatal(r.err)
	}
}

func TestSetLoggerNilUsesDefault(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	// Workers log unconditionally; a nil logger would crash one.
	h.c.SetLogger(nil)
	if err := h.c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseEndsPutBackoff(t *testing.T) {
	t.Parallel()
	// No dial completes, so each attempt expires unsent and backs off.
	h := newHarness(t, Config{RetryTimeout: 100 * time.Millisecond, MaxRetries: 3})
	h.c.dial = func(ctx context.Context) (transport, error) { <-ctx.Done(); return nil, ctx.Err() }
	done := make(chan error, 1)
	go func() { done <- h.c.Put(t.Context(), types.URIUserMode, "clock") }()
	// Inside the second backoff, which lasts 200ms.
	time.Sleep(350 * time.Millisecond)
	closed := time.Now()
	go func() { _ = h.c.Close() }()
	if err := wait(t, done); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	// A push handler may be the caller, and Close waits for those.
	if elapsed := time.Since(closed); elapsed > 100*time.Millisecond {
		t.Fatalf("Put returned %v after Close", elapsed)
	}
}

func TestRetriesStopAtMaxRetries(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{RetryTimeout: 20 * time.Millisecond, MaxRetries: 2})
	h.c.dial = func(ctx context.Context) (transport, error) { <-ctx.Done(); return nil, ctx.Err() }
	if _, err := h.c.Get(t.Context(), types.URIStatus); err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatal("GET:", err)
	}
	if err := h.c.Put(t.Context(), types.URIUserMode, "clock"); err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatal("PUT:", err)
	}
}

// unsentSend refuses every request before writing, as an expired caller is.
type unsentSend struct {
	*fakeTransport
	sends chan time.Time
}

func (u *unsentSend) Send(context.Context, string, *pending) error {
	u.sends <- time.Now()
	return &unsentError{context.DeadlineExceeded}
}

func TestPutBackoffDoublesAndHonoursCaller(t *testing.T) {
	t.Parallel()
	refusing := func(cfg Config) (*harness, *unsentSend) {
		h := newHarness(t, cfg)
		u := &unsentSend{fakeTransport: newFakeTransport(), sends: make(chan time.Time, 8)}
		h.c.dial = func(context.Context) (transport, error) { return u, nil }
		return h, u
	}
	h, u := refusing(Config{RetryTimeout: 100 * time.Millisecond, MaxRetries: 3})
	if err := h.c.Put(t.Context(), "/x", 1); err == nil {
		t.Fatal("refused PUT succeeded")
	}
	if len(u.sends) != 4 {
		t.Fatalf("%d attempts, want 4", len(u.sends))
	}
	previous := <-u.sends
	for _, want := range []time.Duration{100, 200, 400} {
		next := <-u.sends
		if gap := next.Sub(previous); gap < want*time.Millisecond*8/10 {
			t.Fatalf("retried after %v, want a %vms backoff", gap, want)
		}
		previous = next
	}
	// A caller that gives up leaves the backoff at once.
	h, _ = refusing(Config{RetryTimeout: time.Second, MaxRetries: 3})
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	if err := h.c.Put(ctx, "/x", 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("cancelled PUT returned after %v", elapsed)
	}
}

// endedSend's transport died before writing anything, but its reader has
// not noticed yet.
type endedSend struct{ *fakeTransport }

func (e *endedSend) Send(context.Context, string, *pending) error {
	return &unsentError{errSessionEnded}
}

func TestSessionEndedBeforeSendRedials(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	next := newFakeTransport()
	var dials atomic.Int32
	h.c.dial = func(context.Context) (transport, error) {
		if dials.Add(1) == 1 {
			return &endedSend{newFakeTransport()}, nil
		}
		return next, nil
	}
	// Retrying on the dead session would use every attempt at once.
	result := h.get(t.Context(), types.URIStatus)
	next.request(t)
	next.in <- h.reply(t, types.URIStatus, "fresh")
	if r := wait(t, result); valueOf(t, r) != "fresh" {
		t.Fatal(r)
	}
}

func TestStrayInputKeepsSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	f := h.connect(t)
	// Neither may crash the reader or take a later request's place.
	f.in <- inbound{text: "not HTTP at all"}
	f.in <- inbound{error: true, text: "stray error"}
	// Both handled before the request exists: replies carry no id, so a
	// stray error arriving later would be taken as its answer.
	for f.recvs.Load() < 3 {
		time.Sleep(time.Millisecond)
	}
	result := h.get(t.Context(), types.URIStatus)
	f.request(t)
	f.in <- h.reply(t, types.URIStatus, "fresh")
	if r := wait(t, result); valueOf(t, r) != "fresh" {
		t.Fatal(r)
	}
}

func TestUndecryptableReplyFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	f := h.connect(t)
	result := h.get(t.Context(), types.URIStatus)
	f.request(t)
	f.in <- inbound{text: "HTTP/1.1 200 OK\nContent-Type: application/json\n\n%%% not base64"}
	if r := wait(t, result); r.err == nil {
		t.Fatal("undecryptable body returned", r.v)
	}
}

func TestPingWorkerKeepsSessionAlive(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{PingInterval: 10 * time.Millisecond})
	f := h.connect(t)
	deadline := time.Now().Add(2 * time.Second)
	for f.pings.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no keepalive pings")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSubscribeAfterPush(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	pushes := make(chan string, 2)
	h.c.Subscribe(func(uri string, _ any) { pushes <- uri })
	f := h.connect(t)
	f.in <- h.reply(t, types.URIStatus, "push")
	wait(t, pushes)
	// A second subscriber must not wait on a lock the dispatch kept.
	subscribed := make(chan struct{})
	go func() { h.c.Subscribe(func(string, any) {}); close(subscribed) }()
	wait(t, subscribed)
}

func TestSocketWriterKeepsAbandonedRequestUnsent(t *testing.T) {
	t.Parallel()
	socket, peer := net.Pipe()
	t.Cleanup(func() { _ = socket.Close(); _ = peer.Close() })
	w := newSocketWriter(socket, make(chan struct{}), func() {}, time.Second)
	var state atomic.Int32
	state.Store(abandoned)
	// Its caller was told it was not sent and may retry: writing it now
	// would replay the write.
	err := w.write(t.Context(), []byte("<message/>"), &state, nil)
	var unsent *unsentError
	if !errors.As(err, &unsent) {
		t.Fatal(err)
	}
}

func TestUpdateServicesWithBlockingAccepted(t *testing.T) {
	t.Parallel()
	newLocalTestClient(t, Config{}, LocalOptions{UpdatePolicy: UpdatesBlock, UpdateServices: []string{"custom_update@host"}})
}

func TestHostIsCanonical(t *testing.T) {
	// The device's login names the domain this way; DNS answers it too.
	if host := (Config{Host: "WA2.Example."}).WithDefaults().Host; host != "wa2.example" {
		t.Fatal(host)
	}
	if host := (Config{Host: "."}).WithDefaults().Host; host != DefaultHost {
		t.Fatal("root domain left no host:", host)
	}
}
