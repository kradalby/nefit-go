package client

import (
	"context"
	"fmt"
	"sync"
)

// requestItem represents a queued request
type requestItem struct {
	ctx      context.Context
	execute  func() (interface{}, error)
	resultCh chan requestResult
}

// requestResult holds the result of a request execution
type requestResult struct {
	value interface{}
	err   error
}

// RequestQueue serializes requests to ensure only one runs at a time
// This is required by the Nefit backend which can only handle one concurrent request
type RequestQueue struct {
	requestCh chan requestItem
	stopCh    chan struct{}
	wg        sync.WaitGroup
	once      sync.Once
}

// NewRequestQueue creates a new request queue and starts the worker
func NewRequestQueue() *RequestQueue {
	q := &RequestQueue{
		requestCh: make(chan requestItem, 100), // Buffer to handle bursts
		stopCh:    make(chan struct{}),
	}

	// Start the worker goroutine
	q.wg.Add(1)
	go q.worker()

	return q
}

// worker processes requests one at a time
func (q *RequestQueue) worker() {
	defer q.wg.Done()

	for {
		select {
		case <-q.stopCh:
			return
		case req := <-q.requestCh:
			// Execute the request
			value, err := req.execute()

			// Send result back (or timeout if context is cancelled)
			select {
			case req.resultCh <- requestResult{value: value, err: err}:
			case <-req.ctx.Done():
				// Request was cancelled, skip sending result
			}
		}
	}
}

// Submit adds a request to the queue and waits for its execution
func (q *RequestQueue) Submit(ctx context.Context, fn func() (interface{}, error)) (interface{}, error) {
	resultCh := make(chan requestResult, 1)

	req := requestItem{
		ctx:      ctx,
		execute:  fn,
		resultCh: resultCh,
	}

	// Submit request to queue
	select {
	case q.requestCh <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-q.stopCh:
		return nil, fmt.Errorf("queue is stopped")
	}

	// Wait for result or context cancellation
	select {
	case result := <-resultCh:
		return result.value, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops the queue worker and waits for it to finish
func (q *RequestQueue) Close() {
	q.once.Do(func() {
		close(q.stopCh)
		q.wg.Wait()
	})
}
