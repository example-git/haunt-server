// Package identity owns persisted local accounts, invitations, device sessions,
// and community authorization. Each request uses its own pooled connection.
package identity

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/platform/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Service struct {
	runtime    atomic.Pointer[RuntimeConfiguration]
	pool       postgres.Pool
	instanceID string
	passwords  *Passwords
	sessionTTL time.Duration
	artSlots   chan struct{}
}

func New(ctx context.Context, c config.Config, pool postgres.Pool) (*Service, error) {
	passwords, err := NewPasswords(c.AuthParallel)
	if err != nil {
		return nil, err
	}
	s := &Service{pool: pool, instanceID: c.InstanceID, passwords: passwords, sessionTTL: time.Duration(c.SessionDays) * 24 * time.Hour, artSlots: make(chan struct{}, 4)}
	if err := s.bootstrap(ctx, c.BootstrapUsername, c.BootstrapPassword, false); err != nil {
		return nil, err
	}
	if err := s.LoadRuntime(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// ValidateFirstOwner validates the wizard's credentials without touching storage.
func ValidateFirstOwner(username, password string) error {
	if _, err := normalizeUsername(username); err != nil || !validPassword(password) {
		return fmt.Errorf("choose an owner username with 3–32 ASCII letters, numbers or underscores and a password with 8–1024 bytes")
	}
	return nil
}

// CreateFirstOwner refuses an existing owner, including a competing setup request.
// Unlike normal startup, it must never silently accept a different password.
func CreateFirstOwner(ctx context.Context, c config.Config, pool postgres.Pool) error {
	passwords, err := NewPasswords(c.AuthParallel)
	if err != nil {
		return err
	}
	s := &Service{pool: pool, instanceID: c.InstanceID, passwords: passwords}
	return s.bootstrap(ctx, c.BootstrapUsername, c.BootstrapPassword, true)
}

func (s *Service) bootstrap(ctx context.Context, username, password string, firstOnly bool) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.users WHERE instance_id=$1 AND is_owner)`, s.instanceID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		if firstOnly {
			return fmt.Errorf("an owner already exists; setup cannot replace their password")
		}
		return nil
	} // Never reset an existing owner's credentials.
	username, err := normalizeUsername(username)
	if err != nil || !validPassword(password) {
		return fmt.Errorf("first startup requires HAUNT_BOOTSTRAP_USERNAME (3-32 ASCII letters/digits/underscores) and HAUNT_BOOTSTRAP_PASSWORD (8+ characters, at most 1024 bytes)")
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
	// Startup only; serialize owner creation for this installation.
	if _, err = tx.Exec(ctx, `SELECT id FROM chatxo.instances WHERE id=$1 FOR UPDATE`, s.instanceID); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.users WHERE instance_id=$1 AND is_owner)`, s.instanceID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		if firstOnly {
			return fmt.Errorf("an owner already exists; setup cannot replace their password")
		}
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chatxo.roles(id,instance_id,name,permissions,is_default,position) VALUES ($1,$2,'everyone',7,true,0)`, newID(), s.instanceID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chatxo.users(id,instance_id,username,password_hash,is_owner) VALUES ($1,$2,$3,$4,true)`, newID(), s.instanceID, username, hash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Register(ctx context.Context, username, password, invitation, device string, remember ...bool) (Login, error) {
	if err := s.registrationOpen(ctx, s.pool); err != nil {
		return Login{}, err
	}
	username, err := normalizeUsername(username)
	if err != nil || !validPassword(password) || !validName(device, 128) {
		return Login{}, ErrInvalid
	}
	if !validToken(invitation) {
		return Login{}, ErrInvite
	}
	var available bool
	// Cheap precheck avoids hashing garbage invitations. Consumption is still
	// conditional inside the account transaction, so this grants no access.
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.invites WHERE instance_id=$1 AND token_hash=$2 AND remaining_uses>0 AND expires_at>now())`, s.instanceID, tokenHash(invitation)).Scan(&available); err != nil {
		return Login{}, err
	}
	if !available {
		return Login{}, ErrInvite
	}
	var returningID, returningHash, hash string
	lookupErr := s.pool.QueryRow(ctx, `SELECT id,password_hash FROM chatxo.users WHERE instance_id=$1 AND username=$2 AND kicked AND NOT disabled AND deleted_at IS NULL`, s.instanceID, username).Scan(&returningID, &returningHash)
	if lookupErr == nil {
		match, verifyErr := s.passwords.Verify(ctx, password, returningHash)
		if verifyErr != nil {
			return Login{}, verifyErr
		}
		if !match {
			return Login{}, ErrUnauthorized
		}
		if banErr := s.CheckBan(ctx, returningID); banErr != nil {
			return Login{}, banErr
		}
	} else if errors.Is(lookupErr, pgx.ErrNoRows) {
		hash, err = s.passwords.Hash(ctx, password)
		if err != nil {
			return Login{}, err
		}
	} else {
		return Login{}, lookupErr
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Login{}, err
	}
	defer tx.Rollback(ctx)
	if err := s.registrationOpen(ctx, tx); err != nil {
		return Login{}, err
	}
	result, err := tx.Exec(ctx, `UPDATE chatxo.invites SET remaining_uses=remaining_uses-1 WHERE instance_id=$1 AND token_hash=$2 AND remaining_uses>0 AND expires_at>clock_timestamp()`, s.instanceID, tokenHash(invitation))
	if err != nil {
		return Login{}, err
	}
	if result.RowsAffected() != 1 {
		return Login{}, ErrInvite
	}
	user := User{ID: newID(), Username: username}
	var existingID, existingHash string
	existingErr := tx.QueryRow(ctx, `SELECT id,password_hash FROM chatxo.users WHERE instance_id=$1 AND username=$2 AND kicked AND NOT disabled AND deleted_at IS NULL FOR UPDATE`, s.instanceID, username).Scan(&existingID, &existingHash)
	if existingErr == nil {
		if returningID != existingID || returningHash != existingHash {
			return Login{}, ErrConflict
		}
		user.ID = existingID
		err = tx.QueryRow(ctx, `UPDATE chatxo.users SET kicked=false WHERE id=$1 RETURNING created_at`, user.ID).Scan(&user.CreatedAt)
	} else if errors.Is(existingErr, pgx.ErrNoRows) {
		if returningID != "" {
			return Login{}, ErrConflict
		}
		err = tx.QueryRow(ctx, `INSERT INTO chatxo.users(id,instance_id,username,password_hash) VALUES ($1,$2,$3,$4) RETURNING created_at`, user.ID, s.instanceID, username, hash).Scan(&user.CreatedAt)
	} else {
		err = existingErr
	}
	if err != nil {
		return Login{}, translate(err)
	}
	login, err := s.insertSession(ctx, tx, user, device, len(remember) > 0 && remember[0])
	if err != nil {
		return Login{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Login{}, err
	}
	return login, nil
}

func (s *Service) Login(ctx context.Context, username, password, device string, remember ...bool) (Login, error) {
	username, err := normalizeUsername(username)
	if err != nil || len(password) > 1024 || !validName(device, 128) {
		return Login{}, ErrInvalid
	}
	user := User{}
	var kicked bool
	hash := s.passwords.dummy
	err = s.pool.QueryRow(ctx, `SELECT id,username,is_owner,created_at,password_hash,kicked FROM chatxo.users WHERE instance_id=$1 AND username=$2 AND NOT disabled`, s.instanceID, username).Scan(&user.ID, &user.Username, &user.Owner, &user.CreatedAt, &hash, &kicked)
	missing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !missing {
		return Login{}, err
	}
	match, err := s.passwords.Verify(ctx, password, hash)
	if err != nil {
		return Login{}, err
	}
	if missing || !match {
		return Login{}, ErrUnauthorized
	}
	if err := s.CheckBan(ctx, user.ID); err != nil {
		return Login{}, err
	}
	if kicked {
		return Login{}, ErrKicked
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Login{}, err
	}
	defer tx.Rollback(ctx)
	// Per-user lock bounds active sessions, without serializing other accounts.
	var currentHash string
	if err = tx.QueryRow(ctx, `SELECT password_hash FROM chatxo.users WHERE id=$1 AND NOT disabled AND NOT kicked AND deleted_at IS NULL FOR UPDATE`, user.ID).Scan(&currentHash); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Login{}, ErrUnauthorized
		}
		return Login{}, err
	}
	if currentHash != hash {
		return Login{}, ErrUnauthorized
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.sessions WHERE user_id=$1 AND expires_at<=now()`, user.ID); err != nil {
		return Login{}, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.sessions WHERE user_id=$1`, user.ID).Scan(&count); err != nil {
		return Login{}, err
	}
	if count >= 20 {
		return Login{}, ErrSessionLimit
	}
	login, err := s.insertSession(ctx, tx, user, device, len(remember) > 0 && remember[0])
	if err != nil {
		return Login{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Login{}, err
	}
	return login, nil
}

func (s *Service) insertSession(ctx context.Context, tx pgx.Tx, user User, device string, remember bool) (Login, error) {
	if err := s.observeIP(ctx, tx, user.ID); err != nil {
		return Login{}, err
	}
	var changeRequired bool
	if err := tx.QueryRow(ctx, `SELECT password_change_required FROM chatxo.users WHERE id=$1`, user.ID).Scan(&changeRequired); err != nil {
		return Login{}, err
	}
	ttl := s.sessionDuration()
	if changeRequired {
		remember = false
		ttl = 15 * time.Minute
	}
	token := newToken()
	session := Session{ID: newID(), DeviceName: device}
	err := tx.QueryRow(ctx, `INSERT INTO chatxo.sessions(id,instance_id,user_id,token_hash,device_name,expires_at,remote_ip) VALUES ($1,$2,$3,$4,$5,CASE WHEN $7 THEN NULL ELSE now()+$6::bigint*interval '1 second' END,$8) RETURNING created_at,expires_at`, session.ID, s.instanceID, user.ID, tokenHash(token), device, int64(ttl/time.Second), remember, nullableIP(ctx)).Scan(&session.CreatedAt, &session.ExpiresAt)
	return Login{PasswordChangeRequired: changeRequired, InstanceID: s.instanceID, User: user, Session: session, Token: token, TokenType: "Bearer"}, err
}

func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if !validToken(token) {
		return Principal{}, ErrUnauthorized
	}
	return s.authenticateHash(ctx, tokenHash(token))
}

func (s *Service) Refresh(ctx context.Context, p Principal) (Principal, error) {
	return s.principal(ctx, s.pool, p.SessionID)
}
func (s *Service) authenticateHash(ctx context.Context, hash []byte) (Principal, error) {
	return s.principalLookup(ctx, s.pool, "", hash)
}

func (s *Service) Sessions(ctx context.Context, p Principal) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,device_name,created_at,expires_at FROM chatxo.sessions WHERE instance_id=$1 AND user_id=$2 AND (expires_at IS NULL OR expires_at>now()) ORDER BY created_at,id`, s.instanceID, p.User.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := []Session{}
	for rows.Next() {
		var item Session
		if err := rows.Scan(&item.ID, &item.DeviceName, &item.CreatedAt, &item.ExpiresAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, item)
	}
	return sessions, rows.Err()
}

func (s *Service) RevokeSession(ctx context.Context, p Principal, id string) error {
	if !ValidID(id) {
		return ErrInvalid
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM chatxo.sessions WHERE instance_id=$1 AND user_id=$2 AND id=$3`, s.instanceID, p.User.ID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) CreateInvite(ctx context.Context, p Principal, uses, hours int) (Invite, error) {
	if !p.Can(CreateInvites) {
		return Invite{}, ErrForbidden
	}
	if uses < 1 || uses > 100 || hours < 1 || hours > 720 {
		return Invite{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Invite{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT id FROM chatxo.users WHERE id=$1 FOR UPDATE`, p.User.ID); err != nil {
		return Invite{}, err
	}
	var disabled bool
	if err = tx.QueryRow(ctx, `SELECT disabled FROM chatxo.users WHERE id=$1`, p.User.ID).Scan(&disabled); err != nil {
		return Invite{}, err
	}
	if disabled {
		return Invite{}, ErrForbidden
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.invites WHERE created_by=$1 AND (expires_at<=now() OR remaining_uses=0)`, p.User.ID); err != nil {
		return Invite{}, err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.invites WHERE created_by=$1`, p.User.ID).Scan(&count); err != nil {
		return Invite{}, err
	}
	if count >= 100 {
		return Invite{}, ErrConflict
	}
	item := Invite{ID: newID(), CreatedBy: p.User.ID, RemainingUses: uses, Token: newToken()}
	err = tx.QueryRow(ctx, `INSERT INTO chatxo.invites(id,instance_id,token_hash,created_by,remaining_uses,expires_at) VALUES ($1,$2,$3,$4,$5,now()+$6::integer*interval '1 hour') RETURNING created_at,expires_at`, item.ID, s.instanceID, tokenHash(item.Token), p.User.ID, uses, hours).Scan(&item.CreatedAt, &item.ExpiresAt)
	if err != nil {
		return Invite{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Invite{}, err
	}
	return item, nil
}

func (s *Service) Invites(ctx context.Context, p Principal) ([]Invite, error) {
	if !p.Can(CreateInvites) {
		return nil, ErrForbidden
	}
	rows, err := s.pool.Query(ctx, `SELECT id,created_by,remaining_uses,created_at,expires_at FROM chatxo.invites WHERE instance_id=$1 AND ($2 OR created_by=$3) AND remaining_uses>0 AND (expires_at IS NULL OR expires_at>now()) ORDER BY created_at,id LIMIT 1000`, s.instanceID, p.Can(Administrator), p.User.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Invite{}
	for rows.Next() {
		var item Invite
		if err := rows.Scan(&item.ID, &item.CreatedBy, &item.RemainingUses, &item.CreatedAt, &item.ExpiresAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) RevokeInvite(ctx context.Context, p Principal, id string) error {
	if !p.Can(CreateInvites) {
		return ErrForbidden
	}
	if !ValidID(id) {
		return ErrInvalid
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM chatxo.invites WHERE instance_id=$1 AND id=$2 AND ($3 OR created_by=$4)`, s.instanceID, id, p.Can(Administrator), p.User.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Cleanup batches are bounded. This runs periodically even without login traffic.
func (s *Service) Cleanup(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM chatxo.sessions WHERE id IN (SELECT id FROM chatxo.sessions WHERE instance_id=$1 AND expires_at<=now() ORDER BY expires_at LIMIT 1000)`, s.instanceID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM chatxo.invites WHERE id IN (SELECT id FROM chatxo.invites WHERE instance_id=$1 AND (expires_at<=now() OR remaining_uses=0) ORDER BY expires_at LIMIT 1000)`, s.instanceID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM chatxo.audit_log WHERE id IN (SELECT id FROM chatxo.audit_log WHERE instance_id=$1 AND (created_at<now()-interval '90 days' OR id < COALESCE((SELECT id FROM chatxo.audit_log WHERE instance_id=$1 ORDER BY id DESC OFFSET 9999 LIMIT 1),0)) ORDER BY id LIMIT 2000)`, s.instanceID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM chatxo.member_ips WHERE (user_id,ip) IN (SELECT user_id,ip FROM chatxo.member_ips WHERE last_seen<now()-interval '90 days' ORDER BY last_seen LIMIT 2000)`)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM chatxo.ban_appeals WHERE id IN (SELECT id FROM chatxo.ban_appeals WHERE created_at<now()-interval '180 days' LIMIT 1000)`)
	return err
}

func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) {
		if pgerr.Code == "40001" || pgerr.Code == "40P01" {
			return ErrBusy
		}
		if pgerr.Code == "23505" {
			return ErrConflict
		}
		if pgerr.Code == "23503" {
			return ErrNotFound
		}
		if pgerr.Code == "23514" {
			return ErrInvalid
		}
	}
	return err
}
