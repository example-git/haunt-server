package httpapi

import (
	"strings"
	"testing"

	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/serverextension"
)

func TestServerExtensionAdministratorAndConfigurationMenus(t *testing.T) {
	for _, principal := range []identity.Principal{
		{User: identity.User{Owner: true}},
		{Permissions: identity.Administrator},
		{Permissions: identity.ManageConfiguration},
	} {
		found := map[string]bool{}
		for _, page := range serverMenu(principal).Pages {
			found[page.ID] = true
		}
		if !found["server-extensions"] || !found["audio-plugins"] {
			t.Fatal("extension and voice plugin menus missing or not separate")
		}
	}
	for _, page := range serverMenu(identity.Principal{Permissions: identity.ManageServer}).Pages {
		if page.ID == "server-extensions" {
			t.Fatal("appearance manager received configuration access")
		}
	}
}

func TestServerExtensionConfigurationAuthorizationAndCredentialRedaction(t *testing.T) {
	fixture, _ := featureFixture(t, 256_000_000)
	member := fixture.register(t, "extension_member")
	path := "/api/v1/admin/server-extensions/tiktok-tts"
	fixture.call(t, "GET", path, member.Token, nil, 403, nil)
	fixture.call(t, "PUT", path, member.Token, map[string]any{}, 403, nil)
	fixture.call(t, "POST", path+"/test", member.Token, map[string]any{"text": "hello", "voice": "en_us_001"}, 403, nil)
	fixture.call(t, "GET", "/api/v1/server-extensions", "", nil, 401, nil)
	var configuration serverextension.Configuration
	token := strings.Repeat("a", 32)
	fixture.call(t, "PUT", path, fixture.owner.Token, map[string]any{"revision": 0, "endpoint": serverextension.PrimaryEndpoint, "session_token": token, "enabled": false}, 200, &configuration)
	status, body, err := fixture.request("GET", path, fixture.owner.Token, nil)
	if err != nil || status != 200 || strings.Contains(string(body), token) || strings.Contains(string(body), "session_token") {
		t.Fatal("configuration leaked credential")
	}
	var catalog ServerExtensionsResponse
	fixture.call(t, "GET", "/api/v1/server-extensions", member.Token, nil, 200, &catalog)
	if len(catalog.Extensions) != 1 || !catalog.Extensions[0].Configured || catalog.Extensions[0].Enabled {
		t.Fatal("incorrect extension catalog")
	}
	fixture.call(t, "POST", "/api/v1/server-extensions/tiktok-tts/speech", member.Token, map[string]any{"text": "hello", "voice": "en_us_001"}, 503, nil)
	grantRole(t, fixture, member, identity.ManageConfiguration, 10)
	fixture.call(t, "GET", path, member.Token, nil, 200, &configuration)
	fixture.call(t, "PUT", path, member.Token, map[string]any{"revision": configuration.Revision, "endpoint": serverextension.PrimaryEndpoint, "enabled": true}, 200, &configuration)
	if !configuration.Enabled || !configuration.Configured {
		t.Fatal("configuration manager could not enable the existing extension")
	}
	fixture.call(t, "POST", path+"/test", member.Token, map[string]any{}, 400, nil)
	fixture.call(t, "PUT", "/api/v1/members/"+member.User.ID+"/roles", fixture.owner.Token, map[string]any{"role_ids": []string{}}, 204, nil)
	fixture.call(t, "GET", path, member.Token, nil, 403, nil)
	fixture.call(t, "PUT", path, member.Token, map[string]any{}, 403, nil)
	fixture.call(t, "POST", path+"/test", member.Token, map[string]any{}, 403, nil)
}
