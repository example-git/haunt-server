package audioplugin

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
)

// This redistributable package is pinned separately from private operator plugins.
//
//go:embed default/haunt-plugin.zip
var defaultPackage []byte

//go:embed default/SHA256SUMS
var defaultChecksums string

// SeedDefault is called before HTTP startup. Never replace an operator's plugin
// or reinstall a default they subsequently remove.
func (s *Store) SeedDefault() error {
	marker := filepath.Join(s.root, ".default-seeded")
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if len(s.List()) == 0 {
		if _, err := s.InstallVerified(bytes.NewReader(defaultPackage), strings.Fields(defaultChecksums)[0]); err != nil {
			return err
		}
	}
	return os.WriteFile(marker, []byte("1\n"), 0600)
}
