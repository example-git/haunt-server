package httpapi

import (
	"context"
	"fmt"
	"testing"
)

func TestGatewayHintsAreBoundedCoalescedAndAccountScoped(t *testing.T) {
	a := &gatewayConnection{user: "alice", wake: make(chan struct{}, 1), cancel: func() {}}
	b := &gatewayConnection{user: "bob", wake: make(chan struct{}, 1), cancel: func() {}}
	g := &gateway{connections: map[string]*gatewayConnection{"a": a, "b": b}}
	for i := 0; i < 1000; i++ {
		g.hint(fmt.Sprintf("read:alice:room-%d", i))
	}
	if len(a.wake) != 1 || len(b.wake) != 0 {
		t.Fatal("read hints broadcast to unrelated accounts or unbounded wake queue")
	}
	flags, reads := a.take()
	if flags&hintSync == 0 || len(reads) != 100 {
		t.Fatalf("overflow should request snapshot: %d %d", flags, len(reads))
	}
	if flags, reads = a.take(); flags != 0 || len(reads) != 0 {
		t.Fatal("hints not drained")
	}
	g.hint("directory")
	if flags, _ = b.take(); flags&hintDirectory == 0 {
		t.Fatal("directory mutation did not reach other accounts")
	}
	g.listenerCancel = func() {}
	if err := g.close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
