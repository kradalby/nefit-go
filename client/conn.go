package client

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/kradalby/nefit-go/protocol"
)

var (
	errConnectionLost = errors.New("connection lost")
	errClosed         = errors.New("client closed")
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
	// client closed, or a request failed after it may have gone out.
	ctx    context.Context
	cancel context.CancelFunc

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
	ctx, cancel := context.WithCancel(parent)
	return &conn{xmpp: t, ctx: ctx, cancel: cancel}
}

func (cn *conn) alive() bool {
	return cn.ctx.Err() == nil
}

// close retires the session; the blocked reader then fails and exits.
func (cn *conn) close() {
	cn.closeOnce.Do(func() {
		cn.cancel()
		_ = cn.xmpp.Close()
	})
}

func (cn *conn) begin(p *pending) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	cn.inflight = p
}

// match claims the in-flight request r answers, if any.
func (cn *conn) match(r reply) *pending {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	p := cn.inflight
	if p == nil || !p.answeredBy(r) {
		return nil
	}
	cn.inflight = nil
	return p
}
