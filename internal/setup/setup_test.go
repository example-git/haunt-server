package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/haunt-server/internal/config"
)

func TestPrivateConfigurationAndExplicitOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HAUNT_DATA_DIR", dir)
	path := filepath.Join(dir, "configuration.json")
	if err := save(path, map[string]string{"HAUNT_INSTANCE_ID": "original-installation", "HAUNT_NAME": "Saved name", "HAUNT_DATABASE_URL": "postgres://private:secret@localhost/dedicated"}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("configuration must be private")
	}
	t.Setenv("HAUNT_NAME", "Explicit name")
	values, _, err := Values()
	if err != nil {
		t.Fatal(err)
	}
	if values["HAUNT_NAME"] != "Explicit name" || values["HAUNT_INSTANCE_ID"] != "original-installation" {
		t.Fatal("saved identity or explicit override lost")
	}
	t.Setenv("HAUNT_INSTANCE_ID", "replacement-installation")
	if _, _, err = Values(); err == nil {
		t.Fatal("must not silently replace installation identity")
	}
}
func TestSetupAuthorizationAndTransport(t *testing.T) {
	token := randomSecret()
	s := &wizard{token: token, values: map[string]string{"HAUNT_DATABASE_URL": "postgres://private:secret@localhost/dedicated", "HAUNT_TRUSTED_PROXIES": "127.0.0.1/32"}, slots: make(chan struct{}, 1)}
	t.Setenv("HAUNT_DATABASE_URL", s.values["HAUNT_DATABASE_URL"])
	call := func(method, path, authorization, origin, peer, proto string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://public.example"+path, strings.NewReader(`{"values":{},"username":"owner","password":"long-test-password"}`))
		req.Host = "public.example"
		req.RemoteAddr = peer
		req.Header.Set("Authorization", authorization)
		req.Header.Set("Origin", origin)
		req.Header.Set("X-Forwarded-Proto", proto)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	if w := call("GET", "/setup/config", "", "", "203.0.113.2:1234", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "/setup/config", "Bearer "+token, "", "203.0.113.2:1234", ""); w.Code != 200 || strings.Contains(w.Body.String(), "private:secret") {
		t.Fatal("deployment database credentials exposed")
	}
	for _, entry := range []struct{ origin, peer, proto string }{{"https://other.example", "127.0.0.1:12", "https"}, {"http://public.example", "203.0.113.2:12", "https"}, {"http://public.example", "127.0.0.1:12", "http"}, {"", "127.0.0.1:12", "https"}} {
		if w := call("POST", "/setup/complete", "Bearer "+token, entry.origin, entry.peer, entry.proto); w.Code != 403 {
			t.Fatalf("untrusted setup request accepted: %d", w.Code)
		}
	}
	s.submitted.Store(true)
	if w := call("POST", "/setup/complete", "Bearer "+token, "https://public.example", "127.0.0.1:12", "https"); w.Code != 409 {
		t.Fatal("setup replay accepted")
	}
}
func TestConfigurationValidationIsIndependent(t *testing.T) {
	values := map[string]string{"HAUNT_INSTANCE_ID": "test", "HAUNT_PUBLIC_URL": "http://localhost:8080", "HAUNT_DATABASE_URL": "postgres://test@localhost/test", "HAUNT_NAME": "Wizard name"}
	t.Setenv("HAUNT_NAME", "Process name")
	c, err := config.LoadValues(values)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Wizard name" || os.Getenv("HAUNT_NAME") != "Process name" {
		t.Fatal("request values mutated process configuration")
	}
	values["HAUNT_SESSION_DAYS"] = "0"
	if _, err = config.LoadValues(values); err == nil {
		t.Fatal("explicit invalid setting was replaced with a default")
	}
}
func TestSetupHealthIsDistinctAndWizardAssetsAreSelfContained(t *testing.T) {
	s := &wizard{}
	for _, path := range []string{"/readyz", "/setup", "/setup/main.js", "/setup/style.css"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatal(path, w.Code)
		}
		if path == "/readyz" {
			var state map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &state)
			if state["status"] != "setup" {
				t.Fatal(state)
			}
		}
	}
	h := &Host{}
	h.Use(s)
	h.Use(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), "GET", "/", nil))
	if w.Code != 204 {
		t.Fatal("handler transition failed")
	}
}
