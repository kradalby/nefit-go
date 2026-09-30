package client

import (
	"context"
	"errors"
	"sync"
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

// conn is one XMPP session. Its fields never change once Connect publishes
// it; reconnecting builds a new value, so workers of a dead session can never
// touch its successor's stream.
type conn struct {
	xmpp transport

	// ctx is cancelled when the session is torn down.
	ctx    context.Context
	cancel context.CancelFunc

	// done is closed by the reader when the stream fails or is closed.
	done chan struct{}

	closeOnce sync.Once
}

func newConn(parent context.Context, t transport) *conn {
	ctx, cancel := context.WithCancel(parent)
	return &conn{
		xmpp:   t,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
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
