package httpapi

import (
	"context"
	"encoding/json"
	"github.com/example-git/haunt-server/internal/identity"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestPresenceConnectionsCoalesceAndStayInstallationScoped(t *testing.T) {
	observer := &gatewayConnection{user: "observer", watchPresence: true, wake: make(chan struct{}, 1)}
	quiet := &gatewayConnection{user: "quiet", wake: make(chan struct{}, 1)}
	g := &gateway{connections: map[string]*gatewayConnection{"observer": observer, "quiet": quiet}, online: map[string]int{"observer": 1, "quiet": 1}}
	a, b := &gatewayConnection{}, &gatewayConnection{}
	g.setOnlineUser(a, "alice")
	flags, _ := observer.take()
	if flags != hintPresence || len(quiet.wake) != 0 {
		t.Fatal("presence should only wake subscribers")
	}
	g.setOnlineUser(b, "alice")
	g.setOnlineUser(a, "")
	if flags, _ = observer.take(); flags != 0 {
		t.Fatal("extra device/remaining device should not broadcast")
	}
	if !slices.Contains(g.onlineUsers(), "alice") {
		t.Fatal("remaining device lost online status")
	}
	g.setOnlineUser(b, "")
	if flags, _ = observer.take(); flags != hintPresence {
		t.Fatal("last disconnect missing")
	}
	if slices.Contains(g.onlineUsers(), "alice") {
		t.Fatal("offline user remained")
	}
	other := &gateway{online: map[string]int{}, connections: map[string]*gatewayConnection{}}
	var workers sync.WaitGroup
	for i := 0; i < 40; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			c := &gatewayConnection{}
			g.setOnlineUser(c, "parallel")
			g.onlineUsers()
			g.setOnlineUser(c, "")
		}()
	}
	workers.Wait()
	if slices.Contains(g.onlineUsers(), "parallel") || len(other.onlineUsers()) != 0 || len(observer.wake) > 1 {
		t.Fatal("unbounded or cross-instance presence")
	}
}

func TestPresenceGatewaySnapshotMultipleDevicesAndRevocation(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	alice := f.register(t, "alice")
	observer := wsConnect(t, f, f.owner.Token)
	wsSend(t, observer, map[string]any{"type": "events.watch"})
	wsRead(t, observer, "events.watching")
	wsSend(t, observer, map[string]any{"type": "presence.watch"})
	wsRead(t, observer, "presence.watching")
	check := func(want ...string) {
		t.Helper()
		event := wsRead(t, observer, "presence.snapshot")
		var ids []string
		if err := json.Unmarshal(event["online_user_ids"], &ids); err != nil {
			t.Fatal(err)
		}
		slices.Sort(want)
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("online users = %v, want %v", ids, want)
		}
	}
	check(f.owner.User.ID)
	first := wsConnect(t, f, alice.Token)
	check(f.owner.User.ID, alice.User.ID)
	var secondLogin identity.Login
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": testPassword, "device_name": "second"}, 200, &secondLogin)
	second := wsConnect(t, f, secondLogin.Token)
	first.CloseNow()
	// Explicit resume supplies a fresh snapshot, including users already online.
	wsSend(t, observer, map[string]any{"type": "presence.watch", "enabled": false})
	wsRead(t, observer, "presence.watching")
	wsSend(t, observer, map[string]any{"type": "presence.watch"})
	wsRead(t, observer, "presence.watching")
	check(f.owner.User.ID, alice.User.ID)
	f.call(t, "POST", "/api/v1/auth/logout", secondLogin.Token, nil, 204, nil)
	check(f.owner.User.ID)
	second.CloseNow()
	first = wsConnect(t, f, alice.Token)
	check(f.owner.User.ID, alice.User.ID)
	first.CloseNow()
	check(f.owner.User.ID)
	observer.CloseNow()
	deadline := time.Now().Add(3 * time.Second)
	for len(features.Gateway.onlineUsers()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(features.Gateway.onlineUsers()) != 0 {
		t.Fatal("presence leaked after all sockets closed")
	}
	// Merely having an authenticated HTTP session never advertises online status.
	if _, err := f.identities.Authenticate(context.Background(), alice.Token); err != nil {
		t.Fatal(err)
	}
}
