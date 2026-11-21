//go:build bundled_search_test

package searchruntime

import (
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBundledSearch(t *testing.T) {
	r := New(t.Context(), t.TempDir())
	defer r.Close()
	cl, err := r.Client()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unexpected search HTTP status %d", resp.StatusCode)
	}

	// Check the defaults seen by an ordinary query using this release's SearXNG loader.
	root := r.installed
	cmd := exec.CommandContext(ctx, filepath.Join(root, "bin/loader"), "--library-path", filepath.Join(root, "lib"),
		filepath.Join(root, "bin/python"), "-sB", "-c",
		"from searx import settings\nprint(','.join(sorted(e['name'] for e in settings['engines'] if not e.get('disabled') and not e.get('inactive'))))")
	cmd.Env = []string{"PYTHONHOME=" + root, "PYTHONPATH=" + filepath.Join(root, "app") + ":" + filepath.Join(root, "packages"),
		"SEARXNG_SETTINGS_PATH=" + filepath.Join(r.socketDir, "settings.yml"), "SEARXNG_DATA_PATH=" + t.TempDir()}
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "brave,duckduckgo" {
		t.Fatalf("effective default search engines = %q, error = %v", out, err)
	}
}
