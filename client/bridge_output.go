package client

import (
	"context"
	"io"
	"net"
	"slices"
	"time"

	wire "github.com/kradalby/nefit-go/internal/xmpp"
)

type upstreamWrite struct {
	data     []byte
	deadline time.Time
	fallback *wire.IQ
	finish   bool
}

// startUpstreamWriter starts only after login, which still observes each
// write's outcome before progressing. One bounded FIFO keeps all subsequent
// replies, service stanzas and heartbeats in order without blocking the
// device scheduler. The deadline includes time spent waiting in the queue.
func (t *localTransport) startUpstreamWriter() {
	if t.upstream == nil {
		return
	}
	writes := make(chan upstreamWrite, maxCloudRequests)
	t.upstreamWrites = writes
	upstream := t.upstream
	t.spawn(func() {
		for {
			select {
			case <-t.ctx.Done():
				return
			case write, ok := <-writes:
				if !ok || t.ctx.Err() != nil {
					return
				}
				var err error
				if t.upstreamConnected.Load() {
					if write.finish {
						err = io.EOF
					} else {
						err = writeUpstream(upstream, write)
					}
					if err == nil {
						continue
					}
					// Close wakes the reader too. Only runBridge changes relay
					// and request state; either failure notification is enough.
					_ = upstream.Close()
				}
				if err != nil || write.fallback != nil {
					select {
					case t.inputs <- frameEvent{cloud: true, err: err, fallback: write.fallback}:
					case <-t.ctx.Done():
					}
				}
			}
		}
	})
}

// finishUpstream preserves already queued replies before an intentional
// relay close. Backpressure is still bounded by each reply's deadline.
func (t *localTransport) finishUpstream() {
	t.upstreamFinishing = true
	t.queue = slices.DeleteFunc(t.queue, func(r bridgeRequest) bool { return r.sent == nil })
	if t.upstreamWrites != nil && t.upstreamConnected.Load() {
		select {
		case t.upstreamWrites <- upstreamWrite{finish: true}:
			return
		default:
		}
	}
	t.disconnectCloud()
}

func writeUpstream(socket net.Conn, write upstreamWrite) error {
	if !time.Now().Before(write.deadline) {
		return context.DeadlineExceeded
	}
	if err := socket.SetWriteDeadline(write.deadline); err != nil {
		return err
	}
	defer func() { _ = socket.SetWriteDeadline(time.Time{}) }()
	n, err := socket.Write(write.data)
	if err == nil && n != len(write.data) {
		return io.ErrShortWrite
	}
	return err
}
