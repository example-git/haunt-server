package config

import (
	"os"
	"strings"
	"testing"
)

func TestAutomaticMediaTransport(t *testing.T) {
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "HAUNT_") {
			value := os.Getenv(key)
			t.Cleanup(func() { os.Setenv(key, value) })
			os.Unsetenv(key)
		}
	}
	var c Config
	if err := source(Environment()).loadExternalMedia(&c); err != nil || c.MediaTransport() != "webrtc" {
		t.Fatal("direct default", err)
	}
	t.Setenv("HAUNT_CLOUDFLARED_ENABLED", "true")
	c = Config{}
	if err := source(Environment()).loadExternalMedia(&c); err != nil || c.MediaTransport() != "websocket" {
		t.Fatal("missing TURN did not select WebSocket", err)
	}
	c = Config{ICE: []ICEServer{{URLs: []string{"stun:example.test:3478"}}}}
	if err := source(Environment()).loadExternalMedia(&c); err != nil || c.MediaTransport() != "websocket" {
		t.Fatal("STUN was mistaken for TURN", err)
	}
	t.Setenv("HAUNT_MEDIA_TURN_HOST", "2001:db8::10")
	if source(Environment()).loadExternalMedia(&Config{}) == nil {
		t.Fatal("unauthenticated TURN accepted")
	}
	t.Setenv("HAUNT_MEDIA_TURN_USERNAME", "test")
	t.Setenv("HAUNT_MEDIA_TURN_PASSWORD", "test-secret")
	t.Setenv("HAUNT_MEDIA_TURN_TLS_PORT", "5349")
	c = Config{}
	if err := source(Environment()).loadExternalMedia(&c); err != nil || c.MediaTransport() != "webrtc" || !c.MediaRelayOnly || len(c.ICE) != 1 || len(c.ICE[0].URLs) != 3 || c.ICE[0].URLs[0] != "turn:[2001:db8::10]:3478?transport=udp" {
		t.Fatal("external TURN configuration", err)
	}
	for _, tc := range []struct{ key, value string }{{"HAUNT_CLOUDFLARED_ENABLED", "bad"}, {"HAUNT_MEDIA_TURN_HOST", "https://host"}, {"HAUNT_MEDIA_TURN_HOST", "host:3478"}, {"HAUNT_MEDIA_TURN_HOST", ""}, {"HAUNT_MEDIA_TURN_HOST", "0.0.0.0"}, {"HAUNT_MEDIA_TURN_PORT", "0"}, {"HAUNT_MEDIA_TURN_TLS_PORT", "65536"}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if source(Environment()).loadExternalMedia(&Config{}) == nil {
				t.Fatal("invalid media configuration accepted")
			}
		})
	}
	os.Unsetenv("HAUNT_MEDIA_TURN_HOST")
	os.Unsetenv("HAUNT_MEDIA_TURN_USERNAME")
	os.Unsetenv("HAUNT_MEDIA_TURN_PASSWORD")
	os.Unsetenv("HAUNT_MEDIA_TURN_TLS_PORT")
	t.Setenv("HAUNT_CLOUDFLARE_TURN_KEY_ID", strings.Repeat("a", 32))
	if source(Environment()).loadExternalMedia(&Config{}) == nil {
		t.Fatal("missing Cloudflare token accepted")
	}
	t.Setenv("HAUNT_CLOUDFLARE_TURN_API_TOKEN", "server-only-token")
	c = Config{}
	if err := source(Environment()).loadExternalMedia(&c); err != nil || c.MediaTransport() != "webrtc" || !c.MediaRelayOnly {
		t.Fatal("Cloudflare relay selection", err)
	}
}
