package httpapi

import (
	"strings"
	"testing"

	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/serverextension"
)

func TestServerExtensionOwnerMenuIsSeparateFromVoicePlugins(t *testing.T) {
	owner := serverMenu(identity.Principal{User: identity.User{Owner: true}})
	found := map[string]bool{}
	for _, page := range owner.Pages {
		found[page.ID] = true
	}
	if !found["server-extensions"] || !found["audio-plugins"] {
		t.Fatal("extension and voice plugin menus not separate")
	}
	admin := serverMenu(identity.Principal{Permissions: identity.ManageConfiguration})
	for _, page := range admin.Pages {
		if page.ID == "server-extensions" {
			t.Fatal("owner extension menu exposed to delegated administrator")
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
}
