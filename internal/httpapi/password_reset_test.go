package httpapi

import (
	"context"
	"errors"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"strings"
	"testing"
	"time"
)

func TestTemporaryPasswordRequiresReplacement(t *testing.T) {
	f := newFixture(t)
	alice := f.register(t, "alice")
	var temporary identity.TemporaryPassword
	f.call(t, "POST", "/api/v1/admin/members/"+alice.User.ID+"/password-reset", f.owner.Token, map[string]string{"reason": "Account recovery"}, 200, &temporary)
	if len(temporary.Password) < 16 {
		t.Fatal("weak generated temporary password")
	}
	f.call(t, "GET", "/api/v1/me", alice.Token, nil, 401, nil)
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": testPassword, "device_name": "old"}, 401, nil)
	var restricted identity.Login
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": temporary.Password, "device_name": "recovery", "remember": true}, 200, &restricted)
	if !restricted.PasswordChangeRequired || restricted.Session.ExpiresAt == nil || time.Until(*restricted.Session.ExpiresAt) > 15*time.Minute {
		t.Fatal("temporary session is not restricted/expiring")
	}
	for _, path := range []string{"/api/v1/me", "/api/v1/channels", "/api/v1/users", "/api/v1/sessions"} {
		f.call(t, "GET", path, restricted.Token, nil, 403, nil)
	}
	f.call(t, "PUT", "/api/v1/me/session", restricted.Token, map[string]bool{"remember": true}, 403, nil)
	f.call(t, "POST", "/api/v1/sessions/restore", restricted.Token, map[string]string{"device_name": "restore restricted account"}, 403, nil)
	if _, err := f.identities.Authenticate(context.Background(), restricted.Token); !errors.Is(err, identity.ErrPasswordChange) {
		t.Fatal("ordinary/socket auth accepted temporary session", err)
	}
	for _, replacement := range []string{"1234567", temporary.Password} {
		f.call(t, "PUT", "/api/v1/me/password", restricted.Token, map[string]string{"current_password": temporary.Password, "new_password": replacement}, 400, nil)
	}
	f.call(t, "PUT", "/api/v1/me/password", restricted.Token, map[string]string{"current_password": "incorrect", "new_password": "eight888"}, 401, nil)
	f.call(t, "PUT", "/api/v1/me/password", restricted.Token, map[string]string{"current_password": temporary.Password, "new_password": "eight888"}, 204, nil)
	f.call(t, "GET", "/api/v1/me", restricted.Token, nil, 401, nil)
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": temporary.Password, "device_name": "reused"}, 401, nil)
	var fresh identity.Login
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": "eight888", "device_name": "new", "remember": true}, 200, &fresh)
	if fresh.PasswordChangeRequired || fresh.Session.ExpiresAt != nil {
		t.Fatal("normal session not restored")
	}
	f.call(t, "GET", "/api/v1/me", fresh.Token, nil, 200, nil)
	var audit auditResponse
	f.call(t, "GET", "/api/v1/admin/audit", f.owner.Token, nil, 200, &audit)
	found := false
	for _, entry := range audit.Entries {
		if strings.Contains(entry.Reason, temporary.Password) {
			t.Fatal("temporary password in audit")
		}
		if entry.Action == "member.password_reset" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing reset audit")
	}
}
func TestResetPasswordsDelegationAndRevocation(t *testing.T) {
	f := newFixture(t)
	admin := f.register(t, "admin")
	peer := f.register(t, "peer")
	alice := f.register(t, "alice")
	grantRole(t, f, admin, identity.ResetPasswords, 50)
	grantRole(t, f, peer, identity.ResetPasswords, 50)
	f.call(t, "GET", "/api/v1/members", admin.Token, nil, 200, nil)
	for _, target := range []identity.Login{f.owner, admin, peer} {
		f.call(t, "POST", "/api/v1/admin/members/"+target.User.ID+"/password-reset", admin.Token, map[string]string{"reason": "recovery"}, 403, nil)
	}
	f.call(t, "POST", "/api/v1/admin/members/"+alice.User.ID+"/password-reset", alice.Token, map[string]string{"reason": "recovery"}, 403, nil)
	var first, second identity.TemporaryPassword
	f.call(t, "POST", "/api/v1/admin/members/"+alice.User.ID+"/password-reset", admin.Token, map[string]string{"reason": "recovery"}, 200, &first)
	var login identity.Login
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]string{"username": "alice", "password": first.Password, "device_name": "temp"}, 200, &login)
	f.call(t, "POST", "/api/v1/admin/members/"+alice.User.ID+"/password-reset", admin.Token, map[string]string{"reason": "reissue"}, 200, &second)
	f.call(t, "PUT", "/api/v1/me/password", login.Token, map[string]string{"current_password": first.Password, "new_password": "eight888"}, 401, nil)
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]string{"username": "alice", "password": second.Password, "device_name": "temp"}, 200, &login)
	f.call(t, "POST", "/api/v1/auth/logout", login.Token, nil, 204, nil)
	f.call(t, "PUT", "/api/v1/me/password", login.Token, map[string]string{"current_password": second.Password, "new_password": "eight888"}, 401, nil)
	f.call(t, "PUT", "/api/v1/members/"+admin.User.ID+"/roles", f.owner.Token, map[string]any{"role_ids": []string{}}, 204, nil)
	f.call(t, "POST", "/api/v1/admin/members/"+alice.User.ID+"/password-reset", admin.Token, map[string]string{"reason": "recovery"}, 403, nil)
}
func TestEightCharacterRegistrationAndDeletedDirectory(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	invite := f.invite(t, 1)
	body := map[string]string{"username": "alice", "password": "1234567", "invitation": invite.Token, "device_name": "join"}
	f.call(t, "POST", "/api/v1/auth/register", "", body, 400, nil)
	body["password"] = "eight888"
	var alice identity.Login
	f.call(t, "POST", "/api/v1/auth/register", "", body, 201, &alice)
	var dm chat.Conversation
	f.call(t, "POST", "/api/v1/dms", f.owner.Token, map[string]any{"members": []string{alice.User.ID}}, 201, &dm)
	moderate(t, f, f.owner, alice.User.ID, "delete", 204)
	var users usersResponse
	f.call(t, "GET", "/api/v1/users", f.owner.Token, nil, 200, &users)
	for _, u := range users.Users {
		if u.ID == alice.User.ID {
			t.Fatal("deleted user in directory")
		}
	}
	var members membersResponse
	f.call(t, "GET", "/api/v1/members", f.owner.Token, nil, 200, &members)
	for _, u := range members.Members {
		if u.ID == alice.User.ID {
			t.Fatal("deleted user in admin directory")
		}
	}
	var list conversationsResponse
	f.call(t, "GET", "/api/v1/conversations", f.owner.Token, nil, 200, &list)
	for _, c := range list.Conversations {
		if c.ID == dm.ID {
			t.Fatal("deleted DM remains in sidebar directory")
		}
	}
	f.call(t, "GET", "/api/v1/conversations/"+dm.ID+"/messages", f.owner.Token, nil, 404, nil)
	f.call(t, "POST", "/api/v1/dms", f.owner.Token, map[string]any{"members": []string{alice.User.ID}}, 404, nil)
}
