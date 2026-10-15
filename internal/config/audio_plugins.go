package config

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func loadAudioPlugins(c *Config) error {
	c.DefaultAudioPlugin = true
	if value, ok := os.LookupEnv("HAUNT_DEFAULT_AUDIO_PLUGIN"); ok {
		if value != "true" && value != "false" {
			return fmt.Errorf("HAUNT_DEFAULT_AUDIO_PLUGIN must be true or false")
		}
		c.DefaultAudioPlugin = value == "true"
	}
	if path := os.Getenv("HAUNT_AUDIO_PLUGIN_GITHUB_TOKEN_FILE"); path != "" {
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("cannot open audio plugin repository token file")
		}
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, 4097))
		token := strings.TrimSpace(string(raw))
		if err != nil || len(raw) > 4096 || len(token) == 0 || strings.ContainsAny(token, "\r\n\t ") {
			return fmt.Errorf("invalid audio plugin repository token file")
		}
		c.AudioPluginGitHubToken = token
	}
	return nil
}
