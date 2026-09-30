package client

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/kradalby/nefit-go/protocol"
)

var (
	errNotConnected   = errors.New("not connected")
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

	// ctx is cancelled when the session is torn down.
	ctx    context.Context
	cancel context.CancelFunc

	// done is closed by the reader when the stream fails or is closed.
	done chan struct{}

	closeOnce sync.Once

	mu sync.Mutex
	// inflight is the request awaiting a reply; the backend serves one at
	// a time.
	inflight *pending
	// abandoned counts GETs per resource whose caller gave up before the
	// reply came. A reply that never comes, or comes without an id, costs
	// one push for that resource until the session ends.
	abandoned map[string]int
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
	return &conn{
		xmpp:   t,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),

		abandoned: make(map[string]int),
	}
}

func (cn *conn) alive() bool {
	select {
	case <-cn.done:
		return false
	default:
		return true
	}
}

// close tears the session down; the blocked reader then fails and closes done.
// The transport closes in the background: go-xmpp's graceful close takes the
// stream lock, which a failed read can leave held, and would block forever.
// Its own timer still drops the socket.
func (cn *conn) close() {
	cn.closeOnce.Do(func() {
		cn.cancel()
		go func() { _ = cn.xmpp.Close() }()
	})
}

func (cn *conn) begin(p *pending) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	cn.inflight = p
}

// end clears p unless a reply already claimed it. An abandoned GET is
// remembered so its late reply is not taken for a push notification.
func (cn *conn) end(p *pending, abandoned bool) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	if cn.inflight != p {
		return
	}
	cn.inflight = nil
	if abandoned && p.get {
		cn.abandoned[resource(p.uri)]++
	}
}

// match claims the in-flight request r answers. Failing that, late reports
// whether r answers a request its caller already abandoned.
func (cn *conn) match(r reply) (p *pending, late bool) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	if p := cn.inflight; p != nil && p.answeredBy(r) {
		cn.inflight = nil
		return p, false
	}
	if id := resource(r.id); id != "" && cn.abandoned[id] > 0 {
		cn.abandoned[id]--
		return nil, true
	}
	return nil, false
}
