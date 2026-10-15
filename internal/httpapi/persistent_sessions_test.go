package httpapi

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/identity"
)

func TestPersistentSessionsLifecycleAndIsolation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	invite := f.invite(t, 1)
	var saved identity.Login
	f.call(t, "POST", "/api/v1/auth/register", "", map[string]any{
		"username": "saved", "password": testPassword, "device_name": "remembered browser",
		"invitation": invite.Token, "remember": true,
	}, 201, &saved)
	if saved.Session.ExpiresAt != nil {
		t.Fatal("remembered session expires")
	}
	// Age is not an implicit expiry, and cleanup must retain remembered devices.
	if _, err := f.pool.Exec(ctx, `UPDATE chatxo.sessions SET created_at=now()-interval '400 days' WHERE id=$1`, saved.Session.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.identities.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", "/api/v1/me", saved.Token, nil, 200, nil)
	p, err := f.identities.Authenticate(ctx, saved.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.identities.Refresh(ctx, p); err != nil {
		t.Fatal("gateway refresh rejected remembered session", err)
	}
	// Persistence is in PostgreSQL, not tied to one process's memory.
	restarted, err := identity.New(ctx, f.cfg, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Authenticate(ctx, saved.Token); err != nil {
		t.Fatal(err)
	}
	var list sessionsResponse
	f.call(t, "GET", "/api/v1/sessions", saved.Token, nil, 200, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].ExpiresAt != nil {
		t.Fatal("missing persistent device")
	}
	for _, body := range []any{map[string]any{}, map[string]any{"remember": nil}, map[string]any{"remember": "true"}, map[string]any{"remember": true, "session_id": f.owner.Session.ID}} {
		f.call(t, "PUT", "/api/v1/me/session", saved.Token, body, 400, nil)
	}
	f.call(t, "PUT", "/api/v1/me/session", "", map[string]bool{"remember": true}, 401, nil)
	var ordinary identity.Session
	f.call(t, "PUT", "/api/v1/me/session", saved.Token, map[string]bool{"remember": false}, 200, &ordinary)
	if ordinary.ExpiresAt == nil || time.Until(*ordinary.ExpiresAt) < 29*24*time.Hour {
		t.Fatal("ordinary expiry not restored")
	}
	var again identity.Session
	f.call(t, "PUT", "/api/v1/me/session", saved.Token, map[string]bool{"remember": false}, 200, &again)
	if !ordinary.ExpiresAt.Equal(*again.ExpiresAt) {
		t.Fatal("ordinary requests silently extend expiry")
	}
	f.call(t, "PUT", "/api/v1/me/session", saved.Token, map[string]bool{"remember": true}, 200, &again)
	if again.ExpiresAt != nil {
		t.Fatal("existing device not remembered")
	}
	other := newFixture(t)
	other.call(t, "GET", "/api/v1/me", saved.Token, nil, 401, nil)
	f.call(t, "DELETE", "/api/v1/sessions/"+saved.Session.ID, f.owner.Token, nil, 404, nil)
	f.call(t, "POST", "/api/v1/auth/logout", saved.Token, nil, 204, nil)
	f.call(t, "PUT", "/api/v1/me/session", saved.Token, map[string]bool{"remember": true}, 401, nil)
	if _, err := f.identities.RememberSession(ctx, p, true); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("stale principal revived revoked session", err)
	}

	var fresh identity.Login
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "saved", "password": testPassword, "device_name": "temporary"}, 200, &fresh)
	if fresh.Session.ExpiresAt == nil {
		t.Fatal("default login unexpectedly persistent")
	}
	expiredPrincipal, err := f.identities.Authenticate(ctx, fresh.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE chatxo.sessions SET expires_at=now()-interval '1 second' WHERE id=$1`, fresh.Session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.identities.RememberSession(ctx, expiredPrincipal, true); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("expired session revived", err)
	}
	f.call(t, "PUT", "/api/v1/me/session", fresh.Token, map[string]bool{"remember": true}, 401, nil)
}

func TestPersistentSessionRevocationRaceAndCapacity(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	alice := f.register(t, "alice")
	p, err := f.identities.Authenticate(ctx, alice.Token)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.identities.RememberSession(ctx, p, true)
			if err != nil && !errors.Is(err, identity.ErrUnauthorized) {
				t.Error(err)
			}
		}()
	}
	if err := f.identities.RevokeSession(ctx, p, p.SessionID); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if _, err := f.identities.Authenticate(ctx, alice.Token); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("revoked token survived race", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO chatxo.sessions(id,instance_id,user_id,token_hash,device_name,expires_at)
        SELECT gen_random_uuid(),$1,$2,decode(md5(n::text),'hex'),'saved device',NULL FROM generate_series(1,20) n`, f.cfg.InstanceID, alice.User.ID); err != nil {
		t.Fatal(err)
	}
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": testPassword, "device_name": "extra", "remember": true}, 409, nil)
}
