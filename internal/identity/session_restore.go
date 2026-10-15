package identity

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// RestoreSession gives an imported backup its own device credential. It never
// revokes the source or extends its expiry. A per-account lock bounds concurrent
// logins/restores and serializes against password changes and account removal.
func (s *Service) RestoreSession(ctx context.Context, p Principal, device string) (Login, error) {
	if !validName(device, 128) {
		return Login{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Login{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM chatxo.users WHERE instance_id=$1 AND id=$2 FOR UPDATE`, s.instanceID, p.User.ID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Login{}, ErrUnauthorized
		}
		return Login{}, err
	}
	// Hold the source row until commit so a concurrent logout cannot restore a
	// session it has already revoked. Recheck all bans and password-reset state.
	var expires *time.Time
	if err = tx.QueryRow(ctx, `SELECT expires_at FROM chatxo.sessions WHERE instance_id=$1 AND user_id=$2 AND id=$3 AND (expires_at IS NULL OR expires_at>now()) FOR SHARE`, s.instanceID, p.User.ID, p.SessionID).Scan(&expires); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Login{}, ErrUnauthorized
		}
		return Login{}, err
	}
	p, err = s.principal(ctx, tx, p.SessionID)
	if err != nil {
		return Login{}, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.sessions WHERE user_id=$1 AND expires_at<=now()`, p.User.ID); err != nil {
		return Login{}, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.sessions WHERE user_id=$1`, p.User.ID).Scan(&count); err != nil {
		return Login{}, err
	}
	if count >= 20 {
		return Login{}, ErrSessionLimit
	}
	login, err := s.insertSession(ctx, tx, p.User, device, expires == nil)
	if err != nil {
		return Login{}, err
	}
	if expires != nil {
		if _, err = tx.Exec(ctx, `UPDATE chatxo.sessions SET expires_at=$1 WHERE id=$2`, expires, login.Session.ID); err != nil {
			return Login{}, err
		}
		login.Session.ExpiresAt = expires
	}
	if err = tx.Commit(ctx); err != nil {
		return Login{}, err
	}
	return login, nil
}
