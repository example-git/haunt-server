package httpapi

import (
	"context"
	"github.com/example-git/haunt-server/internal/identity"
	"strings"
	"testing"
)

func TestChannelIconsPersistenceAndAuthorization(t *testing.T) {
	f := newFixture(t)
	member := f.register(t, "iconmember")
	body := map[string]any{"name": "games", "kind": "text", "icon": "gamepad-2"}
	f.call(t, "POST", "/api/v1/channels", member.Token, body, 403, nil)
	var ch identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, body, 201, &ch)
	id := ch.ID
	if ch.Icon != "gamepad-2" {
		t.Fatal("create did not return icon")
	}
	path := "/api/v1/channels/" + id
	for _, bad := range []any{"<svg onload=alert(1)>", "https://example.invalid/icon.svg", "unknown", strings.Repeat("x", 33), 1} {
		body["icon"] = bad
		f.call(t, "PUT", path, f.owner.Token, body, 400, nil)
	}
	body["icon"] = "music"
	f.call(t, "PUT", path, member.Token, body, 403, nil)
	f.call(t, "GET", path, member.Token, nil, 200, &ch)
	if ch.Icon != "gamepad-2" {
		t.Fatal("rejected update changed icon")
	}
	f.call(t, "PUT", path, f.owner.Token, body, 200, &ch)
	if ch.Icon != "music" {
		t.Fatal("update did not return icon")
	}
	delete(body, "icon")
	body["name"] = "renamed"
	f.call(t, "PUT", path, f.owner.Token, body, 200, &ch)
	if ch.Icon != "music" {
		t.Fatal("older client update erased icon")
	}
	restarted, err := identity.New(context.Background(), f.cfg, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := restarted.Authenticate(context.Background(), member.Token)
	if err != nil {
		t.Fatal(err)
	}
	channels, err := restarted.Channels(context.Background(), principal, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range channels {
		if c.ID == id {
			found = c.Icon == "music"
		}
	}
	if !found {
		t.Fatal("icon missing after service restart and directory read")
	}
	body["icon"] = ""
	ch = identity.Channel{}
	f.call(t, "PUT", path, f.owner.Token, body, 200, &ch)
	if ch.Icon != "" {
		t.Fatal("reset failed")
	}
	ch = identity.Channel{}
	f.call(t, "GET", path, member.Token, nil, 200, &ch)
	if ch.Icon != "" {
		t.Fatal("reset did not persist")
	}
}
