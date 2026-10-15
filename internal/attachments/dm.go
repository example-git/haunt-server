package attachments

import (
	"context"
	"errors"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
)

var ErrDMFull = errors.New("dm_storage_full")
var ErrDMTransfer = errors.New("dm_transfer_limit")
var ErrDMDisabled = errors.New("dm_images_disabled")

type DMUsage struct {
	UsedBytes       int64           `json:"used_bytes"`
	LimitBytes      int64           `json:"limit_bytes"`
	Uploads         int64           `json:"uploads"`
	UploadedToday   int64           `json:"uploaded_today"`
	DownloadedToday int64           `json:"downloaded_today"`
	Policy          config.DMPolicy `json:"policy"`
}

type DMPurge struct {
	Before time.Time `json:"before"`
	Files  int64     `json:"files"`
	Bytes  int64     `json:"bytes"`
}

// Each reservation locks only this user's daily budget row. Different users
// never share a transfer-budget lock or allowance.
func chargeDMTransfer(ctx context.Context, tx pgx.Tx, user string, size int64, upload bool, p config.DMPolicy) error {
	column, limit := "downloaded", p.DownloadBytesPerDay
	if upload {
		column, limit = "uploaded", p.UploadBytesPerDay
	}
	result, err := tx.Exec(ctx, `INSERT INTO chatxo.dm_transfers(day,account,`+column+`) SELECT (now() AT TIME ZONE 'UTC')::date,$1,$2::bigint WHERE $2::bigint<=$3::bigint ON CONFLICT(day,account) DO UPDATE SET `+column+`=chatxo.dm_transfers.`+column+`+EXCLUDED.`+column+` WHERE chatxo.dm_transfers.`+column+`<=$3::bigint-EXCLUDED.`+column, user, size, limit)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrDMTransfer
	}
	return nil
}
func (s *Store) ReserveDMDownload(ctx context.Context, p identity.Principal, a Attachment) error {
	if !a.IsDM {
		return nil
	}
	tx, err := s.chat.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	policy, err := s.chat.PolicyForTransaction(ctx, tx)
	if err != nil {
		return err
	}
	if err = chargeDMTransfer(ctx, tx, p.User.ID, a.Bytes, false, policy.DM); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) DMUsage(ctx context.Context, p identity.Principal) (DMUsage, error) {
	policy := s.chat.CurrentPolicy().DM
	out := DMUsage{Policy: policy, LimitBytes: policy.UserStorageBytes}
	err := s.chat.Pool.QueryRow(ctx, `SELECT COALESCE(sum(bytes),0),count(*) FROM chatxo.attachments WHERE is_dm AND user_id=$1`, p.User.ID).Scan(&out.UsedBytes, &out.Uploads)
	if err != nil {
		return out, err
	}
	err = s.chat.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT uploaded FROM chatxo.dm_transfers WHERE account=$1 AND day=(now() AT TIME ZONE 'UTC')::date),0),COALESCE((SELECT downloaded FROM chatxo.dm_transfers WHERE account=$1 AND day=(now() AT TIME ZONE 'UTC')::date),0)`, p.User.ID).Scan(&out.UploadedToday, &out.DownloadedToday)
	return out, err
}
func (s *Store) PurgeDM(ctx context.Context, p identity.Principal, before time.Time, confirm bool) (DMPurge, error) {
	out := DMPurge{Before: before}
	if before.IsZero() || before.After(time.Now()) {
		return out, identity.ErrInvalid
	}
	if !confirm {
		err := s.chat.Pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(bytes),0) FROM chatxo.attachments WHERE is_dm AND user_id=$1 AND created_at<$2 AND status<>'uploading'`, p.User.ID, before).Scan(&out.Files, &out.Bytes)
		return out, err
	}
	// Mark only this sender's uploads, regardless of which DM they were sent to.
	// Bounded batches make repeated purge requests safe and inexpensive.
	tx, err := s.chat.Pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if err = s.chat.LockStorage(ctx, tx); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `UPDATE chatxo.attachments SET status='deleting' WHERE id IN(SELECT id FROM chatxo.attachments WHERE is_dm AND user_id=$1 AND created_at<$2 AND status<>'uploading' ORDER BY created_at,id LIMIT 128) RETURNING id,bytes`, p.User.ID, before)
	if err != nil {
		return out, err
	}
	type file struct {
		id    string
		bytes int64
	}
	files := []file{}
	for rows.Next() {
		var f file
		if err = rows.Scan(&f.id, &f.bytes); err != nil {
			rows.Close()
			return out, err
		}
		files = append(files, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	for _, f := range files {
		if err = s.remove(ctx, f.id); err != nil {
			return out, err
		}
		out.Files++
		out.Bytes += f.bytes
	}
	return out, nil
}
