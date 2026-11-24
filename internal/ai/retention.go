package ai

import (
	"context"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
	"sort"
)

const channelUsageSQL = `SELECT COALESCE((SELECT sum(bytes) FROM chatxo.messages WHERE conversation_id=$1),0)+COALESCE((SELECT sum(e.bytes) FROM chatxo.events e WHERE e.conversation_id=$1),0)+COALESCE((SELECT sum(bytes) FROM chatxo.ai_turns WHERE channel_id=$1),0)+COALESCE((SELECT sum(st.bytes) FROM chatxo.ai_turn_steps st JOIN chatxo.ai_turns t ON t.id=st.turn_id WHERE t.channel_id=$1),0)+COALESCE((SELECT bytes FROM chatxo.ai_channel_settings WHERE channel_id=$1),0)+COALESCE((SELECT sum(bytes) FROM chatxo.ai_presets WHERE channel_id=$1),0)+COALESCE((SELECT sum(bytes) FROM chatxo.ai_avatars WHERE channel_id=$1),0)`
const globalUsageSQL = `SELECT COALESCE((SELECT sum(m.bytes) FROM chatxo.messages m JOIN chatxo.ai_channel_settings s ON s.channel_id=m.conversation_id),0)+COALESCE((SELECT sum(e.bytes) FROM chatxo.events e JOIN chatxo.ai_channel_settings s ON s.channel_id=e.conversation_id),0)+COALESCE((SELECT sum(bytes) FROM chatxo.ai_turns),0)+COALESCE((SELECT sum(bytes) FROM chatxo.ai_turn_steps),0)+COALESCE((SELECT sum(bytes) FROM chatxo.ai_channel_settings),0)+COALESCE((SELECT sum(bytes) FROM chatxo.ai_presets),0)+COALESCE((SELECT sum(bytes) FROM chatxo.ai_avatars),0)`

func (s *Service) Usage(ctx context.Context, p identity.Principal, id string) (Usage, error) {
	if _, err := s.channel(ctx, p, id, false); err != nil {
		return Usage{}, err
	}
	if _, err := s.Settings(ctx, p, id); err != nil {
		return Usage{}, err
	}
	var u Usage
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, channelUsageSQL, id).Scan(&u.ChannelBytes); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, globalUsageSQL).Scan(&u.GlobalBytes); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT s.max_bytes,s.history_epoch,COALESCE(c.pruned_through,0) FROM chatxo.ai_channel_settings s LEFT JOIN chatxo.conversations c ON c.id=s.channel_id WHERE s.channel_id=$1`, id).Scan(&u.ChannelMaxBytes, &u.HistoryEpoch, &u.PrunedThrough); err != nil {
			return err
		}
		u.PrunedThroughSeq = u.PrunedThrough
		policy, err := s.chat.PolicyForTransaction(ctx, tx)
		if err != nil {
			return err
		}
		u.ChannelMaxBytes = min(u.ChannelMaxBytes, policy.AIChannelMaxBytes, policy.TextMaxBytes)
		u.GlobalMaxBytes = min(policy.AITextMaxBytes, policy.TextMaxBytes)
		return nil
	})
	return u, err
}
func (s *Service) Prune(ctx context.Context, tx pgx.Tx, id string, through int64) error {
	var visible bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.messages WHERE conversation_id=$1 AND seq<=$2 AND ai_visible)`, id, through).Scan(&visible); err != nil {
		return err
	}
	if !visible {
		return nil
	}
	if err := s.refreshBatchInputs(ctx, tx, id, through); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM chatxo.ai_turns WHERE channel_id=$1 AND boundary_seq<=$2`, id, through); err != nil {
		return err
	}
	if err := s.invalidateHistory(ctx, tx, id); err != nil {
		return err
	}
	return s.cleanupAvatars(ctx, tx, id, through)
}
func (s *Service) Invalidate(ctx context.Context, tx pgx.Tx, id string) error {
	if err := s.refreshBatchInputs(ctx, tx, id, 0); err != nil {
		return err
	}
	return s.invalidateHistory(ctx, tx, id)
}

func (s *Service) invalidateHistory(ctx context.Context, tx pgx.Tx, id string) error {
	var epoch int64
	err := tx.QueryRow(ctx, `UPDATE chatxo.ai_channel_settings SET history_epoch=history_epoch+1 WHERE channel_id=$1 RETURNING history_epoch`, id).Scan(&epoch)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.ai_turn_steps WHERE turn_id IN (SELECT id FROM chatxo.ai_turns WHERE channel_id=$1)`, id); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `UPDATE chatxo.ai_turns t SET history_epoch=$2,reservation_bytes=CASE WHEN status='running' OR EXISTS(SELECT 1 FROM chatxo.messages m WHERE m.id=t.trigger_message_id AND m.deleted) THEN 0 ELSE reservation_bytes END,status=CASE WHEN EXISTS(SELECT 1 FROM chatxo.messages m WHERE m.id=t.trigger_message_id AND m.deleted) THEN 'cancelled' WHEN status='running' THEN 'interrupted' ELSE status END,error_code=CASE WHEN EXISTS(SELECT 1 FROM chatxo.messages m WHERE m.id=t.trigger_message_id AND m.deleted) THEN 'trigger_deleted' WHEN status='running' THEN 'history_changed' ELSE error_code END,revision=revision+1,updated_at=now() WHERE channel_id=$1 AND status IN ('queued','waiting','running') RETURNING id,status`, id, epoch)
	if err != nil {
		return err
	}
	var changed []string
	for rows.Next() {
		var turn, status string
		if err = rows.Scan(&turn, &status); err != nil {
			rows.Close()
			return err
		}
		if status == "interrupted" || status == "cancelled" {
			changed = append(changed, turn)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, turn := range changed {
		if err = s.emitTurn(ctx, tx, turn); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) enforce(ctx context.Context, tx pgx.Tx, id string) error {
	return s.enforceProtected(ctx, tx, id, "")
}
func (s *Service) enforceProtected(ctx context.Context, tx pgx.Tx, id, protected string) error {
	if err := s.cleanupAvatars(ctx, tx, id, 0); err != nil {
		return err
	}
	policy, err := s.chat.PolicyForTransaction(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT channel_id,least(max_bytes,$1::bigint,$2::bigint) FROM chatxo.ai_channel_settings WHERE ($3='' OR channel_id=NULLIF($3,'')::uuid) ORDER BY channel_id`, policy.AIChannelMaxBytes, policy.TextMaxBytes, id)
	if err != nil {
		return err
	}
	type budget struct {
		id    string
		limit int64
	}
	var budgets []budget
	for rows.Next() {
		var b budget
		if err = rows.Scan(&b.id, &b.limit); err != nil {
			rows.Close()
			return err
		}
		budgets = append(budgets, b)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, b := range budgets {
		var used int64
		if err = tx.QueryRow(ctx, channelUsageSQL, b.id).Scan(&used); err != nil {
			return err
		}
		if used > b.limit {
			if err = s.pruneBudget(ctx, tx, b.id, b.limit*4/5, protected); err != nil {
				return err
			}
		}
	}
	var used int64
	if err = tx.QueryRow(ctx, globalUsageSQL).Scan(&used); err != nil {
		return err
	}
	limit := min(policy.AITextMaxBytes, policy.TextMaxBytes)
	if used > limit {
		if err = s.pruneBudget(ctx, tx, "", limit*4/5, protected); err != nil {
			return err
		}
	}
	if err = s.chat.PruneStorage(ctx, tx); err != nil {
		return err
	}
	if protected != "" {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.ai_turns WHERE id=$1)`, protected).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrStorage
		}
	}
	return nil
}
func (s *Service) pruneBudget(ctx context.Context, tx pgx.Tx, id string, target int64, protected string) error {
	for {
		var used int64
		var err error
		if id == "" {
			err = tx.QueryRow(ctx, globalUsageSQL).Scan(&used)
		} else {
			err = tx.QueryRow(ctx, channelUsageSQL, id).Scan(&used)
		}
		if err != nil {
			return err
		}
		if used <= target {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT m.conversation_id,m.seq,m.bytes+COALESCE((SELECT sum(e.bytes) FROM chatxo.events e WHERE e.message_id=m.id),0)+COALESCE((SELECT sum(t.bytes) FROM chatxo.ai_turns t WHERE t.trigger_message_id=m.id),0) FROM chatxo.messages m JOIN chatxo.ai_channel_settings s ON s.channel_id=m.conversation_id WHERE ($1='' OR m.conversation_id=NULLIF($1,'')::uuid) AND NOT EXISTS(SELECT 1 FROM chatxo.ai_turns active WHERE active.channel_id=m.conversation_id AND (active.status='running' OR active.id=NULLIF($2,'')::uuid) AND active.boundary_seq<=m.seq) ORDER BY m.created_at,m.id LIMIT 512`, id, protected)
		if err != nil {
			return err
		}
		boundaries := map[string]int64{}
		var reclaimed int64
		for rows.Next() {
			var conv string
			var seq, size int64
			if err = rows.Scan(&conv, &seq, &size); err != nil {
				rows.Close()
				return err
			}
			boundaries[conv] = max(boundaries[conv], seq)
			reclaimed += size
			if used-reclaimed <= target {
				break
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		if len(boundaries) == 0 {
			return ErrStorage
		}
		var channels []string
		for ch := range boundaries {
			channels = append(channels, ch)
		}
		sort.Strings(channels)
		for _, ch := range channels {
			if err = s.chat.PruneAIHistory(ctx, tx, ch, boundaries[ch]); err != nil {
				return err
			}
		}
	}
}
func (s *Service) Maintain(ctx context.Context) error {
	s.mu.Lock()
	pending := make([]pendingFinish, 0, len(s.unfinished))
	for _, v := range s.unfinished {
		pending = append(pending, v)
	}
	s.mu.Unlock()
	for _, v := range pending {
		s.finish(ctx, v.turn, v.status, v.code, v.retry)
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM chatxo.ai_oauth_sessions WHERE expires_at<=now() OR consumed_at<now()-interval '1 hour'`); err != nil {
			return err
		}
		return s.enforce(ctx, tx, "")
	})
}

func (s *Service) reservation(ctx context.Context, tx pgx.Tx, id string) (int64, error) {
	policy, err := s.chat.PolicyForTransaction(ctx, tx)
	if err != nil {
		return 0, err
	}
	var limit int64
	if err = tx.QueryRow(ctx, `SELECT max_bytes FROM chatxo.ai_channel_settings WHERE channel_id=$1`, id).Scan(&limit); err != nil {
		return 0, err
	}
	return min(1<<20, limit/4, policy.AIChannelMaxBytes/4, policy.AITextMaxBytes/4, policy.TextMaxBytes/4), nil
}
