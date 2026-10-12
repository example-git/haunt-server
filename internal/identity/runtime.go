package identity

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/jackc/pgx/v5"
	"time"
)

var ErrConfiguration = errors.New("invalid_configuration")
var ErrBudget = errors.New("attachment_budget_too_small")
var ErrRegistrationClosed = errors.New("registration_closed")

type RuntimeConfiguration struct {
	Revision int64                  `json:"revision"`
	Settings config.RuntimeSettings `json:"settings"`
}

// publish never lets a slower completed request replace a newer configuration.
func (s *Service) publish(r *RuntimeConfiguration) {
	for {
		old := s.runtime.Load()
		if old != nil && old.Revision >= r.Revision {
			return
		}
		if s.runtime.CompareAndSwap(old, r) {
			return
		}
	}
}
func (s *Service) LoadRuntime(ctx context.Context) error {
	var raw []byte
	var r RuntimeConfiguration
	if err := s.pool.QueryRow(ctx, `SELECT runtime_settings,settings_revision FROM chatxo.instances WHERE singleton`).Scan(&raw, &r.Revision); err != nil {
		return err
	}
	if raw == nil {
		return nil
	}
	if err := json.Unmarshal(raw, &r.Settings); err != nil {
		return err
	}
	r.Settings.Limits = r.Settings.Limits.WithDefaults()
	if err := r.Settings.Validate(); err != nil {
		return err
	}
	s.publish(&r)
	return nil
}
func (s *Service) Runtime(fallback config.RuntimeSettings) RuntimeConfiguration {
	if r := s.runtime.Load(); r != nil {
		return *r
	}
	fallback.Limits = fallback.Limits.WithDefaults()
	return RuntimeConfiguration{Settings: fallback}
}
func (s *Service) CurrentPolicy(fallback config.Policy) config.Policy {
	if r := s.runtime.Load(); r != nil {
		return r.Settings.Limits.WithDefaults()
	}
	return fallback.WithDefaults()
}
func (s *Service) RoomLimit(fallback int) int {
	if r := s.runtime.Load(); r != nil {
		return r.Settings.MediaRoomLimit
	}
	return fallback
}
func (s *Service) PeerLimit(fallback int) int {
	if r := s.runtime.Load(); r != nil {
		return r.Settings.MediaMaxPeers
	}
	return fallback
}
func (s *Service) sessionDuration() time.Duration {
	if r := s.runtime.Load(); r != nil {
		return time.Duration(r.Settings.SessionDays) * 24 * time.Hour
	}
	return s.sessionTTL
}

// Read through the caller's transaction so admission and budget checks serialize
// with configuration changes without acquiring another pooled connection.
func (s *Service) PolicyForTransaction(ctx context.Context, tx pgx.Tx, fallback config.Policy) (config.Policy, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT runtime_settings FROM chatxo.instances WHERE singleton`).Scan(&raw); err != nil {
		return config.Policy{}, err
	}
	if raw == nil {
		return fallback.WithDefaults(), nil
	}
	var r config.RuntimeSettings
	err := json.Unmarshal(raw, &r)
	return r.Limits.WithDefaults(), err
}
func (s *Service) registrationOpen(ctx context.Context, q rowQuerier) error {
	var enabled bool
	if err := q.QueryRow(ctx, `SELECT COALESCE((runtime_settings->>'registration_enabled')::boolean,true) FROM chatxo.instances WHERE singleton FOR SHARE`).Scan(&enabled); err != nil {
		return err
	}
	if !enabled {
		return ErrRegistrationClosed
	}
	return nil
}
func (s *Service) SaveRuntime(ctx context.Context, p Principal, r RuntimeConfiguration) (RuntimeConfiguration, error) {
	if !p.Can(ManageConfiguration) {
		return RuntimeConfiguration{}, ErrForbidden
	}
	r.Settings.Limits = r.Settings.Limits.WithDefaults()
	if r.Revision < 0 || r.Settings.Validate() != nil {
		return RuntimeConfiguration{}, ErrConfiguration
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RuntimeConfiguration{}, err
	}
	defer tx.Rollback(ctx)
	var used int64
	if err = tx.QueryRow(ctx, `SELECT attachment_bytes-dm_attachment_bytes FROM chatxo.storage WHERE singleton FOR UPDATE`).Scan(&used); err != nil {
		return RuntimeConfiguration{}, err
	}
	fresh, err := s.principal(ctx, tx, p.SessionID)
	if err != nil {
		return RuntimeConfiguration{}, err
	}
	if !fresh.Can(ManageConfiguration) {
		return RuntimeConfiguration{}, ErrForbidden
	}
	if r.Settings.Limits.AttachmentMaxBytes < used {
		return RuntimeConfiguration{}, ErrBudget
	}
	raw, _ := json.Marshal(r.Settings)
	err = tx.QueryRow(ctx, `UPDATE chatxo.instances SET runtime_settings=$1,settings_revision=settings_revision+1 WHERE singleton AND settings_revision=$2 RETURNING settings_revision`, raw, r.Revision).Scan(&r.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeConfiguration{}, ErrConflict
	}
	if err != nil {
		return RuntimeConfiguration{}, err
	}
	auditReason, _ := json.Marshal(r)
	if err = s.audit(ctx, tx, p.User.ID, "", "server.configuration.updated", string(auditReason)); err != nil {
		return RuntimeConfiguration{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RuntimeConfiguration{}, err
	}
	s.publish(&r)
	return r, nil
}

func (s *Service) AuthorizeConfiguration(ctx context.Context, tx pgx.Tx, p Principal) error {
	fresh, err := s.principal(ctx, tx, p.SessionID)
	if err != nil {
		return err
	}
	if !fresh.Can(ManageConfiguration) {
		return ErrForbidden
	}
	return nil
}
