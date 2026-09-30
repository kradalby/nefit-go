package client

import (
	"context"
	"errors"
	"sync"
)

var errQueueStopped = errors.New("queue is stopped")

type requestItem struct {
	ctx      context.Context
	execute  func() (any, error)
	resultCh chan requestResult
}

type requestResult struct {
	value any
	err   error
}

// RequestQueue serializes requests to ensure only one runs at a time.
// This is required by the Nefit backend, which can only handle one concurrent request.
type RequestQueue struct {
	requestCh chan requestItem
	stopCh    chan struct{}
	wg        sync.WaitGroup
	once      sync.Once
}

// NewRequestQueue creates and starts a new request queue with background worker.
func NewRequestQueue() *RequestQueue {
	q := &RequestQueue{
		requestCh: make(chan requestItem, 100), // Buffer to handle bursts
		stopCh:    make(chan struct{}),
	}

	q.wg.Add(1)
	go q.worker()

	return q
}

func (q *RequestQueue) worker() {
	defer q.wg.Done()

	for {
		select {
		case <-q.stopCh:
			return
		case req := <-q.requestCh:
			// select picks randomly when both are ready; Submit may already
			// have reported the queue stopped.
			select {
			case <-q.stopCh:
				return
			default:
			}

			// The caller has given up; sending now would only leave an
			// orphaned reply on the wire.
			if req.ctx.Err() != nil {
				continue
			}

			value, err := req.execute()

			select {
			case req.resultCh <- requestResult{value: value, err: err}:
			case <-req.ctx.Done():
			}
		}
	}
}

// Submit queues a request for execution and blocks until it completes or the context is cancelled.
func (q *RequestQueue) Submit(ctx context.Context, fn func() (any, error)) (any, error) {
	resultCh := make(chan requestResult, 1)

	req := requestItem{
		ctx:      ctx,
		execute:  fn,
		resultCh: resultCh,
	}

	select {
	case q.requestCh <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-q.stopCh:
		return nil, errQueueStopped
	}

	select {
	case result := <-resultCh:
		return result.value, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-q.stopCh:
		return nil, errQueueStopped
	}
}

// Close gracefully shuts down the queue worker.
func (q *RequestQueue) Close() {
	q.once.Do(func() {
		close(q.stopCh)
		q.wg.Wait()
	})
}
