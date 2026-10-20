package httpapi

import (
	"testing"

	"github.com/example-git/haunt-server/internal/identity"
)

func TestAdministratorEffectivePermissionsAndRevocation(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	admin := f.register(t, "administrator")
	member := f.register(t, "ordinary_member")
	peer := f.register(t, "administrator_peer")
	manager := f.register(t, "role_manager")
	extension := "/api/v1/admin/server-extensions/tiktok-tts"
	f.call(t, "GET", extension, admin.Token, nil, 403, nil)
	role := grantRole(t, f, admin, identity.Administrator, 50)
	grantRole(t, f, peer, identity.Administrator, 50)
	grantRole(t, f, manager, identity.ManageRoles, 40)
	var principal identity.Principal
	f.call(t, "GET", "/api/v1/me", admin.Token, nil, 200, &principal)
	if principal.User.Owner || principal.Permissions != identity.AllPermissions || principal.Position != 50 {
		t.Fatal("administrator did not receive all permissions while retaining identity and rank", principal)
	}
	var menu, ownerMenu MenuManifest
	f.call(t, "GET", "/api/v1/menus/server", f.owner.Token, nil, 200, &ownerMenu)
	f.call(t, "GET", "/api/v1/menus/server", admin.Token, nil, 200, &menu)
	if len(menu.Pages) != len(ownerMenu.Pages) {
		t.Fatal("administrator menu omits owner permission pages")
	}
	for i, page := range ownerMenu.Pages {
		if menu.Pages[i].ID != page.ID {
			t.Fatal("administrator menu differs", menu.Pages[i].ID, page.ID)
		}
	}
	f.call(t, "GET", extension, admin.Token, nil, 200, nil)
	f.call(t, "PUT", extension, admin.Token, map[string]any{"revision": 0, "enabled": false, "endpoint": "https://api16-normal-useast5.us.tiktokv.com"}, 200, nil)
	f.call(t, "POST", extension+"/test", admin.Token, map[string]any{}, 400, nil)
	f.call(t, "GET", "/api/v1/admin/stats", admin.Token, nil, 200, nil)
	f.call(t, "POST", "/api/v1/admin/maintenance", admin.Token, nil, 204, nil)

	var roles rolesResponse
	f.call(t, "GET", "/api/v1/roles", admin.Token, nil, 200, &roles)
	var everyone identity.Role
	for _, value := range roles.Roles {
		if value.Default {
			everyone = value
		}
	}
	if everyone.ID == "" {
		t.Fatal("missing default role")
	}
	f.call(t, "PUT", "/api/v1/roles/"+everyone.ID, admin.Token, map[string]any{"name": everyone.Name, "permissions": 7}, 200, nil)
	var category identity.Category
	var channel identity.Channel
	f.call(t, "POST", "/api/v1/categories", admin.Token, map[string]any{"name": "Private"}, 201, &category)
	f.call(t, "POST", "/api/v1/channels", admin.Token, map[string]any{"name": "restricted", "kind": "text", "category_id": category.ID}, 201, &channel)
	for _, path := range []string{"/api/v1/categories/" + category.ID, "/api/v1/channels/" + channel.ID} {
		f.call(t, "PUT", path+"/permissions/"+everyone.ID, admin.Token, map[string]any{"allow": 0, "deny": 7}, 204, nil)
	}
	f.call(t, "GET", "/api/v1/channels/"+channel.ID, member.Token, nil, 404, nil)
	f.call(t, "GET", "/api/v1/channels/"+channel.ID, admin.Token, nil, 200, &channel)
	if channel.Permissions != identity.AllPermissions {
		t.Fatal("channel override defeated administrator", channel.Permissions)
	}
	sendMessage(t, f, admin.Token, channel.ID, "Administrator can send in a denied channel.")

	invite := f.invite(t, 1)
	var invitations invitesResponse
	f.call(t, "GET", "/api/v1/invites", admin.Token, nil, 200, &invitations)
	found := false
	for _, value := range invitations.Invites {
		found = found || value.ID == invite.ID
	}
	if !found {
		t.Fatal("administrator cannot see another creator's invitation")
	}
	f.call(t, "DELETE", "/api/v1/invites/"+invite.ID, admin.Token, nil, 204, nil)
	for _, target := range []string{f.owner.User.ID, peer.User.ID, admin.User.ID} {
		f.call(t, "PUT", "/api/v1/admin/members/"+target, admin.Token, map[string]bool{"disabled": true}, 403, nil)
	}
	f.call(t, "PUT", "/api/v1/admin/members/"+member.User.ID, admin.Token, map[string]bool{"disabled": true}, 204, nil)
	f.call(t, "GET", "/api/v1/me", member.Token, nil, 401, nil)
	f.call(t, "PUT", "/api/v1/admin/members/"+member.User.ID, admin.Token, map[string]bool{"disabled": false}, 204, nil)
	f.call(t, "POST", "/api/v1/auth/login", "", map[string]any{"username": member.User.Username, "password": testPassword, "device_name": "re-enabled"}, 200, nil)
	f.call(t, "POST", "/api/v1/roles", manager.Token, map[string]any{"name": "Escalation", "permissions": identity.Administrator, "position": 1}, 403, nil)
	f.call(t, "PUT", "/api/v1/roles/"+role.ID, admin.Token, map[string]any{"name": role.Name, "permissions": identity.AllPermissions, "position": 99}, 403, nil)

	// A token issued before promotion must also lose the grant immediately.
	f.call(t, "PUT", "/api/v1/members/"+admin.User.ID+"/roles", f.owner.Token, map[string]any{"role_ids": []string{}}, 204, nil)
	f.call(t, "GET", "/api/v1/me", admin.Token, nil, 200, &principal)
	if principal.Permissions&identity.Administrator != 0 {
		t.Fatal("administrator grant survived revocation")
	}
	f.call(t, "GET", "/api/v1/menus/server", admin.Token, nil, 200, &menu)
	if len(menu.Pages) != 0 {
		t.Fatal("administrator menus survived revocation")
	}
	f.call(t, "GET", extension, admin.Token, nil, 403, nil)
	f.call(t, "PUT", extension, admin.Token, map[string]any{}, 403, nil)
	f.call(t, "POST", extension+"/test", admin.Token, map[string]any{}, 403, nil)
	f.call(t, "GET", "/api/v1/admin/stats", admin.Token, nil, 403, nil)
	f.call(t, "GET", "/api/v1/channels/"+channel.ID, admin.Token, nil, 404, nil)
}
