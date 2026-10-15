package httpapi

import (
	"sync/atomic"
	"testing"
)

func TestEventQueueByteAndFrameBounds(t *testing.T) {
	var used atomic.Int64
	q := newEventQueue(func(delta int64) { used.Add(delta) })
	frame := make([]byte, eventQueueBytes/2)
	if !q.push(frame) || !q.push(frame) || q.push([]byte{1}) {
		t.Fatal("byte limit failed")
	}
	q.received(<-q.frames)
	if used.Load() != eventQueueBytes/2 || !q.push(frame) {
		t.Fatal("dequeue accounting failed")
	}
	q.close()
	q.close()
	if used.Load() != 0 || q.push(nil) {
		t.Fatal("closed queue leaked accounting or accepted data")
	}
	q = newEventQueue(func(delta int64) { used.Add(delta) })
	for range 128 {
		if !q.push([]byte{1}) {
			t.Fatal("queue rejected capacity")
		}
	}
	if q.push([]byte{1}) {
		t.Fatal("frame limit failed")
	}
	q.close()
	if used.Load() != 0 {
		t.Fatal("close did not release queued byte metric")
	}
}
