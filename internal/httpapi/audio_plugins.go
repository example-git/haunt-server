package httpapi

import (
	"encoding/json"
	"errors"
	"github.com/example-git/haunt-server/internal/audioplugin"
	"github.com/example-git/haunt-server/internal/identity"
	"io"
	"net/http"
	"strconv"
	"time"
)

type AudioPluginsResponse struct {
	Default         *audioplugin.Preferences `json:"default"`
	Plugins         []audioplugin.Plugin     `json:"plugins" required:"true"`
	MaxPackageBytes int64                    `json:"max_package_bytes"`
}

func (f *Features) audioPluginRoutes(a *api, mux routeRegistrar) {
	fail := func(w http.ResponseWriter, r *http.Request, err error) {
		status := 500
		code := "audio_plugin_unavailable"
		if errors.Is(err, audioplugin.ErrInvalid) {
			status = 400
			code = err.Error()
		}
		if errors.Is(err, audioplugin.ErrIntegrity) {
			status = 400
			code = err.Error()
		}
		if errors.Is(err, audioplugin.ErrRepository) {
			status = 502
			code = err.Error()
		}
		if errors.Is(err, audioplugin.ErrNotFound) {
			status = 404
			code = err.Error()
		}
		if errors.Is(err, audioplugin.ErrBusy) {
			status = 429
			code = err.Error()
		}
		writeJSON(w, status, errorResponse{Error: code})
	}
	mux.HandleFunc("GET /api/v1/audio-plugins", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		writeJSON(w, 200, AudioPluginsResponse{Plugins: f.AudioPlugins.List(), Default: f.AudioPlugins.Default(), MaxPackageBytes: audioplugin.MaxBytes})
	}))
	mux.HandleFunc("PUT /api/v1/admin/audio-plugins/default", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !p.Can(identity.ManageConfiguration) {
			a.fail(w, r, identity.ErrForbidden)
			return
		}
		var request audioplugin.Preferences
		if err := decodeLimit(w, r, &request, 32768); err != nil {
			a.fail(w, r, err)
			return
		}
		value, err := f.AudioPlugins.SetDefault(request)
		if err != nil {
			fail(w, r, err)
			return
		}
		detail, _ := json.Marshal(value)
		if err := a.identity.RecordAudit(r.Context(), p.User.ID, "", "audio_plugin.default_changed", string(detail)); err != nil {
			a.logger.Error("audio plugin audit failed", "error", err)
		}
		writeJSON(w, 200, value)
	}))
	mux.HandleFunc("POST /api/v1/admin/audio-plugins/repository", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !p.Can(identity.ManageConfiguration) {
			a.fail(w, r, identity.ErrForbidden)
			return
		}
		var request audioplugin.RepositoryInstall
		if err := decodeLimit(w, r, &request, 4096); err != nil {
			a.fail(w, r, err)
			return
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(100 * time.Second))
		plugin, err := f.PluginRepository.Install(r.Context(), f.AudioPlugins, request)
		if err != nil {
			_ = a.identity.RecordAudit(r.Context(), p.User.ID, "", "audio_plugin.repository_rejected", "repository installation rejected")
			fail(w, r, err)
			return
		}
		detail := plugin.Manifest.ID + ":" + plugin.Digest + " " + request.Repository + "@" + request.Commit
		if err := a.identity.RecordAudit(r.Context(), p.User.ID, "", "audio_plugin.installed", detail); err != nil {
			a.logger.Error("audio plugin audit failed", "error", err)
		}
		writeJSON(w, 201, plugin)
	}))
	mux.HandleFunc("GET /api/v1/audio-plugins/{digest}/files/{name}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		reader, size, err := f.AudioPlugins.Open(r.PathValue("digest"), r.PathValue("name"))
		if err != nil {
			fail(w, r, err)
			return
		}
		defer reader.Close()
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(120 * time.Second))
		w.Header().Set("Content-Type", "application/wasm")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		_, _ = io.Copy(w, reader)
	}))
	mux.HandleFunc("POST /api/v1/admin/audio-plugins", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !p.Can(identity.ManageConfiguration) {
			a.fail(w, r, identity.ErrForbidden)
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(120 * time.Second))
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(125 * time.Second))
		r.Body = http.MaxBytesReader(w, r.Body, audioplugin.MaxBytes+1)
		plugin, err := f.AudioPlugins.Install(r.Body)
		if err != nil {
			fail(w, r, err)
			return
		}
		if err = a.identity.RecordAudit(r.Context(), p.User.ID, "", "audio_plugin.installed", plugin.Manifest.ID+":"+plugin.Digest); err != nil {
			a.logger.Error("audio plugin audit failed", "error", err)
		}
		writeJSON(w, 201, plugin)
	}))
	mux.HandleFunc("DELETE /api/v1/admin/audio-plugins/{id}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !p.Can(identity.ManageConfiguration) {
			a.fail(w, r, identity.ErrForbidden)
			return
		}
		if err := f.AudioPlugins.Remove(r.PathValue("id")); err != nil {
			fail(w, r, err)
			return
		}
		if err := a.identity.RecordAudit(r.Context(), p.User.ID, "", "audio_plugin.removed", r.PathValue("id")); err != nil {
			a.logger.Error("audio plugin audit failed", "error", err)
		}
		w.WriteHeader(204)
	}))
}
