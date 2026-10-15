package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/platform/postgres"
	"github.com/example-git/haunt-server/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testPassword = "a long test passphrase"

type fixture struct {
	cfg        config.Config
	pool       *pgxpool.Pool
	identities *identity.Service
	server     *httptest.Server
	owner      identity.Login
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool, name := testutil.Database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := config.Config{InstanceID: name, Name: "Integration community", PublicURL: "http://localhost", ClientOrigins: []string{"http://localhost", "https://client.example.test"}, BootstrapUsername: "owner", BootstrapPassword: testPassword, SessionDays: 30, AuthParallel: 4}
	if err := postgres.Migrate(ctx, pool, cfg.InstanceID); err != nil {
		t.Fatal(err)
	}
	identities, err := identity.New(ctx, cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(cfg, identities, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler)
	t.Cleanup(server.Close)
	f := &fixture{cfg: cfg, pool: pool, identities: identities, server: server}
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "OWNER", "password": testPassword, "device_name": "owner desktop"}, 200, &f.owner)
	return f
}

func (f *fixture) request(method, path, token string, body any) (int, []byte, error) {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}
	req, err := http.NewRequest(method, f.server.URL+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := f.server.Client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err = io.ReadAll(response.Body)
	return response.StatusCode, data, err
}

func (f *fixture) call(t *testing.T, method, path, token string, body any, want int, out any) {
	t.Helper()
	status, data, err := f.request(method, path, token, body)
	if err != nil {
		t.Fatal(err)
	}
	// Never print successful authentication payloads: they contain tokens.
	if status != want {
		t.Fatalf("%s %s: status %d, want %d", method, path, status, want)
	}
	validateDocumentedResponse(t, method, path, status, data)
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: invalid response: %v", method, path, err)
		}
	}
}
func (f *fixture) invite(t *testing.T, uses int) identity.Invite {
	t.Helper()
	var v identity.Invite
	f.call(t, "POST", "/api/v1/invites", f.owner.Token, map[string]any{"uses": uses, "expires_in_hours": 24}, 201, &v)
	return v
}
func (f *fixture) register(t *testing.T, name string) identity.Login {
	t.Helper()
	invite := f.invite(t, 1)
	var login identity.Login
	f.call(t, "POST", "/api/v1/auth/register", "", map[string]any{"username": name, "password": testPassword, "device_name": "test device", "invitation": invite.Token}, 201, &login)
	return login
}

func TestIdentityLifecycleAndIsolation(t *testing.T) {
	f := newFixture(t)
	f.call(t, "GET", "/healthz", "", nil, 200, nil)
	f.call(t, "GET", "/readyz", "", nil, 200, nil)
	f.call(t, "GET", "/api/v1/me", "", nil, 401, nil)
	f.call(t, "POST", "/api/v1/auth/register", "", map[string]any{"username": "guest", "password": testPassword, "device_name": "phone"}, 400, nil)
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "owner", "password": "wrong password", "device_name": "phone"}, 401, nil)
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "missing", "password": "wrong password", "device_name": "phone"}, 401, nil)
	alice := f.register(t, "alice")
	var second identity.Login
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "alice", "password": testPassword, "device_name": "second device"}, 200, &second)
	var sessions struct {
		Sessions []identity.Session `json:"sessions"`
	}
	f.call(t, "GET", "/api/v1/sessions", alice.Token, nil, 200, &sessions)
	if len(sessions.Sessions) != 2 {
		t.Fatalf("got %d sessions", len(sessions.Sessions))
	}
	f.call(t, "DELETE", "/api/v1/sessions/"+f.owner.Session.ID, alice.Token, nil, 404, nil)
	f.call(t, "DELETE", "/api/v1/sessions/"+alice.Session.ID, second.Token, nil, 204, nil)
	f.call(t, "GET", "/api/v1/me", alice.Token, nil, 401, nil)
	f.call(t, "GET", "/api/v1/me", second.Token, nil, 200, nil)

	// Reinitialization preserves owner credentials and tokens; config identity
	// mismatch is rejected rather than relabeling an existing installation.
	ctx := context.Background()
	if err := postgres.Migrate(ctx, f.pool, f.cfg.InstanceID); err != nil {
		t.Fatal(err)
	}
	cfg := f.cfg
	cfg.BootstrapUsername = "replacement"
	cfg.BootstrapPassword = "replacement passphrase"
	if _, err := identity.New(ctx, cfg, f.pool); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, f.pool, "wrong-instance"); err == nil {
		t.Fatal("accepted installation identity mismatch")
	}
	f.call(t, "GET", "/api/v1/me", f.owner.Token, nil, 200, nil)

	other := newFixture(t)
	other.call(t, "GET", "/api/v1/me", f.owner.Token, nil, 401, nil)
	f.call(t, "GET", "/api/v1/me", other.owner.Token, nil, 401, nil)
	var channel identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "general", "kind": "text"}, 201, &channel)
	other.call(t, "GET", "/api/v1/channels/"+channel.ID, other.owner.Token, nil, 404, nil)
	if _, err := f.pool.Exec(ctx, `UPDATE chatxo.sessions SET expires_at=now()-interval '1 second' WHERE id=$1`, second.Session.ID); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", "/api/v1/me", second.Token, nil, 401, nil)
	if err := f.identities.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM chatxo.sessions WHERE id=$1`, second.Session.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired session cleanup failed: count=%d err=%v", count, err)
	}
	f.call(t, "POST", "/api/v1/auth/logout", f.owner.Token, nil, 204, nil)
	f.call(t, "GET", "/api/v1/me", f.owner.Token, nil, 401, nil)
	f.pool.Close()
	f.call(t, "GET", "/readyz", "", nil, 503, nil)
}

func TestInvitationRaceAndRollback(t *testing.T) {
	f := newFixture(t)
	invite := f.invite(t, 1)
	type result struct {
		status int
		err    error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, _, err := f.request("POST", "/api/v1/auth/register", "", map[string]any{"username": name, "password": testPassword, "device_name": "racing device", "invitation": invite.Token})
			results <- result{status, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	statuses := map[int]int{}
	for v := range results {
		if v.err != nil {
			t.Fatal(v.err)
		}
		statuses[v.status]++
	}
	if statuses[201] != 1 || statuses[400] != 1 {
		t.Fatalf("single-use invitation race returned %v", statuses)
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM chatxo.users WHERE NOT is_owner`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("race created %d accounts: %v", count, err)
	}
	// A failed unique-username insert must roll back invitation consumption.
	reusable := f.invite(t, 1)
	f.call(t, "POST", "/api/v1/auth/register", "", map[string]any{"username": "owner", "password": testPassword, "device_name": "duplicate", "invitation": reusable.Token}, 409, nil)
	f.call(t, "POST", "/api/v1/auth/register", "", map[string]any{"username": "third", "password": testPassword, "device_name": "valid", "invitation": reusable.Token}, 201, nil)
	revoked := f.invite(t, 1)
	f.call(t, "DELETE", "/api/v1/invites/"+revoked.ID, f.owner.Token, nil, 204, nil)
	f.call(t, "POST", "/api/v1/auth/register", "", map[string]any{"username": "fourth", "password": testPassword, "device_name": "valid", "invitation": revoked.Token}, 400, nil)
	expired := f.invite(t, 1)
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.invites SET expires_at=now()-interval '1 second' WHERE id=$1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	f.call(t, "POST", "/api/v1/auth/register", "", map[string]any{"username": "expired", "password": testPassword, "device_name": "valid", "invitation": expired.Token}, 400, nil)
}

func TestCommunityPermissionChanges(t *testing.T) {
	f := newFixture(t)
	alice := f.register(t, "alice")
	bob := f.register(t, "bob")
	var roles struct {
		Roles []identity.Role `json:"roles"`
	}
	f.call(t, "GET", "/api/v1/roles", f.owner.Token, nil, 200, &roles)
	everyone := roles.Roles[0]
	var category identity.Category
	f.call(t, "POST", "/api/v1/categories", f.owner.Token, map[string]any{"name": "Friends"}, 201, &category)
	var channel identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "general", "kind": "text", "category_id": category.ID}, 201, &channel)
	f.call(t, "GET", "/api/v1/channels/"+channel.ID, alice.Token, nil, 200, nil)
	f.call(t, "POST", "/api/v1/roles", alice.Token, map[string]any{"name": "elevated", "permissions": 31}, 403, nil)
	f.call(t, "POST", "/api/v1/channels", alice.Token, map[string]any{"name": "forbidden", "kind": "voice"}, 403, nil)
	f.call(t, "POST", "/api/v1/invites", alice.Token, map[string]any{"uses": 1, "expires_in_hours": 24}, 403, nil)
	f.call(t, "PUT", "/api/v1/categories/"+category.ID+"/permissions/"+everyone.ID, f.owner.Token, map[string]any{"allow": 0, "deny": 1}, 204, nil)
	f.call(t, "GET", "/api/v1/channels/"+channel.ID, alice.Token, nil, 404, nil)
	var categories struct {
		Categories []identity.Category `json:"categories"`
	}
	f.call(t, "GET", "/api/v1/categories", alice.Token, nil, 200, &categories)
	if len(categories.Categories) != 0 {
		t.Fatal("hidden category leaked")
	}
	var friend identity.Role
	f.call(t, "POST", "/api/v1/roles", f.owner.Token, map[string]any{"name": "friend", "permissions": 8}, 201, &friend)
	f.call(t, "PUT", "/api/v1/members/"+alice.User.ID+"/roles", f.owner.Token, map[string]any{"role_ids": []string{friend.ID}}, 204, nil)
	f.call(t, "PUT", "/api/v1/channels/"+channel.ID+"/permissions/"+friend.ID, f.owner.Token, map[string]any{"allow": 1, "deny": 0}, 204, nil)
	f.call(t, "GET", "/api/v1/channels/"+channel.ID, alice.Token, nil, 200, &channel)
	if channel.Permissions&identity.ViewChannel == 0 {
		t.Fatal("channel override did not restore category-denied read")
	}
	f.call(t, "GET", "/api/v1/channels/"+channel.ID, bob.Token, nil, 404, nil)
	f.call(t, "POST", "/api/v1/invites", alice.Token, map[string]any{"uses": 1, "expires_in_hours": 24}, 201, nil)
	// Same-scope deny wins over a grant from another assigned role.
	var muted identity.Role
	f.call(t, "POST", "/api/v1/roles", f.owner.Token, map[string]any{"name": "muted", "permissions": 0}, 201, &muted)
	f.call(t, "PUT", "/api/v1/channels/"+channel.ID+"/permissions/"+muted.ID, f.owner.Token, map[string]any{"allow": 0, "deny": 1}, 204, nil)
	f.call(t, "PUT", "/api/v1/members/"+alice.User.ID+"/roles", f.owner.Token, map[string]any{"role_ids": []string{friend.ID, muted.ID}}, 204, nil)
	f.call(t, "GET", "/api/v1/channels/"+channel.ID, alice.Token, nil, 404, nil)
	f.call(t, "GET", "/api/v1/channels/"+channel.ID, f.owner.Token, nil, 200, nil)
	// Existing bearer tokens see new permissions without re-login.
	f.call(t, "PUT", "/api/v1/roles/"+friend.ID, f.owner.Token, map[string]any{"name": "friend", "permissions": 0}, 200, nil)
	f.call(t, "POST", "/api/v1/invites", alice.Token, map[string]any{"uses": 1, "expires_in_hours": 24}, 403, nil)
	f.call(t, "PUT", "/api/v1/members/"+f.owner.User.ID+"/roles", f.owner.Token, map[string]any{"role_ids": []string{}}, 403, nil)
	f.call(t, "DELETE", "/api/v1/roles/"+everyone.ID, f.owner.Token, nil, 404, nil)
	f.call(t, "PUT", "/api/v1/channels/"+channel.ID+"/permissions/"+friend.ID, f.owner.Token, map[string]any{"allow": 1, "deny": 1}, 400, nil)
	f.call(t, "GET", "/api/v1/channels/not-a-uuid", f.owner.Token, nil, 400, nil)
	f.call(t, "GET", "/api/v1/members", f.owner.Token, nil, 200, nil)
	f.call(t, "GET", "/api/v1/members", alice.Token, nil, 403, nil)
	f.call(t, "GET", "/api/v1/channels/"+channel.ID+"/permissions", f.owner.Token, nil, 200, nil)
	f.call(t, "DELETE", "/api/v1/channels/"+channel.ID, f.owner.Token, nil, 204, nil)
	f.call(t, "DELETE", "/api/v1/categories/"+category.ID, f.owner.Token, nil, 204, nil)
}

func TestHTTPBoundaries(t *testing.T) {
	f := newFixture(t)
	req, err := http.NewRequest("OPTIONS", f.server.URL+"/api/v1/auth/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://client.example.test")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "https://client.example.test" {
		t.Fatal("allowed-origin preflight failed")
	}
	req.Header.Set("Origin", "https://disallowed.example.test")
	resp, err = f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("disallowed origin accepted")
	}
	req, err = http.NewRequest("GET", f.server.URL+"/api/v1/me?token="+f.owner.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("accepted URL bearer token")
	}
	for _, body := range []string{`{"username":"owner","password":"wrong","device_name":"test","unknown":true}`, `{} {}`, "{\"username\":\"" + string(bytes.Repeat([]byte("x"), 17<<10)) + "\"}"} {
		req, err := http.NewRequest("POST", f.server.URL+"/api/v1/auth/login", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatal("malformed/oversized request accepted")
		}
	}
	// Bounded login work and rate limits return explicit failures.
	for i := 0; i < 11; i++ {
		f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": "!invalid", "password": "wrong", "device_name": "test"}, func() int {
			if i < 10 {
				return 400
			}
			return 429
		}(), nil)
	}
}

func TestConcurrentSessionLimit(t *testing.T) {
	f := newFixture(t)
	// Leave one session slot. Two real password logins must compete for that
	// slot under the account's row lock, not overshoot the cap.
	_, err := f.pool.Exec(context.Background(), `INSERT INTO chatxo.sessions(id,instance_id,user_id,token_hash,device_name,expires_at)
		SELECT gen_random_uuid(),$1,$2,convert_to(md5(random()::text),'UTF8'),'seeded',now()+interval '1 day' FROM generate_series(1,18)`, f.cfg.InstanceID, f.owner.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan int, 2)
	errors := make(chan error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, _, err := f.request("POST", "/api/v1/auth/login", "", map[string]any{"username": "owner", "password": testPassword, "device_name": "racing login"})
			results <- status
			errors <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	statuses := map[int]int{}
	for status := range results {
		statuses[status]++
	}
	if statuses[200] != 1 || statuses[409] != 1 {
		t.Fatalf("session limit race returned %v", statuses)
	}
	var sessions struct {
		Sessions []identity.Session `json:"sessions"`
	}
	f.call(t, "GET", "/api/v1/sessions", f.owner.Token, nil, 200, &sessions)
	if len(sessions.Sessions) != 20 {
		t.Fatalf("got %d sessions, want 20", len(sessions.Sessions))
	}
}
