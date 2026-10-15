package httpapi

import (
	"bytes"
	"github.com/example-git/haunt-server/internal/identity"
	"io"
	"net/http"
	"time"
)

type emojisResponse struct {
	Emojis []identity.Emoji `json:"emojis"`
}

func (a *api) emojiRoutes(mux routeRegistrar) {
	mux.HandleFunc("GET /api/v1/emojis", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		items, err := a.identity.Emojis(r.Context())
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, emojisResponse{items})
	}))
	mux.HandleFunc("PUT /api/v1/emojis/{name}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !p.Can(identity.ManageServer) {
			a.fail(w, r, identity.ErrForbidden)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, identity.MaxEmojiBytes))
		if err != nil {
			writeJSON(w, 413, errorResponse{Error: "image_too_large"})
			return
		}
		v, err := a.identity.SaveEmoji(r.Context(), p, r.PathValue("name"), raw)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, v)
	}))
	mux.HandleFunc("DELETE /api/v1/emojis/{id}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if err := a.identity.DeleteEmoji(r.Context(), p, r.PathValue("id")); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("GET /api/v1/emojis/{id}/image", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		data, err := a.identity.EmojiImage(r.Context(), r.PathValue("id"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		w.Header().Set("Content-Type", http.DetectContentType(data))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, no-store")
		http.ServeContent(w, r, "emoji", time.Time{}, bytes.NewReader(data))
	}))
}
