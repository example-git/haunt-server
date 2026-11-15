package ai

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"math"
	"regexp"
	"time"
)

var presetMention = regexp.MustCompile(`<@ai:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})>`)

const turnColumns = `id,channel_id,trigger_message_id,actor_id,status,revision,partial_text,error_code,retry_at,created_at,updated_at`

func scanTurn(row interface{ Scan(...any) error }) (Turn, error) {
	var t Turn
	err := row.Scan(&t.ID, &t.ChannelID, &t.TriggerMessageID, &t.ActorID, &t.Status, &t.Revision, &t.PartialText, &t.ErrorCode, &t.RetryAt, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}
func (s *Service) Accept(ctx context.Context, tx pgx.Tx, p identity.Principal, m chat.Message) error {
	var kind string
	err := tx.QueryRow(ctx, `SELECT kind FROM chatxo.channels WHERE id=$1`, m.ConversationID).Scan(&kind)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if kind != "ai" {
		return nil
	}
	ch, err := s.ids.ChannelForTransaction(ctx, tx, p, m.ConversationID)
	if err != nil {
		return err
	}
	if ch.Kind != "ai" {
		return nil
	}
	if ch.Permissions&identity.SendMessages == 0 {
		return identity.ErrForbidden
	}
	if err = s.ensureSettings(ctx, tx, ch.ID); err != nil {
		return err
	}
	matches := presetMention.FindAllStringSubmatch(m.Body, -1)
	if len(matches) > 0 {
		if ch.Permissions&identity.SwitchAIPreset == 0 {
			return identity.ErrForbidden
		}
		selected := matches[len(matches)-1][1]
		for _, match := range matches {
			if match[1] != selected {
				return identity.ErrInvalid
			}
		}
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.ai_presets WHERE id=$1 AND channel_id=$2)`, selected, ch.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return identity.ErrInvalid
		}
		if _, err = tx.Exec(ctx, `UPDATE chatxo.ai_channel_settings SET active_preset_id=$2,revision=revision+1 WHERE channel_id=$1`, ch.ID, selected); err != nil {
			return err
		}
	}
	if ch.Permissions&identity.UseAI == 0 {
		return s.enforce(ctx, tx, ch.ID)
	}
	var snap snapshot
	var epoch int64
	snap.SessionID = p.SessionID
	err = tx.QueryRow(ctx, `SELECT s.provider,COALESCE(s.account_id::text,''),s.model,s.instructions,COALESCE(p.id::text,s.channel_id::text),COALESCE(p.name,s.name),COALESCE(p.avatar_url,s.avatar_url),COALESCE(p.instructions,''),s.revision,s.history_epoch FROM chatxo.ai_channel_settings s LEFT JOIN chatxo.ai_presets p ON p.id=s.active_preset_id WHERE s.channel_id=$1 FOR UPDATE OF s`, ch.ID).Scan(&snap.Provider, &snap.AccountID, &snap.Model, &snap.Instructions, &snap.Author.ID, &snap.Author.Name, &snap.Author.AvatarURL, &snap.PresetInstructions, &snap.Revision, &epoch)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(snap)
	status, code := "queued", ""
	if snap.AccountID == "" || snap.Model == "" {
		status = "requires_auth"
		code = "configuration_required"
	}
	t := identity.NewID()
	reserve, err := s.reservation(ctx, tx, ch.ID)
	if err != nil {
		return err
	}
	if status != "queued" {
		reserve = 0
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chatxo.ai_turns(id,channel_id,trigger_message_id,actor_id,snapshot,boundary_seq,history_epoch,status,error_code,response_request_id,reservation_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, t, ch.ID, m.ID, p.User.ID, raw, m.Seq, epoch, status, code, identity.NewID(), reserve); err != nil {
		return err
	}
	if status == "queued" {
		allowance, err := s.terminalAllowance(ctx, tx, t)
		if err != nil {
			return err
		}
		if reserve < allowance {
			return ErrStorage
		}
	}
	if err = s.emitTurn(ctx, tx, t); err != nil {
		return err
	}
	return s.enforceProtected(ctx, tx, ch.ID, t)
}
func (s *Service) Runs(ctx context.Context, p identity.Principal, id string) ([]Turn, error) {
	if _, err := s.channel(ctx, p, id, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+turnColumns+` FROM chatxo.ai_turns WHERE channel_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Turn{}
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Service) Cancel(ctx context.Context, p identity.Principal, ch, id string) (Turn, error) {
	return s.changeTurn(ctx, p, ch, id, false)
}
func (s *Service) Retry(ctx context.Context, p identity.Principal, ch, id string) (Turn, error) {
	return s.changeTurn(ctx, p, ch, id, true)
}
func (s *Service) changeTurn(ctx context.Context, p identity.Principal, ch, id string, retry bool) (Turn, error) {
	channel, err := s.channel(ctx, p, ch, false)
	if err != nil {
		return Turn{}, err
	}
	if !identity.ValidID(id) {
		return Turn{}, identity.ErrInvalid
	}
	var out Turn
	var cancelRequest string
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		var err error
		channel, err = s.ids.ChannelForTransaction(ctx, tx, p, ch)
		if err != nil {
			return err
		}
		t, e := scanTurn(tx.QueryRow(ctx, `SELECT `+turnColumns+` FROM chatxo.ai_turns WHERE id=$1 AND channel_id=$2 FOR UPDATE`, id, ch))
		if e != nil {
			return translate(e)
		}
		if err = tx.QueryRow(ctx, `SELECT response_request_id FROM chatxo.ai_turns WHERE id=$1`, id).Scan(&cancelRequest); err != nil {
			return err
		}
		if t.ActorID != p.User.ID && channel.Permissions&identity.ManageChannels == 0 {
			return identity.ErrForbidden
		}
		if retry {
			if channel.Permissions&(identity.UseAI|identity.SendMessages) != (identity.UseAI | identity.SendMessages) {
				return identity.ErrForbidden
			}
			if t.Status != "failed" && t.Status != "requires_auth" && t.Status != "cancelled" && t.Status != "interrupted" {
				return identity.ErrConflict
			}
			var raw []byte
			if e = tx.QueryRow(ctx, `SELECT snapshot FROM chatxo.ai_turns WHERE id=$1`, id).Scan(&raw); e != nil {
				return e
			}
			var snap snapshot
			if e = json.Unmarshal(raw, &snap); e != nil {
				return e
			}
			snap.SessionID = p.SessionID
			reserve, e := s.reservation(ctx, tx, ch)
			if e != nil {
				return e
			}
			raw, _ = json.Marshal(snap)
			_, e = tx.Exec(ctx, `UPDATE chatxo.ai_turns SET status='queued',actor_id=$6,snapshot=$2,error_code='',retry_at=NULL,partial_text='',attempt=attempt+1,response_request_id=$4,reservation_bytes=$5,history_epoch=(SELECT history_epoch FROM chatxo.ai_channel_settings WHERE channel_id=$3),revision=revision+1,updated_at=now() WHERE id=$1`, id, raw, ch, identity.NewID(), reserve, p.User.ID)
		} else {
			if t.Status == "completed" || t.Status == "cancelled" {
				return identity.ErrConflict
			}
			_, e = tx.Exec(ctx, `UPDATE chatxo.ai_turns SET status='cancelled',reservation_bytes=0,error_code='',revision=revision+1,updated_at=now() WHERE id=$1`, id)
		}
		if e != nil {
			return e
		}
		out, e = scanTurn(tx.QueryRow(ctx, `SELECT `+turnColumns+` FROM chatxo.ai_turns WHERE id=$1`, id))
		if e != nil {
			return e
		}
		if e = s.emitTurn(ctx, tx, id); e != nil {
			return e
		}
		return s.enforceProtected(ctx, tx, ch, id)
	})
	if err == nil && !retry {
		s.mu.Lock()
		cancel := s.running[cancelRequest]
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	return out, err
}
func (s *Service) emitTurn(ctx context.Context, tx pgx.Tx, id string) error {
	t, err := scanTurn(tx.QueryRow(ctx, `SELECT `+turnColumns+` FROM chatxo.ai_turns WHERE id=$1`, id))
	if err != nil {
		return err
	}
	return s.chat.Emit(ctx, tx, t.ChannelID, &t.TriggerMessageID, "ai.turn.updated", t)
}

type claimedTurn struct {
	Turn
	snapshot        snapshot
	boundary        int64
	epoch           int64
	attempt         int
	responseRequest string
}

func (s *Service) claim(ctx context.Context) (claimedTurn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	busy := []string{}
	for channel := range s.leases {
		busy = append(busy, channel)
	}
	var t claimedTurn
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT t.id,t.channel_id,t.trigger_message_id,t.actor_id,t.snapshot,t.boundary_seq,t.history_epoch,t.attempt,t.response_request_id FROM chatxo.ai_turns t WHERE NOT (t.channel_id=ANY($1::uuid[])) AND (t.status='queued' OR (t.status='waiting' AND t.retry_at<=now())) AND NOT EXISTS(SELECT 1 FROM chatxo.ai_turns active WHERE active.channel_id=t.channel_id AND active.status='running') AND NOT EXISTS(SELECT 1 FROM chatxo.ai_turns earlier WHERE earlier.channel_id=t.channel_id AND earlier.status IN ('queued','waiting') AND (earlier.created_at,earlier.id)<(t.created_at,t.id)) ORDER BY t.created_at,t.id LIMIT 1 FOR UPDATE OF t SKIP LOCKED`, busy).Scan(&t.ID, &t.ChannelID, &t.TriggerMessageID, &t.ActorID, &raw, &t.boundary, &t.epoch, &t.attempt, &t.responseRequest)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &t.snapshot); err != nil {
			return err
		}
		var before, remaining int64
		if err = tx.QueryRow(ctx, channelUsageSQL, t.ChannelID).Scan(&before); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `SELECT reservation_bytes FROM chatxo.ai_turns WHERE id=$1`, t.ID).Scan(&remaining); err != nil {
			return err
		}
		allowance, err := s.terminalAllowance(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		if remaining < allowance*2 {
			if _, err = tx.Exec(ctx, `UPDATE chatxo.ai_turns SET status='failed',reservation_bytes=0,error_code='ai_storage_full',revision=revision+1,updated_at=now() WHERE id=$1`, t.ID); err != nil {
				return err
			}
			if err = s.emitTurn(ctx, tx, t.ID); err != nil {
				return err
			}
			return s.enforceProtected(ctx, tx, t.ChannelID, t.ID)
		}
		_, err = tx.Exec(ctx, `UPDATE chatxo.ai_turns SET status='running',revision=revision+1,updated_at=now() WHERE id=$1`, t.ID)
		if err != nil {
			return err
		}
		if err = s.emitTurn(ctx, tx, t.ID); err != nil {
			return err
		}
		var after int64
		if err = tx.QueryRow(ctx, channelUsageSQL, t.ChannelID).Scan(&after); err != nil {
			return err
		}
		cost := max(0, after-before)
		if cost+allowance > remaining {
			return ErrStorage
		}
		if _, err = tx.Exec(ctx, `UPDATE chatxo.ai_turns SET reservation_bytes=reservation_bytes-$2 WHERE id=$1`, t.ID, cost); err != nil {
			return err
		}
		return s.enforceProtected(ctx, tx, t.ChannelID, t.ID)
	})
	if err == nil {
		s.leases[t.ChannelID] = t.responseRequest
	}
	return t, err
}
func (s *Service) worker(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var lastError time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		t, err := s.claim(ctx)
		if err == nil {
			s.execute(ctx, t)
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil && time.Since(lastError) >= time.Minute {
			lastError = time.Now()
			slog.Error("AI queued turn could not be claimed", "error_code", errorCode(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Service) recover(ctx context.Context) error {
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE chatxo.ai_turns SET status='interrupted',reservation_bytes=0,error_code='remote_outcome_unknown',revision=revision+1,updated_at=now() WHERE status='running' RETURNING id`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if err = s.emitTurn(ctx, tx, id); err != nil {
				return err
			}
		}
		return s.enforce(ctx, tx, "")
	})
	if err == nil {
		s.leases = map[string]string{}
	}
	return err
}
func (s *Service) checkExecution(ctx context.Context, t claimedTurn) (identity.Principal, error) {
	p, err := s.ids.Refresh(ctx, identity.Principal{SessionID: t.snapshot.SessionID})
	if err != nil {
		return p, err
	}
	if p.User.ID != t.ActorID {
		return p, identity.ErrForbidden
	}
	ch, err := s.ids.Channel(ctx, p, t.ChannelID)
	if err != nil {
		return p, err
	}
	if ch.Permissions&(identity.ViewChannel|identity.SendMessages|identity.UseAI) != (identity.ViewChannel | identity.SendMessages | identity.UseAI) {
		return p, identity.ErrForbidden
	}
	var valid bool
	err = s.pool.QueryRow(ctx, `SELECT t.status='running' AND t.history_epoch=s.history_epoch AND t.history_epoch=$2 AND EXISTS(SELECT 1 FROM chatxo.messages m WHERE m.id=t.trigger_message_id AND NOT m.deleted) FROM chatxo.ai_turns t JOIN chatxo.ai_channel_settings s ON s.channel_id=t.channel_id WHERE t.id=$1 AND t.attempt=$3 AND t.response_request_id=$4`, t.ID, t.epoch, t.attempt, t.responseRequest).Scan(&valid)
	if err != nil {
		return p, translate(err)
	}
	if !valid {
		return p, ErrHistoryChanged
	}
	return p, nil
}
func (s *Service) execute(parent context.Context, t claimedTurn) {
	timeout, cancelTimeout := context.WithTimeout(parent, 5*time.Minute)
	ctx, stop := context.WithCancelCause(timeout)
	cancel := func() { stop(context.Canceled) }
	s.mu.Lock()
	s.running[t.responseRequest] = cancel
	s.mu.Unlock()
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.checkExecution(ctx, t); err != nil {
					stop(err)
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		cancelTimeout()
		<-monitorDone
		s.mu.Lock()
		delete(s.running, t.responseRequest)
		if s.leases[t.ChannelID] == t.responseRequest {
			delete(s.leases, t.ChannelID)
		}
		s.mu.Unlock()
	}()
	p, err := s.checkExecution(ctx, t)
	if err != nil {
		s.finish(parent, t, "failed", errorCode(err), nil)
		return
	}
	a, c, err := s.credentials(ctx, t.snapshot.AccountID)
	if err != nil {
		s.fail(parent, t, err)
		return
	}
	if string(a.ID()) != t.snapshot.Provider {
		s.fail(parent, t, ErrConfiguration)
		return
	}
	req, err := s.context(ctx, p, t, a, c)
	if err != nil {
		s.fail(parent, t, err)
		return
	}
	text := ""
	last := time.Time{}
	for step := 0; step < 8; step++ {
		if _, err = s.checkExecution(ctx, t); err != nil {
			s.fail(parent, t, err)
			return
		}
		callStart := len(text)
		response, e := a.Generate(ctx, req, func(event provider.Event) error {
			if event.Type == "text" || event.Type == "text_delta" {
				text += event.Text
				if len(text) > 65536 {
					return ErrStorage
				}
				if time.Since(last) >= time.Second {
					last = time.Now()
					return s.checkpoint(ctx, t, text, nil)
				}
			}
			return nil
		})
		if e != nil {
			if cause := context.Cause(ctx); cause != nil {
				e = cause
			}
			if text != "" {
				s.checkpoint(parent, t, text, nil)
			}
			s.fail(parent, t, e)
			return
		}
		if response.Text != "" {
			emitted := text[callStart:]
			if emitted == "" {
				text += response.Text
			} else if emitted != response.Text {
				s.fail(parent, t, &provider.Error{Kind: provider.Protocol, Message: "stream and final text disagree"})
				return
			}
		}
		if len(text) > 65536 {
			s.fail(parent, t, ErrStorage)
			return
		}
		raw, _ := json.Marshal(turnStep{Replay: response.Replay, Calls: response.Calls, Usage: &response.Usage})
		if len(raw) > 1<<20 {
			s.fail(parent, t, ErrStorage)
			return
		}
		if err = s.checkpoint(ctx, t, text, raw); err != nil {
			s.fail(parent, t, err)
			return
		}
		if len(response.Calls) == 0 {
			s.finish(parent, t, "completed", "", nil)
			return
		}
		if len(response.Calls) > 8 {
			s.fail(parent, t, errors.New("tool_call_limit"))
			return
		}
		req.Replay = response.Replay
		req.ToolResults = nil
		for _, call := range response.Calls {
			if _, err = s.checkExecution(ctx, t); err != nil {
				s.fail(parent, t, err)
				return
			}
			result, e := s.tools.Execute(ctx, call)
			if e != nil && (!result.IsError || ctx.Err() != nil) {
				s.fail(parent, t, e)
				return
			}
			req.ToolResults = append(req.ToolResults, result)
		}
		results, _ := json.Marshal(turnStep{Results: req.ToolResults})
		if len(results) > 1<<20 {
			s.fail(parent, t, ErrStorage)
			return
		}
		if err = s.checkpoint(ctx, t, text, results); err != nil {
			s.fail(parent, t, err)
			return
		}
	}
	s.fail(parent, t, errors.New("tool_step_limit"))
}

type turnStep struct {
	Replay  []json.RawMessage     `json:"replay,omitempty"`
	Calls   []provider.ToolCall   `json:"calls,omitempty"`
	Results []provider.ToolResult `json:"results,omitempty"`
	Usage   *provider.Usage       `json:"usage,omitempty"`
}

func (s *Service) checkpoint(ctx context.Context, t claimedTurn, text string, step []byte) error {
	if _, err := s.checkExecution(ctx, t); err != nil {
		return err
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		var valid bool
		var remaining, before int64
		if err := tx.QueryRow(ctx, `SELECT t.status='running' AND s.history_epoch=$2 AND t.history_epoch=$2,t.reservation_bytes FROM chatxo.ai_turns t JOIN chatxo.ai_channel_settings s ON s.channel_id=t.channel_id WHERE t.id=$1 AND t.attempt=$3 AND t.response_request_id=$4 FOR UPDATE OF t,s`, t.ID, t.epoch, t.attempt, t.responseRequest).Scan(&valid, &remaining); err != nil {
			return translate(err)
		}
		if !valid {
			return ErrHistoryChanged
		}
		if err := tx.QueryRow(ctx, channelUsageSQL, t.ChannelID).Scan(&before); err != nil {
			return err
		}
		if text != "" {
			if _, err := s.chat.WriteAI(ctx, tx, t.ChannelID, t.responseRequest, text, t.snapshot.Author); err != nil {
				return err
			}
		}
		if len(step) > 0 {
			var record turnStep
			if err := json.Unmarshal(step, &record); err != nil {
				return err
			}
			if len(record.Replay) > 0 {
				raw, _ := json.Marshal(record.Replay)
				if len(raw) > 1<<20 {
					return ErrStorage
				}
				if _, err := tx.Exec(ctx, `INSERT INTO chatxo.ai_turn_steps(turn_id,attempt,kind,data) VALUES($1,$2,'replay',$3) ON CONFLICT(turn_id,attempt) WHERE kind='replay' DO UPDATE SET data=excluded.data`, t.ID, t.attempt, raw); err != nil {
					return err
				}
			}
			record.Replay = nil
			raw, _ := json.Marshal(record)
			if _, err := tx.Exec(ctx, `INSERT INTO chatxo.ai_turn_steps(turn_id,attempt,data) VALUES($1,$2,$3)`, t.ID, t.attempt, raw); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE chatxo.ai_turns SET partial_text=$2,revision=revision+1,updated_at=now() WHERE id=$1`, t.ID, text); err != nil {
			return err
		}
		if err := s.emitTurn(ctx, tx, t.ID); err != nil {
			return err
		}
		var after int64
		if err := tx.QueryRow(ctx, channelUsageSQL, t.ChannelID).Scan(&after); err != nil {
			return err
		}
		cost := max(0, after-before)
		allowance, err := s.terminalAllowance(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		if cost+allowance > remaining {
			return ErrStorage
		}
		if _, err := tx.Exec(ctx, `UPDATE chatxo.ai_turns SET reservation_bytes=reservation_bytes-$2 WHERE id=$1`, t.ID, cost); err != nil {
			return err
		}
		return s.enforceProtected(ctx, tx, t.ChannelID, t.ID)
	})
}
func (s *Service) terminalAllowance(ctx context.Context, tx pgx.Tx, id string) (int64, error) {
	turn, err := scanTurn(tx.QueryRow(ctx, `SELECT `+turnColumns+` FROM chatxo.ai_turns WHERE id=$1`, id))
	if err != nil {
		return 0, err
	}
	turn.Status = "requires_auth"
	turn.ErrorCode = "configuration_required"
	turn.Revision = math.MaxInt64
	turn.UpdatedAt = time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)
	turn.RetryAt = &turn.UpdatedAt
	payload, err := json.Marshal(chat.Event{ID: math.MaxInt64, Type: "ai.turn.updated", ConversationID: turn.ChannelID, Data: turn})
	if err != nil {
		return 0, err
	}
	var rowGrowth int64
	err = tx.QueryRow(ctx, `SELECT octet_length(((to_jsonb(t)-'bytes') || jsonb_build_object('status',$2::text,'error_code',$3::text,'revision',$4::bigint,'updated_at',$5::text,'retry_at',$5::text,'reservation_bytes',0))::text)-octet_length((to_jsonb(t)-'bytes')::text) FROM chatxo.ai_turns t WHERE id=$1`, id, turn.Status, turn.ErrorCode, turn.Revision, "9999-12-31T23:59:59.999999999+00:00").Scan(&rowGrowth)
	return int64(len(payload)+64) + max(0, rowGrowth), err
}

func (s *Service) finish(ctx context.Context, t claimedTurn, status, code string, retry *time.Time) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := s.transaction(bounded, func(tx pgx.Tx) error {
		var before, remaining int64
		if status == "waiting" {
			if err := tx.QueryRow(bounded, `SELECT reservation_bytes FROM chatxo.ai_turns WHERE id=$1 AND status='running' AND attempt=$2 AND response_request_id=$3 AND history_epoch=$4`, t.ID, t.attempt, t.responseRequest, t.epoch).Scan(&remaining); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return err
			}
			allowance, err := s.terminalAllowance(bounded, tx, t.ID)
			if err != nil {
				return err
			}
			if remaining < allowance*2 {
				status, code, retry = "failed", "ai_storage_full", nil
			} else if err = tx.QueryRow(bounded, channelUsageSQL, t.ChannelID).Scan(&before); err != nil {
				return err
			}
		}
		r, err := tx.Exec(bounded, `UPDATE chatxo.ai_turns SET status=$2,reservation_bytes=CASE WHEN $2='waiting' THEN reservation_bytes ELSE 0 END,error_code=$3,retry_at=$4,revision=revision+1,updated_at=now() WHERE id=$1 AND status='running' AND attempt=$5 AND response_request_id=$6 AND history_epoch=$7`, t.ID, status, code, retry, t.attempt, t.responseRequest, t.epoch)
		if err != nil || r.RowsAffected() == 0 {
			return err
		}
		if err = s.emitTurn(bounded, tx, t.ID); err != nil {
			return err
		}
		if status == "waiting" {
			var after int64
			if err = tx.QueryRow(bounded, channelUsageSQL, t.ChannelID).Scan(&after); err != nil {
				return err
			}
			if _, err = tx.Exec(bounded, `UPDATE chatxo.ai_turns SET reservation_bytes=reservation_bytes-$2 WHERE id=$1`, t.ID, max(0, after-before)); err != nil {
				return err
			}
		}
		return s.enforceProtected(bounded, tx, t.ChannelID, t.ID)
	})
	if err != nil {
		slog.Error("AI turn final state could not be saved", "turn_id", t.ID, "error_code", errorCode(err))
	}
}
func errorCode(err error) string {
	if errors.Is(err, ErrStorage) || errors.Is(err, chat.ErrAIStorageFull) {
		return "ai_storage_full"
	}
	if errors.Is(err, ErrConfiguration) {
		return "configuration_required"
	}
	if errors.Is(err, ErrHistoryChanged) {
		return "history_changed"
	}
	if errors.Is(err, identity.ErrForbidden) || errors.Is(err, identity.ErrUnauthorized) {
		return "permission_revoked"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "remote_outcome_unknown"
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		return string(pe.Kind)
	}
	return "generation_failed"
}
func (s *Service) fail(ctx context.Context, t claimedTurn, err error) {
	status := "failed"
	code := errorCode(err)
	var retry *time.Time
	var pe *provider.Error
	if errors.As(err, &pe) {
		if pe.Kind == provider.RequiresAuth {
			status = "requires_auth"
		}
		if pe.Kind == provider.Ambiguous {
			status = "interrupted"
		}
		if pe.Kind == provider.RateLimit {
			var emitted bool
			if e := s.pool.QueryRow(ctx, `SELECT partial_text<>'' OR EXISTS(SELECT 1 FROM chatxo.ai_turn_steps WHERE turn_id=$1 AND attempt=$2) FROM chatxo.ai_turns WHERE id=$1 AND attempt=$2 AND response_request_id=$3`, t.ID, t.attempt, t.responseRequest).Scan(&emitted); e != nil || emitted {
				s.finish(ctx, t, "interrupted", "remote_outcome_unknown", nil)
				return
			}
			status = "waiting"
			delay := pe.RetryAfter
			if delay <= 0 {
				delay = time.Minute
			}
			v := time.Now().Add(delay)
			retry = &v
		}
	}
	if code == "remote_outcome_unknown" {
		status = "interrupted"
	}
	s.finish(ctx, t, status, code, retry)
}
