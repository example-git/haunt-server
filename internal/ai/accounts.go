package ai

import (
	"context"
	"errors"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"sync"
	"time"
)

type ProviderInfo struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Models       []provider.Model `json:"models"`
	RedirectMode string           `json:"redirect_mode"`
	RedirectURI  string           `json:"redirect_uri"`
}

var providerNames = map[provider.ID]string{
	provider.Claude: "Claude", provider.Codex: "Codex / ChatGPT", provider.Antigravity: "Antigravity",
}

func (s *Service) Providers(ctx context.Context, p identity.Principal) ([]ProviderInfo, error) {
	if _, err := s.ids.Refresh(ctx, p); err != nil {
		return nil, err
	}
	out := []ProviderInfo{}
	for _, id := range []provider.ID{provider.Claude, provider.Codex, provider.Antigravity} {
		a := s.adapters[id]
		if a == nil {
			continue
		}
		models, err := a.Models(ctx, provider.Credentials{})
		if err != nil {
			return nil, err
		}
		o := a.OAuthConfig()
		out = append(out, ProviderInfo{ID: string(id), Name: providerNames[id], Models: models, RedirectMode: o.RedirectMode, RedirectURI: o.RedirectURI})
	}
	return out, nil
}
func (s *Service) Accounts(ctx context.Context, p identity.Principal, channel string) ([]Account, error) {
	if err := s.authorizeAccountRead(ctx, p, channel); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,provider,status,label,expires_at,created_at FROM chatxo.ai_accounts WHERE instance_id=$1 ORDER BY created_at,id`, s.options.InstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		var a Account
		if err = rows.Scan(&a.ID, &a.Provider, &a.Status, &a.Label, &a.ExpiresAt, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Service) Disconnect(ctx context.Context, p identity.Principal, id string) error {
	if err := s.authorizeAccounts(ctx, p); err != nil {
		return err
	}
	if !identity.ValidID(id) {
		return identity.ErrInvalid
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := s.ids.AuthorizeConfiguration(ctx, tx, p); err != nil {
			return err
		}
		r, err := tx.Exec(ctx, `UPDATE chatxo.ai_accounts SET credentials=NULL,status='disconnected',credential_revision=credential_revision+1,updated_at=now() WHERE id=$1 AND instance_id=$2`, id, s.options.InstanceID)
		if err != nil {
			return err
		}
		if r.RowsAffected() == 0 {
			return identity.ErrNotFound
		}
		rows, err := tx.Query(ctx, `UPDATE chatxo.ai_turns SET status='requires_auth',reservation_bytes=0,error_code='account_disconnected',revision=revision+1,updated_at=now() WHERE snapshot->>'account_id'=$1 AND status IN ('queued','running','waiting') RETURNING id`, id)
		if err != nil {
			return err
		}
		var turns []string
		for rows.Next() {
			var turn string
			if err = rows.Scan(&turn); err != nil {
				rows.Close()
				return err
			}
			turns = append(turns, turn)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		for _, turn := range turns {
			if err = s.emitTurn(ctx, tx, turn); err != nil {
				return err
			}
		}
		channels, err := tx.Query(ctx, `SELECT channel_id FROM chatxo.ai_channel_settings WHERE account_id=$1`, id)
		if err != nil {
			return err
		}
		var ids []string
		for channels.Next() {
			var ch string
			if err = channels.Scan(&ch); err != nil {
				channels.Close()
				return err
			}
			ids = append(ids, ch)
		}
		channels.Close()
		if err = channels.Err(); err != nil {
			return err
		}
		for _, ch := range ids {
			if err = s.settingsChanged(ctx, tx, ch); err != nil {
				return err
			}
		}
		return s.enforce(ctx, tx, "")
	})
}
func (s *Service) Models(ctx context.Context, p identity.Principal, id, channel string) ([]provider.Model, error) {
	if err := s.authorizeAccountRead(ctx, p, channel); err != nil {
		return nil, err
	}
	a, c, err := s.credentials(ctx, id)
	if err != nil {
		return nil, err
	}
	return a.Models(ctx, c)
}
func (s *Service) accountMutex(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.accounts[id]
	if m == nil {
		m = &sync.Mutex{}
		s.accounts[id] = m
	}
	return m
}
func (s *Service) credentials(ctx context.Context, id string) (provider.Adapter, provider.Credentials, error) {
	var empty provider.Credentials
	if !identity.ValidID(id) {
		return nil, empty, ErrConfiguration
	}
	m := s.accountMutex(id)
	m.Lock()
	defer m.Unlock()
	var kind, status string
	var raw []byte
	var rev int64
	err := s.pool.QueryRow(ctx, `SELECT provider,status,credentials,credential_revision FROM chatxo.ai_accounts WHERE id=$1 AND instance_id=$2`, id, s.options.InstanceID).Scan(&kind, &status, &raw, &rev)
	if err != nil {
		return nil, empty, translate(err)
	}
	a := s.adapters[provider.ID(kind)]
	if a == nil {
		return nil, empty, ErrConfiguration
	}
	if status != "connected" || len(raw) == 0 {
		return a, empty, &provider.Error{Kind: provider.RequiresAuth, Message: "account requires reconnection"}
	}
	c, err := s.decryptCredentials(id, raw)
	if err != nil {
		return a, c, err
	}
	if c.ExpiresAt.IsZero() || c.ExpiresAt.After(time.Now().Add(2*time.Minute)) {
		return a, c, nil
	}
	if c.RefreshToken == "" {
		s.markRequiresAuth(ctx, id, rev)
		return a, c, &provider.Error{Kind: provider.RequiresAuth, Message: "account requires reconnection"}
	}
	next, err := a.Refresh(ctx, c)
	if err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Kind == provider.RequiresAuth {
			s.markRequiresAuth(ctx, id, rev)
		}
		return a, c, err
	}
	if next.RefreshToken == "" {
		next.RefreshToken = c.RefreshToken
	}
	if next.AccountID == "" {
		next.AccountID = c.AccountID
	}
	if next.ProjectID == "" {
		next.ProjectID = c.ProjectID
	}
	if len(next.Metadata) == 0 {
		next.Metadata = c.Metadata
	}
	if next.AccessToken == "" {
		return a, c, &provider.Error{Kind: provider.Protocol, Message: "refresh returned no access token"}
	}
	sealed, err := s.encryptCredentials(id, next)
	if err != nil {
		return a, c, err
	}
	r, err := s.pool.Exec(ctx, `UPDATE chatxo.ai_accounts SET credentials=$2,credential_revision=credential_revision+1,expires_at=$3,updated_at=now() WHERE id=$1 AND credential_revision=$4 AND status='connected'`, id, sealed, expiry(next.ExpiresAt), rev)
	if err != nil {
		return a, c, err
	}
	if r.RowsAffected() != 1 {
		return a, c, identity.ErrConflict
	}
	return a, next, nil
}
func expiry(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
func (s *Service) refreshAccounts(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM chatxo.ai_accounts WHERE instance_id=$1 AND status='connected' AND expires_at<now()+interval '2 minutes' ORDER BY id LIMIT 32`, s.options.InstanceID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s.credentials(refreshCtx, id)
		cancel()
	}
}

func (s *Service) authorizeAccountRead(ctx context.Context, p identity.Principal, channel string) error {
	if channel != "" {
		_, err := s.channel(ctx, p, channel, true)
		return err
	}
	p, err := s.ids.Refresh(ctx, p)
	if err != nil {
		return err
	}
	if !p.Can(identity.ManageConfiguration) && !p.Can(identity.ManageChannels) {
		return identity.ErrForbidden
	}
	return nil
}

func (s *Service) markRequiresAuth(ctx context.Context, id string, revision int64) {
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `UPDATE chatxo.ai_accounts SET status='requires_auth',updated_at=now() WHERE id=$1 AND credential_revision=$2`, id, revision)
		if err != nil || result.RowsAffected() == 0 {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE chatxo.ai_turns SET status='requires_auth',reservation_bytes=0,error_code='account_requires_auth',revision=revision+1,updated_at=now() WHERE snapshot->>'account_id'=$1 AND status IN ('queued','running','waiting') RETURNING id`, id)
		if err != nil {
			return err
		}
		var turns []string
		for rows.Next() {
			var turn string
			if err = rows.Scan(&turn); err != nil {
				rows.Close()
				return err
			}
			turns = append(turns, turn)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		for _, turn := range turns {
			if err = s.emitTurn(ctx, tx, turn); err != nil {
				return err
			}
		}
		rows, err = tx.Query(ctx, `SELECT channel_id FROM chatxo.ai_channel_settings WHERE account_id=$1`, id)
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
		for _, ch := range channels {
			if err = s.settingsChanged(ctx, tx, ch); err != nil {
				return err
			}
		}
		return s.enforce(ctx, tx, "")
	})
	if err != nil {
		slog.Error("AI account failure state could not be saved", "account_id", id, "error_code", errorCode(err))
	}
}
