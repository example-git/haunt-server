package httpapi

import (
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestEmbeddedWebClient(t *testing.T) {
	handler := webClientHandler(fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html><title>Haunt</title>")},
		"assets/app-abcdefgh.js": {Data: []byte("export const ready = true;")},
		"haunt-build.json":       {Data: []byte(`{"id":"test"}`)},
	})
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/", 200}, {"HEAD", "/", 200}, {"GET", "/assets/app-abcdefgh.js", 200},
		{"GET", "/haunt-build.json", 200}, {"GET", "/assets/missing.js", 404},
		{"GET", "/api/v1/missing", 404}, {"GET", "/client-updates/haunt-updates.json", 404},
		{"GET", "/../go.mod", 404}, {"GET", "/conversation/test", 200},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Accept", "text/html")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Fatalf("%s: got %d, want %d", tc.path, w.Code, tc.code)
		}
		if tc.method == "HEAD" && w.Body.Len() != 0 {
			t.Fatal("HEAD returned a body")
		}
		if tc.path == "/" && w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("entrypoint must refresh")
		}
	}
	req := httptest.NewRequest("GET", "/assets/app-abcdefgh.js", nil)
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, req)
	req.Header.Set("If-None-Match", first.Header().Get("ETag"))
	cached := httptest.NewRecorder()
	handler.ServeHTTP(cached, req)
	if cached.Code != 304 {
		t.Fatal("asset revalidation failed")
	}
}
