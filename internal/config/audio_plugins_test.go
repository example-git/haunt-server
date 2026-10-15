package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAudioPluginRepositorySecretAndDefault(t *testing.T) {
	t.Setenv("HAUNT_AUDIO_PLUGIN_GITHUB_TOKEN_FILE", "")
	t.Setenv("HAUNT_DEFAULT_AUDIO_PLUGIN", "true")
	var c Config
	if err := loadAudioPlugins(&c); err != nil || !c.DefaultAudioPlugin {
		t.Fatal("missing default", err)
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("private-test-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAUNT_AUDIO_PLUGIN_GITHUB_TOKEN_FILE", path)
	t.Setenv("HAUNT_DEFAULT_AUDIO_PLUGIN", "false")
	if err := loadAudioPlugins(&c); err != nil || c.DefaultAudioPlugin || c.AudioPluginGitHubToken != "private-test-token" {
		t.Fatal("token/default loading", err)
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "private-test-token") {
		t.Fatal("secret serialized")
	}
	for _, token := range []string{"", "token\nsecond-line", strings.Repeat("a", 4097)} {
		os.WriteFile(path, []byte(token), 0600)
		if err := loadAudioPlugins(&c); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
}
