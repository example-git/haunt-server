package config

import (
	"os"
	"strings"
	"testing"
)

func TestIdentityConfiguration(t *testing.T) {
	// Isolate tests from any developer/deployment environment settings.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "HAUNT_") {
			value := os.Getenv(key)
			t.Cleanup(func() { os.Setenv(key, value) })
			os.Unsetenv(key)
		}
	}
	t.Setenv("HAUNT_INSTANCE_ID", "test-installation")
	t.Setenv("HAUNT_PUBLIC_URL", "http://localhost:8080")
	t.Setenv("HAUNT_DATABASE_URL", "postgres://localhost/test")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SessionDays != 30 || c.AuthParallel != 4 {
		t.Fatal("unexpected authentication defaults")
	}
	for _, v := range []struct{ key, value string }{
		{"HAUNT_BOOTSTRAP_USERNAME", "owner"}, {"HAUNT_BOOTSTRAP_PASSWORD", "secret"},
		{"HAUNT_SESSION_DAYS", "0"}, {"HAUNT_SESSION_DAYS", "366"},
		{"HAUNT_AUTH_PARALLEL", "0"}, {"HAUNT_AUTH_PARALLEL", "33"},
		{"HAUNT_MEDIA_PORT_MIN", "1234"}, {"HAUNT_MEDIA_PORT_MAX", "1234"},
		{"HAUNT_MEDIA_PUBLIC_IP", "hostname.invalid"}, {"HAUNT_MEDIA_ROOM_LIMIT", "1"}, {"HAUNT_MEDIA_MAX_PEERS", "1"},
		{"HAUNT_ICE_SERVERS", "null"}, {"HAUNT_ICE_SERVERS", "[] []"}, {"HAUNT_ICE_SERVERS", `[{"urls":["stun:"]}]`}, {"HAUNT_ICE_SERVERS", `[{"urls":["turn:example.test"]}]`}, {"HAUNT_ICE_SERVERS", "not-json"}, {"HAUNT_ICE_SERVERS", `[{"urls":["https://invalid"]}]`},
		{"HAUNT_ICE_SERVERS", `[{"urls":[]}]`}, {"HAUNT_TRUSTED_PROXIES", "0.0.0.0/0"}, {"HAUNT_TRUSTED_PROXIES", "localhost"},
		{"HAUNT_TEXT_MAX_BYTES", "4095"}, {"HAUNT_IMAGE_MAX_BYTES", "1073741825"}, {"HAUNT_TEXT_RETENTION_DAYS", "0"},
		{"HAUNT_PUBLIC_URL", "http://untrusted.example"}, {"HAUNT_PUBLIC_URL", "https://example.test/path"},
		{"HAUNT_CLIENT_ORIGINS", "*"}, {"HAUNT_LISTEN_ADDR", "localhost:0"}, {"HAUNT_DB_MAX_CONNS", "1"},
		{"HAUNT_INSTANCE_ID", " padded "}, {"HAUNT_INSTANCE_ID", strings.Repeat("x", 129)},
	} {
		t.Run(v.key+"/"+v.value, func(t *testing.T) {
			t.Setenv(v.key, v.value)
			if _, err := Load(); err == nil {
				t.Fatal("accepted invalid explicit configuration")
			}
		})
	}
	t.Setenv("HAUNT_BOOTSTRAP_USERNAME", "owner")
	t.Setenv("HAUNT_BOOTSTRAP_PASSWORD", "long owner passphrase")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestOriginNormalization(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"https://EXAMPLE.test:443/", "https://example.test"},
		{"http://[::1]:80", "http://[::1]"},
		{"http://localhost:8080/", "http://localhost:8080"},
		{"http://192.168.1.117:8080/", "http://192.168.1.117:8080"},
		{"http://10.0.0.1:80", "http://10.0.0.1"},
		{"http://172.16.0.1:8080", "http://172.16.0.1:8080"},
		{"http://172.31.255.254:8080", "http://172.31.255.254:8080"},
		{"http://[fd00::1]:8080", "http://[fd00::1]:8080"},
		{"http://[::ffff:192.168.1.1]:8080", "http://[::ffff:192.168.1.1]:8080"},
	} {
		got, err := origin(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("%q => %q, %v", tc.input, got, err)
		}
	}
}

func TestHTTPOriginBoundary(t *testing.T) {
	for _, input := range []string{
		"http://8.8.8.8", "http://172.15.255.255", "http://172.32.0.1",
		"http://192.169.1.1", "http://169.254.169.254", "http://0.0.0.0",
		"http://[::]", "http://[fe80::1]", "http://[2001:db8::1]",
		"http://[::ffff:8.8.8.8]", "http://192.168.1.1.example.test",
		"http://192.168.1.1@public.example", "http://192.168.1.1/path",
		"http://192.168.1.1?token=secret", "http://192.168.1.1:0",
	} {
		if _, err := origin(input); err == nil {
			t.Errorf("accepted HTTP origin %q", input)
		}
	}
}

func TestRelayConfiguration(t *testing.T) {
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "HAUNT_") {
			value := os.Getenv(key)
			t.Cleanup(func() { os.Setenv(key, value) })
			os.Unsetenv(key)
		}
	}
	t.Setenv("HAUNT_RELAY_ENABLED", "true")
	var c Config
	if loadTURN(&c) == nil {
		t.Fatal("relay without public IP accepted")
	}
	t.Setenv("HAUNT_RELAY_PUBLIC_IP", "198.51.100.8")
	if err := loadTURN(&c); err != nil {
		t.Fatal(err)
	}
	if c.TURN.RelayMin != 49160 || c.TURN.RelayMax != 49999 || c.TURN.MaxAllocations != 512 {
		t.Fatal("relay defaults changed")
	}
	for _, tc := range []struct{ key, value string }{
		{"HAUNT_RELAY_ENABLED", "maybe"}, {"HAUNT_RELAY_PUBLIC_IP", "0.0.0.0"}, {"HAUNT_RELAY_LISTEN_ADDR", "bad"},
		{"HAUNT_RELAY_PORT_MAX", "49100"}, {"HAUNT_RELAY_PORT_MAX", "65535"}, {"HAUNT_RELAY_CREDENTIAL_MINUTES", "0"},
		{"HAUNT_RELAY_TLS_ADDR", "0.0.0.0:5349"}, {"HAUNT_RELAY_PUBLIC_HOST", "http://bad"}, {"HAUNT_RELAY_ALLOWED_PEER_CIDRS", "0.0.0.0/0"},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			var c Config
			if loadTURN(&c) == nil {
				t.Fatal("invalid relay setting accepted")
			}
		})
	}
}
