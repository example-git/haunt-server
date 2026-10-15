package httpapi

import "sync"

const eventQueueBytes = 1 << 20

// eventQueue bounds both frame count and memory. Its mutex protects accounting
// only; callers perform socket writes after releasing it.
type eventQueue struct {
	frames chan []byte
	mu     sync.Mutex
	bytes  int
	closed bool
	delta  func(int64)
}

func newEventQueue(delta func(int64)) *eventQueue {
	return &eventQueue{frames: make(chan []byte, 128), delta: delta}
}
func (q *eventQueue) push(frame []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || len(frame) > eventQueueBytes-q.bytes {
		return false
	}
	select {
	case q.frames <- frame:
		q.bytes += len(frame)
		q.delta(int64(len(frame)))
		return true
	default:
		return false
	}
}
func (q *eventQueue) received(frame []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.bytes -= len(frame)
		q.delta(-int64(len(frame)))
	}
}
func (q *eventQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		q.delta(-int64(q.bytes))
		q.bytes = 0
	}
}
