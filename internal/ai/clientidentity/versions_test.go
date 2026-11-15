package clientidentity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/haunt-server/internal/ai/provider"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestMetadataRefreshCacheAndStableSnapshot(t *testing.T) {
	for _, key := range environments {
		old, exists := os.LookupEnv(key)
		_ = os.Unsetenv(key)
		t.Cleanup(func() {
			if exists {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "versions.json")
	m, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := m.Snapshot(provider.Claude)
	m.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		header := http.Header{}
		status := 200
		body := "2.1.999"
		if r.URL.Hostname() == "github.com" {
			if r.Method != http.MethodHead {
				t.Error("Codex metadata must use latest redirect")
			}
			status = 302
			header.Set("Location", "https://github.com/openai/codex/releases/tag/rust-v0.999.0")
			body = ""
		} else if strings.Contains(r.URL.Hostname(), "antigravity") {
			body = `{"version":"1.9.9"}`
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	if err = m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := m.Snapshot(provider.Claude)
	if before.Version != fallbacks[provider.Claude] || after.Version != "2.1.999" || before.UserAgent == after.UserAgent {
		t.Fatal("operation snapshot changed or refresh not applied")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cached map[string]string
	_ = json.Unmarshal(b, &cached)
	if cached["codex"] != "0.999.0" || cached["antigravity"] != "1.9.9" {
		t.Fatalf("cache incomplete %s", b)
	}
	restored, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := restored.Snapshot(provider.Codex)
	if s.Version != "0.999.0" || !strings.Contains(s.UserAgent, "/0.999.0 (") {
		t.Error("persistent native identity not restored")
	}
	m.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
	if err = m.Refresh(context.Background()); err == nil {
		t.Error("metadata failure hidden")
	}
	current, _ := m.Snapshot(provider.Claude)
	if current.Version != after.Version {
		t.Error("metadata failure replaced last known version")
	}
}
func TestInvalidExplicitVersionFails(t *testing.T) {
	t.Setenv("CODEX_VERSION", "invalid\r\nheader")
	if _, err := New(""); err == nil {
		t.Fatal("invalid explicit version silently defaulted")
	}
	t.Setenv("CODEX_VERSION", "0.987.0")
	m, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Snapshot(provider.Codex)
	if s.Version != "0.987.0" {
		t.Fatal("explicit version ignored")
	}
	t.Setenv("CODEX_VERSION", "")
	if _, err = m.Snapshot(provider.Codex); err == nil {
		t.Fatal("explicit empty version silently defaulted")
	}
}

func TestNullVersionCacheUsesReviewedFallbacks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "versions.json")
	if err := os.WriteFile(path, []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.versions) != 3 {
		t.Fatal("missing provider identities after null cache")
	}
}
