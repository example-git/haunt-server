package audioplugin

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// Preferences are unscaled UI values, matching the client's saved options.
type Preferences struct {
	ID      string             `json:"id" required:"true"`
	Variant string             `json:"variant" required:"true"`
	Values  map[string]float64 `json:"values" required:"true"`
}

func normalizedDefaults(p Plugin, raw *Preferences) Preferences {
	value := Preferences{ID: p.Manifest.ID, Variant: p.Manifest.DefaultVariant, Values: map[string]float64{}}
	if raw != nil && raw.ID == value.ID {
		for _, v := range p.Manifest.Variants {
			if v.ID == raw.Variant {
				value.Variant = v.ID
			}
		}
	}
	for _, f := range p.Manifest.Fields {
		v := f.Default
		if raw != nil && raw.ID == value.ID {
			if candidate, ok := raw.Values[f.Key]; ok && !math.IsNaN(candidate) && !math.IsInf(candidate, 0) {
				v = math.Max(f.Min, math.Min(f.Max, candidate))
			}
		}
		value.Values[f.Key] = v
	}
	return value
}

func (s *Store) loadDefaults() error {
	file, err := os.Open(filepath.Join(s.root, "defaults.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 32769))
	if err != nil {
		return err
	}
	if len(raw) > 32768 {
		return ErrInvalid
	}
	var value Preferences
	if json.Unmarshal(raw, &value) != nil {
		return ErrInvalid
	}
	s.defaults = &value
	return nil
}

func (s *Store) Default() *Preferences {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.defaults != nil {
		if p, ok := s.plugins[s.defaults.ID]; ok {
			value := normalizedDefaults(p, s.defaults)
			return &value
		}
	}
	if p, ok := s.plugins["rnnoise"]; ok {
		value := normalizedDefaults(p, nil)
		return &value
	}
	ids := make([]string, 0, len(s.plugins))
	for id := range s.plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil
	}
	value := normalizedDefaults(s.plugins[ids[0]], nil)
	return &value
}

func (s *Store) SetDefault(value Preferences) (*Preferences, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plugins[value.ID]
	if !ok {
		return nil, ErrNotFound
	}
	validVariant := false
	for _, v := range p.Manifest.Variants {
		if value.Variant == v.ID {
			validVariant = true
		}
	}
	if !validVariant || value.Values == nil {
		return nil, ErrInvalid
	}
	fields := map[string]Field{}
	for _, f := range p.Manifest.Fields {
		fields[f.Key] = f
	}
	for key, v := range value.Values {
		f, ok := fields[key]
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < f.Min || v > f.Max {
			return nil, ErrInvalid
		}
	}
	normalized := normalizedDefaults(p, &value)
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(s.root, ".defaults-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err = os.Rename(file.Name(), filepath.Join(s.root, "defaults.json")); err != nil {
		return nil, err
	}
	s.defaults = &normalized
	dir, err := os.Open(s.root)
	if err == nil {
		err = dir.Sync()
		dir.Close()
	}
	if err != nil {
		return nil, err
	}
	result := normalizedDefaults(p, &normalized)
	return &result, nil
}
