package client

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
)

func TestQueueSkipsRequestExpiredWhileQueued(t *testing.T) {
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
