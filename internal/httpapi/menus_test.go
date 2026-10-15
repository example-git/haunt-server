package httpapi

import (
	"encoding/json"
	"github.com/example-git/haunt-server/internal/identity"
	"strings"
	"testing"
)

func TestServerProvidedMenusPermissionsAndForms(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	member := f.register(t, "menu_member")
	var menu MenuManifest
	f.call(t, "GET", "/api/v1/menus/server", f.owner.Token, nil, 200, &menu)
	if menu.Version != 1 || len(menu.Pages) < 8 {
		t.Fatalf("missing owner menus: %+v", menu)
	}
	var form *MenuForm
	for _, page := range menu.Pages {
		if page.ID == "configuration" {
			form = page.Form
		}
	}
	if form == nil || form.LoadPath != "/api/v1/instance/configuration" || len(form.Fields) == 0 || form.CarryFields[0] != "revision" {
		t.Fatal("configuration isn't server described")
	}
	encoded, err := json.Marshal(menu)
	if err != nil || strings.Contains(string(encoded), "server_upload_bytes_per_day") || strings.Contains(string(encoded), "server_download_bytes_per_day") {
		t.Fatal("removed server-wide DM allowances still exposed")
	}
	if !strings.Contains(string(encoded), "settings.limits.dm.upload_bytes_per_day") || !strings.Contains(string(encoded), "settings.limits.dm.download_bytes_per_day") {
		t.Fatal("per-account transfer limits missing")
	}
	f.call(t, "GET", "/api/v1/menus/server", member.Token, nil, 200, &menu)
	if len(menu.Pages) != 0 {
		t.Fatal("management menus exposed to member")
	}
	channel := textChannel(t, f)
	f.call(t, "GET", "/api/v1/channels/"+channel.ID+"/menu", f.owner.Token, nil, 200, &menu)
	if len(menu.Pages) != 3 || menu.Pages[2].Action.Path != "/api/v1/channels/"+channel.ID || menu.Pages[2].Action.ConfirmationValue != channel.Name {
		t.Fatal("channel context missing")
	}
	f.call(t, "GET", "/api/v1/channels/"+channel.ID+"/menu", member.Token, nil, 200, &menu)
	if len(menu.Pages) != 0 {
		t.Fatal("channel actions exposed")
	}
	f.call(t, "GET", "/api/v1/menus/server", "", nil, 401, nil)
	f.call(t, "GET", "/api/v1/channels/"+identity.NewID()+"/menu", f.owner.Token, nil, 404, nil)
}
