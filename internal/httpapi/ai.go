package httpapi

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"github.com/example-git/haunt-server/internal/ai"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/identity"
)

type aiProvidersResponse struct {
	Providers []ai.ProviderInfo `json:"providers"`
}
type aiAccountsResponse struct {
	Accounts []ai.Account `json:"accounts"`
}
type aiModelsResponse struct {
	Models []provider.Model `json:"models"`
}
type aiPresetsResponse struct {
	Presets []ai.Preset `json:"presets"`
}
type aiTurnsResponse struct {
	Turns []ai.Turn `json:"turns"`
}

func (f *Features) aiRoutes(a *api, mux routeRegistrar) {
	result := func(w http.ResponseWriter, r *http.Request, value any, err error) {
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, value)
	}
	origin := func(r *http.Request) string {
		if value := r.Header.Get("Origin"); value != "" {
			return value
		}
		return f.AIOrigin
	}
	mux.HandleFunc("GET /api/v1/ai/providers", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		items, err := f.AI.Providers(r.Context(), p)
		result(w, r, aiProvidersResponse{Providers: items}, err)
	}))
	mux.HandleFunc("GET /api/v1/ai/accounts", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		items, err := f.AI.Accounts(r.Context(), p, r.URL.Query().Get("channel_id"))
		result(w, r, aiAccountsResponse{Accounts: items}, err)
	}))
	mux.HandleFunc("DELETE /api/v1/ai/accounts/{account}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if err := f.AI.Disconnect(r.Context(), p, r.PathValue("account")); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/v1/ai/accounts/{account}/models", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		items, err := f.AI.Models(r.Context(), p, r.PathValue("account"), r.URL.Query().Get("channel_id"))
		result(w, r, aiModelsResponse{Models: items}, err)
	}))
	mux.HandleFunc("POST /api/v1/ai/oauth/start", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var input ai.OAuthPrepareInput
		if err := decodeLimit(w, r, &input, 8192); err != nil {
			a.fail(w, r, err)
			return
		}
		item, err := f.AI.PrepareOAuth(r.Context(), p, origin(r), input)
		result(w, r, item, err)
	}))
	mux.HandleFunc("POST /api/v1/ai/oauth/{session}/complete", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var input ai.OAuthCompleteInput
		if err := decodeLimit(w, r, &input, 16384); err != nil {
			a.fail(w, r, err)
			return
		}
		item, err := f.AI.CompleteOAuth(r.Context(), p, origin(r), r.PathValue("session"), input)
		result(w, r, item, err)
	}))
	mux.HandleFunc("GET /api/v1/channels/{id}/ai", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		item, err := f.AI.Settings(r.Context(), p, r.PathValue("id"))
		result(w, r, item, err)
	}))
	mux.HandleFunc("PUT /api/v1/channels/{id}/ai", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var input ai.SettingsInput
		if err := decodeLimit(w, r, &input, 65536); err != nil {
			a.fail(w, r, err)
			return
		}
		item, err := f.AI.SaveSettings(r.Context(), p, r.PathValue("id"), input)
		result(w, r, item, err)
	}))
	mux.HandleFunc("GET /api/v1/channels/{id}/ai/presets", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		items, err := f.AI.Presets(r.Context(), p, r.PathValue("id"))
		result(w, r, aiPresetsResponse{Presets: items}, err)
	}))
	for _, method := range []string{"POST", "PUT"} {
		path := " /api/v1/channels/{id}/ai/presets"
		if method == "PUT" {
			path += "/{preset}"
		}
		mux.HandleFunc(method+path, a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
			var input ai.PresetInput
			if err := decodeLimit(w, r, &input, (identity.MaxAIAvatarInput+2)/3*4+65536); err != nil {
				a.fail(w, r, err)
				return
			}
			item, err := f.AI.SavePreset(r.Context(), p, r.PathValue("id"), r.PathValue("preset"), input)
			result(w, r, item, err)
		}))
	}
	mux.HandleFunc("DELETE /api/v1/channels/{id}/ai/presets/{preset}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if err := f.AI.DeletePreset(r.Context(), p, r.PathValue("id"), r.PathValue("preset")); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/v1/channels/{id}/ai/usage", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		item, err := f.AI.Usage(r.Context(), p, r.PathValue("id"))
		result(w, r, item, err)
	}))
	mux.HandleFunc("GET /api/v1/channels/{id}/ai/turns", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		items, err := f.AI.Runs(r.Context(), p, r.PathValue("id"))
		result(w, r, aiTurnsResponse{Turns: items}, err)
	}))
	for _, action := range []string{"cancel", "retry"} {
		mux.HandleFunc("POST /api/v1/channels/{id}/ai/turns/{turn}/"+action, a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
			var item ai.Turn
			var err error
			if action == "cancel" {
				item, err = f.AI.Cancel(r.Context(), p, r.PathValue("id"), r.PathValue("turn"))
			} else {
				item, err = f.AI.Retry(r.Context(), p, r.PathValue("id"), r.PathValue("turn"))
			}
			result(w, r, item, err)
		}))
	}
	for _, path := range []string{"/api/v1/channels/{id}/ai/avatar", "/api/v1/channels/{id}/ai/presets/{preset}/avatar"} {
		mux.HandleFunc("PUT "+path, a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, identity.MaxAIAvatarInput))
			if err != nil {
				a.fail(w, r, identity.ErrInvalid)
				return
			}
			_, err = f.AI.SaveAvatar(r.Context(), p, r.PathValue("id"), r.PathValue("preset"), raw)
			if err != nil {
				a.fail(w, r, err)
				return
			}
			if r.PathValue("preset") == "" {
				item, err := f.AI.Settings(r.Context(), p, r.PathValue("id"))
				result(w, r, item, err)
				return
			}
			items, err := f.AI.Presets(r.Context(), p, r.PathValue("id"))
			if err != nil {
				a.fail(w, r, err)
				return
			}
			for _, item := range items {
				if item.ID == r.PathValue("preset") {
					writeJSON(w, 200, item)
					return
				}
			}
			a.fail(w, r, identity.ErrNotFound)
		}))
		mux.HandleFunc("DELETE "+path, a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
			if err := f.AI.DeleteAvatar(r.Context(), p, r.PathValue("id"), r.PathValue("preset")); err != nil {
				a.fail(w, r, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		mux.HandleFunc("GET "+path, a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
			raw, err := f.AI.Avatar(r.Context(), p, r.PathValue("id"), r.PathValue("preset"), r.URL.Query().Get("v"))
			if err != nil {
				a.fail(w, r, err)
				return
			}
			w.Header().Set("Content-Type", http.DetectContentType(raw))
			w.Header().Set("Cache-Control", "private, no-cache")
			http.ServeContent(w, r, "avatar", time.Time{}, bytes.NewReader(raw))
		}))
	}
}
