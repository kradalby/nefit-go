package client

import (
	"context"
	"fmt"
	"sync"
)

type requestItem struct {
	ctx      context.Context
	execute  func() (interface{}, error)
	resultCh chan requestResult
}

type requestResult struct {
	value interface{}
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
			value, err := req.execute()

			select {
			case req.resultCh <- requestResult{value: value, err: err}:
			case <-req.ctx.Done():
			}
		}
	}
}

// Submit queues a request for execution and blocks until it completes or the context is cancelled.
func (q *RequestQueue) Submit(ctx context.Context, fn func() (interface{}, error)) (interface{}, error) {
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
		return nil, fmt.Errorf("queue is stopped")
	}

	select {
	case result := <-resultCh:
		return result.value, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close gracefully shuts down the queue worker.
func (q *RequestQueue) Close() {
	q.once.Do(func() {
		close(q.stopCh)
		q.wg.Wait()
	})
}
