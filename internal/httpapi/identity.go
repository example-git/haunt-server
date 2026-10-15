package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/example-git/haunt-server/internal/attachments"
	"github.com/example-git/haunt-server/internal/blobstore"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/turnclient"
	"github.com/example-git/haunt-server/internal/voice"
	"github.com/jackc/pgx/v5/pgconn"
)

type api struct {
	identity       *identity.Service
	logger         *slog.Logger
	limits         *limiter
	trustedProxies []netip.Prefix
	cloudflared    bool
}
type action func(http.ResponseWriter, *http.Request, identity.Principal)

func (a *api) fail(w http.ResponseWriter, r *http.Request, err error) {
	var conflict *pgconn.PgError
	if errors.As(err, &conflict) && (conflict.Code == "40001" || conflict.Code == "40P01") {
		err = identity.ErrBusy
	}
	if b := banError(err); b != nil {
		a.auditAccess(r, "access.banned")
		writeJSON(w, 403, map[string]string{"error": "banned", "ban_type": b.Type, "appeal_ticket": b.Ticket})
		return
	}
	if errors.Is(err, identity.ErrUnauthorized) {
		if r.URL.Path == "/api/v1/auth/login" {
			a.auditAccess(r, "auth.login.failed")
		} else {
			a.auditAccess(r, "access.unauthorized")
		}
	}
	if (errors.Is(err, identity.ErrInvite) || errors.Is(err, identity.ErrInvalid)) && r.URL.Path == "/api/v1/auth/register" {
		a.auditAccess(r, "auth.register.failed")
	}
	if errors.Is(err, identity.ErrForbidden) {
		a.auditAccess(r, "access.forbidden")
	}
	status := http.StatusInternalServerError
	code := "internal_error"
	switch {
	case errors.Is(err, blobstore.ErrInvalid):
		status, code = 400, "invalid_storage_provider"
	case errors.Is(err, blobstore.ErrInUse):
		status, code = 409, "storage_provider_in_use"
	case errors.Is(err, blobstore.ErrUnavailable):
		status, code = 503, "storage_provider_unavailable"
	case errors.Is(err, identity.ErrConfiguration):
		status, code = 400, "invalid_configuration"
	case errors.Is(err, identity.ErrBudget):
		status, code = 409, "attachment_budget_too_small"
	case errors.Is(err, identity.ErrRegistrationClosed):
		status, code = 403, "registration_closed"
	case errors.Is(err, turnclient.ErrUnavailable):
		status, code = http.StatusServiceUnavailable, "media_relay_unavailable"
	case errors.Is(err, voice.ErrCapacity):
		status = http.StatusServiceUnavailable
		code = "media_capacity_reached"
	case errors.Is(err, chat.ErrEncryption):
		status, code = 409, err.Error()
	case errors.Is(err, chat.ErrEpoch):
		status, code = 409, err.Error()
	case errors.Is(err, attachments.ErrDMTransfer):
		status, code = 429, err.Error()
	case errors.Is(err, attachments.ErrDMDisabled):
		status, code = 403, err.Error()
	case errors.Is(err, attachments.ErrDMFull), errors.Is(err, chat.ErrDMTextFull):
		status, code = 507, err.Error()
	case errors.Is(err, attachments.ErrFull):
		status = http.StatusInsufficientStorage
		code = "attachment_storage_full"
	case errors.Is(err, attachments.ErrMedia):
		status = http.StatusUnsupportedMediaType
		code = "unsupported_or_invalid_media"
	case errors.Is(err, identity.ErrInvalid):
		status = http.StatusBadRequest
		code = "invalid_request"
	case errors.Is(err, identity.ErrInvite):
		status = http.StatusBadRequest
		code = "invalid_invitation"
	case errors.Is(err, identity.ErrPasswordChange):
		status, code = http.StatusForbidden, "password_change_required"
	case errors.Is(err, identity.ErrSamePassword):
		status, code = http.StatusBadRequest, "password_unchanged"
	case errors.Is(err, identity.ErrKicked):
		status, code = http.StatusUnauthorized, "kicked"
	case errors.Is(err, identity.ErrUnauthorized):
		status = http.StatusUnauthorized
		code = "unauthorized"
		w.Header().Set("WWW-Authenticate", `Bearer realm="haunt"`)
	case errors.Is(err, identity.ErrForbidden):
		status = http.StatusForbidden
		code = "forbidden"
	case errors.Is(err, identity.ErrNotFound):
		status = http.StatusNotFound
		code = "not_found"
	case errors.Is(err, identity.ErrConflict):
		status = http.StatusConflict
		code = "conflict"
	case errors.Is(err, identity.ErrSessionLimit):
		status = http.StatusConflict
		code = "session_limit_reached"
	case errors.Is(err, identity.ErrBusy):
		status = http.StatusServiceUnavailable
		code = "authentication_busy"
		w.Header().Set("Retry-After", "1")
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusServiceUnavailable
		code = "request_timeout"
	}
	if status == http.StatusInternalServerError {
		a.logger.Error("API request failed", "method", r.Method, "route", r.Pattern, "error", err)
	}
	writeJSON(w, status, map[string]string{"error": code})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeLimit(w, r, dst, 16<<10)
}

func decodeLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	if mediaType := strings.Split(r.Header.Get("Content-Type"), ";")[0]; strings.TrimSpace(mediaType) != "application/json" {
		return identity.ErrInvalid
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return identity.ErrInvalid
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return identity.ErrInvalid
	}
	return nil
}

func (a *api) limit(w http.ResponseWriter, key string, max int, window time.Duration) bool {
	if a.limits.allow(key, max, window) {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(window/time.Second)))
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
	return false
}

func (a *api) remoteIP(r *http.Request) string {
	// A configured proxy must replace X-Forwarded-For with one client IP.
	// Chains, malformed values, and headers from other peers are ignored.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	ip = ip.Unmap()
	for _, prefix := range a.trustedProxies {
		if prefix.Contains(ip) {
			header := "X-Forwarded-For"
			if a.cloudflared {
				header = "CF-Connecting-IP"
			}
			if forwarded, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(header))); err == nil && forwarded.Zone() == "" {
				return forwarded.Unmap().String()
			}
			break
		}
	}
	return ip.String()
}

func (a *api) authenticated(next action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fields := strings.Fields(r.Header.Get("Authorization"))
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
			a.fail(w, r, identity.ErrUnauthorized)
			return
		}
		var p identity.Principal
		var err error
		if (r.Method == "PUT" && r.URL.Path == "/api/v1/me/password") || (r.Method == "POST" && r.URL.Path == "/api/v1/auth/logout") {
			p, err = a.identity.AuthenticatePasswordChange(r.Context(), fields[1])
		} else {
			p, err = a.identity.Authenticate(r.Context(), fields[1])
		}
		if err != nil {
			a.fail(w, r, err)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), auditActorKey{}, p.User.ID))
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !a.limit(w, "mutate:"+p.User.ID, 60, time.Minute) {
				return
			}
		}
		if a.limits.allow("observe:"+p.User.ID+":"+a.remoteIP(r), 1, 5*time.Minute) {
			if err := a.identity.ObserveSessionIP(r.Context(), p); err != nil {
				a.fail(w, r, err)
				return
			}
		}
		a.auditSettings(w, r, p, next)
	}
}

func (a *api) routes(mux routeRegistrar) {
	a.moderationRoutes(mux)
	a.customizationRoutes(mux)
	a.emojiRoutes(mux)
	mux.HandleFunc("GET /api/v1/users", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		users, err := a.identity.Directory(r.Context(), r.URL.Query().Get("after"), "")
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, usersResponse{Users: users})
	}))
	mux.HandleFunc("GET /api/v1/users/{id}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		users, err := a.identity.Directory(r.Context(), "", r.PathValue("id"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if len(users) == 0 {
			a.fail(w, r, identity.ErrNotFound)
			return
		}
		writeJSON(w, 200, users[0])
	}))
	mux.HandleFunc("PUT /api/v1/me/password", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v ChangePasswordRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		if err := a.identity.ChangePassword(r.Context(), p, v.Current, v.Password); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("PUT /api/v1/admin/members/{id}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v DisableMemberRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		if v.Disabled == nil {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		if err := a.identity.Disable(r.Context(), p, r.PathValue("id"), *v.Disabled); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST /api/v1/admin/members/{id}/password-reset", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v TemporaryPasswordRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		result, err := a.identity.IssueTemporaryPassword(r.Context(), p, r.PathValue("id"), v.Reason)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, result)
	}))
	mux.HandleFunc("PUT /api/v1/admin/members/{id}/password", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v ResetPasswordRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		if err := a.identity.ResetMember(r.Context(), p, r.PathValue("id"), v.Password); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))

	mux.HandleFunc("POST /api/v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
		if !a.limit(w, "register-ip:"+a.remoteIP(r), 10, 5*time.Minute) {
			return
		}
		var v CredentialsRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		result, err := a.identity.Register(r.Context(), v.Username, v.Password, v.Invitation, v.Device, v.Remember)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, result)
	})
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if !a.limit(w, "login-ip:"+a.remoteIP(r), 30, 5*time.Minute) {
			return
		}
		var v CredentialsRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		if !a.limit(w, "login-user:"+strings.ToLower(v.Username), 10, 5*time.Minute) {
			return
		}
		result, err := a.identity.Login(r.Context(), v.Username, v.Password, v.Device, v.Remember)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /api/v1/me", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		user, err := a.identity.Profile(r.Context(), p.User.ID)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		p.User = user
		writeJSON(w, http.StatusOK, p)
	}))
	mux.HandleFunc("PUT /api/v1/me/session", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v RememberSessionRequest
		if err := decode(w, r, &v); err != nil || v.Remember == nil {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		session, err := a.identity.RememberSession(r.Context(), p, *v.Remember)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, session)
	}))
	mux.HandleFunc("POST /api/v1/sessions/restore", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !a.limit(w, "session-restore:"+p.User.ID, 10, 5*time.Minute) {
			return
		}
		var v RestoreSessionRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		result, err := a.identity.RestoreSession(r.Context(), p, v.Device)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		a.auditAccess(r, "session.restored")
		writeJSON(w, http.StatusCreated, result)
	}))
	mux.HandleFunc("GET /api/v1/sessions", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		result, err := a.identity.Sessions(r.Context(), p)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, sessionsResponse{Sessions: result})
	}))
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if err := a.identity.RevokeSession(r.Context(), p, r.PathValue("id")); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/v1/auth/logout", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if err := a.identity.RevokeSession(r.Context(), p, p.SessionID); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/v1/invites", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v CreateInviteRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		result, err := a.identity.CreateInvite(r.Context(), p, v.Uses, v.Hours)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, result)
	}))
	mux.HandleFunc("GET /api/v1/invites", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		result, err := a.identity.Invites(r.Context(), p)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, invitesResponse{Invites: result})
	}))
	mux.HandleFunc("DELETE /api/v1/invites/{id}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if err := a.identity.RevokeInvite(r.Context(), p, r.PathValue("id")); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/v1/roles", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		result, err := a.identity.Roles(r.Context())
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, rolesResponse{Roles: result})
	}))
	roleSave := a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v SaveRoleRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		if v.Permissions == nil {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		result, err := a.identity.SaveRole(r.Context(), p, r.PathValue("id"), v.Name, *v.Permissions, v.Position, v.DisplaySeparately)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		writeJSON(w, status, result)
	})
	mux.HandleFunc("POST /api/v1/roles", roleSave)
	mux.HandleFunc("PUT /api/v1/roles/{id}", roleSave)
	mux.HandleFunc("DELETE /api/v1/roles/{id}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if err := a.identity.DeleteRole(r.Context(), p, r.PathValue("id")); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/v1/members", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		result, err := a.identity.Members(r.Context(), p, r.URL.Query().Get("after"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, membersResponse{Members: result})
	}))
	mux.HandleFunc("PUT /api/v1/members/{id}/roles", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v AssignRolesRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		if v.Roles == nil {
			a.fail(w, r, identity.ErrInvalid)
			return
		}
		if err := a.identity.AssignRoles(r.Context(), p, r.PathValue("id"), *v.Roles); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/v1/categories", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		result, err := a.identity.Categories(r.Context(), p)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, categoriesResponse{Categories: result})
	}))
	categorySave := a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v SaveCategoryRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		result, err := a.identity.SaveCategory(r.Context(), p, r.PathValue("id"), v.Name)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		writeJSON(w, status, result)
	})
	mux.HandleFunc("POST /api/v1/categories", categorySave)
	mux.HandleFunc("PUT /api/v1/categories/{id}", categorySave)
	mux.HandleFunc("GET /api/v1/channels", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		result, err := a.identity.Channels(r.Context(), p, "")
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, channelsResponse{Channels: result})
	}))
	mux.HandleFunc("GET /api/v1/channels/{id}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		result, err := a.identity.Channel(r.Context(), p, r.PathValue("id"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}))
	channelSave := a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v SaveChannelRequest
		if err := decode(w, r, &v); err != nil {
			a.fail(w, r, err)
			return
		}
		result, err := a.identity.SaveChannel(r.Context(), p, r.PathValue("id"), v.Name, v.Kind, v.Category, identity.ChannelDetails{Topic: v.Topic, Icon: v.Icon})
		if err != nil {
			a.fail(w, r, err)
			return
		}
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		writeJSON(w, status, result)
	})
	mux.HandleFunc("POST /api/v1/channels", channelSave)
	mux.HandleFunc("PUT /api/v1/channels/{id}", channelSave)
	for _, resource := range []struct{ path, kind string }{{"categories", "category"}, {"channels", "channel"}} {
		mux.HandleFunc("DELETE /api/v1/"+resource.path+"/{id}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
			if err := a.identity.DeleteResource(r.Context(), p, resource.kind, r.PathValue("id")); err != nil {
				a.fail(w, r, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		mux.HandleFunc("GET /api/v1/"+resource.path+"/{id}/permissions", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
			result, err := a.identity.Overrides(r.Context(), p, resource.kind, r.PathValue("id"))
			if err != nil {
				a.fail(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, overridesResponse{Overrides: result})
		}))
		mux.HandleFunc("PUT /api/v1/"+resource.path+"/{id}/permissions/{role}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
			var v SetPermissionsRequest
			if err := decode(w, r, &v); err != nil {
				a.fail(w, r, err)
				return
			}
			if v.Allow == nil || v.Deny == nil {
				a.fail(w, r, identity.ErrInvalid)
				return
			}
			if err := a.identity.SetOverride(r.Context(), p, resource.kind, r.PathValue("id"), r.PathValue("role"), *v.Allow, *v.Deny); err != nil {
				a.fail(w, r, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
	}
}
