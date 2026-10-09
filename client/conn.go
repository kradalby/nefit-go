package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/kradalby/nefit-go/protocol"
)

// HTTPError is a request the device answered with a failure status; a 400
// means it refused the request or its value.
type HTTPError struct {
	StatusCode int
	Status     string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP error %d: %s", e.StatusCode, e.Status) }

var (
	errConnectionLost = errors.New("connection lost")
	// ErrClosed is returned once Close has been called.
	ErrClosed = errors.New("client closed")
	// errUnanswered marks a request that went out and got no reply in time.
	errUnanswered = errors.New("no reply")
)

// closedChan is what Done returns when there is no connection, so waiting on
// it never blocks.
var closedChan = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// conn is one XMPP session and the request state bound to it. Connect
// publishes it once; reconnecting builds a new value rather than resetting
// this one, so a dead session's workers and late replies cannot reach its
// successor.
type conn struct {
	xmpp transport

	// ctx is cancelled when the session is retired: the stream failed, the
	// client closed, or a sent request went unanswered.
	ctx    context.Context
	cancel context.CancelCauseFunc

	closeOnce sync.Once

	mu sync.Mutex
	// inflight is the request awaiting a reply; the backend serves one at
	// a time.
	inflight *pending
}

// reply is a decoded backend response.
type reply struct {
	resp *protocol.HTTPResponse
	id   string // resource path named by a JSON body
	data any    // decrypted body, JSON-decoded when possible
	err  error
}

type pending struct {
	uri   string
	get   bool
	reply chan reply
	// sent is set once the request's bytes may have reached the backend.
	// Earlier replies belong to someone else.
	sent atomic.Bool
	// replied retires a transport's ownership window once its answer arrives,
	// so later notifications still reach subscribers.
	replied atomic.Bool
}

// markSent is nil-safe so transports can call it for requests without an owner.
func (p *pending) markSent() {
	if p != nil {
		p.sent.Store(true)
	}
}

// answeredBy reports whether r can be the reply to p. Replies carry no
// request id, only the resource path in JSON bodies; replies without one can
// only be checked by shape.
func (p *pending) answeredBy(r reply) bool {
	if !p.get {
		// PUT acks are bodiless; a reply naming a resource is a push.
		return r.id == ""
	}
	if r.id != "" {
		return resource(r.id) == resource(p.uri)
	}
	// A GET's success always carries a body.
	return r.resp == nil || r.resp.StatusCode >= 300 || r.resp.Body != ""
}

// resource drops the query, which the echoed id is not known to carry.
func resource(uri string) string {
	path, _, _ := strings.Cut(uri, "?")
	return path
}

func newConn(parent context.Context, t transport) *conn {
	ctx, cancel := context.WithCancelCause(parent)
	return &conn{xmpp: t, ctx: ctx, cancel: cancel}
}

func (cn *conn) alive() bool {
	return cn.ctx.Err() == nil
}

// close retires the session; the blocked reader then fails and exits.
func (cn *conn) close() { cn.fail(nil) }

// fail retires the session with the stream's error as its cause.
func (cn *conn) fail(err error) {
	cn.closeOnce.Do(func() {
		cn.cancel(err)
		_ = cn.xmpp.Close()
	})
}

func (cn *conn) begin(p *pending) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	cn.inflight = p
}

// abandon withdraws p, reporting false if a reply already claimed it.
func (cn *conn) abandon(p *pending) bool {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	if cn.inflight != p {
		return false
	}
	cn.inflight = nil
	return true
}

// match claims the in-flight request r answers, if any. A reply the
// transport already assigned to owner can only answer that request.
func (cn *conn) match(r reply, owner *pending) *pending {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	p := cn.inflight
	if p == nil || owner != nil && p != owner || !p.sent.Load() || !p.answeredBy(r) {
		return nil
	}
	cn.inflight = nil
	return p
}
