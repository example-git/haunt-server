package ai

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/example-git/haunt-server/internal/chat"
	"github.com/jackc/pgx/v5"
)

const (
	historyMessageLimit = 256
	contextByteLimit    = 256 << 10
	maxContextImages    = 10
)

// Accept and claim hold the same chat storage lock. Once the worker changes a
// turn to running, later arrivals can only extend the next queued turn.
func (s *Service) appendPending(ctx context.Context, tx pgx.Tx, message chat.Message, incoming snapshot, epoch int64) (string, error) {
	var id string
	var raw []byte
	var first turnInput
	var firstBytes int
	err := tx.QueryRow(ctx, `SELECT tail.id,tail.snapshot,tail.trigger_message_id,tail.actor_id,tail.boundary_seq,octet_length(m.body)
		FROM (SELECT * FROM chatxo.ai_turns WHERE channel_id=$1 ORDER BY boundary_seq DESC LIMIT 1) tail
		JOIN chatxo.messages m ON m.id=tail.trigger_message_id
		WHERE tail.status='queued' AND tail.attempt=0 AND tail.history_epoch=$2 AND NOT m.deleted`,
		message.ConversationID, epoch).Scan(&id, &raw, &first.MessageID, &first.ActorID, &first.Seq, &firstBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var pending snapshot
	if err = json.Unmarshal(raw, &pending); err != nil {
		return "", err
	}
	if pending.Author != incoming.Author || pending.Provider != incoming.Provider ||
		pending.AccountID != incoming.AccountID || pending.Model != incoming.Model ||
		pending.Instructions != incoming.Instructions || pending.PresetInstructions != incoming.PresetInstructions {
		return "", nil
	}
	if len(pending.Inputs) == 0 {
		first.SessionID = pending.SessionID
		pending.Inputs = []turnInput{first}
		pending.InputBytes = firstBytes
	}
	if len(pending.Inputs) >= historyMessageLimit || pending.InputBytes+len(message.Body) > contextByteLimit {
		return "", nil
	}
	pending.Inputs = append(pending.Inputs, turnInput{MessageID: message.ID, ActorID: message.AuthorID, SessionID: incoming.SessionID, Seq: message.Seq})
	pending.InputBytes += len(message.Body)
	pending.SelectionSeq = incoming.SelectionSeq
	pending.Revision = incoming.Revision
	raw, err = json.Marshal(pending)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `UPDATE chatxo.ai_turns SET snapshot=$2,revision=revision+1,updated_at=now() WHERE id=$1`, id, raw)
	return id, err
}

func (t claimedTurn) inputIDs() []string {
	if len(t.snapshot.Inputs) == 0 {
		return []string{t.TriggerMessageID}
	}
	ids := make([]string, 0, len(t.snapshot.Inputs))
	for _, input := range t.snapshot.Inputs {
		ids = append(ids, input.MessageID)
	}
	return ids
}

func (t claimedTurn) otherRequesters() []turnInput {
	if len(t.snapshot.Inputs) == 0 {
		return nil
	}
	seen := map[string]bool{t.snapshot.SessionID: true}
	var out []turnInput
	for _, input := range t.snapshot.Inputs {
		if !seen[input.SessionID] {
			out = append(out, input)
			seen[input.SessionID] = true
		}
	}
	return out
}

// Retiring one message in a pending batch must not remove another member's
// request. Repoint the original foreign key before retention deletes its row.
func (s *Service) refreshBatchInputs(ctx context.Context, tx pgx.Tx, channel string, through int64) error {
	rows, err := tx.Query(ctx, `WITH surviving AS (
		SELECT t.id, jsonb_agg(input ORDER BY m.seq) AS inputs, sum(octet_length(m.body)) AS body_bytes
		FROM chatxo.ai_turns t CROSS JOIN LATERAL jsonb_array_elements(t.snapshot->'inputs') input
		JOIN chatxo.messages m ON m.id=(input->>'message_id')::uuid
		WHERE t.channel_id=$1 AND t.snapshot ? 'inputs' AND t.status<>'completed'
			AND NOT m.deleted AND m.seq>$2
		GROUP BY t.id
	)
	UPDATE chatxo.ai_turns t SET
		trigger_message_id=(s.inputs->0->>'message_id')::uuid,
		boundary_seq=(s.inputs->0->>'seq')::bigint,
		actor_id=CASE WHEN t.attempt=0 THEN (s.inputs->0->>'actor_id')::uuid ELSE t.actor_id END,
		snapshot=t.snapshot || jsonb_build_object('inputs',s.inputs,'input_bytes',s.body_bytes,
			'session_id',CASE WHEN t.attempt=0 THEN s.inputs->0->>'session_id' ELSE t.snapshot->>'session_id' END),
		revision=t.revision+1,updated_at=now()
	FROM surviving s WHERE t.id=s.id AND
		(t.snapshot->'inputs' <> s.inputs OR (t.snapshot->>'input_bytes')::bigint <> s.body_bytes)
	RETURNING t.id`, channel, through)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.emitTurn(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}
