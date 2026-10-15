package audioplugin

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("../../client/tests/fixtures/audio-plugin/test-audio.zip")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestInstallVerifyStreamRestartRemove(t *testing.T) {
	root := t.TempDir()
	s, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.Install(bytes.NewReader(fixture(t)))
	if e != nil {
		t.Fatal(e)
	}
	if len(s.List()) != 1 || len(p.Files) != 1 {
		t.Fatal("missing catalog")
	}
	r, n, e := s.Open(p.Digest, p.Files[0].Name)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(r)
	r.Close()
	if e != nil || int64(len(b)) != n {
		t.Fatal("bad stream")
	}
	s, e = New(root)
	if e != nil || len(s.List()) != 1 {
		t.Fatal("lost on restart")
	}
	if e = s.Remove(p.Manifest.ID); e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.Open(p.Digest, p.Files[0].Name); e != ErrNotFound {
		t.Fatal("removed file available")
	}
}
func TestRejectTraversalScriptsOversizeAndMalformedManifest(t *testing.T) {
	s, _ := New(t.TempDir())
	original := fixture(t)
	z, _ := zip.NewReader(bytes.NewReader(original), int64(len(original)))
	for _, extra := range []string{"../escape.wasm", "script.js", "default.wasm"} {
		var b bytes.Buffer
		w := zip.NewWriter(&b)
		for _, f := range z.File {
			r, _ := f.Open()
			v, _ := w.Create(f.Name)
			io.Copy(v, r)
			r.Close()
		}
		v, _ := w.Create(extra)
		v.Write([]byte("bad"))
		w.Close()
		if _, e := s.Install(&b); e == nil {
			t.Fatalf("accepted %s", extra)
		}
	}
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, f := range z.File {
		r, _ := f.Open()
		data, _ := io.ReadAll(r)
		r.Close()
		if f.Name == "manifest.json" {
			var m map[string]any
			json.Unmarshal(data, &m)
			m["javascript"] = "alert(1)"
			data, _ = json.Marshal(m)
		}
		v, _ := w.Create(f.Name)
		v.Write(data)
	}
	w.Close()
	if _, e := s.Install(&b); e == nil {
		t.Fatal("unknown manifest accepted")
	}
	if _, e := s.Install(io.LimitReader(zeroReader{}, MaxBytes+1)); e == nil {
		t.Fatal("oversize accepted")
	}
	entries, _ := os.ReadDir(s.root)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".zip" {
			t.Fatal("invalid archive persisted")
		}
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
