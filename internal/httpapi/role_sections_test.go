package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/example-git/haunt-server/internal/identity"
)

func TestRoleSectionsPersistenceSelectionAndAuthorization(t *testing.T) {
	f := newFixture(t)
	alice := f.register(t, "alice")
	admin := f.register(t, "delegate")
	adminRole := grantRole(t, f, admin, identity.ManageRoles, 50)
	create := func(name string, rank int, display bool) identity.Role {
		var role identity.Role
		f.call(t, "POST", "/api/v1/roles", f.owner.Token, map[string]any{"name": name, "position": rank, "permissions": 0, "display_separately": display}, 201, &role)
		if role.DisplaySeparately != display {
			t.Fatal("display setting not returned")
		}
		return role
	}
	lower := create("Hosts", 10, true)
	higher := create("Organizers", 20, true)
	hidden := create("Unlisted", 30, false)
	check := func(want *identity.Role) {
		t.Helper()
		var profile identity.User
		f.call(t, "GET", "/api/v1/users/"+alice.User.ID, alice.Token, nil, 200, &profile)
		if want == nil {
			if profile.DisplayRole != nil {
				t.Fatal("unexpected section")
			}
			return
		}
		if profile.DisplayRole == nil || profile.DisplayRole.ID != want.ID || profile.DisplayRole.Name != want.Name || profile.DisplayRole.Position != want.Position {
			t.Fatalf("incorrect display role: %+v", profile.DisplayRole)
		}
		var page usersResponse
		f.call(t, "GET", "/api/v1/users", alice.Token, nil, 200, &page)
		for _, user := range page.Users {
			if user.ID == alice.User.ID && user.DisplayRole != nil && *user.DisplayRole == *profile.DisplayRole {
				return
			}
		}
		t.Fatal("directory and individual profile disagree")
	}
	check(nil)
	f.call(t, "PUT", "/api/v1/members/"+alice.User.ID+"/roles", f.owner.Token, map[string]any{"role_ids": []string{lower.ID, higher.ID, hidden.ID}}, 204, nil)
	check(&higher)
	// The public roster exposes only the chosen section's display metadata.
	_, raw, err := f.request("GET", "/api/v1/users/"+alice.User.ID, alice.Token, nil)
	if err != nil || strings.Contains(string(raw), "permissions") || strings.Contains(string(raw), "role_ids") || strings.Contains(string(raw), hidden.ID) {
		t.Fatal("non-display role data leaked into public profile")
	}
	// Ordinary members and equal/higher-ranked delegates cannot change the flag.
	update := map[string]any{"name": lower.Name, "permissions": 0, "display_separately": false}
	f.call(t, "PUT", "/api/v1/roles/"+lower.ID, alice.Token, update, 403, nil)
	f.call(t, "PUT", "/api/v1/roles/"+adminRole.ID, admin.Token, update, 403, nil)
	f.call(t, "PUT", "/api/v1/roles/"+higher.ID, admin.Token, map[string]any{"name": higher.Name, "permissions": 0, "display_separately": false}, 200, nil)
	check(&lower)
	// Older clients omit the new option; other edits must preserve its value.
	lower.Name = "Renamed hosts"
	f.call(t, "PUT", "/api/v1/roles/"+lower.ID, f.owner.Token, map[string]any{"name": lower.Name, "permissions": 0}, 200, nil)
	check(&lower)
	restarted, err := identity.New(context.Background(), f.cfg, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	roles, err := restarted.Roles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var everyone identity.Role
	found := false
	for _, role := range roles {
		if role.Default {
			everyone = role
		}
		if role.ID == lower.ID {
			found = role.DisplaySeparately && role.Position == lower.Position
		}
	}
	if !found {
		t.Fatal("flag did not persist")
	}
	// Rank changes immediately change which enabled role represents the user.
	f.call(t, "PUT", "/api/v1/roles/"+higher.ID, f.owner.Token, map[string]any{"name": higher.Name, "permissions": 0, "position": 5, "display_separately": true}, 200, nil)
	check(&lower)
	f.call(t, "DELETE", "/api/v1/roles/"+lower.ID, f.owner.Token, nil, 204, nil)
	higher.Position = 5
	check(&higher)
	f.call(t, "PUT", "/api/v1/members/"+alice.User.ID+"/roles", f.owner.Token, map[string]any{"role_ids": []string{}}, 204, nil)
	check(nil)
	// The implicit everyone role may also opt in, without explicit assignments.
	f.call(t, "PUT", "/api/v1/roles/"+everyone.ID, f.owner.Token, map[string]any{"name": everyone.Name, "permissions": everyone.Permissions, "display_separately": true}, 200, nil)
	check(&everyone)
	f.call(t, "PUT", "/api/v1/roles/"+everyone.ID, f.owner.Token, map[string]any{"name": everyone.Name, "permissions": everyone.Permissions, "display_separately": "yes"}, 400, nil)
	check(&everyone)
}
