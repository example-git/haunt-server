package attachments

import (
	"context"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
)

// Serialize admissions with pressure cleanup, without holding the database's
// shared chat lock across filesystem or remote storage operations.
func (s *Store) lockAdmission(ctx context.Context) error {
	select {
	case s.admission <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) unlockAdmission() { <-s.admission }

type shortage struct {
	total  int64
	sender int64
}

func (q shortage) needed() bool { return q.total > 0 || q.sender > 0 }

func (a Attachment) fullError() error {
	if a.IsDM {
		return ErrDMFull
	}
	return ErrFull
}

func (a Attachment) spaceNeeded(ctx context.Context, tx pgx.Tx, policy config.Policy) (shortage, error) {
	limit := policy.AttachmentMaxBytes
	var used, own, senderShort int64
	var err error
	if a.IsDM {
		limit = policy.DM.StorageBytes
		if !policy.DM.ImagesEnabled {
			return shortage{}, ErrDMDisabled
		}
		if a.Bytes > policy.DM.UserStorageBytes || a.Bytes > policy.DM.ImageMaxBytes {
			return shortage{}, ErrDMFull
		}
		err = tx.QueryRow(ctx, `SELECT dm_attachment_bytes,(SELECT COALESCE(sum(bytes),0)
			FROM chatxo.attachments WHERE is_dm AND NOT quota_pending AND user_id=$1)
			FROM chatxo.storage WHERE singleton`, a.UserID).Scan(&used, &own)
		senderShort = a.Bytes - (policy.DM.UserStorageBytes - own)
	} else {
		err = tx.QueryRow(ctx, `SELECT attachment_bytes-dm_attachment_bytes FROM chatxo.storage WHERE singleton`).Scan(&used)
	}
	if err != nil {
		return shortage{}, err
	}
	if a.Bytes > limit {
		return shortage{}, a.fullError()
	}
	return shortage{total: a.Bytes - (limit - used), sender: senderShort}, nil
}

// Recently uploaded, unclaimed files can still be part of someone’s draft.
// In-flight uploads are never victims, including incoming staged replacements.
const reclaimable = `is_dm=$1 AND NOT quota_pending AND
	(status='deleting' OR (status='ready' AND
	(claimed OR expires_at<=now() OR created_at<=now()-interval '10 minutes')))`

func (a Attachment) checkReclaimable(ctx context.Context, tx pgx.Tx, q shortage) error {
	var all, own int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(sum(bytes),0),
		COALESCE(sum(bytes) FILTER (WHERE user_id=NULLIF($2,'')::uuid),0)
		FROM chatxo.attachments WHERE `+reclaimable, a.IsDM, a.UserID).Scan(&all, &own)
	if err == nil && (q.total > all || q.sender > own) {
		return a.fullError()
	}
	return err
}

func (s *Store) finishUpload(ctx context.Context, p identity.Principal, a *Attachment) error {
	if a.quotaPending {
		if err := s.lockAdmission(ctx); err != nil {
			return err
		}
		defer s.unlockAdmission()
		fresh, err := s.chat.Identity.Refresh(ctx, p)
		if err != nil {
			return err
		}
		if err = s.chat.Access(ctx, fresh, a.ConversationID, true); err != nil {
			return err
		}
	}
	for {
		victims, err := s.finishOrReclaim(ctx, a)
		if err != nil || len(victims) == 0 {
			return err
		}
		for _, id := range victims {
			if err := s.remove(ctx, id); err != nil {
				return err
			}
		}
	}
}

func (s *Store) finishOrReclaim(ctx context.Context, a *Attachment) ([]string, error) {
	tx, err := s.chat.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err = s.chat.LockStorage(ctx, tx); err != nil {
		return nil, err
	}
	if a.quotaPending {
		// Re-check that the staged upload still exists before retiring any files.
		var uploading bool
		if err = tx.QueryRow(ctx, `SELECT status='uploading' AND quota_pending FROM chatxo.attachments WHERE id=$1 FOR UPDATE`, a.ID).Scan(&uploading); err != nil {
			return nil, err
		}
		if !uploading {
			return nil, identity.ErrConflict
		}
		policy, err := s.chat.PolicyForTransaction(ctx, tx)
		if err != nil {
			return nil, err
		}
		space, err := a.spaceNeeded(ctx, tx, policy)
		if err != nil {
			return nil, err
		}
		if space.needed() {
			if err = a.checkReclaimable(ctx, tx, space); err != nil {
				return nil, err
			}
			ids, err := a.markOldest(ctx, tx, space)
			if err != nil {
				return nil, err
			}
			return ids, tx.Commit(ctx)
		}
		if _, err = tx.Exec(ctx, `UPDATE chatxo.storage SET attachment_bytes=attachment_bytes+$1 WHERE singleton`, a.Bytes); err != nil {
			return nil, err
		}
	}
	retention := a.policy.AttachmentRetentionDays
	if a.IsDM {
		retention = 1 // Claiming sets the configured DM retention.
	}
	err = tx.QueryRow(ctx, `UPDATE chatxo.attachments SET status='ready',quota_pending=false,
		content_type=$2,created_at=clock_timestamp(),expires_at=clock_timestamp()+$3::bigint*interval '1 day'
		WHERE id=$1 AND status='uploading' RETURNING expires_at`, a.ID, a.ContentType, retention).Scan(&a.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return nil, tx.Commit(ctx)
}

func (a Attachment) markOldest(ctx context.Context, tx pgx.Tx, q shortage) ([]string, error) {
	need := q.total
	if q.sender > 0 {
		need = q.sender
	}
	// Finish existing deletions first. For a sender shortage, reclaim only
	// that sender’s files, then reevaluate the independent server budget.
	rows, err := tx.Query(ctx, `WITH candidates AS (
		SELECT id, bytes, status='deleting' AS deleting, created_at FROM chatxo.attachments
		WHERE `+reclaimable+` AND (NOT $3::boolean OR user_id=NULLIF($2,'')::uuid)
		ORDER BY deleting DESC,created_at,id LIMIT 64
	), ranked AS (
		SELECT id,sum(bytes) OVER (ORDER BY deleting DESC,created_at,id)-bytes AS bytes_before FROM candidates
	)
	UPDATE chatxo.attachments SET status='deleting'
	WHERE id IN (SELECT id FROM ranked WHERE bytes_before<$4::bigint) RETURNING id`, a.IsDM, a.UserID, q.sender > 0, need)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err == nil && len(ids) == 0 {
		return nil, a.fullError()
	}
	return ids, err
}
