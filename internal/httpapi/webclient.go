package httpapi

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

func webClientHandler(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "api" || strings.HasPrefix(name, "api/") || name == "client-updates" || strings.HasPrefix(name, "client-updates/") || strings.Contains(name, "\\") || path.Clean("/"+name) != "/"+name && name != "" {
			http.NotFound(w, r)
			return
		}
		if name == "" {
			name = "index.html"
		}
		data, err := fs.ReadFile(files, name)
		if err != nil && path.Ext(name) == "" && strings.Contains(r.Header.Get("Accept"), "text/html") {
			name = "index.html"
			data, err = fs.ReadFile(files, name)
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(data)))
		w.Header().Set("Cache-Control", "public, max-age=3600")
		if name == "index.html" || name == "haunt-build.json" {
			w.Header().Set("Cache-Control", "no-store")
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' https: http: data: blob:; media-src 'self' https: http: blob:; connect-src 'self' https: wss: http: ws:; worker-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-src https://www.youtube-nocookie.com; frame-ancestors 'none'; form-action 'none'")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	})
}
