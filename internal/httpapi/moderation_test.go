package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
)

func grantRole(t *testing.T, f *fixture, user identity.Login, bits int64, rank int) identity.Role {
	t.Helper()
	var role identity.Role
	f.call(t, "POST", "/api/v1/roles", f.owner.Token, map[string]any{"name": user.User.Username + " role", "permissions": bits, "position": rank}, 201, &role)
	f.call(t, "PUT", "/api/v1/members/"+user.User.ID+"/roles", f.owner.Token, map[string]any{"role_ids": []string{role.ID}}, 204, nil)
	return role
}
func moderate(t *testing.T, f *fixture, actor identity.Login, target, action string, want int) {
	t.Helper()
	f.call(t, "POST", "/api/v1/admin/members/"+target+"/moderate", actor.Token, map[string]any{"action": action, "reason": "Test moderation reason"}, want, nil)
}
func TestModerationHierarchyAndPermissions(t *testing.T) {
	f := newFixture(t)
	admin := f.register(t, "admin")
	peer := f.register(t, "peer")
	member := f.register(t, "member")
	role := grantRole(t, f, admin, identity.ManageRoles|identity.KickMembers|identity.BanMembers|identity.ViewAuditLog, 50)
	grantRole(t, f, peer, identity.KickMembers, 50)
	f.call(t, "GET", "/api/v1/members", member.Token, nil, 403, nil)
	f.call(t, "GET", "/api/v1/admin/audit", member.Token, nil, 403, nil)
	f.call(t, "GET", "/api/v1/admin/members/"+member.User.ID+"/ips", admin.Token, nil, 403, nil)
	f.call(t, "GET", "/api/v1/members", admin.Token, nil, 200, nil)
	moderate(t, f, admin, f.owner.User.ID, "kick", 403)
	moderate(t, f, admin, peer.User.ID, "ban", 403)
	moderate(t, f, admin, admin.User.ID, "delete", 403)
	f.call(t, "PUT", "/api/v1/roles/"+role.ID, admin.Token, map[string]any{"name": "self promotion", "permissions": identity.AllPermissions, "position": 99}, 403, nil)
	f.call(t, "POST", "/api/v1/roles", admin.Token, map[string]any{"name": "escalation", "permissions": identity.DeleteMembers, "position": 1}, 403, nil)
	f.call(t, "POST", "/api/v1/roles", admin.Token, map[string]any{"name": "equal rank", "permissions": identity.KickMembers, "position": 50}, 403, nil)
	var lower identity.Role
	f.call(t, "POST", "/api/v1/roles", admin.Token, map[string]any{"name": "junior", "permissions": identity.KickMembers, "position": 1}, 201, &lower)
	f.call(t, "PUT", "/api/v1/members/"+member.User.ID+"/roles", admin.Token, map[string]any{"role_ids": []string{role.ID}}, 403, nil)
	f.call(t, "PUT", "/api/v1/members/"+member.User.ID+"/roles", admin.Token, map[string]any{"role_ids": []string{lower.ID}}, 204, nil)
	moderate(t, f, admin, member.User.ID, "kick", 204)
	f.call(t, "GET", "/api/v1/me", member.Token, nil, 401, nil)
	var audit auditResponse
	f.call(t, "GET", "/api/v1/admin/audit", admin.Token, nil, 200, &audit)
	found := false
	for _, a := range audit.Entries {
		if a.IP != nil {
			t.Fatal("IP leaked without ViewMemberIPs")
		}
		if a.Action == "member.kick" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing kick audit")
	}
	// Revoked delegation applies to an already-issued token.
	f.call(t, "PUT", "/api/v1/members/"+admin.User.ID+"/roles", f.owner.Token, map[string]any{"role_ids": []string{}}, 204, nil)
	moderate(t, f, admin, member.User.ID, "restore", 403)
}
func TestModerationKickBanAppealAndDeletion(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	alice := f.register(t, "alice")
	ch := textChannel(t, f)
	msg := sendMessage(t, f, alice.Token, ch.ID, "Kept history")
	moderate(t, f, f.owner, alice.User.ID, "kick", 204)
	creds := map[string]any{"username": "alice", "password": testPassword, "device_name": "return"}
	f.call(t, "POST", "/api/v1/auth/login", "", creds, 401, nil)
	invite := f.invite(t, 1)
	creds["invitation"] = invite.Token
	var returned identity.Login
	f.call(t, "POST", "/api/v1/auth/register", "", creds, 201, &returned)
	if returned.User.ID != alice.User.ID {
		t.Fatal("kick rejoin changed identity")
	}
	moderate(t, f, f.owner, alice.User.ID, "ban", 204)
	f.call(t, "GET", "/api/v1/me", returned.Token, nil, 401, nil)
	var denial errorResponse
	f.call(t, "POST", "/api/v1/auth/login", "", creds, 403, &denial)
	if denial.BanType != "user" || denial.AppealTicket == "" {
		t.Fatal("missing private appeal ticket")
	}
	creds["password"] = "incorrect"
	f.call(t, "POST", "/api/v1/auth/login", "", creds, 401, nil)
	var status identity.BanStatus
	f.call(t, "POST", "/api/v1/auth/ban/status", "", map[string]string{"appeal_ticket": denial.AppealTicket}, 200, &status)
	if status.Type != "user" || !status.Active {
		t.Fatal(status)
	}
	// Parallel requests cannot bypass the per-ban monthly limit, even across IPs.
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.identities.AppealBan(context.Background(), denial.AppealTicket, "Please review this test ban.")
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, identity.ErrConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatal("accepted concurrent appeals", accepted.Load())
	}
	for _, body := range []string{"short", strings.Repeat("x", 1001), "bad control \x00 data", "www.spam.test here", "<b>markup is forbidden</b>", "hidden \u202e direction"} {
		if err := f.identities.AppealBan(context.Background(), denial.AppealTicket, body); !errors.Is(err, identity.ErrInvalid) {
			t.Fatal("invalid appeal accepted", err)
		}
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.bans SET last_appeal_at=now()-interval '29 days' WHERE appeal_token=$1`, denial.AppealTicket); err != nil {
		t.Fatal(err)
	}
	if err := f.identities.AppealBan(context.Background(), denial.AppealTicket, "An early second appeal."); !errors.Is(err, identity.ErrConflict) {
		t.Fatal("monthly limit not enforced", err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.bans SET last_appeal_at=now()-interval '31 days' WHERE appeal_token=$1`, denial.AppealTicket); err != nil {
		t.Fatal(err)
	}
	if err := f.identities.AppealBan(context.Background(), denial.AppealTicket, "A new eligible monthly appeal."); err != nil {
		t.Fatal(err)
	}

	f.call(t, "POST", "/api/v1/auth/ban/appeal", "", map[string]string{"appeal_ticket": denial.AppealTicket, "body": "Please visit https://spam.test"}, 400, nil)
	var bans bansResponse
	f.call(t, "GET", "/api/v1/admin/bans", f.owner.Token, nil, 200, &bans)
	if len(bans.Bans) != 1 {
		t.Fatal("wrong ban count")
	}
	f.call(t, "POST", "/api/v1/admin/bans/"+bans.Bans[0].ID+"/revoke", f.owner.Token, map[string]string{"reason": "Appeal accepted"}, 204, nil)
	creds["password"] = testPassword
	f.call(t, "POST", "/api/v1/auth/login", "", creds, 200, &returned)
	moderate(t, f, f.owner, alice.User.ID, "delete", 204)
	f.call(t, "GET", "/api/v1/me", returned.Token, nil, 401, nil)
	// A stale principal from a request already in flight cannot repopulate profile data.
	stale := identity.Principal{User: alice.User, SessionID: returned.Session.ID}
	f.identities.SaveProfile(context.Background(), stale, identity.ProfileSettingsRequest{DisplayName: "Resurrected", Bio: "old request", AccentColor: "#123456"})
	f.call(t, "GET", "/api/v1/users/"+alice.User.ID, f.owner.Token, nil, 404, nil)
	var user identity.User
	if err := f.pool.QueryRow(context.Background(), `SELECT username,display_name,bio,avatar_version FROM chatxo.users WHERE id=$1`, alice.User.ID).Scan(&user.Username, &user.DisplayName, &user.Bio, &user.AvatarVersion); err != nil {
		t.Fatal(err)
	}
	if user.DisplayName != "Deleted user" || user.Bio != "" || user.AvatarVersion != "" || user.Username == "alice" {
		t.Fatal("identity not scrubbed")
	}
	var history chat.History
	f.call(t, "GET", "/api/v1/conversations/"+ch.ID+"/messages", f.owner.Token, nil, 200, &history)
	if len(history.Messages) != 1 || history.Messages[0].ID != msg.ID {
		t.Fatal("deletion broke history")
	}
	var count int
	f.pool.QueryRow(context.Background(), `SELECT count(*) FROM chatxo.member_ips WHERE user_id=$1`, alice.User.ID).Scan(&count)
	if count != 0 {
		t.Fatal("IP history survived deletion")
	}
	f.call(t, "PUT", "/api/v1/admin/members/"+alice.User.ID, f.owner.Token, map[string]bool{"disabled": false}, 404, nil)
}
func TestModerationIPRangesProxyAndPrivacy(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000, func(c *config.Config) { c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")} })
	alice := f.register(t, "alice")
	request := func(ip, method, path, token string, body any, want int) []byte {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, f.server.URL+path, bytes.NewReader(raw))
		req.Header.Set("X-Forwarded-For", ip)
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s %s status %d want %d: %s", method, path, res.StatusCode, want, data)
		}
		return data
	}
	request("198.51.100.44", "GET", "/api/v1/me", alice.Token, nil, 200)
	var ips memberIPsResponse
	f.call(t, "GET", "/api/v1/admin/members/"+alice.User.ID+"/ips", f.owner.Token, nil, 200, &ips)
	found := false
	for _, ip := range ips.IPs {
		if ip.IP == "198.51.100.44" {
			found = true
		}
	}
	if !found {
		t.Fatal("proxy address not tracked")
	}
	public := request("198.51.100.44", "GET", "/api/v1/users", alice.Token, nil, 200)
	if bytes.Contains(public, []byte("198.51.100")) {
		t.Fatal("IP leaked to directory")
	}
	request("203.0.113.10", "POST", "/api/v1/admin/bans", f.owner.Token, map[string]string{"network": "198.51.100.77/24", "reason": "Network abuse"}, 201)
	denied := request("::ffff:198.51.100.44", "GET", "/api/v1/me", alice.Token, nil, 403)
	var d errorResponse
	json.Unmarshal(denied, &d)
	if d.BanType != "ip" {
		t.Fatal("missing IP ban metadata")
	}
	request("198.51.100.88", "POST", "/api/v1/auth/register", "", map[string]string{}, 403)
	request("198.51.100.88", "POST", "/api/v1/auth/ban/status", "", map[string]string{"appeal_ticket": d.AppealTicket}, 200)
	request("203.0.113.10", "POST", "/api/v1/admin/bans", f.owner.Token, map[string]string{"network": "2001:db8:5::99/64", "reason": "IPv6 range"}, 201)
	request("2001:db8:5::123", "GET", "/api/v1/instance", "", nil, 403)
	request("2001:db8:6::123", "GET", "/api/v1/instance", "", nil, 200)
	request("203.0.113.10", "POST", "/api/v1/admin/bans", f.owner.Token, map[string]string{"network": "0.0.0.0/0", "reason": "would block owner"}, 403)
	request("203.0.113.10", "POST", "/api/v1/admin/bans", f.owner.Token, map[string]string{"network": "not-an-ip", "reason": "bad"}, 400)
	// An untrusted socket cannot select an arbitrary address through headers.
	boundary := &api{trustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	req := &http.Request{RemoteAddr: "192.0.2.5:1000", Header: http.Header{"X-Forwarded-For": []string{"203.0.113.100"}}}
	if boundary.remoteIP(req) != "192.0.2.5" {
		t.Fatal("untrusted proxy bypass")
	}
}
func TestModerationDisconnectsVoiceAndLimitsMessageDelete(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	alice := f.register(t, "alice")
	mod := f.register(t, "moderator")
	grantRole(t, f, mod, identity.ManageMessages|identity.KickMembers, 10)
	var room identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]string{"name": "voice", "kind": "voice"}, 201, &room)
	ws, _ := joinVoice(t, f, alice.Token, room.ID)
	moderate(t, f, mod, alice.User.ID, "kick", 204)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, _, err := ws.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatal("gateway did not disconnect promptly")
			}
			break
		}
	}
	deadline := time.Now().Add(time.Second)
	for features.Voice.Stats()["participants"].(int) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if features.Voice.Stats()["participants"].(int) != 0 {
		t.Fatal("voice participant survived kick")
	}
	ch := textChannel(t, f)
	m := sendMessage(t, f, f.owner.Token, ch.ID, "moderate me")
	f.call(t, "PUT", "/api/v1/conversations/"+ch.ID+"/messages/"+m.ID, mod.Token, map[string]string{"body": "rewrite author"}, 403, nil)
	f.call(t, "DELETE", "/api/v1/conversations/"+ch.ID+"/messages/"+m.ID, mod.Token, nil, 200, nil)
	var dm chat.Conversation
	f.call(t, "POST", "/api/v1/dms", f.owner.Token, map[string]any{"members": []string{mod.User.ID}}, 201, &dm)
	dmMessage := sendMessage(t, f, f.owner.Token, dm.ID, "private")
	f.call(t, "DELETE", "/api/v1/conversations/"+dm.ID+"/messages/"+dmMessage.ID, mod.Token, nil, 403, nil)
	accounting(t, f)
}
func TestModerationAuditSamplingAndRetention(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 20; i++ {
		f.call(t, "GET", "/api/v1/me", "invalid", nil, 401, nil)
	}
	var log auditResponse
	f.call(t, "GET", "/api/v1/admin/audit", f.owner.Token, nil, 200, &log)
	denied := 0
	for _, e := range log.Entries {
		if e.Action == "access.unauthorized" {
			denied++
		}
		if strings.Contains(e.Reason, "invalid") {
			t.Fatal("credentials logged")
		}
	}
	if denied != 1 {
		t.Fatal("access failures not sampled", denied)
	}
	_, err := f.pool.Exec(context.Background(), `INSERT INTO chatxo.audit_log(instance_id,action,created_at) SELECT $1,'test.old',now()-interval '91 days' FROM generate_series(1,100)`, f.cfg.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.identities.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	f.pool.QueryRow(context.Background(), `SELECT count(*) FROM chatxo.audit_log WHERE action='test.old'`).Scan(&n)
	if n != 0 {
		t.Fatal("audit retention failed")
	}
}
