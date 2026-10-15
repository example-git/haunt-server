package config

import (
	"fmt"
	"net/url"
	"strings"
)

func (e source) loadClientUpdates(c *Config) error {
	if value, ok := e.lookup("HAUNT_CLIENT_UPDATE_LOCAL"); ok {
		if value != "true" && value != "false" {
			return fmt.Errorf("HAUNT_CLIENT_UPDATE_LOCAL must be true or false")
		}
		c.ClientUpdateLocal = value == "true"
	}
	if value, ok := e.lookup("HAUNT_CLIENT_UPDATE_INSTALLERS"); ok {
		if value != "true" && value != "false" {
			return fmt.Errorf("HAUNT_CLIENT_UPDATE_INSTALLERS must be true or false")
		}
		c.ClientUpdateInstallers = value == "true"
	}
	if value, ok := e.lookup("HAUNT_CLIENT_UPDATE_MANIFEST_URL"); ok {
		u, err := url.Parse(value)
		if err != nil || len(value) > 2048 || strings.TrimSpace(value) != value || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != "" || !strings.HasSuffix(u.Path, ".json") {
			return fmt.Errorf("HAUNT_CLIENT_UPDATE_MANIFEST_URL must be an HTTPS JSON URL without credentials, query, fragment or custom port")
		}
		c.ClientUpdateManifestURL = value
	}
	return nil
}
