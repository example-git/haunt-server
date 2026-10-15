package httpapi

import (
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"net/http"
	"strconv"
	"time"
)

type DMPurgeRequest struct {
	Before  time.Time `json:"before" required:"true"`
	Confirm bool      `json:"confirm"`
}

func (f *Features) dmRoutes(a *api, mux routeRegistrar) {
	f.dmCryptoRoutes(a, mux)
	mux.HandleFunc("GET /api/v1/me/dm-storage", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		value, err := f.Attachments.DMUsage(r.Context(), p)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, value)
	}))
	mux.HandleFunc("POST /api/v1/me/dm-storage/purge", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !a.limit(w, "dm-purge:"+p.User.ID, 30, time.Minute) {
			return
		}
		var input DMPurgeRequest
		if decode(w, r, &input) != nil {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		value, err := f.Attachments.PurgeDM(r.Context(), p, input.Before, input.Confirm)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, value)
	}))
}

type DMCryptoAfter struct {
	After int64 `query:"after" minimum:"0"`
}

func (f *Features) dmCryptoRoutes(a *api, mux routeRegistrar) {
	mux.HandleFunc("GET /api/v1/me/dm-devices/{device}/packages", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		value, err := f.Chat.OwnDMPackages(r.Context(), p, r.PathValue("device"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, value)
	}))
	mux.HandleFunc("POST /api/v1/me/dm-devices", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !a.limit(w, "dm-device:"+p.User.ID, 30, time.Minute) {
			return
		}
		var v chat.RegisterDMDevice
		if decodeLimit(w, r, &v, 1<<20) != nil {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		result, err := f.Chat.RegisterDMDevice(r.Context(), p, v)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, result)
	}))
	mux.HandleFunc("PUT /api/v1/me/dm-devices/{device}/packages", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !a.limit(w, "dm-packages:"+p.User.ID, 30, time.Minute) {
			return
		}
		var v chat.DMKeyPackages
		if decodeLimit(w, r, &v, 1<<20) != nil {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		if err := f.Chat.AddDMPackages(r.Context(), p, r.PathValue("device"), v); err != nil {
			a.fail(w, r, err)
			return
		}
		if len(v.Packages) > 0 {
			if err := f.Chat.AnnounceDMDevice(r.Context(), p); err != nil {
				a.fail(w, r, err)
				return
			}
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("GET /api/v1/conversations/{id}/encryption", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		value, err := f.Chat.DMBundle(r.Context(), p, r.PathValue("id"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, value)
	}))
	mux.HandleFunc("GET /api/v1/conversations/{id}/encryption/events", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		after := int64(0)
		var err error
		if value := r.URL.Query().Get("after"); value != "" {
			after, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				a.fail(w, r, identity.ErrInvalid)
				return
			}
		}
		value, err := f.Chat.DMEvents(r.Context(), p, r.PathValue("id"), after)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, value)
	}))
	mux.HandleFunc("POST /api/v1/conversations/{id}/encryption/events", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !a.limit(w, "dm-commit:"+p.User.ID, 60, time.Minute) {
			return
		}
		var v chat.DMCryptoCommit
		if decodeLimit(w, r, &v, 1<<20) != nil {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		value, err := f.Chat.CommitDM(r.Context(), p, r.PathValue("id"), v)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, value)
	}))
}
