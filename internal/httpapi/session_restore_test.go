package httpapi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/example-git/haunt-server/internal/identity"
)

func TestRestoredDeviceSessionsAreIndependent(t *testing.T) {
	f := newFixture(t)
	alice := f.register(t, "alice")
	var second, restored identity.Login
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": testPassword, "device_name": "second device", "remember": true}, 200, &second)
	f.call(t, "POST", "/api/v1/sessions/restore", second.Token, map[string]string{"device_name": "restored browser"}, 201, &restored)
	if restored.Token == second.Token || restored.Session.ID == second.Session.ID || restored.User.ID != alice.User.ID || restored.Session.ExpiresAt != nil || restored.Session.DeviceName != "restored browser" {
		t.Fatal("restore failed to create an independent remembered device")
	}
	f.call(t, "POST", "/api/v1/auth/logout", restored.Token, nil, 204, nil)
	f.call(t, "GET", "/api/v1/me", restored.Token, nil, 401, nil)
	f.call(t, "GET", "/api/v1/me", alice.Token, nil, 200, nil)
	f.call(t, "GET", "/api/v1/me", second.Token, nil, 200, nil)
	f.call(t, "POST", "/api/v1/sessions/restore", second.Token, map[string]string{"device_name": "another restored browser"}, 201, &restored)
	f.call(t, "POST", "/api/v1/auth/logout", second.Token, nil, 204, nil)
	f.call(t, "GET", "/api/v1/me", restored.Token, nil, 200, nil)
	f.call(t, "GET", "/api/v1/me", alice.Token, nil, 200, nil)
	f.call(t, "POST", "/api/v1/sessions/restore", second.Token, map[string]string{"device_name": "revoked backup"}, 401, nil)
	f.call(t, "DELETE", "/api/v1/sessions/"+restored.Session.ID, f.owner.Token, nil, 404, nil)
	for _, body := range []any{map[string]string{}, map[string]string{"device_name": "", "user_id": f.owner.User.ID}} {
		f.call(t, "POST", "/api/v1/sessions/restore", alice.Token, body, 400, nil)
	}
	f.call(t, "POST", "/api/v1/sessions/restore", "", map[string]string{"device_name": "unauthenticated"}, 401, nil)
	var temporary identity.Login
	f.call(t, "POST", "/api/v1/sessions/restore", alice.Token, map[string]string{"device_name": "temporary clone"}, 201, &temporary)
	if temporary.Session.ExpiresAt == nil || !temporary.Session.ExpiresAt.Equal(*alice.Session.ExpiresAt) {
		t.Fatal("restore extended source expiry")
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.sessions SET expires_at=now()-interval '1 second' WHERE id=$1`, temporary.Session.ID); err != nil {
		t.Fatal(err)
	}
	f.call(t, "POST", "/api/v1/sessions/restore", temporary.Token, map[string]string{"device_name": "expired backup"}, 401, nil)
}

func TestRestoreSessionLimitsAndStalePrincipals(t *testing.T) {
	f := newFixture(t)
	alice := f.register(t, "alice")
	ctx := context.Background()
	p, err := f.identities.Authenticate(ctx, alice.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `INSERT INTO chatxo.sessions(id,instance_id,user_id,token_hash,device_name,expires_at)
        SELECT gen_random_uuid(),$1,$2,decode(md5(n::text),'hex'),'saved device',NULL FROM generate_series(1,18) n`, f.cfg.InstanceID, alice.User.ID); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.identities.RestoreSession(ctx, p, "concurrent restore")
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, identity.ErrSessionLimit) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("concurrent restores bypassed 20-session limit", successes.Load())
	}
	f.call(t, "POST", "/api/v1/sessions/restore", alice.Token, map[string]string{"device_name": "over limit"}, 409, nil)
	if err = f.identities.RevokeSession(ctx, p, p.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.identities.RestoreSession(ctx, p, "stale principal"); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("revoked source restored", err)
	}
}
