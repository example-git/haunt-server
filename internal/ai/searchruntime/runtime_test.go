//go:build bundled_search_test

package searchruntime

import (
	"context"
	"net/http"
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
}
