package httpapi

import (
	"bytes"
	"github.com/example-git/haunt-server/internal/identity"
	"io"
	"net/http"
	"time"
)

type SaveCommunityRequest struct {
	Name        string `json:"name" required:"true" minLength:"1" maxLength:"100"`
	Description string `json:"description" maxLength:"1000"`
	AccentColor string `json:"accent_color" required:"true" pattern:"^#[0-9a-fA-F]{6}$"`
}
type artResponse struct {
	Version string `json:"version"`
}

func (a *api) customizationRoutes(mux routeRegistrar) {
	mux.HandleFunc("PUT /api/v1/me/profile", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v identity.ProfileSettingsRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		saved, err := a.identity.SaveProfile(r.Context(), p, v)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, saved)
	}))
	mux.HandleFunc("PUT /api/v1/instance/settings", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v SaveCommunityRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		saved, err := a.identity.SaveCommunity(r.Context(), p, identity.CommunitySettings{Name: v.Name, Description: v.Description, AccentColor: v.AccentColor})
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, saved)
	}))
	for _, kind := range []string{"avatar", "icon", "banner"} {
		base := "/api/v1/instance/" + kind
		get := base
		if kind == "avatar" {
			base = "/api/v1/me/avatar"
			get = "/api/v1/users/{id}/avatar"
		}
		mux.HandleFunc("PUT "+base, a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
			if kind != "avatar" && !p.Can(identity.ManageServer) {
				a.fail(w, r, identity.ErrForbidden)
				return
			}
			max := int64(identity.MaxArtBytes)
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
			if err != nil {
				writeJSON(w, 413, errorResponse{Error: "image_too_large"})
				return
			}
			staticMax := 2 << 20
			if kind == "banner" {
				staticMax = 4 << 20
			}
			if http.DetectContentType(raw) != "image/gif" && len(raw) > staticMax {
				writeJSON(w, 413, errorResponse{Error: "image_too_large"})
				return
			}
			version, err := a.identity.SaveArt(r.Context(), p, kind, raw)
			if err != nil {
				a.fail(w, r, err)
				return
			}
			writeJSON(w, 200, artResponse{Version: version})
		}))
		mux.HandleFunc("DELETE "+base, a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
			if _, err := a.identity.SaveArt(r.Context(), p, kind, nil); err != nil {
				a.fail(w, r, err)
				return
			}
			w.WriteHeader(204)
		}))
		download := func(w http.ResponseWriter, r *http.Request) {
			data, err := a.identity.Art(r.Context(), kind, r.PathValue("id"))
			if err != nil {
				a.fail(w, r, err)
				return
			}
			w.Header().Set("Content-Type", http.DetectContentType(data))
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cache-Control", "private, no-store")
			http.ServeContent(w, r, kind, time.Time{}, bytes.NewReader(data))
		}
		if kind == "avatar" {
			mux.HandleFunc("GET "+get, a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) { download(w, r) }))
		} else {
			mux.HandleFunc("GET "+get, download)
		}
	}
}
