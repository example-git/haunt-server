package ai

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func randomToken() (string, error) {
	v := make([]byte, 32)
	_, err := rand.Read(v)
	return base64.RawURLEncoding.EncodeToString(v), err
}
func validRedirect(id provider.ID, raw string, c provider.OAuthConfig) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	switch id {
	case provider.Codex:
		return raw == "http://localhost:1455/auth/callback"
	case provider.Antigravity:
		return raw == "https://antigravity.google/oauth-callback"
	case provider.Claude:
		port, err := strconv.Atoi(u.Port())
		return err == nil && port > 0 && port <= 65535 && u.Scheme == "http" && u.Hostname() == "localhost" && u.Path == "/callback"
	}
	return false
}
func (s *Service) PrepareOAuth(ctx context.Context, p identity.Principal, origin string, v OAuthPrepareInput) (OAuthSession, error) {
	if err := s.authorizeAccounts(ctx, p); err != nil {
		return OAuthSession{}, err
	}
	a := s.adapters[provider.ID(v.Provider)]
	if a == nil || !validRedirect(a.ID(), v.RedirectURI, a.OAuthConfig()) || (v.AccountID != "" && !identity.ValidID(v.AccountID)) {
		return OAuthSession{}, identity.ErrInvalid
	}
	if len(origin) > 2048 {
		return OAuthSession{}, identity.ErrInvalid
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return OAuthSession{}, identity.ErrInvalid
	}
	state, err := randomToken()
	if err != nil {
		return OAuthSession{}, err
	}
	verifier, err := randomToken()
	if err != nil {
		return OAuthSession{}, err
	}
	hash := sha256.Sum256([]byte(verifier))
	out := OAuthSession{ID: identity.NewID(), Provider: v.Provider, RedirectURI: v.RedirectURI, State: state, ExpiresAt: time.Now().Add(10 * time.Minute)}
	out.AuthorizationURL, err = a.OAuthConfig().AuthorizeURL(state, base64.RawURLEncoding.EncodeToString(hash[:]), v.RedirectURI)
	if err != nil {
		return out, err
	}
	encrypted, err := s.seal(out.ID, []byte(verifier))
	if err != nil {
		return out, err
	}
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		if err := s.ids.AuthorizeConfiguration(ctx, tx, p); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM chatxo.ai_oauth_sessions WHERE expires_at<=now() OR consumed_at IS NOT NULL`); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.ai_oauth_sessions WHERE user_id=$1`, p.User.ID).Scan(&count); err != nil {
			return err
		}
		if count >= 8 {
			return identity.ErrBusy
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.ai_oauth_sessions`).Scan(&count); err != nil {
			return err
		}
		if count >= 128 {
			return identity.ErrBusy
		}
		if v.AccountID != "" {
			var kind string
			if err := tx.QueryRow(ctx, `SELECT provider FROM chatxo.ai_accounts WHERE id=$1 AND instance_id=$2`, v.AccountID, s.options.InstanceID).Scan(&kind); err != nil {
				return translate(err)
			}
			if kind != v.Provider {
				return identity.ErrInvalid
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO chatxo.ai_oauth_sessions(id,instance_id,user_id,provider,origin,redirect_uri,state,verifier,account_id,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, out.ID, s.options.InstanceID, p.User.ID, v.Provider, origin, v.RedirectURI, state, encrypted, nullable(v.AccountID), out.ExpiresAt)
		return err
	})
	return out, err
}
func (s *Service) CompleteOAuth(ctx context.Context, p identity.Principal, origin, id string, v OAuthCompleteInput) (Account, error) {
	if err := s.authorizeAccounts(ctx, p); err != nil {
		return Account{}, err
	}
	if !identity.ValidID(id) || len(v.Code) > 8192 || len(v.State) > 256 {
		return Account{}, identity.ErrInvalid
	}
	var kind, redirect, state, accountID string
	var sealed []byte
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		if err := s.ids.AuthorizeConfiguration(ctx, tx, p); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT provider,redirect_uri,state,verifier,COALESCE(account_id::text,'') FROM chatxo.ai_oauth_sessions WHERE id=$1 AND instance_id=$2 AND user_id=$3 AND origin=$4 AND expires_at>now() AND consumed_at IS NULL FOR UPDATE`, id, s.options.InstanceID, p.User.ID, origin).Scan(&kind, &redirect, &state, &sealed, &accountID); err != nil {
			return translate(err)
		}
		code, gotState, err := parseCompletion(kind, v.Code, v.State, redirect)
		if err != nil {
			return err
		}
		v.Code = code
		v.State = gotState
		if subtle.ConstantTimeCompare([]byte(gotState), []byte(state)) != 1 {
			return identity.ErrInvalid
		}
		_, err = tx.Exec(ctx, `UPDATE chatxo.ai_oauth_sessions SET consumed_at=now() WHERE id=$1`, id)
		return err
	})
	if err != nil {
		return Account{}, err
	}
	a := s.adapters[provider.ID(kind)]
	if a == nil {
		return Account{}, identity.ErrInvalid
	}
	verifier, err := s.open(id, sealed)
	if err != nil {
		return Account{}, err
	}
	c, err := a.Exchange(ctx, provider.OAuthExchange{Code: v.Code, Verifier: string(verifier), RedirectURI: redirect, State: state})
	if err != nil {
		return Account{}, err
	}
	if len(c.AccountID) > 256 {
		return Account{}, &provider.Error{Kind: provider.Protocol, Message: "provider account label exceeds limit"}
	}
	if c.AccessToken == "" {
		return Account{}, &provider.Error{Kind: provider.Protocol, Message: "provider returned no access token"}
	}
	if accountID == "" {
		accountID = identity.NewID()
	}
	sealed, err = s.encryptCredentials(accountID, c)
	if err != nil {
		return Account{}, err
	}
	out := Account{ID: accountID, Provider: kind, Status: "connected", Label: c.AccountID}
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		if err := s.ids.AuthorizeConfiguration(ctx, tx, p); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.ai_accounts WHERE instance_id=$1`, s.options.InstanceID).Scan(&count); err != nil {
			return err
		}
		if count >= 32 {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.ai_accounts WHERE id=$1)`, accountID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return identity.ErrInvalid
			}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO chatxo.ai_accounts(id,instance_id,provider,created_by,label,credentials,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(id) DO UPDATE SET credentials=excluded.credentials,expires_at=excluded.expires_at,label=excluded.label,status='connected',credential_revision=chatxo.ai_accounts.credential_revision+1,updated_at=now() RETURNING created_at,expires_at`, accountID, s.options.InstanceID, kind, p.User.ID, c.AccountID, sealed, expiry(c.ExpiresAt)).Scan(&out.CreatedAt, &out.ExpiresAt); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT channel_id FROM chatxo.ai_channel_settings WHERE account_id=$1`, accountID)
		if err != nil {
			return err
		}
		var channels []string
		for rows.Next() {
			var channel string
			if err = rows.Scan(&channel); err != nil {
				rows.Close()
				return err
			}
			channels = append(channels, channel)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		for _, channel := range channels {
			if err = s.settingsChanged(ctx, tx, channel); err != nil {
				return err
			}
		}
		return s.enforce(ctx, tx, "")
	})
	return out, err
}
func parseCompletion(kind, raw, state, redirect string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
		u, err := url.Parse(raw)
		expected, _ := url.Parse(redirect)
		if err != nil || u.Scheme != expected.Scheme || u.Host != expected.Host || u.Path != expected.Path || u.User != nil || u.Query().Get("error") != "" {
			return "", "", identity.ErrInvalid
		}
		raw = u.Query().Get("code")
		received := u.Query().Get("state")
		if state != "" && state != received {
			return "", "", identity.ErrInvalid
		}
		state = received
	}
	if kind == "claude" && strings.Contains(raw, "#") {
		parts := strings.SplitN(raw, "#", 2)
		raw = parts[0]
		if state != "" && state != parts[1] {
			return "", "", identity.ErrInvalid
		}
		state = parts[1]
	}
	if raw == "" || state == "" || strings.ContainsAny(raw, "\r\n\x00") {
		return "", "", identity.ErrInvalid
	}
	return raw, state, nil
}
