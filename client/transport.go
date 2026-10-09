package client

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"

	wire "github.com/kradalby/nefit-go/internal/xmpp"
)

// transport carries one session's HTTP-over-XMPP traffic.
type transport interface {
	// Send writes one request for p, marking p sent immediately before its
	// bytes are written; an *unsentError means nothing was written, and
	// unless it wraps errSessionEnded the session is intact.
	Send(ctx context.Context, body string, p *pending) error
	// Ping keeps the session alive.
	Ping() error
	// Recv returns the next message addressed to this client.
	Recv() (inbound, error)
	Close() error
}

// inbound is a message addressed to this client.
type inbound struct {
	text  string
	error bool // XMPP stanza type "error"
	// push marks a message the transport knows cannot answer the in-flight request.
	push bool
	// owner is the request permitted to take this message, if the transport
	// tracks ownership; a different resource is still a push notification.
	owner *pending
}

// receivedReader counts bytes actually read, including decoder read-ahead.
// A reply that began before a request was written cannot answer that request.
type receivedReader struct {
	reader io.Reader
	bytes  atomic.Int64
}

func (r *receivedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes.Add(int64(n))
	return n, err
}

// iqReply answers an IQ request, as RFC 6120 8.2.3 requires of every one:
// pings succeed and anything else is unavailable.
func iqReply(e *wire.Element, from string) (wire.IQ, bool) {
	kind := e.Get("type")
	if e.Name != (xml.Name{Space: wire.ClientNS, Local: "iq"}) || kind != "get" && kind != "set" {
		return wire.IQ{}, false
	}
	reply := wire.IQ{Type: "result", ID: e.Get("id"), From: from, To: e.Get("from")}
	if kind != "get" || e.Child(wire.PingNS, "ping") == nil {
		unavailable := wire.E(wire.ClientNS, "error", wire.E("urn:ietf:params:xml:ns:xmpp-stanzas", "service-unavailable"))
		unavailable.Set("type", "cancel")
		reply.Type, reply.Extensions = "error", []wire.Element{unavailable}
	}
	return reply, true
}

// Session loss permits a retry only when the transport proves it was unsent.
var errSessionEnded = errors.New("session ended before send")

type unsentError struct{ err error }

func (e *unsentError) Error() string { return e.err.Error() }
func (e *unsentError) Unwrap() error { return e.err }

// socketWriter serialises stanza writes on one socket. A request cancelled
// before its first byte leaves the session intact; one cancelled or failed
// mid-write retires it, since a partial stanza or a late reply could not be
// attributed.
type socketWriter struct {
	socket  net.Conn
	slot    chan struct{}
	done    <-chan struct{} // closed when the session ends
	abort   func()          // retires the session without waiting on its workers
	timeout time.Duration
}

func newSocketWriter(socket net.Conn, done <-chan struct{}, abort func(), timeout time.Duration) socketWriter {
	return socketWriter{socket: socket, slot: make(chan struct{}, 1), done: done, abort: abort, timeout: timeout}
}

// write sends data. state, when set, is claimed from queued to started just
// before writing, so a caller that abandoned the request keeps it unsent.
func (w *socketWriter) write(ctx context.Context, data []byte, state *atomic.Int32, onStart func()) error {
	select {
	case w.slot <- struct{}{}:
	case <-ctx.Done():
		return &unsentError{ctx.Err()}
	case <-w.done:
		return &unsentError{errSessionEnded}
	}
	defer func() { <-w.slot }()
	select {
	case <-w.done:
		return &unsentError{errSessionEnded}
	default:
	}
	if err := ctx.Err(); err != nil {
		return &unsentError{err}
	}
	now := time.Now()
	deadline := now.Add(w.timeout)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		// Err lags the deadline until its timer runs; a write deadline
		// already past would fail before any byte left, yet abort the session.
		if !now.Before(end) {
			return &unsentError{context.DeadlineExceeded}
		}
		deadline = end
	}
	if err := w.socket.SetWriteDeadline(deadline); err != nil {
		w.abort()
		return &unsentError{errSessionEnded}
	}
	defer func() { _ = w.socket.SetWriteDeadline(time.Time{}) }()
	if state != nil && !state.CompareAndSwap(queued, started) {
		return &unsentError{context.Canceled}
	}
	if onStart != nil {
		onStart()
	}
	stop := context.AfterFunc(ctx, w.abort)
	_, err := w.socket.Write(data)
	stop()
	if err != nil {
		w.abort()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if end, ok := ctx.Deadline(); ok && !time.Now().Before(end) {
			return context.DeadlineExceeded
		}
	}
	return err
}
