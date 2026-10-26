// Package eventq is an unbounded FIFO that feeds a channel, so producers
// never block on a slow consumer.
package eventq

import "sync"

// Queue delivers pushed values on Out in order.
type Queue[T any] struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []T
	closed bool
	out    chan T
}

// New starts a queue.
func New[T any]() *Queue[T] {
	q := &Queue[T]{out: make(chan T)}
	q.cond = sync.NewCond(&q.mu)
	go q.run()
	return q
}

// Out is closed after Close once every pushed value has been received.
func (q *Queue[T]) Out() <-chan T { return q.out }

// Push enqueues v. Pushing after Close is a no-op.
func (q *Queue[T]) Push(v T) {
	q.mu.Lock()
	if !q.closed {
		q.items = append(q.items, v)
		q.cond.Signal()
	}
	q.mu.Unlock()
}

// Close stops accepting values; queued values are still delivered.
func (q *Queue[T]) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Signal()
	q.mu.Unlock()
}

func (q *Queue[T]) run() {
	for {
		q.mu.Lock()
		for len(q.items) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.items) == 0 {
			q.mu.Unlock()
			close(q.out)
			return
		}
		v := q.items[0]
		var zero T
		q.items[0] = zero
		q.items = q.items[1:]
		q.mu.Unlock()
		q.out <- v
	}
}
