package identity

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

// RememberSession changes only the caller's existing, live device session.
// An expired/deleted session cannot be revived, including if revocation races
// this UPDATE. Repeated requests do not extend an ordinary session's expiry.
func (s *Service) RememberSession(ctx context.Context, p Principal, remember bool) (Session, error) {
	var session Session
	err := s.pool.QueryRow(ctx, `UPDATE chatxo.sessions s SET expires_at=
        CASE WHEN $4 THEN NULL ELSE COALESCE(s.expires_at,now()+$5::bigint*interval '1 second') END
        FROM chatxo.users u WHERE s.id=$1 AND s.instance_id=$2 AND s.user_id=$3
        AND u.id=s.user_id AND NOT u.disabled AND (s.expires_at IS NULL OR s.expires_at>now())
        RETURNING s.id,s.device_name,s.created_at,s.expires_at`, p.SessionID, s.instanceID, p.User.ID, remember, int64(s.sessionDuration()/time.Second)).Scan(&session.ID, &session.DeviceName, &session.CreatedAt, &session.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrUnauthorized
	}
	return session, err
}

func (s *Service) ChangePassword(ctx context.Context, p Principal, current, replacement string) error {
	if len(current) > 1024 || !validPassword(replacement) {
		return ErrInvalid
	}
	if current == replacement {
		return ErrSamePassword
	}
	var previous string
	if err := s.pool.QueryRow(ctx, `SELECT password_hash FROM chatxo.users WHERE id=$1 AND instance_id=$2 AND NOT disabled`, p.User.ID, s.instanceID).Scan(&previous); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthorized
		}
		return err
	}
	match, err := s.passwords.Verify(ctx, current, previous)
	if err != nil {
		return err
	}
	if !match {
		return ErrUnauthorized
	}
	next, err := s.passwords.Hash(ctx, replacement)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT password_hash FROM chatxo.users WHERE id=$1 AND NOT disabled FOR UPDATE`, p.User.ID).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthorized
		}
		return err
	}
	if _, err = s.principalLookup(ctx, tx, p.SessionID, nil, true); err != nil {
		return err
	}
	if locked != previous {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.users SET password_hash=$2,password_change_required=false WHERE id=$1`, p.User.ID, next); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.sessions WHERE user_id=$1`, p.User.ID); err != nil {
		return err
	}
	if err = s.audit(ctx, tx, p.User.ID, p.User.ID, "password.changed", ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Service) Disable(ctx context.Context, p Principal, id string, disabled bool) error {
	if !p.User.Owner {
		return ErrForbidden
	}
	if !ValidID(id) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owner bool
	if err = tx.QueryRow(ctx, `SELECT is_owner FROM chatxo.users WHERE instance_id=$1 AND id=$2 AND deleted_at IS NULL FOR UPDATE`, s.instanceID, id).Scan(&owner); err != nil {
		return translate(err)
	}
	if owner {
		return ErrForbidden
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.users SET disabled=$2 WHERE id=$1`, id, disabled); err != nil {
		return err
	}
	if disabled {
		if _, err = tx.Exec(ctx, `DELETE FROM chatxo.sessions WHERE user_id=$1`, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM chatxo.invites WHERE created_by=$1`, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ResetOwner is exposed only by the host-operated CLI. It never changes owner
// identity or bypasses transport authentication in the running HTTP service.
func (s *Service) ResetOwner(ctx context.Context, password string) error {
	if !validPassword(password) {
		return ErrInvalid
	}
	hash, err := s.passwords.Hash(ctx, password)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM chatxo.users WHERE instance_id=$1 AND is_owner FOR UPDATE`, s.instanceID).Scan(&id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.users SET password_hash=$2,password_change_required=false WHERE id=$1`, id, hash); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.sessions WHERE user_id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AuthenticatePasswordChange accepts restricted temporary sessions only for the
// password-change/logout routes. Every ordinary API and socket uses Authenticate.
func (s *Service) AuthenticatePasswordChange(ctx context.Context, token string) (Principal, error) {
	if !validToken(token) {
		return Principal{}, ErrUnauthorized
	}
	return s.principalLookup(ctx, s.pool, "", tokenHash(token), true)
}

type TemporaryPassword struct {
	Password string `json:"temporary_password" description:"Shown once. Share privately; the member must replace it before server access."`
}

func (s *Service) IssueTemporaryPassword(ctx context.Context, p Principal, id, reason string) (TemporaryPassword, error) {
	if !moderationText(reason, 1, 512) {
		return TemporaryPassword{}, ErrInvalid
	}
	password := newToken()[:24]
	err := s.resetMember(ctx, p, id, password, reason)
	if err != nil {
		return TemporaryPassword{}, err
	}
	return TemporaryPassword{Password: password}, nil
}

// The legacy supplied-password endpoint also always forces a password change.
func (s *Service) ResetMember(ctx context.Context, p Principal, id, password string) error {
	return s.resetMember(ctx, p, id, password, "Administrator supplied temporary password")
}
func (s *Service) resetMember(ctx context.Context, p Principal, id, password, reason string) error {
	if !p.Can(ResetPasswords) {
		return ErrForbidden
	}
	if !ValidID(id) || !validPassword(password) {
		return ErrInvalid
	}
	hash, err := s.passwords.Hash(ctx, password)
	if err != nil {
		return err
	}
	tx, p, err := s.moderationTx(ctx, p, ResetPasswords)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.moderateTarget(ctx, tx, p, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.users SET password_hash=$2,password_change_required=true WHERE id=$1`, id, hash); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.sessions WHERE user_id=$1`, id); err != nil {
		return err
	}
	if err = s.audit(ctx, tx, p.User.ID, id, "member.password_reset", reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
