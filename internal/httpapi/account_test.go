package httpapi

import (
	"github.com/example-git/haunt-server/internal/identity"
	"testing"
)

func TestPasswordChangeRecoveryAndAccountDisable(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	alice := f.register(t, "alice")
	f.call(t, "PUT", "/api/v1/me/session", alice.Token, map[string]bool{"remember": true}, 200, nil)
	f.call(t, "GET", "/api/v1/admin/stats", alice.Token, nil, 403, nil)
	f.call(t, "GET", "/api/v1/admin/stats", f.owner.Token, nil, 200, nil)
	f.call(t, "POST", "/api/v1/admin/maintenance", f.owner.Token, nil, 204, nil)
	f.call(t, "PUT", "/api/v1/me/password", alice.Token, map[string]any{"current_password": "wrong", "new_password": "a replacement password"}, 401, nil)
	f.call(t, "PUT", "/api/v1/me/password", alice.Token, map[string]any{"current_password": testPassword, "new_password": "a replacement password"}, 204, nil)
	f.call(t, "GET", "/api/v1/me", alice.Token, nil, 401, nil)
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": testPassword, "device_name": "old password"}, 401, nil)
	var updated identity.Login
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": "a replacement password", "device_name": "new password", "remember": true}, 200, &updated)
	f.call(t, "PUT", "/api/v1/admin/members/"+alice.User.ID, updated.Token, map[string]any{"disabled": true}, 403, nil)
	f.call(t, "PUT", "/api/v1/admin/members/"+alice.User.ID, f.owner.Token, map[string]any{"disabled": true}, 204, nil)
	f.call(t, "GET", "/api/v1/me", updated.Token, nil, 401, nil)
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": "a replacement password", "device_name": "disabled"}, 401, nil)
	f.call(t, "PUT", "/api/v1/admin/members/"+f.owner.User.ID, f.owner.Token, map[string]any{"disabled": true}, 403, nil)
	f.call(t, "PUT", "/api/v1/admin/members/"+alice.User.ID, f.owner.Token, map[string]any{"disabled": false}, 204, nil)
	f.call(t, "PUT", "/api/v1/admin/members/"+alice.User.ID+"/password", f.owner.Token, map[string]any{"new_password": "another recovered password"}, 204, nil)
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": "another recovered password", "device_name": "recovered"}, 200, nil)
}
