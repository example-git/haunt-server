package httpapi

import (
	"errors"
	"github.com/example-git/haunt-server/internal/identity"

	"net/http"
	"strconv"
	"strings"
	"time"
)

type ModerateMemberRequest struct {
	Action string `json:"action" required:"true" enum:"kick,ban,restore,delete"`
	Reason string `json:"reason" required:"true" minLength:"1" maxLength:"512"`
}
type BanNetworkRequest struct {
	Network string `json:"network" required:"true" description:"IPv4 or IPv6 address or CIDR range; normalized by the service."`
	Reason  string `json:"reason" required:"true" minLength:"1" maxLength:"512"`
}
type ModerationReasonRequest struct {
	Reason string `json:"reason" required:"true" minLength:"1" maxLength:"512"`
}
type BanTicketRequest struct {
	Ticket string `json:"appeal_ticket" required:"true"`
}
type BanAppealRequest struct {
	Ticket string `json:"appeal_ticket" required:"true"`
	Body   string `json:"body" required:"true" minLength:"10" maxLength:"1000" description:"Plain text only, no URLs or attachments. One submission per ban per rolling 30 days."`
}
type bansResponse struct {
	Bans []identity.Ban `json:"bans"`
}
type appealsResponse struct {
	Appeals []identity.Appeal `json:"appeals"`
}
type memberIPsResponse struct {
	IPs []identity.MemberIP `json:"ips"`
}
type auditResponse struct {
	Entries []identity.AuditEntry `json:"entries"`
}
type auditQuery struct {
	Before int64 `query:"before" description:"Exclusive audit ID cursor; newest 100 entries by default."`
}

// Capture the trusted socket/proxy address once for HTTP and long-lived media
// contexts. Appeal routes remain reachable from a banned IP.
func (a *api) moderationBoundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(identity.WithRemoteIP(r.Context(), a.remoteIP(r)))
		if strings.HasPrefix(r.URL.Path, "/api/v1/") && !strings.HasPrefix(r.URL.Path, "/api/v1/auth/ban/") {
			if err := a.identity.CheckBan(r.Context(), ""); err != nil {
				a.fail(w, r, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type auditActorKey struct{}

func (a *api) auditAccess(r *http.Request, action string) {
	// Both per-source and global budgets bound writes even under distributed
	// probing. No credentials, raw request bodies, or arbitrary URLs are logged.
	if !a.limits.allow("audit-source:"+a.remoteIP(r)+":"+action, 1, time.Minute) || !a.limits.allow("audit-global", 60, time.Minute) {
		return
	}
	actor, _ := r.Context().Value(auditActorKey{}).(string)
	target := r.PathValue("id")
	if !identity.ValidID(target) {
		target = ""
	}
	if err := a.identity.RecordAudit(r.Context(), actor, target, action, r.Pattern); err != nil {
		a.logger.Warn("access audit write failed", "error", err)
	}
}
func (a *api) moderationRoutes(mux routeRegistrar) {
	mux.HandleFunc("POST /api/v1/admin/members/{id}/moderate", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v ModerateMemberRequest
		if err := decodeLimit(w, r, &v, 4096); err != nil {
			a.fail(w, r, err)
			return
		}
		if err := a.identity.ModerateMember(r.Context(), p, r.PathValue("id"), v.Action, v.Reason); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("GET /api/v1/admin/members/{id}/ips", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		v, err := a.identity.MemberIPs(r.Context(), p, r.PathValue("id"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, memberIPsResponse{v})
	}))
	mux.HandleFunc("GET /api/v1/admin/bans", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		v, err := a.identity.Bans(r.Context(), p, r.URL.Query().Get("after"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, bansResponse{v})
	}))
	mux.HandleFunc("POST /api/v1/admin/bans", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v BanNetworkRequest
		if err := decodeLimit(w, r, &v, 4096); err != nil {
			a.fail(w, r, err)
			return
		}
		b, err := a.identity.BanNetwork(r.Context(), p, v.Network, v.Reason)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 201, b)
	}))
	mux.HandleFunc("POST /api/v1/admin/bans/{id}/revoke", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var v ModerationReasonRequest
		if err := decodeLimit(w, r, &v, 4096); err != nil {
			a.fail(w, r, err)
			return
		}
		if err := a.identity.RevokeBan(r.Context(), p, r.PathValue("id"), v.Reason); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("GET /api/v1/admin/appeals", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		v, err := a.identity.Appeals(r.Context(), p, r.URL.Query().Get("after"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, appealsResponse{v})
	}))
	mux.HandleFunc("POST /api/v1/admin/appeals/{id}/review", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if err := a.identity.ReviewAppeal(r.Context(), p, r.PathValue("id")); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("GET /api/v1/admin/audit", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		var before int64
		var err error
		if raw := r.URL.Query().Get("before"); raw != "" {
			before, err = strconv.ParseInt(raw, 10, 64)
			if err != nil {
				a.fail(w, r, identity.ErrInvalid)
				return
			}
		}
		v, err := a.identity.AuditLog(r.Context(), p, before)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, auditResponse{v})
	}))
	mux.HandleFunc("POST /api/v1/auth/ban/status", func(w http.ResponseWriter, r *http.Request) {
		if !a.limit(w, "appeal-status:"+a.remoteIP(r), 10, time.Hour) {
			return
		}
		var v BanTicketRequest
		if err := decodeLimit(w, r, &v, 1024); err != nil {
			a.fail(w, r, err)
			return
		}
		result, err := a.identity.BanStatus(r.Context(), v.Ticket)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, 200, result)
	})
	mux.HandleFunc("POST /api/v1/auth/ban/appeal", func(w http.ResponseWriter, r *http.Request) {
		if !a.limit(w, "appeal-send:"+a.remoteIP(r), 3, 24*time.Hour) {
			return
		}
		var v BanAppealRequest
		if err := decodeLimit(w, r, &v, 6144); err != nil {
			a.fail(w, r, err)
			return
		}
		if err := a.identity.AppealBan(r.Context(), v.Ticket, v.Body); err != nil {
			a.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	})
}
func banError(err error) *identity.AccessBan { var b *identity.AccessBan; errors.As(err, &b); return b }

// Authenticated legacy settings routes keep a small, credential-free audit.
// Core role/member/ban operations already audit atomically in their own tx.
type auditWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *auditWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(b)
}
func (w *auditWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (a *api) auditSettings(w http.ResponseWriter, r *http.Request, p identity.Principal, next action) {
	actionName := ""
	switch {
	case r.Pattern == "GET /api/v1/admin/members/{id}/ips":
		actionName = "member.ips.read"
	case r.Method != "GET" && r.Method != "HEAD" && (strings.HasPrefix(r.Pattern, r.Method+" /api/v1/instance/") || strings.HasPrefix(r.Pattern, r.Method+" /api/v1/emojis/") || strings.HasPrefix(r.Pattern, r.Method+" /api/v1/channels") || strings.HasPrefix(r.Pattern, r.Method+" /api/v1/categories") || r.Pattern == "PUT /api/v1/admin/members/{id}" || r.Pattern == "PUT /api/v1/admin/members/{id}/password"):
		actionName = "settings." + strings.ToLower(r.Method)
	}
	if actionName == "" {
		next(w, r, p)
		return
	}
	response := &auditWriter{ResponseWriter: w}
	next(response, r, p)
	if response.status >= 200 && response.status < 300 {
		if err := a.identity.RecordAudit(r.Context(), p.User.ID, r.PathValue("id"), actionName, r.Pattern); err != nil {
			a.logger.Error("settings audit write failed", "route", r.Pattern, "error", err)
		}
	}
}
