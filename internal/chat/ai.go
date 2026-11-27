package chat

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
)

type AIAuthor struct {
	ID        string `json:"id" required:"true"`
	Name      string `json:"name" required:"true"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

type AIAcceptance interface {
	Accept(context.Context, pgx.Tx, identity.Principal, Message) error
	Prune(context.Context, pgx.Tx, string, int64) error
	Invalidate(context.Context, pgx.Tx, string) error
}

var ErrAIStorageFull = errors.New("ai_storage_full")

type GeneratedAttachment struct{ ID, Generation string }

func (s *Store) WriteAI(ctx context.Context, tx pgx.Tx, conv, request, responseRequest, body string, author AIAuthor, images ...string) (Message, error) {
	var claims []GeneratedAttachment
	for _, id := range images {
		claims = append(claims, GeneratedAttachment{id, request})
	}
	return s.writeAI(ctx, tx, conv, request, responseRequest, body, author, false, nil, claims...)
}
func (s *Store) WriteAIReply(ctx context.Context, tx pgx.Tx, conv, request, responseRequest, body string, author AIAuthor, card *Consultation, images ...GeneratedAttachment) (Message, error) {
	return s.writeAI(ctx, tx, conv, request, responseRequest, body, author, true, card, images...)
}
func (s *Store) writeAI(ctx context.Context, tx pgx.Tx, conv, request, responseRequest, body string, author AIAuthor, streaming bool, card *Consultation, images ...GeneratedAttachment) (Message, error) {
	if !identity.ValidID(conv) || !identity.ValidID(request) || !identity.ValidID(responseRequest) || !identity.ValidID(author.ID) || strings.TrimSpace(author.Name) == "" || !utf8.ValidString(author.Name) || utf8.RuneCountInString(author.Name) > 80 || strings.ContainsAny(author.Name, "\x00\r\n") || !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return Message{}, identity.ErrInvalid
	}
	var kind string
	if err := tx.QueryRow(ctx, `SELECT kind FROM chatxo.channels WHERE id=$1 AND instance_id=$2`, conv, s.InstanceID).Scan(&kind); err != nil {
		return Message{}, err
	}
	if kind != "ai" {
		return Message{}, identity.ErrInvalid
	}
	policy, err := s.PolicyForTransaction(ctx, tx)
	if err != nil {
		return Message{}, err
	}
	m, err := scanMessage(tx.QueryRow(ctx, `SELECT `+messageColumns+` FROM chatxo.messages WHERE conversation_id=$1 AND request_id=$2 AND author_id IS NULL`, conv, request))
	create := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !create {
		return Message{}, err
	}
	var oldBytes int64
	if create {
		m = Message{AIStreaming: streaming, ID: identity.NewID(), ConversationID: conv, AIAuthor: &author, Body: body, Attachments: []string{}, RequestID: request}
		if err = tx.QueryRow(ctx, `UPDATE chatxo.conversations SET last_seq=last_seq+1 WHERE id=$1 RETURNING last_seq,clock_timestamp()`, conv).Scan(&m.Seq, &m.CreatedAt); err != nil {
			return Message{}, err
		}
	} else {
		if m.Deleted || m.AIAuthor == nil || m.AIAuthor.ID != author.ID {
			return Message{}, identity.ErrConflict
		}
		if m.Body == body && card == nil && len(images) == 0 {
			return m, nil
		}
		if err = tx.QueryRow(ctx, `SELECT bytes FROM chatxo.messages WHERE id=$1`, m.ID).Scan(&oldBytes); err != nil {
			return Message{}, err
		}
		m.Body = body
		if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&m.EditedAt); err != nil {
			return Message{}, err
		}
	}
	for _, image := range images {
		if !slices.Contains(m.Attachments, image.ID) {
			m.Attachments = append(m.Attachments, image.ID)
		}
	}
	position := len(m.Consultations)
	if card != nil {
		for i, c := range m.Consultations {
			if c.CallID == card.CallID {
				position = i
				break
			}
		}
		if position == len(m.Consultations) {
			m.Consultations = append(m.Consultations, *card)
		} else {
			if m.Consultations[position].Status != "waiting" {
				return m, nil
			}
			card.PresetID = m.Consultations[position].PresetID
			m.Consultations[position] = *card
		}
	}
	if messageBytes(m) > policy.TextMaxBytes*3/4 || int64(len(body)) > s.maxBody(policy) {
		return Message{}, ErrAIStorageFull
	}
	kind = "message.updated"
	if create {
		hash := sha256.Sum256([]byte(request))
		_, err = tx.Exec(ctx, `INSERT INTO chatxo.messages(id,conversation_id,ai_author,seq,body,attachment_ids,created_at,request_id,request_hash,bytes,ai_response_request_id,ai_streaming) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11::uuid,$8::uuid),$12)`, m.ID, conv, author, m.Seq, body, m.Attachments, m.CreatedAt, request, hash[:], messageBytes(m), responseRequest, streaming)
		kind = "message.created"
	} else {
		_, err = tx.Exec(ctx, `UPDATE chatxo.messages SET body=$2,edited_at=$3,bytes=$4,attachment_ids=$5 WHERE id=$1`, m.ID, body, m.EditedAt, messageBytes(m), m.Attachments)
	}
	if err != nil {
		return Message{}, err
	}
	if card != nil {
		// Selecting the live ID also handles deletion between provider submission and persistence.
		_, err = tx.Exec(ctx, `INSERT INTO chatxo.message_consultations(message_id,call_id,position,preset_id,question,answer,status)
    VALUES($1,$2,$3,(SELECT id FROM chatxo.ai_presets WHERE id=NULLIF($4,'')::uuid AND channel_id=$8),$5,$6,$7)
    ON CONFLICT(message_id,call_id) DO UPDATE SET answer=excluded.answer,status=excluded.status`, m.ID, card.CallID, position, card.PresetID, card.Question, card.Answer, card.Status, conv)
		if err != nil {
			return Message{}, err
		}
	}
	{
		for _, image := range images {
			tag, e := tx.Exec(ctx, `UPDATE chatxo.attachments SET claimed=true,message_id=$1 WHERE id=$2 AND conversation_id=$3 AND ai_generation_id=$4 AND status='ready' AND (NOT claimed OR message_id=$1)`, m.ID, image.ID, conv, image.Generation)
			if e != nil {
				return Message{}, e
			}
			if tag.RowsAffected() != 1 {
				return Message{}, identity.ErrConflict
			}
		}
	}

	if err = changeText(ctx, tx, messageBytes(m)-oldBytes); err != nil {
		return Message{}, err
	}
	if err = s.emit(ctx, tx, conv, &m.ID, kind, m); err != nil {
		return Message{}, err
	}
	return m, nil
}

func (s *Store) Emit(ctx context.Context, tx pgx.Tx, conv string, messageID *string, kind string, data any) error {
	return s.emit(ctx, tx, conv, messageID, kind, data)
}

func (s *Store) PruneStorage(ctx context.Context, tx pgx.Tx) error {
	policy, err := s.PolicyForTransaction(ctx, tx)
	if err != nil {
		return err
	}
	return s.prune(ctx, tx, policy)
}

func (s *Store) PruneAIHistory(ctx context.Context, tx pgx.Tx, conv string, through int64) error {
	var isAI bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.channels WHERE id=$1 AND instance_id=$2 AND kind='ai')`, conv, s.InstanceID).Scan(&isAI); err != nil {
		return err
	}
	if !isAI || through < 1 {
		return identity.ErrInvalid
	}
	if s.AI != nil {
		if err := s.AI.Prune(ctx, tx, conv, through); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE chatxo.attachments SET status='deleting',expires_at=now() WHERE message_id IN (SELECT id FROM chatxo.messages WHERE conversation_id=$1 AND seq<=$2)`, conv, through); err != nil {
		return err
	}
	var reclaimed, eventBytes, floor int64
	if err := tx.QueryRow(ctx, `WITH gone AS (DELETE FROM chatxo.events WHERE message_id IN (SELECT id FROM chatxo.messages WHERE conversation_id=$1 AND seq<=$2) RETURNING id,bytes) SELECT COALESCE(sum(bytes),0),COALESCE(max(id),0) FROM gone`, conv, through).Scan(&eventBytes, &floor); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `WITH gone AS (DELETE FROM chatxo.messages WHERE conversation_id=$1 AND seq<=$2 RETURNING bytes) SELECT COALESCE(sum(bytes),0) FROM gone`, conv, through).Scan(&reclaimed); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE chatxo.storage SET text_bytes=text_bytes-$1,event_floor=greatest(event_floor,$2) WHERE singleton`, reclaimed+eventBytes, floor); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE chatxo.conversations SET pruned_through=greatest(pruned_through,$2) WHERE id=$1`, conv, through); err != nil {
		return err
	}
	return s.emit(ctx, tx, conv, nil, "history.pruned", map[string]any{"pruned_through": through})
}

// FinalizeAI updates retained partial replies, including earlier speakers in the same turn.
func (s *Store) FinalizeAI(ctx context.Context, tx pgx.Tx, channel, request string) ([]string, error) {
	rows, err := tx.Query(ctx, `UPDATE chatxo.messages SET ai_streaming=false,edited_at=clock_timestamp() WHERE conversation_id=$1 AND COALESCE(ai_response_request_id,request_id)=$2 AND ai_streaming RETURNING id`, channel, request)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err = tx.Exec(ctx, `UPDATE chatxo.message_consultations SET status='unavailable' WHERE message_id=$1 AND status='waiting'`, id); err != nil {
			return nil, err
		}
		m, err := scanMessage(tx.QueryRow(ctx, `SELECT `+messageColumns+` FROM chatxo.messages WHERE id=$1`, id))
		if err != nil {
			return nil, err
		}
		var oldBytes int64
		if err = tx.QueryRow(ctx, `SELECT bytes FROM chatxo.messages WHERE id=$1`, id).Scan(&oldBytes); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `UPDATE chatxo.messages SET bytes=$2 WHERE id=$1`, id, messageBytes(m)); err != nil {
			return nil, err
		}
		if err = changeText(ctx, tx, messageBytes(m)-oldBytes); err != nil {
			return nil, err
		}
		if err = s.emit(ctx, tx, channel, &id, "message.updated", m); err != nil {
			return nil, err
		}
	}
	return ids, nil
}
