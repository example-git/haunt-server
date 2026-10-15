// Package audioplugin stores restricted Wasm audio packages. The service never
// executes a plugin. Clients enforce the Wasm ABI and resource sandbox.
package audioplugin

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const MaxBytes int64 = 50 << 20
const MaxPlugins = 4

var ErrInvalid = errors.New("invalid_audio_plugin")
var ErrNotFound = errors.New("audio_plugin_not_found")
var ErrBusy = errors.New("audio_plugin_busy")
var identifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var exportName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)

func validID(id string) bool {
	return identifier.MatchString(id) && id != "constructor" && id != "prototype"
}

var hashName = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Field struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"`
	Default  float64  `json:"default"`
	Min      float64  `json:"min"`
	Max      float64  `json:"max"`
	Step     float64  `json:"step"`
	Scale    float64  `json:"scale"`
	Export   string   `json:"export,omitempty"`
	Help     string   `json:"help,omitempty"`
	Variants []string `json:"variants,omitempty"`
}
type Variant struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	File        string `json:"file"`
	Description string `json:"description,omitempty"`
}
type ABI struct {
	Initialize      string   `json:"initialize,omitempty"`
	Create          string   `json:"create"`
	Input           string   `json:"input"`
	Output          string   `json:"output"`
	Process         string   `json:"process"`
	Reset           string   `json:"reset,omitempty"`
	FrameSamples    int      `json:"frame_samples"`
	ProcessControls []string `json:"process_controls,omitempty"`
}
type Diagnostic struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Export   string   `json:"export"`
	Variants []string `json:"variants,omitempty"`
}
type LegacyPreferences struct {
	VariantKey string            `json:"variant_key"`
	Fields     map[string]string `json:"fields"`
}
type Manifest struct {
	Diagnostics       []Diagnostic       `json:"diagnostics,omitempty"`
	LegacyPreferences *LegacyPreferences `json:"legacy_preferences,omitempty"`
	Version           int                `json:"version"`
	ID                string             `json:"id"`
	Name              string             `json:"name"`
	Description       string             `json:"description"`
	DefaultVariant    string             `json:"default_variant"`
	ABI               ABI                `json:"abi"`
	Variants          []Variant          `json:"variants"`
	Fields            []Field            `json:"fields"`
}
type File struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type Plugin struct {
	Manifest Manifest `json:"manifest"`
	Digest   string   `json:"digest"`
	Bytes    int64    `json:"bytes"`
	Files    []File   `json:"files"`
}
type Store struct {
	root     string
	mu       sync.RWMutex
	plugins  map[string]Plugin
	defaults *Preferences
	upload   chan struct{}
}

func New(root string) (*Store, error) {
	root = filepath.Join(root, "audio-plugins")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	s := &Store{root: root, plugins: map[string]Plugin{}, upload: make(chan struct{}, 1)}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".zip" {
			continue
		}
		if len(s.plugins) >= MaxPlugins {
			return nil, ErrInvalid
		}
		p, err := inspect(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		if entry.Name() != p.Manifest.ID+".zip" {
			return nil, ErrInvalid
		}
		s.plugins[p.Manifest.ID] = p
	}
	if err := s.loadDefaults(); err != nil {
		return nil, err
	}
	return s, nil
}
func validManifest(m Manifest) bool {
	if m.Version != 1 || !validID(m.ID) || len(m.Name) < 1 || len(m.Name) > 100 || len(m.Description) > 2000 || len(m.Variants) < 1 || len(m.Variants) > 16 || m.Fields == nil || len(m.Fields) > 32 || m.ABI.FrameSamples < 120 || m.ABI.FrameSamples > 960 || len(m.ABI.ProcessControls) > 8 {
		return false
	}
	for _, name := range []string{m.ABI.Create, m.ABI.Input, m.ABI.Output, m.ABI.Process} {
		if !exportName.MatchString(name) {
			return false
		}
	}
	for _, name := range []string{m.ABI.Initialize, m.ABI.Reset} {
		if name != "" && !exportName.MatchString(name) {
			return false
		}
	}
	variants := map[string]bool{}
	for _, v := range m.Variants {
		if variants[v.ID] || !validID(v.ID) || v.File != v.ID+".wasm" || len(v.Label) < 1 || len(v.Label) > 150 || len(v.Description) > 2000 {
			return false
		}
		variants[v.ID] = true
	}
	if !variants[m.DefaultVariant] {
		return false
	}
	keys := map[string]bool{}
	for _, f := range m.Fields {
		if keys[f.Key] || f.Key == "constructor" || f.Key == "prototype" || !validID(f.Key) || len(f.Label) < 1 || len(f.Label) > 100 || (f.Type != "range" && f.Type != "number") || f.Min > f.Default || f.Default > f.Max || f.Step <= 0 || f.Scale <= 0 || f.Max-f.Min > 1e6 || math.IsInf(f.Min*f.Scale, 0) || math.IsInf(f.Max*f.Scale, 0) || len(f.Variants) > 16 || len(f.Help) > 2000 || (f.Export != "" && !exportName.MatchString(f.Export)) {
			return false
		}
		keys[f.Key] = true
		for _, variant := range f.Variants {
			if !variants[variant] {
				return false
			}
		}
	}
	if len(m.Diagnostics) > 8 {
		return false
	}
	diagnostics := map[string]bool{}
	for _, d := range m.Diagnostics {
		if !validID(d.Key) || diagnostics[d.Key] || len(d.Variants) > 16 || !exportName.MatchString(d.Export) || len(d.Label) < 1 || len(d.Label) > 100 {
			return false
		}
		diagnostics[d.Key] = true
		for _, v := range d.Variants {
			if !variants[v] {
				return false
			}
		}
	}
	if m.LegacyPreferences != nil {
		if m.LegacyPreferences.VariantKey != "noiseModel" || len(m.LegacyPreferences.Fields) > 3 {
			return false
		}
		for key, value := range m.LegacyPreferences.Fields {
			if !keys[key] || (value != "noiseStrength" && value != "noisePncInputGain" && value != "noiseCsdGate") {
				return false
			}
		}
	}
	for _, key := range m.ABI.ProcessControls {
		if !keys[key] {
			return false
		}
	}
	return true
}
func inspect(path string) (Plugin, error) {
	var p Plugin
	stat, err := os.Stat(path)
	if err != nil {
		return p, err
	}
	if stat.Size() > MaxBytes {
		return p, ErrInvalid
	}
	z, err := zip.OpenReader(path)
	if err != nil {
		return p, ErrInvalid
	}
	defer z.Close()
	if len(z.File) < 2 || len(z.File) > 19 {
		return p, ErrInvalid
	}
	byName := map[string]*zip.File{}
	var total uint64
	for _, f := range z.File {
		if f.Name != "manifest.json" && f.Name != "LICENSE.txt" && !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}\.wasm$`).MatchString(f.Name) {
			return p, ErrInvalid
		}
		if byName[f.Name] != nil || f.Mode()&os.ModeSymlink != 0 {
			return p, ErrInvalid
		}
		total += f.UncompressedSize64
		if total > uint64(MaxBytes) || f.UncompressedSize64 > uint64(MaxBytes) {
			return p, ErrInvalid
		}
		byName[f.Name] = f
	}
	// License text is inert package material; it is never rendered as HTML or
	// executed. Keep it bounded and read it to verify the ZIP's CRC as well.
	if license := byName["LICENSE.txt"]; license != nil {
		if license.UncompressedSize64 > 64<<10 {
			return p, ErrInvalid
		}
		r, err := license.Open()
		if err != nil {
			return p, ErrInvalid
		}
		_, err = io.Copy(io.Discard, io.LimitReader(r, (64<<10)+1))
		r.Close()
		if err != nil {
			return p, ErrInvalid
		}
		delete(byName, "LICENSE.txt")
	}
	meta := byName["manifest.json"]
	if meta == nil || meta.UncompressedSize64 > 64<<10 {
		return p, ErrInvalid
	}
	r, err := meta.Open()
	if err != nil {
		return p, ErrInvalid
	}
	decoder := json.NewDecoder(io.LimitReader(r, 64<<10))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&p.Manifest)
	var extra any
	end := decoder.Decode(&extra)
	r.Close()
	if err != nil || end != io.EOF || !validManifest(p.Manifest) {
		return p, ErrInvalid
	}
	if len(byName) != len(p.Manifest.Variants)+1 {
		return p, ErrInvalid
	}
	for _, v := range p.Manifest.Variants {
		f := byName[v.File]
		if f == nil {
			return p, ErrInvalid
		}
		r, err := f.Open()
		if err != nil {
			return p, ErrInvalid
		}
		data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
		r.Close()
		if err != nil || int64(len(data)) != int64(f.UncompressedSize64) || !validWasm(data) {
			return p, ErrInvalid
		}
		sum := sha256.Sum256(data)
		p.Files = append(p.Files, File{Name: f.Name, Bytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	}
	p.Bytes = int64(total)
	file, err := os.Open(path)
	if err != nil {
		return p, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return p, err
	}
	p.Digest = hex.EncodeToString(hash.Sum(nil))
	return p, nil
}
func (s *Store) List() []Plugin {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]Plugin, 0, len(s.plugins))
	for _, p := range s.plugins {
		items = append(items, p)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Manifest.ID < items[j].Manifest.ID })
	return items
}
func (s *Store) Install(reader io.Reader) (Plugin, error) {
	return s.install(reader, "")
}

// InstallVerified checks the publisher's expected archive hash before parsing or
// replacing any installed plugin. It is separate from hashes generated locally.
func (s *Store) InstallVerified(reader io.Reader, expected string) (Plugin, error) {
	if !hashName.MatchString(expected) {
		return Plugin{}, ErrInvalid
	}
	return s.install(reader, expected)
}

func (s *Store) install(reader io.Reader, expected string) (Plugin, error) {
	select {
	case s.upload <- struct{}{}:
		defer func() { <-s.upload }()
	default:
		return Plugin{}, ErrBusy
	}
	file, err := os.CreateTemp(s.root, ".upload-")
	if err != nil {
		return Plugin{}, err
	}
	defer os.Remove(file.Name())
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(reader, MaxBytes+1))
	if err == nil {
		err = file.Sync()
	}
	file.Close()
	if err != nil {
		return Plugin{}, err
	}
	if n > MaxBytes {
		return Plugin{}, ErrInvalid
	}
	if expected != "" && !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected) {
		return Plugin{}, ErrIntegrity
	}
	p, err := inspect(file.Name())
	if err != nil {
		return p, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.plugins[p.Manifest.ID]; !ok && len(s.plugins) >= MaxPlugins {
		return p, ErrInvalid
	}
	if err = os.Rename(file.Name(), filepath.Join(s.root, p.Manifest.ID+".zip")); err != nil {
		return p, err
	}
	s.plugins[p.Manifest.ID] = p
	dir, err := os.Open(s.root)
	if err == nil {
		err = dir.Sync()
		dir.Close()
	}
	return p, err
}
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.plugins[id]; !ok {
		return ErrNotFound
	}
	if err := os.Remove(filepath.Join(s.root, id+".zip")); err != nil {
		return err
	}
	delete(s.plugins, id)
	dir, err := os.Open(s.root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// The open ZIP holds the inode across an atomic replacement; readers always see
// one complete version. Only catalog/file opening uses a small metadata lock.
func (s *Store) Open(digest, name string) (io.ReadCloser, int64, error) {
	if !hashName.MatchString(digest) {
		return nil, 0, ErrNotFound
	}
	s.mu.RLock()
	var z *zip.ReadCloser
	var size int64
	var err error
	for _, p := range s.plugins {
		if p.Digest != digest {
			continue
		}
		for _, f := range p.Files {
			if f.Name == name {
				size = f.Bytes
				z, err = zip.OpenReader(filepath.Join(s.root, p.Manifest.ID+".zip"))
				break
			}
		}
	}
	s.mu.RUnlock()
	if err != nil {
		return nil, 0, err
	}
	if z == nil {
		return nil, 0, ErrNotFound
	}
	for _, f := range z.File {
		if f.Name == name {
			r, err := f.Open()
			if err != nil {
				z.Close()
				return nil, 0, err
			}
			return &zipStream{ReadCloser: r, zip: z}, size, nil
		}
	}
	z.Close()
	return nil, 0, ErrNotFound
}

type zipStream struct {
	io.ReadCloser
	zip *zip.ReadCloser
}

func (r *zipStream) Close() error { return errors.Join(r.ReadCloser.Close(), r.zip.Close()) }
