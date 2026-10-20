package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/serverextension"
)

type ServerExtensionsResponse struct {
	Extensions []serverextension.Extension `json:"extensions" required:"true"`
}

type ExtensionSpeechRequest struct {
	serverextension.SpeechRequest
	RoomID string `json:"room_id,omitempty"`
}

func extensionFailure(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "server_extension_unavailable"
	switch {
	case errors.Is(err, serverextension.ErrInvalid):
		status, code = 400, serverextension.ErrInvalid.Error()
	case errors.Is(err, serverextension.ErrConflict):
		status, code = 409, serverextension.ErrConflict.Error()
	case errors.Is(err, serverextension.ErrUnavailable):
		status, code = 503, serverextension.ErrUnavailable.Error()
	case errors.Is(err, serverextension.ErrCredentials):
		status, code = 502, serverextension.ErrCredentials.Error()
	case errors.Is(err, serverextension.ErrUpstream):
		status, code = 502, serverextension.ErrUpstream.Error()
	case errors.Is(err, serverextension.ErrBusy):
		status, code = 429, serverextension.ErrBusy.Error()
		w.Header().Set("Retry-After", "30")
	}
	writeJSON(w, status, errorResponse{Error: code})
}

func (f *Features) serverExtensionRoutes(a *api, mux routeRegistrar) {
	owner := func(operation action) http.HandlerFunc {
		return a.authenticated(func(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
			if !principal.User.Owner {
				a.fail(w, r, identity.ErrForbidden)
				return
			}
			operation(w, r, principal)
		})
	}
	mux.HandleFunc("GET /api/v1/server-extensions", a.authenticated(func(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
		writeJSON(w, 200, ServerExtensionsResponse{Extensions: f.Extensions.Catalog()})
	}))
	mux.HandleFunc("GET /api/v1/admin/server-extensions/{id}", owner(func(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
		configuration, err := f.Extensions.Configuration(r.PathValue("id"))
		if err != nil {
			extensionFailure(w, err)
			return
		}
		writeJSON(w, 200, configuration)
	}))
	mux.HandleFunc("PUT /api/v1/admin/server-extensions/{id}", owner(func(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
		var request serverextension.SaveRequest
		if err := decodeLimit(w, r, &request, 4096); err != nil {
			a.fail(w, r, err)
			return
		}
		configuration, err := f.Extensions.Save(r.PathValue("id"), request)
		if err != nil {
			extensionFailure(w, err)
			return
		}
		_ = a.identity.RecordAudit(r.Context(), principal.User.ID, "", "server_extension.configured", r.PathValue("id"))
		writeJSON(w, 200, configuration)
	}))
	mux.HandleFunc("POST /api/v1/admin/server-extensions/{id}/test", owner(func(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
		if !a.limit(w, "extension-test:"+principal.User.ID, 6, time.Minute) {
			return
		}
		var request serverextension.SpeechRequest
		if err := decodeLimit(w, r, &request, 4096); err != nil {
			a.fail(w, r, err)
			return
		}
		audio, err := f.Extensions.Speech(r.Context(), r.PathValue("id"), request, true)
		if err != nil {
			extensionFailure(w, err)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(200)
		_, _ = w.Write(audio)
	}))
	mux.HandleFunc("POST /api/v1/server-extensions/{id}/speech", a.authenticated(func(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
		if !a.limit(w, "extension-speech:"+principal.User.ID, 30, time.Minute) {
			return
		}
		var request ExtensionSpeechRequest
		if err := decodeLimit(w, r, &request, 4096); err != nil {
			a.fail(w, r, err)
			return
		}
		if request.RoomID != "" {
			members, err := f.Voice.Room(r.Context(), principal, request.RoomID)
			if err != nil {
				a.fail(w, r, err)
				return
			}
			joined := false
			for _, member := range members {
				if member.UserID == principal.User.ID {
					joined = true
					break
				}
			}
			if !joined {
				a.fail(w, r, identity.ErrForbidden)
				return
			}
		}
		audio, err := f.Extensions.Speech(r.Context(), r.PathValue("id"), request.SpeechRequest, false)
		if err != nil {
			extensionFailure(w, err)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(200)
		_, _ = w.Write(audio)
	}))
}
