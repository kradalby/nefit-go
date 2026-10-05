package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	xmpp "github.com/xmppo/go-xmpp"
)

// dialXMPP logs in to addr with go-xmpp talking through a loopback relay.
// go-xmpp takes no dialer and sets no deadlines, so on a socket of its own a
// stalled handshake or write could never be aborted; the relay's sockets are
// ours to close at any stage. A local process racing go-xmpp to the relay
// port can only fail the dial: TLS still runs end to end.
func dialXMPP(ctx context.Context, addr string, o xmpp.Options) (transport, error) {
	var d net.Dialer
	up, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = up.Close()
		return nil, err
	}

	r := &relay{up: up, ln: ln}
	r.wg.Go(r.serve)

	stop := context.AfterFunc(ctx, r.shut)
	// go-xmpp still dials around the relay: through HTTP_PROXY unless
	// NO_PROXY matches this address, and to follow a <see-other-host>
	// redirect inside TLS. Neither socket is ours, so ctx and Close cannot
	// end them.
	o.Host = ln.Addr().String()
	xc, err := o.NewClient()
	if !stop() {
		// The relay went with ctx, whatever NewClient made of that.
		_ = r.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("failed to create XMPP client: %w", err)
	}

	return &xmppTransport{Client: xc, relay: r}, nil
}

// xmppTransport closes by dropping the relay. go-xmpp's graceful Close
// writes the stream end before arming its timeout and takes the stream lock a
// failed read can leave held; either blocks it, and the reader, forever.
type xmppTransport struct {
	*xmpp.Client
	relay *relay
}

func (t *xmppTransport) Close() error {
	return t.relay.Close()
}

// relay pipes the one connection go-xmpp makes to ln through to up.
type relay struct {
	up net.Conn
	ln net.Listener
	wg sync.WaitGroup

	mu     sync.Mutex
	local  net.Conn
	closed bool
}

func (r *relay) serve() {
	local, err := r.ln.Accept()
	_ = r.ln.Close()
	if err != nil {
		r.shut()
		return
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = local.Close()
		return
	}
	r.local = local
	r.mu.Unlock()

	r.wg.Go(func() {
		_, _ = io.Copy(local, r.up)
		r.shut()
	})
	_, _ = io.Copy(r.up, local)
	r.shut()
}

// shut closes every socket, failing whatever go-xmpp or the copies are
// blocked on.
func (r *relay) shut() {
	r.mu.Lock()
	r.closed = true
	local := r.local
	r.mu.Unlock()

	_ = r.ln.Close()
	_ = r.up.Close()
	if local != nil {
		_ = local.Close()
	}
}

// Close shuts the relay and waits for its copies to stop.
func (r *relay) Close() error {
	r.shut()
	r.wg.Wait()
	return nil
}
