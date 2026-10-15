package config

import "testing"

func TestClientUpdateConfiguration(t *testing.T) {
	t.Setenv("HAUNT_CLIENT_UPDATE_LOCAL", "false")
	t.Setenv("HAUNT_CLIENT_UPDATE_INSTALLERS", "true")
	t.Setenv("HAUNT_CLIENT_UPDATE_MANIFEST_URL", "https://downloads.example.test/haunt-updates.json")
	var c Config
	if err := source(Environment()).loadClientUpdates(&c); err != nil || c.ClientUpdateManifestURL == "" || !c.ClientUpdateInstallers || c.ClientUpdateLocal {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{"", "http://download.test/a.json", "https://user:secret@download.test/a.json", "https://download.test/a.json?token=secret", "https://download.test/a.json#fragment", "https://download.test:443/a.json", "file:///a.json"} {
		t.Setenv("HAUNT_CLIENT_UPDATE_MANIFEST_URL", bad)
		if source(Environment()).loadClientUpdates(&c) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
