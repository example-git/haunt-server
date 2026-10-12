package chat

import (
	"context"
	"sort"

	"github.com/example-git/haunt-server/internal/identity"
)

type ReadState struct {
	Kind           string `json:"kind"`
	ConversationID string `json:"conversation_id"`
	ReadSeq        int64  `json:"read_seq"`
	LastSeq        int64  `json:"last_seq"`
	Unread         int64  `json:"unread_count"`
	Mentions       int64  `json:"mention_count"`
	EventCursor    int64  `json:"event_cursor"`
}
type ReadPage struct {
	States []ReadState `json:"states"`
	Next   string      `json:"next,omitempty"`
}

// VisibleIDs is a directory snapshot, cached by account-feed connections until
// an authorization/directory change. Owners still need membership for DMs.
func (s *Store) VisibleIDs(ctx context.Context, p identity.Principal) ([]string, error) {
	channels, err := s.Identity.Channels(ctx, p, "")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(channels))
	for _, c := range channels {
		ids = append(ids, c.ID)
	}
	rows, err := s.Pool.Query(ctx, `SELECT c.id FROM chatxo.conversations c JOIN chatxo.conversation_members m ON m.conversation_id=c.id WHERE c.instance_id=$1 AND c.kind='dm' AND m.user_id=$2 AND EXISTS(SELECT 1 FROM chatxo.conversation_members other JOIN chatxo.users u ON u.id=other.user_id WHERE other.conversation_id=c.id AND other.user_id<>$2 AND u.deleted_at IS NULL)`, s.InstanceID, p.User.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, rows.Err()
}

// ReadStates requires an authorized directory snapshot; the query uses one MVCC
// snapshot for counts, read positions and their durable event version.
func (s *Store) ReadStates(ctx context.Context, p identity.Principal, ids []string) ([]ReadState, error) {
	states := []ReadState{}
	if len(ids) == 0 {
		return states, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT c.kind,c.id,COALESCE(r.last_seq,0),c.last_seq,count(m.id),count(m.id) FILTER (WHERE $2=ANY(m.mention_user_ids)),v.event_seq
      FROM chatxo.conversations c CROSS JOIN chatxo.storage v
      LEFT JOIN chatxo.read_positions r ON r.conversation_id=c.id AND r.user_id=$2
      LEFT JOIN chatxo.messages m ON m.conversation_id=c.id AND m.seq>COALESCE(r.last_seq,0) AND NOT m.deleted AND m.author_id<>$2 AND chatxo.message_retained(c.id,m.created_at,$4,$5)
      WHERE c.instance_id=$1 AND c.id=ANY($3::uuid[])
      GROUP BY c.id,r.last_seq,v.event_seq ORDER BY c.id`, s.InstanceID, p.User.ID, ids, s.CurrentPolicy().TextRetentionDays, s.CurrentPolicy().DM.TextRetentionDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var state ReadState
		if err := rows.Scan(&state.Kind, &state.ConversationID, &state.ReadSeq, &state.LastSeq, &state.Unread, &state.Mentions, &state.EventCursor); err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, rows.Err()
}
func (s *Store) ReadPage(ctx context.Context, p identity.Principal, after string) (ReadPage, error) {
	result := ReadPage{States: []ReadState{}}
	if after != "" && !identity.ValidID(after) {
		return result, identity.ErrInvalid
	}
	ids, err := s.VisibleIDs(ctx, p)
	if err != nil {
		return result, err
	}
	start := sort.SearchStrings(ids, after)
	if start < len(ids) && ids[start] == after {
		start++
	}
	ids = ids[start:]
	if len(ids) > 100 {
		result.Next = ids[99]
		ids = ids[:100]
	}
	result.States, err = s.ReadStates(ctx, p, ids)
	return result, err
}
func (s *Store) MarkRead(ctx context.Context, p identity.Principal, conversation string, seq int64) (ReadState, error) {
	if seq < 0 {
		return ReadState{}, identity.ErrInvalid
	}
	if err := s.Access(ctx, p, conversation, false); err != nil {
		return ReadState{}, err
	}
	// Monotonic per-account/per-conversation row lock, never the storage lock.
	_, err := s.Pool.Exec(ctx, `INSERT INTO chatxo.read_positions(user_id,conversation_id,last_seq) SELECT $1,id,LEAST(last_seq,$3) FROM chatxo.conversations WHERE id=$2
      ON CONFLICT(user_id,conversation_id) DO UPDATE SET last_seq=GREATEST(chatxo.read_positions.last_seq,excluded.last_seq) WHERE chatxo.read_positions.last_seq<excluded.last_seq`, p.User.ID, conversation, seq)
	if err != nil {
		return ReadState{}, err
	}
	states, err := s.ReadStates(ctx, p, []string{conversation})
	if err != nil {
		return ReadState{}, err
	}
	if len(states) == 0 {
		return ReadState{}, identity.ErrNotFound
	}
	return states[0], nil
}
