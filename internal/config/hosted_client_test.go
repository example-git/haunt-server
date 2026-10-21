package config

import (
	"os"
	"slices"
	"testing"
)

func TestActionsHostedClientOrigin(t *testing.T) {
	if expected := os.Getenv("HAUNT_WEB_CLIENT_ORIGIN"); expected != "" && HostedClientOrigin != expected {
		t.Fatal("Actions build omitted the hosted client origin")
	}
}

func TestBuiltInWebClientOrigin(t *testing.T) {
	previous := HostedClientOrigin
	t.Cleanup(func() { HostedClientOrigin = previous })
	HostedClientOrigin = "https://client.example.test"
	values := map[string]string{"HAUNT_INSTANCE_ID": "test", "HAUNT_PUBLIC_URL": "https://community.example.test", "HAUNT_DATABASE_URL": "postgres://fixture", "HAUNT_CLIENT_ORIGINS": "https://other.example.test"}
	configuration, err := LoadValues(values)
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{HostedClientOrigin, "https://community.example.test", "https://other.example.test"} {
		if !slices.Contains(configuration.ClientOrigins, origin) {
			t.Fatalf("missing allowed origin %q", origin)
		}
	}
	HostedClientOrigin = "https://client.example.test/path"
	if _, err := LoadValues(values); err == nil {
		t.Fatal("invalid built-in origin accepted")
	}
}
