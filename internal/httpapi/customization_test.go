package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/voice"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func artRequest(t *testing.T, f *fixture, method, path, token string, body []byte, want int) []byte {
	t.Helper()
	r, err := http.NewRequest(method, f.server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/octet-stream")
	response, err := f.server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if method == "GET" && response.StatusCode == 200 && bytes.HasPrefix(data, []byte("GIF")) && response.Header.Get("Content-Type") != "image/gif" {
		t.Fatal("GIF download has incorrect MIME type")
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s status %d want %d", method, path, response.StatusCode, want)
	}
	return data
}
func TestCustomizationPersistenceAuthorizationAndImages(t *testing.T) {
	f := newFixture(t)
	alice := f.register(t, "alice")
	settings := map[string]any{"name": "Evening room", "description": "A quiet community", "accent_color": "#345678"}
	f.call(t, "PUT", "/api/v1/instance/settings", alice.Token, settings, 403, nil)
	f.call(t, "PUT", "/api/v1/instance/settings", f.owner.Token, settings, 200, nil)
	var info instanceResponse
	f.call(t, "GET", "/api/v1/instance", "", nil, 200, &info)
	if info.Name != "Evening room" || info.Description != "A quiet community" || info.AccentColor != "#345678" {
		t.Fatal("server customization missing from discovery")
	}
	profile := map[string]any{"display_name": "Alice Example", "bio": "Hello\nthere", "accent_color": "#abcdef"}
	f.call(t, "PUT", "/api/v1/me/profile", alice.Token, profile, 200, nil)
	var me identity.Principal
	f.call(t, "GET", "/api/v1/me", alice.Token, nil, 200, &me)
	if me.User.ID != alice.User.ID || me.User.Username != "alice" || me.User.DisplayName != "Alice Example" {
		t.Fatal("profile changed identity or was not saved")
	}
	profile["user_id"] = f.owner.User.ID
	f.call(t, "PUT", "/api/v1/me/profile", alice.Token, profile, 400, nil)
	delete(profile, "user_id")
	profile["accent_color"] = "url(https://invalid.test)"
	f.call(t, "PUT", "/api/v1/me/profile", alice.Token, profile, 400, nil)
	profile["accent_color"] = "#abcdef"
	profile["bio"] = strings.Repeat("x", 501)
	f.call(t, "PUT", "/api/v1/me/profile", alice.Token, profile, 400, nil)
	var owner identity.User
	f.call(t, "GET", "/api/v1/users/"+f.owner.User.ID, alice.Token, nil, 200, &owner)
	if owner.DisplayName != "" {
		t.Fatal("another account's profile changed")
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	artRequest(t, f, "PUT", "/api/v1/me/avatar", alice.Token, append(encoded.Bytes(), []byte("untrusted trailing data")...), 200)
	artRequest(t, f, "GET", "/api/v1/users/"+alice.User.ID+"/avatar", "", nil, 401)
	avatar := artRequest(t, f, "GET", "/api/v1/users/"+alice.User.ID+"/avatar", f.owner.Token, nil, 200)
	if bytes.Contains(avatar, []byte("untrusted trailing data")) {
		t.Fatal("uploaded image was not canonicalized")
	}
	if _, err := png.Decode(bytes.NewReader(avatar)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"icon", "banner"} {
		path := "/api/v1/instance/" + kind
		artRequest(t, f, "PUT", path, alice.Token, encoded.Bytes(), 403)
		artRequest(t, f, "PUT", path, f.owner.Token, encoded.Bytes(), 200)
		artRequest(t, f, "GET", path, "", nil, 200)
	}
	artRequest(t, f, "PUT", "/api/v1/me/avatar", alice.Token, []byte("not an image"), 400)
	artRequest(t, f, "PUT", "/api/v1/me/avatar", alice.Token, make([]byte, (2<<20)+1), 413)
	var wide bytes.Buffer
	if err := png.Encode(&wide, image.NewRGBA(image.Rect(0, 0, 1025, 1))); err != nil {
		t.Fatal(err)
	}
	artRequest(t, f, "PUT", "/api/v1/me/avatar", alice.Token, wide.Bytes(), 400)
	restarted, err := identity.New(context.Background(), f.cfg, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := restarted.Community(context.Background(), "Changed config default")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Name != "Evening room" || persisted.IconVersion == "" || persisted.BannerVersion == "" {
		t.Fatal("server settings did not persist independently of bootstrap config")
	}
	user, err := restarted.Profile(context.Background(), alice.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if user.DisplayName != "Alice Example" || user.AvatarVersion == "" {
		t.Fatal("profile did not persist")
	}
	f.call(t, "DELETE", "/api/v1/me/avatar", alice.Token, nil, 204, nil)
	artRequest(t, f, "GET", "/api/v1/users/"+alice.User.ID+"/avatar", alice.Token, nil, 404)
	f.call(t, "DELETE", "/api/v1/instance/banner", alice.Token, nil, 403, nil)
	f.call(t, "DELETE", "/api/v1/instance/banner", f.owner.Token, nil, 204, nil)
	artRequest(t, f, "GET", "/api/v1/instance/banner", "", nil, 404)
	var channel identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "introductions", "kind": "text", "topic": "Say hello"}, 201, &channel)
	f.call(t, "PUT", "/api/v1/channels/"+channel.ID, f.owner.Token, map[string]any{"name": "welcome", "kind": "text"}, 200, &channel)
	if channel.Topic != "Say hello" {
		t.Fatal("legacy channel update erased topic")
	}
	f.call(t, "PUT", "/api/v1/channels/"+channel.ID, f.owner.Token, map[string]any{"name": "invalid", "kind": "text", "topic": strings.Repeat("x", 513)}, 400, nil)
	f.call(t, "GET", "/api/v1/channels/"+channel.ID, f.owner.Token, nil, 200, &channel)
	if channel.Name != "welcome" {
		t.Fatal("invalid customization partially modified channel")
	}
}

func TestVoicePresenceRespectsVisibilityAndTracksMembership(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	alice, bob := f.register(t, "alice"), f.register(t, "bob")
	var public, hidden identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "lounge", "kind": "voice"}, 201, &public)
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "private", "kind": "voice"}, 201, &hidden)
	var roles rolesResponse
	f.call(t, "GET", "/api/v1/roles", f.owner.Token, nil, 200, &roles)
	everyone := roles.Roles[0].ID
	f.call(t, "PUT", "/api/v1/channels/"+hidden.ID+"/permissions/"+everyone, f.owner.Token, map[string]any{"allow": 0, "deny": 1}, 204, nil)
	_, _ = joinVoice(t, f, f.owner.Token, hidden.ID)
	aliceWS, peer := joinVoice(t, f, alice.Token, public.ID)
	observer := wsConnect(t, f, bob.Token)
	wsSend(t, observer, map[string]any{"type": "voice.watch"})
	wsRead(t, observer, "voice.watching")
	read := func() map[string][]voice.PresenceMember {
		t.Helper()
		event := wsRead(t, observer, "voice.presence")
		var rooms map[string][]voice.PresenceMember
		if err := json.Unmarshal(event["rooms"], &rooms); err != nil {
			t.Fatal(err)
		}
		return rooms
	}
	rooms := read()
	if len(rooms[public.ID]) != 1 || rooms[public.ID][0].ID != peer.ID || len(rooms[hidden.ID]) != 0 {
		t.Fatal("presence missing member or exposing private room")
	}
	joined := rooms[public.ID][0].JoinedAt
	if joined.IsZero() || joined.Location() != time.UTC || time.Since(joined) < 0 || time.Since(joined) > 10*time.Second || !joined.Equal(peer.JoinedAt) {
		t.Fatal("join timestamp missing, non-UTC or inconsistent")
	}
	wsSend(t, aliceWS, map[string]any{"type": "media.state", "state": voice.State{Muted: true, CameraOff: true, ScreenOff: true}})
	wsRead(t, aliceWS, "media.state.accepted")
	updated := read()[public.ID][0]
	if !updated.JoinedAt.Equal(joined) {
		t.Fatal("state update reset the join time")
	}
	if !updated.State.Muted {
		t.Fatal("presence did not update mute state")
	}
	wsSend(t, aliceWS, map[string]any{"type": "media.leave"})
	wsRead(t, aliceWS, "media.left")
	if len(read()) != 0 {
		t.Fatal("presence retained a departed participant")
	}
	_, _ = joinVoice(t, f, alice.Token, public.ID)
	rejoined := read()[public.ID]
	if len(rejoined) == 1 && !rejoined[0].JoinedAt.After(joined) {
		t.Fatal("fresh join retained old timestamp")
	}
	if len(rejoined) != 1 {
		t.Fatal("new join was not visible")
	}
	f.call(t, "PUT", "/api/v1/channels/"+public.ID+"/permissions/"+everyone, f.owner.Token, map[string]any{"allow": 0, "deny": 1}, 204, nil)
	if len(read()) != 0 {
		t.Fatal("presence retained permission-revoked room")
	}
}
