package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"github.com/example-git/haunt-server/internal/config"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func updateFixtureFiles(t *testing.T, dir string) ClientUpdateEnvelope {
	t.Helper()
	targets := map[string]updateTarget{}
	for _, name := range []string{"win32-x64", "linux-x64", "linux-arm64", "darwin-arm64"} {
		targets[name] = updateTarget{ASAR: updateArtifact{File: name + ".asar", Size: 4, SHA256: strings.Repeat("a", 64)}, Installer: updateArtifact{File: name + ".exe", Size: 4, SHA256: strings.Repeat("b", 64)}}
	}
	for _, name := range []string{"linux-x64", "linux-arm64"} {
		entry := targets[name]
		entry.RPM = &updateArtifact{File: name + ".rpm", Size: 4, SHA256: strings.Repeat("c", 64)}
		targets[name] = entry
	}
	data, _ := json.Marshal(updatePayload{Version: "0.2.0", Targets: targets})
	envelope := ClientUpdateEnvelope{Schema: 1, Payload: base64.StdEncoding.EncodeToString(data), Signature: base64.StdEncoding.EncodeToString(make([]byte, 64))}
	bytes, _ := json.Marshal(envelope)
	if err := os.MkdirAll(filepath.Join(dir, "client-v0.2.0"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"haunt-updates.json", "client-v0.2.0/haunt-updates.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), bytes, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"win32-x64.exe", "win32-x64.asar", "linux-x64.rpm", "unlisted.exe"} {
		if err := os.WriteFile(filepath.Join(dir, "client-v0.2.0", name), []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return envelope
}
func TestClientUpdateSourcePrecedence(t *testing.T) {
	c := config.Config{PublicURL: "https://chat.example.test"}
	if clientUpdateSource(c) != nil {
		t.Fatal("enabled by default")
	}
	c.ClientUpdateLocal = true
	c.ClientUpdateInstallers = true
	source := clientUpdateSource(c)
	if !source.Authenticated || !source.Installers {
		t.Fatal(source)
	}
	c.ClientUpdateManifestURL = "https://downloads.example.test/a.json"
	source = clientUpdateSource(c)
	if source.Authenticated || !source.Installers || source.ManifestURL != c.ClientUpdateManifestURL {
		t.Fatal(source)
	}
	c.ClientUpdateManifestURL = "https://github.com/example-git/haunt-client/releases/latest/download/haunt-updates.json"
	source = clientUpdateSource(c)
	if source.Authenticated || !source.Installers || source.ManifestURL != c.PublicURL+"/api/v1/client-updates/haunt-updates.json" {
		t.Fatal(source)
	}
}
func TestClientUpdateMirrorAuthorizationAndFiles(t *testing.T) {
	f := newFixture(t)
	f.cfg.DataDir = t.TempDir()
	f.cfg.ClientUpdateLocal = true
	f.cfg.ClientUpdateInstallers = true
	updateFixtureFiles(t, filepath.Join(f.cfg.DataDir, "client-updates"))
	f.server.Close()
	f.server = httptest.NewServer(New(f.cfg, f.identities, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler)
	t.Cleanup(f.server.Close)
	manifest := "/api/v1/client-updates/haunt-updates.json?version=0.1.0&target=win32-x64"
	f.call(t, "GET", manifest, "", nil, 401, nil)
	var envelope ClientUpdateEnvelope
	f.call(t, "GET", manifest, f.owner.Token, nil, 200, &envelope)
	f.call(t, "GET", "/api/v1/client-updates/haunt-updates.json?version=bad&target=win32-x64", f.owner.Token, nil, 400, nil)
	var installers ClientInstallers
	f.call(t, "GET", "/api/v1/client-updates/installers", f.owner.Token, nil, 200, &installers)
	if len(installers.Available) != 1 || installers.Available[0] != "win32-x64" {
		t.Fatal(installers.Available)
	}
	for _, name := range []string{"unlisted.exe", "missing.exe"} {
		f.call(t, "GET", "/api/v1/client-updates/0.2.0/"+name, f.owner.Token, nil, 404, nil)
	}
	for _, name := range []string{"win32-x64.exe", "linux-x64.rpm"} {
		url := "/api/v1/client-updates/0.2.0/" + name
		f.call(t, "GET", url, "", nil, 401, nil)
		status, data, err := f.request("GET", url, f.owner.Token, nil)
		if err != nil || status != 200 || string(data) != "test" {
			t.Fatal(status, string(data), err)
		}
	}
	url := "/api/v1/client-updates/0.2.0/win32-x64.exe"
	req, _ := http.NewRequest("GET", f.server.URL+url, nil)
	req.Header.Set("Authorization", "Bearer "+f.owner.Token)
	req.Header.Set("Range", "bytes=1-2")
	response, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 206 {
		t.Fatal(response.StatusCode)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("test"), 0600)
	target := filepath.Join(f.cfg.DataDir, "client-updates/client-v0.2.0/win32-x64.exe")
	os.Remove(target)
	os.Symlink(outside, target)
	f.call(t, "GET", url, f.owner.Token, nil, 404, nil)
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer unavailable.Close()
	f.cfg.ClientUpdateManifestURL = unavailable.URL + "/a.json"
	f.server.Close()
	f.server = httptest.NewServer(New(f.cfg, f.identities, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler)
	t.Cleanup(f.server.Close)
	f.call(t, "GET", manifest, f.owner.Token, nil, 502, nil)
}
