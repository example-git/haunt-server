package httpapi

import (
	"context"
	"github.com/example-git/haunt-server/internal/blobstore"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"net/http"
	"time"
)

type SaveConfigurationRequest struct {
	Revision *int64                  `json:"revision" required:"true" minimum:"0"`
	Settings *config.RuntimeSettings `json:"settings" required:"true"`
}
type ConfigurationResponse struct {
	identity.RuntimeConfiguration
	AttachmentReservedBytes int64 `json:"attachment_reserved_bytes"`
}
type StorageProvidersResponse struct {
	Providers          []blobstore.Provider `json:"providers"`
	DefaultActive      bool                 `json:"default_active"`
	DefaultAttachments int64                `json:"default_attachments"`
	DefaultBytes       int64                `json:"default_bytes"`
}
type ActivateStorageRequest struct {
	ProviderID *string `json:"provider_id" required:"true" description:"Provider UUID, or an empty string to use the built-in local storage."`
}

func (f *Features) configurationRoutes(a *api, mux routeRegistrar) {
	admin := func(fn action) http.HandlerFunc {
		return a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
			if !p.Can(identity.ManageConfiguration) {
				a.fail(w, r, identity.ErrForbidden)
				return
			}
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(35 * time.Second))
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			fn(w, r.WithContext(ctx), p)
		})
	}
	mux.HandleFunc("GET /api/v1/instance/configuration", admin(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if err := a.identity.LoadRuntime(r.Context()); err != nil {
			a.fail(w, r, err)
			return
		}
		response := ConfigurationResponse{RuntimeConfiguration: a.identity.Runtime(f.Defaults)}
		if err := f.Chat.Pool.QueryRow(r.Context(), `SELECT attachment_bytes-dm_attachment_bytes FROM chatxo.storage WHERE singleton`).Scan(&response.AttachmentReservedBytes); err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, response)
	}))
	mux.HandleFunc("PUT /api/v1/instance/configuration", admin(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v SaveConfigurationRequest
		if decode(w, r, &v) != nil || v.Settings == nil || v.Revision == nil {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		saved, err := a.identity.SaveRuntime(r.Context(), p, identity.RuntimeConfiguration{Revision: *v.Revision, Settings: *v.Settings})
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, saved)
	}))
	mux.HandleFunc("GET /api/v1/admin/storage", admin(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		items, err := f.Attachments.Providers.List(r.Context())
		if err != nil {
			a.fail(w, r, err)
			return
		}
		v := StorageProvidersResponse{Providers: items, DefaultActive: true}
		for _, item := range items {
			if item.Active {
				v.DefaultActive = false
			}
		}
		if err = f.Chat.Pool.QueryRow(r.Context(), `SELECT count(*),COALESCE(sum(bytes),0) FROM chatxo.attachments WHERE storage_provider_id IS NULL`).Scan(&v.DefaultAttachments, &v.DefaultBytes); err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, v)
	}))
	mux.HandleFunc("POST /api/v1/admin/storage", admin(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v blobstore.RegisterRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		provider, err := f.Attachments.Providers.Register(ctx, p, v)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 201, provider)
	}))
	mux.HandleFunc("PUT /api/v1/admin/storage/active", admin(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v ActivateStorageRequest
		if decode(w, r, &v) != nil || v.ProviderID == nil {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		if err := f.Attachments.Providers.Activate(r.Context(), p, *v.ProviderID); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST /api/v1/admin/storage/{id}/check", admin(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if err := f.Attachments.Providers.Check(r.Context(), r.PathValue("id")); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("PUT /api/v1/admin/storage/{id}/credentials", admin(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v blobstore.Credentials
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		if err := f.Attachments.Providers.RotateCredentials(r.Context(), p, r.PathValue("id"), v); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("DELETE /api/v1/admin/storage/{id}", admin(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if err := f.Attachments.Providers.Delete(r.Context(), p, r.PathValue("id")); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))
}
