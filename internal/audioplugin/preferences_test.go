package audioplugin

import (
	"bytes"
	"testing"
)

func TestServerDefaultsPersistValidateAndHandleRemovedPlugin(t *testing.T) {
	root := t.TempDir()
	s, _ := New(root)
	p, err := s.Install(bytes.NewReader(fixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	field := p.Manifest.Fields[0]
	saved := Preferences{ID: p.Manifest.ID, Variant: p.Manifest.DefaultVariant, Values: map[string]float64{field.Key: field.Min}}
	if _, err = s.SetDefault(saved); err != nil {
		t.Fatal(err)
	}
	restored, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.Default(); got == nil || got.ID != saved.ID || got.Values[field.Key] != field.Min {
		t.Fatalf("lost default: %+v", got)
	}
	invalid := saved
	invalid.Variant = "missing"
	if _, err = s.SetDefault(invalid); err != ErrInvalid {
		t.Fatal(err)
	}
	invalid = saved
	invalid.Values = map[string]float64{"unknown": 1}
	if _, err = s.SetDefault(invalid); err != ErrInvalid {
		t.Fatal(err)
	}
	invalid.Values = map[string]float64{field.Key: field.Max + 1}
	if _, err = s.SetDefault(invalid); err != ErrInvalid {
		t.Fatal(err)
	}
	if err = s.Remove(p.Manifest.ID); err != nil {
		t.Fatal(err)
	}
	if s.Default() != nil {
		t.Fatal("removed plugin still default")
	}
}
