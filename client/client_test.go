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
	return <-h.dials
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
