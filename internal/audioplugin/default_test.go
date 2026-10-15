package audioplugin

import (
	"bytes"
	"testing"
)

func TestDefaultSeedHonorsRemovalAndExistingPlugins(t *testing.T) {
	root := t.TempDir()
	s, _ := New(root)
	if err := s.SeedDefault(); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 1 || s.List()[0].Manifest.ID != "rnnoise" {
		t.Fatal("missing bundled default")
	}
	if err := s.Remove("rnnoise"); err != nil {
		t.Fatal(err)
	}
	s, _ = New(root)
	if err := s.SeedDefault(); err != nil || len(s.List()) != 0 {
		t.Fatal("removed plugin returned", err)
	}
	s, _ = New(t.TempDir())
	p, err := s.Install(bytes.NewReader(fixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SeedDefault(); err != nil || len(s.List()) != 1 || s.List()[0].Digest != p.Digest {
		t.Fatal("existing plugin changed", err)
	}
}
