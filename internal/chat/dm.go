package chat

import (
	"context"
	"errors"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/jackc/pgx/v5"
)

var ErrDMTextFull = errors.New("dm_message_storage_full")

func (s *Store) dmTextBudget(ctx context.Context, tx pgx.Tx, conv string, policy config.Policy) error {
	var used int64
	if err := tx.QueryRow(ctx, `SELECT CASE WHEN (SELECT kind FROM chatxo.conversations WHERE id=$1)='dm' THEN dm_text_bytes+dm_crypto_bytes ELSE 0 END FROM chatxo.storage WHERE singleton`, conv).Scan(&used); err != nil {
		return err
	}
	if used > policy.DM.TextMaxBytes {
		return ErrDMTextFull
	}
	return nil
}
func (s *Store) pruneDM(ctx context.Context, tx pgx.Tx, policy config.Policy) error {
	if policy.DM.TextRetentionDays == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT m.id,m.conversation_id,m.seq,m.bytes+COALESCE(e.bytes,0),COALESCE(e.floor,0) FROM chatxo.messages m JOIN chatxo.conversations c ON c.id=m.conversation_id LEFT JOIN LATERAL(SELECT sum(bytes) bytes,max(id) floor FROM chatxo.events WHERE message_id=m.id)e ON true WHERE c.kind='dm' AND m.created_at<=now()-$1::bigint*interval '1 day' ORDER BY m.created_at,m.id LIMIT 512`, policy.DM.TextRetentionDays)
	if err != nil {
		return err
	}
	ids := []string{}
	bounds := map[string]int64{}
	var bytes, floor int64
	for rows.Next() {
		var id, conv string
		var seq, n, f int64
		if err = rows.Scan(&id, &conv, &seq, &n, &f); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		bounds[conv] = max(bounds[conv], seq)
		bytes += n
		floor = max(floor, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.messages WHERE id=ANY($1::uuid[])`, ids); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.storage SET text_bytes=text_bytes-$1,event_floor=greatest(event_floor,$2) WHERE singleton`, bytes, floor); err != nil {
		return err
	}
	for conv, seq := range bounds {
		if _, err = tx.Exec(ctx, `UPDATE chatxo.conversations SET pruned_through=greatest(pruned_through,$2) WHERE id=$1`, conv, seq); err != nil {
			return err
		}
		if err = s.emit(ctx, tx, conv, nil, "history.pruned", map[string]any{"pruned_through": seq}); err != nil {
			return err
		}
	}
	return nil
}

// Channel history pressure must not reduce the encrypted DM message limit.
func (s *Store) messagePolicy(ctx context.Context, tx pgx.Tx, conv string, p config.Policy) (config.Policy, error) {
	var dm bool
	if err := tx.QueryRow(ctx, `SELECT kind='dm' FROM chatxo.conversations WHERE id=$1`, conv).Scan(&dm); err != nil {
		return p, err
	}
	if dm {
		p.TextMaxBytes = p.DM.TextMaxBytes
	}
	return p, nil
}
