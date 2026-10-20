package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool         *pgxpool.Pool
	Identity     *identity.Service
	InstanceID   string
	Policy       config.Policy
	lockAcquires atomic.Uint64
	lockDuration atomic.Uint64
	lockPending  atomic.Int64
}
type Conversation struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Members []string `json:"members,omitempty"`
}
type Message struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	AuthorID       string     `json:"author_id"`
	Seq            int64      `json:"seq"`
	Body           string     `json:"body"`
	Attachments    []string   `json:"attachment_ids"`
	CreatedAt      time.Time  `json:"created_at"`
	EditedAt       *time.Time `json:"edited_at,omitempty"`
	Deleted        bool       `json:"deleted"`
	RequestID      string     `json:"request_id"`
	ReplyToID      *string    `json:"reply_to_id,omitempty"`
	MentionUserIDs []string   `json:"mention_user_ids,omitempty"`
}
type History struct {
	Messages      []Message `json:"messages"`
	PrunedThrough int64     `json:"pruned_through"`
	LastSeq       int64     `json:"last_seq"`
	EventCursor   int64     `json:"event_cursor"`
	Gap           bool      `json:"gap"`
}
type Event struct {
	ID             int64  `json:"id"`
	Type           string `json:"type"`
	ConversationID string `json:"conversation_id"`
	Data           any    `json:"data"`
}

const messageColumns = `id,conversation_id,author_id,seq,body,attachment_ids,created_at,edited_at,deleted,request_id,reply_to_id,mention_user_ids`

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	err := row.Scan(&m.ID, &m.ConversationID, &m.AuthorID, &m.Seq, &m.Body, &m.Attachments, &m.CreatedAt, &m.EditedAt, &m.Deleted, &m.RequestID, &m.ReplyToID, &m.MentionUserIDs)
	return m, err
}

func New(pool *pgxpool.Pool, ids *identity.Service, c config.Config) (*Store, error) {
	if c.Policy.TextMaxBytes < 4096 {
		return nil, errors.New("text budget must be at least 4096 bytes")
	}
	s := &Store{Pool: pool, Identity: ids, InstanceID: c.InstanceID, Policy: c.Policy}
	return s, nil
}

// Access always uses membership for DMs, including for the owner. Channel
// access uses current category/channel roles and never trusts a claimed author.
// Both text and voice channels have persistent chat; voice admission remains
// separate, so reading or sending a message never joins a media session.
func (s *Store) Access(ctx context.Context, p identity.Principal, id string, write bool) error {
	if !identity.ValidID(id) {
		return identity.ErrInvalid
	}
	var kind string
	var channel *string
	var member bool
	err := s.Pool.QueryRow(ctx, `SELECT kind,channel_id,EXISTS(SELECT 1 FROM chatxo.conversation_members WHERE conversation_id=c.id AND user_id=$2) AND EXISTS(SELECT 1 FROM chatxo.conversation_members other JOIN chatxo.users u ON u.id=other.user_id WHERE other.conversation_id=c.id AND other.user_id<>$2 AND u.deleted_at IS NULL) FROM chatxo.conversations c WHERE instance_id=$1 AND id=$3`, s.InstanceID, p.User.ID, id).Scan(&kind, &channel, &member)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && kind == "dm" {
		if !member {
			return identity.ErrNotFound
		}
		return nil
	}
	if err == nil && channel == nil {
		return identity.ErrNotFound
	}
	ch, err := s.Identity.Channel(ctx, p, id)
	if err != nil {
		return err
	}
	if write && ch.Permissions&identity.SendMessages == 0 {
		return identity.ErrForbidden
	}
	if kind == "channel" {
		return nil
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO chatxo.conversations(id,instance_id,kind,channel_id) VALUES ($1,$2,'channel',$1) ON CONFLICT DO NOTHING`, id, s.InstanceID)
	return err
}

func (s *Store) DM(ctx context.Context, p identity.Principal, members []string) (Conversation, error) {
	if len(members) < 1 || len(members) > 15 {
		return Conversation{}, identity.ErrInvalid
	}
	members = append(append([]string{}, members...), p.User.ID)
	sort.Strings(members)
	for i, id := range members {
		if !identity.ValidID(id) || (i > 0 && id == members[i-1]) {
			return Conversation{}, identity.ErrInvalid
		}
	}
	digest := sha256.Sum256([]byte(strings.Join(members, ",")))
	key := hex.EncodeToString(digest[:])
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Conversation{}, err
	}
	defer tx.Rollback(ctx)
	// Lock only these accounts so a concurrent deletion cannot leave a freshly
	// inserted membership for a deleted user after its cleanup has committed.
	rows, err := tx.Query(ctx, `SELECT id FROM chatxo.users WHERE instance_id=$1 AND id=ANY($2::uuid[]) AND deleted_at IS NULL ORDER BY id FOR SHARE`, s.InstanceID, members)
	if err != nil {
		return Conversation{}, err
	}
	count := 0
	for rows.Next() {
		count++
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return Conversation{}, err
	}
	if count != len(members) {
		return Conversation{}, identity.ErrNotFound
	}
	conv := Conversation{ID: identity.NewID(), Kind: "dm", Members: members}
	err = tx.QueryRow(ctx, `INSERT INTO chatxo.conversations(id,instance_id,kind,dm_key) VALUES ($1,$2,'dm',$3) ON CONFLICT(dm_key) DO UPDATE SET dm_key=excluded.dm_key RETURNING id`, conv.ID, s.InstanceID, key).Scan(&conv.ID)
	if err != nil {
		return Conversation{}, err
	}
	for _, id := range members {
		if _, err = tx.Exec(ctx, `INSERT INTO chatxo.conversation_members(conversation_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, conv.ID, id); err != nil {
			return Conversation{}, err
		}
	}
	return conv, tx.Commit(ctx)
}

func (s *Store) Conversations(ctx context.Context, p identity.Principal, after string) ([]Conversation, error) {
	if after != "" && !identity.ValidID(after) {
		return nil, identity.ErrInvalid
	}
	var cursor any
	if after != "" {
		cursor = after
	}
	channels, err := s.Identity.Channels(ctx, p, "")
	if err != nil {
		return nil, err
	}
	result := []Conversation{}
	for _, ch := range channels {
		if ch.ID > after {
			result = append(result, Conversation{ID: ch.ID, Kind: "channel"})
		}
	}
	rows, err := s.Pool.Query(ctx, `SELECT c.id,array_agg(m.user_id::text ORDER BY m.user_id) FROM chatxo.conversations c JOIN chatxo.conversation_members m ON m.conversation_id=c.id WHERE c.instance_id=$1 AND c.kind='dm' AND ($3::uuid IS NULL OR c.id>$3::uuid) AND EXISTS(SELECT 1 FROM chatxo.conversation_members own WHERE own.conversation_id=c.id AND own.user_id=$2) AND EXISTS(SELECT 1 FROM chatxo.conversation_members other JOIN chatxo.users u ON u.id=other.user_id WHERE other.conversation_id=c.id AND other.user_id<>$2 AND u.deleted_at IS NULL) GROUP BY c.id ORDER BY c.id LIMIT 100`, s.InstanceID, p.User.ID, cursor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		v := Conversation{Kind: "dm"}
		if err := rows.Scan(&v.ID, &v.Members); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	if len(result) > 100 {
		result = result[:100]
	}
	return result, rows.Err()
}

func (s *Store) maxBody(policy config.Policy) int64 {
	n := (policy.TextMaxBytes - 2048) / 4
	if n > 65536 {
		n = 65536
	}
	return n
}
func (s *Store) validate(body string, attachments []string, policy config.Policy) error {
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) || int64(len(body)) > s.maxBody(policy) || len(attachments) > 8 || (strings.TrimSpace(body) == "" && len(attachments) == 0) {
		return identity.ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range attachments {
		if !identity.ValidID(id) || seen[id] {
			return identity.ErrInvalid
		}
		seen[id] = true
	}
	return nil
}

// LockStorage is the shared lock order for text and attachment accounting.
// Metrics include database round-trip and contention time, not CPU time.
func (s *Store) LockStorage(ctx context.Context, tx pgx.Tx) error {
	start := time.Now()
	s.lockPending.Add(1)
	defer func() { s.lockPending.Add(-1); s.lockAcquires.Add(1); s.lockDuration.Add(uint64(time.Since(start))) }()
	_, err := tx.Exec(ctx, `SELECT singleton FROM chatxo.storage WHERE singleton FOR UPDATE`)
	return err
}
func (s *Store) LockStats() map[string]any {
	return map[string]any{"acquire_count": s.lockAcquires.Load(), "acquire_duration_ns": s.lockDuration.Load(), "pending_acquires": s.lockPending.Load()}
}
func changeText(ctx context.Context, tx pgx.Tx, delta int64) error {
	_, err := tx.Exec(ctx, `UPDATE chatxo.storage SET text_bytes=text_bytes+$1 WHERE singleton`, delta)
	return err
}
func messageBytes(m Message) int64 { b, _ := json.Marshal(m); return int64(len(b) + 64) }

func (s *Store) Create(ctx context.Context, p identity.Principal, conv, request, body, reply string, attachments []string) (Message, error) {
	if err := s.Access(ctx, p, conv, true); err != nil {
		return Message{}, err
	}
	if !identity.ValidID(request) {
		return Message{}, identity.ErrInvalid
	}
	if attachments == nil {
		attachments = []string{}
	}
	input, _ := json.Marshal(struct {
		Conversation string
		Body         string
		Reply        string
		Attachments  []string
	}{conv, body, reply, attachments})
	hash := sha256.Sum256(input)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.LockStorage(ctx, tx); err != nil {
		return Message{}, err
	}
	policy, err := s.PolicyForTransaction(ctx, tx)
	if err != nil {
		return Message{}, err
	}
	admission, err := s.messagePolicy(ctx, tx, conv, policy)
	if err != nil {
		return Message{}, err
	}
	if err := s.validate(body, attachments, admission); err != nil {
		return Message{}, err
	}
	var previousHash []byte
	previous, err := scanMessage(tx.QueryRow(ctx, `SELECT `+messageColumns+` FROM chatxo.messages WHERE author_id=$1 AND request_id=$2`, p.User.ID, request))
	if err == nil {
		if err := tx.QueryRow(ctx, `SELECT request_hash FROM chatxo.messages WHERE id=$1`, previous.ID).Scan(&previousHash); err != nil {
			return Message{}, err
		}
		if string(previousHash) != string(hash[:]) {
			return Message{}, identity.ErrConflict
		}
		// Deduplication lasts while the message row is retained. An expired row
		// awaiting maintenance must not be returned as a successful retry.
		var retained bool
		if err = tx.QueryRow(ctx, `SELECT chatxo.message_retained($1,$2,$3,$4)`, conv, previous.CreatedAt, policy.TextRetentionDays, policy.DM.TextRetentionDays).Scan(&retained); err != nil {
			return Message{}, err
		}
		if !retained {
			return Message{}, identity.ErrConflict
		}
		return previous, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Message{}, err
	}
	replyAuthor := ""
	if reply != "" {
		if !identity.ValidID(reply) {
			return Message{}, identity.ErrInvalid
		}
		if err = tx.QueryRow(ctx, `SELECT author_id FROM chatxo.messages WHERE id=$1 AND conversation_id=$2 AND NOT deleted AND chatxo.message_retained(conversation_id,created_at,$3,$4)`, reply, conv, policy.TextRetentionDays, policy.DM.TextRetentionDays).Scan(&replyAuthor); errors.Is(err, pgx.ErrNoRows) {
			return Message{}, identity.ErrInvalid
		} else if err != nil {
			return Message{}, err
		}
	}
	if err = s.validateMLS(ctx, tx, p, conv, body); err != nil {
		return Message{}, err
	}
	m := Message{ID: identity.NewID(), ConversationID: conv, AuthorID: p.User.ID, Body: body, Attachments: attachments, RequestID: request}
	if reply != "" {
		m.ReplyToID = &reply
	}
	m.MentionUserIDs, err = s.resolveMentions(ctx, tx, body)
	if err != nil {
		return Message{}, err
	}
	if replyAuthor != "" && replyAuthor != p.User.ID {
		found := false
		for _, id := range m.MentionUserIDs {
			found = found || id == replyAuthor
		}
		if !found {
			m.MentionUserIDs = append(m.MentionUserIDs, replyAuthor)
			sort.Strings(m.MentionUserIDs)
		}
	}
	if err = tx.QueryRow(ctx, `UPDATE chatxo.conversations SET last_seq=last_seq+1 WHERE id=$1 RETURNING last_seq,clock_timestamp()`, conv).Scan(&m.Seq, &m.CreatedAt); err != nil {
		return Message{}, err
	}
	// JSON escaping can expand UTF-8 text sixfold. Reserve room for the
	// bounded replay log so a successful write cannot immediately evict itself.
	if messageBytes(m) > admission.TextMaxBytes*3/4 {
		return Message{}, identity.ErrInvalid
	}
	for _, id := range attachments {
		var valid bool
		err = tx.QueryRow(ctx, `SELECT status='ready' AND (expires_at IS NULL OR expires_at>clock_timestamp()) AND NOT claimed AND (NOT is_dm OR encrypted) FROM chatxo.attachments WHERE id=$1 AND conversation_id=$2 AND user_id=$3 FOR UPDATE`, id, conv, p.User.ID).Scan(&valid)
		if errors.Is(err, pgx.ErrNoRows) || !valid {
			return Message{}, identity.ErrInvalid
		}
		if err != nil {
			return Message{}, err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO chatxo.messages(id,conversation_id,author_id,seq,body,attachment_ids,created_at,request_id,request_hash,bytes,reply_to_id,mention_user_ids) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, m.ID, conv, p.User.ID, m.Seq, body, attachments, m.CreatedAt, request, hash[:], messageBytes(m), m.ReplyToID, m.MentionUserIDs)
	if err != nil {
		return Message{}, err
	}
	for _, id := range attachments {
		if _, err = tx.Exec(ctx, `UPDATE chatxo.attachments SET claimed=true,message_id=$2,expires_at=CASE WHEN is_dm THEN CASE WHEN $3::bigint=0 THEN NULL ELSE now()+$3::bigint*interval '1 day' END ELSE expires_at END WHERE id=$1`, id, m.ID, policy.DM.ImageRetentionDays); err != nil {
			return Message{}, err
		}
	}
	if err = changeText(ctx, tx, messageBytes(m)); err != nil {
		return Message{}, err
	}
	if err = s.dmTextBudget(ctx, tx, conv, policy); err != nil {
		return Message{}, err
	}
	if err = s.emit(ctx, tx, conv, &m.ID, "message.created", m); err != nil {
		return Message{}, err
	}
	if err = s.prune(ctx, tx, policy); err != nil {
		return Message{}, err
	}
	return m, tx.Commit(ctx)
}

func (s *Store) Edit(ctx context.Context, p identity.Principal, conv, id, body string, remove bool) (Message, error) {
	if err := s.Access(ctx, p, conv, !remove); err != nil {
		return Message{}, err
	}
	if !identity.ValidID(id) {
		return Message{}, identity.ErrInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.LockStorage(ctx, tx); err != nil {
		return Message{}, err
	}
	policy, err := s.PolicyForTransaction(ctx, tx)
	if err != nil {
		return Message{}, err
	}
	admission, err := s.messagePolicy(ctx, tx, conv, policy)
	if err != nil {
		return Message{}, err
	}
	m, err := scanMessage(tx.QueryRow(ctx, `SELECT `+messageColumns+` FROM chatxo.messages WHERE conversation_id=$1 AND id=$2 AND chatxo.message_retained(conversation_id,created_at,$3,$4)`, conv, id, policy.TextRetentionDays, policy.DM.TextRetentionDays))
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, identity.ErrNotFound
	}
	if err != nil {
		return Message{}, err
	}
	if m.AuthorID != p.User.ID {
		var kind string
		if err = tx.QueryRow(ctx, `SELECT kind FROM chatxo.conversations WHERE id=$1`, conv).Scan(&kind); err != nil {
			return Message{}, err
		}
		if !remove || kind != "channel" || !p.Can(identity.ManageMessages) {
			return Message{}, identity.ErrForbidden
		}
		if err = s.Identity.AuditTransaction(ctx, tx, p, id, "message.delete"); err != nil {
			return Message{}, err
		}
	}
	if m.Deleted {
		return Message{}, identity.ErrConflict
	}
	var oldBytes int64
	if err = tx.QueryRow(ctx, `SELECT bytes FROM chatxo.messages WHERE id=$1`, m.ID).Scan(&oldBytes); err != nil {
		return Message{}, err
	}
	kind := "message.updated"
	if remove {
		if _, err = tx.Exec(ctx, `UPDATE chatxo.attachments SET expires_at=now(),status='deleting' WHERE message_id=$1`, m.ID); err != nil {
			return Message{}, err
		}
		var eventBytes, floor int64
		if err = tx.QueryRow(ctx, `WITH gone AS (DELETE FROM chatxo.events WHERE message_id=$1 RETURNING id,bytes) SELECT COALESCE(sum(bytes),0),COALESCE(max(id),0) FROM gone`, m.ID).Scan(&eventBytes, &floor); err != nil {
			return Message{}, err
		}
		if _, err = tx.Exec(ctx, `UPDATE chatxo.storage SET text_bytes=text_bytes-$1,event_floor=greatest(event_floor,$2) WHERE singleton`, eventBytes, floor); err != nil {
			return Message{}, err
		}
		body = ""
		m.Attachments = []string{}
		m.Deleted = true
		kind = "message.deleted"
	} else if err := s.validate(body, m.Attachments, admission); err != nil {
		return Message{}, err
	}
	if !remove {
		if err = s.validateMLS(ctx, tx, p, conv, body); err != nil {
			return Message{}, err
		}
	}
	m.Body = body
	m.MentionUserIDs, err = s.resolveMentions(ctx, tx, body)
	if err != nil {
		return Message{}, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return Message{}, err
	}
	m.EditedAt = &now
	if messageBytes(m) > admission.TextMaxBytes*3/4 {
		return Message{}, identity.ErrInvalid
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.messages SET body=$2,attachment_ids=$3,deleted=$4,edited_at=$5,bytes=$6,mention_user_ids=$7 WHERE id=$1`, m.ID, m.Body, m.Attachments, m.Deleted, m.EditedAt, messageBytes(m), m.MentionUserIDs); err != nil {
		return Message{}, err
	}
	if err = changeText(ctx, tx, messageBytes(m)-oldBytes); err != nil {
		return Message{}, err
	}
	if !remove {
		if err = s.dmTextBudget(ctx, tx, conv, policy); err != nil {
			return Message{}, err
		}
	}
	if err = s.emit(ctx, tx, conv, &m.ID, kind, m); err != nil {
		return Message{}, err
	}
	if err = s.prune(ctx, tx, policy); err != nil {
		return Message{}, err
	}
	return m, tx.Commit(ctx)
}

func (s *Store) History(ctx context.Context, p identity.Principal, conv string, after int64, limit int) (History, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return History{}, identity.ErrInvalid
	}
	if err := s.Access(ctx, p, conv, false); err != nil {
		return History{}, err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return History{}, err
	}
	defer tx.Rollback(ctx)
	policy, err := s.PolicyForTransaction(ctx, tx)
	if err != nil {
		return History{}, err
	}
	h := History{Messages: []Message{}}
	if err = tx.QueryRow(ctx, `SELECT c.last_seq,c.pruned_through,s.event_seq FROM chatxo.conversations c CROSS JOIN chatxo.storage s WHERE c.id=$1`, conv).Scan(&h.LastSeq, &h.PrunedThrough, &h.EventCursor); err != nil {
		return History{}, err
	}
	// Age filtering also advances the visible boundary before maintenance runs.
	var expired int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(seq),0) FROM chatxo.messages WHERE conversation_id=$1 AND NOT chatxo.message_retained(conversation_id,created_at,$2,$3)`, conv, policy.TextRetentionDays, policy.DM.TextRetentionDays).Scan(&expired); err != nil {
		return History{}, err
	}
	if expired > h.PrunedThrough {
		h.PrunedThrough = expired
	}
	h.Gap = after < h.PrunedThrough || after > h.LastSeq
	rows, err := tx.Query(ctx, `SELECT `+messageColumns+` FROM chatxo.messages WHERE conversation_id=$1 AND seq>$2 AND chatxo.message_retained(conversation_id,created_at,$3,$5) ORDER BY seq LIMIT $4`, conv, after, policy.TextRetentionDays, limit, policy.DM.TextRetentionDays)
	if err != nil {
		return History{}, err
	}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return History{}, err
		}
		h.Messages = append(h.Messages, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return History{}, err
	}
	return h, tx.Commit(ctx)
}

func (s *Store) emit(ctx context.Context, tx pgx.Tx, conv string, messageID *string, kind string, data any) error {
	var seq int64
	if err := tx.QueryRow(ctx, `UPDATE chatxo.storage SET event_seq=event_seq+1 WHERE singleton RETURNING event_seq`).Scan(&seq); err != nil {
		return err
	}
	payload, err := json.Marshal(Event{ID: seq, Type: kind, ConversationID: conv, Data: data})
	if err != nil {
		return err
	}
	bytes := int64(len(payload) + 64)
	if _, err = tx.Exec(ctx, `INSERT INTO chatxo.events(id,conversation_id,message_id,payload,bytes) VALUES($1,$2,$3,$4,$5)`, seq, conv, messageID, payload, bytes); err != nil {
		return err
	}
	return changeText(ctx, tx, bytes)
}

// All accounting mutations lock storage first, then conversations/attachments.
// Readers remain concurrent. Event replay is bounded to an hour, 10k rows, and
// one quarter of the text budget; evictions persist a visible replay floor.
func (s *Store) trimEvents(ctx context.Context, tx pgx.Tx, policy config.Policy) error {
	var reclaimed, floor int64
	err := tx.QueryRow(ctx, `WITH ranked AS (SELECT id,created_at,sum(bytes) OVER(ORDER BY id DESC) total,row_number() OVER(ORDER BY id DESC) n FROM chatxo.events),gone AS (DELETE FROM chatxo.events e USING ranked r WHERE e.id=r.id AND (r.total>$1 OR r.n>10000 OR r.created_at<now()-interval '1 hour') RETURNING e.id,e.bytes) SELECT COALESCE(sum(bytes),0),COALESCE(max(id),0) FROM gone`, policy.TextMaxBytes/4).Scan(&reclaimed, &floor)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE chatxo.storage SET text_bytes=text_bytes-$1,event_floor=greatest(event_floor,$2) WHERE singleton`, reclaimed, floor)
	return err
}

func (s *Store) prune(ctx context.Context, tx pgx.Tx, policy config.Policy) error {
	if err := s.pruneDM(ctx, tx, policy); err != nil {
		return err
	}
	if err := s.trimEvents(ctx, tx, policy); err != nil {
		return err
	}
	for {
		var used int64
		if err := tx.QueryRow(ctx, `SELECT text_bytes-dm_text_bytes FROM chatxo.storage WHERE singleton`).Scan(&used); err != nil {
			return err
		}
		// Inspect at most 512 oldest rows per batch. The running reclaimed
		// count selects the exact oldest prefix needed, including replay bytes.
		rows, err := tx.Query(ctx, `SELECT m.id,m.conversation_id,m.seq,m.bytes+COALESCE(e.bytes,0),COALESCE(e.floor,0),m.created_at<=now()-$2::bigint*interval '1 day'
		 FROM (SELECT * FROM chatxo.messages WHERE conversation_id IN (SELECT id FROM chatxo.conversations WHERE kind='channel') AND ($1 OR created_at<=now()-$2::bigint*interval '1 day') ORDER BY created_at,id LIMIT 512) m
		 LEFT JOIN LATERAL (SELECT sum(bytes) bytes,max(id) floor FROM chatxo.events WHERE message_id=m.id) e ON true ORDER BY m.created_at,m.id`, used > policy.TextMaxBytes, policy.TextRetentionDays)
		if err != nil {
			return err
		}
		victims := []string{}
		boundaries := map[string]int64{}
		var reclaimed, floor int64
		for rows.Next() {
			var id, conv string
			var seq, bytes, eventFloor int64
			var expired bool
			if err := rows.Scan(&id, &conv, &seq, &bytes, &eventFloor, &expired); err != nil {
				rows.Close()
				return err
			}
			if !expired && used-reclaimed <= policy.TextMaxBytes {
				break
			}
			victims = append(victims, id)
			reclaimed += bytes
			floor = max(floor, eventFloor)
			boundaries[conv] = max(boundaries[conv], seq)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(victims) == 0 {
			return nil
		}
		if _, err = tx.Exec(ctx, `DELETE FROM chatxo.messages WHERE id=ANY($1::uuid[])`, victims); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE chatxo.storage SET text_bytes=text_bytes-$1,event_floor=greatest(event_floor,$2) WHERE singleton`, reclaimed, floor); err != nil {
			return err
		}
		conversations := make([]string, 0, len(boundaries))
		for conv := range boundaries {
			conversations = append(conversations, conv)
		}
		sort.Strings(conversations)
		for _, conv := range conversations {
			seq := boundaries[conv]
			if _, err = tx.Exec(ctx, `UPDATE chatxo.conversations SET pruned_through=greatest(pruned_through,$2) WHERE id=$1`, conv, seq); err != nil {
				return err
			}
			if err = s.emit(ctx, tx, conv, nil, "history.pruned", map[string]any{"pruned_through": seq}); err != nil {
				return err
			}
		}
		if err = s.trimEvents(ctx, tx, policy); err != nil {
			return err
		}
	}
}

func (s *Store) Maintain(ctx context.Context) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.LockStorage(ctx, tx); err != nil {
		return err
	}
	policy, err := s.PolicyForTransaction(ctx, tx)
	if err != nil {
		return err
	}
	if err = s.prune(ctx, tx, policy); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Events returns a snapshot cursor even when none of the subscriptions had
// changes. Callers must authorize every subscribed conversation before reading.
func (s *Store) Events(ctx context.Context, after int64, conversations []string) ([][]byte, int64, bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, false, err
	}
	defer tx.Rollback(ctx)
	policy, err := s.PolicyForTransaction(ctx, tx)
	if err != nil {
		return nil, 0, false, err
	}
	var cursor, floor int64
	if err = tx.QueryRow(ctx, `SELECT event_seq,event_floor FROM chatxo.storage WHERE singleton`).Scan(&cursor, &floor); err != nil {
		return nil, 0, false, err
	}
	if after < floor || after > cursor {
		return nil, cursor, true, tx.Commit(ctx)
	}
	if after == cursor {
		return [][]byte{}, cursor, false, tx.Commit(ctx)
	}
	// Expiry applies immediately, including during the interval between
	// maintenance runs. Never replay the old body of an aged-out message.
	var expired bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.events e LEFT JOIN chatxo.messages m ON m.id=e.message_id WHERE e.id>$1 AND e.conversation_id=ANY($2::uuid[]) AND (e.created_at<=now()-interval '1 hour' OR NOT chatxo.message_retained(e.conversation_id,m.created_at,$3,$4)))`, after, conversations, policy.TextRetentionDays, policy.DM.TextRetentionDays).Scan(&expired); err != nil {
		return nil, 0, false, err
	}
	if expired {
		return nil, cursor, true, tx.Commit(ctx)
	}
	rows, err := tx.Query(ctx, `WITH candidates AS (SELECT id,bytes FROM chatxo.events WHERE id>$1 AND conversation_id=ANY($2::uuid[]) ORDER BY id LIMIT 100), bounded AS (SELECT id,sum(bytes) OVER(ORDER BY id) total FROM candidates) SELECT e.id,e.payload FROM bounded b JOIN chatxo.events e ON e.id=b.id WHERE b.total<=786432 ORDER BY e.id`, after, conversations)
	if err != nil {
		return nil, 0, false, err
	}
	items := [][]byte{}
	var last int64
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&last, &b); err != nil {
			rows.Close()
			return nil, 0, false, err
		}
		items = append(items, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, false, err
	}
	if len(items) > 0 {
		var more bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.events WHERE id>$1 AND conversation_id=ANY($2::uuid[]))`, last, conversations).Scan(&more); err != nil {
			return nil, 0, false, err
		}
		if more {
			cursor = last
		}
	}
	return items, cursor, false, tx.Commit(ctx)
}

// Message returns one currently retained, visible message for derived views.
// It never grants an owner access to another member's private conversation.
func (s *Store) Message(ctx context.Context, p identity.Principal, conversation, id string) (Message, error) {
	if !identity.ValidID(id) {
		return Message{}, identity.ErrInvalid
	}
	if err := s.Access(ctx, p, conversation, false); err != nil {
		return Message{}, err
	}
	m, err := scanMessage(s.Pool.QueryRow(ctx, `SELECT `+messageColumns+` FROM chatxo.messages WHERE conversation_id=$1 AND id=$2 AND NOT deleted AND chatxo.message_retained(conversation_id,created_at,$3,$4)`, conversation, id, s.CurrentPolicy().TextRetentionDays, s.CurrentPolicy().DM.TextRetentionDays))
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, identity.ErrNotFound
	}
	return m, err
}

func (s *Store) CurrentPolicy() config.Policy { return s.Identity.CurrentPolicy(s.Policy) }
func (s *Store) PolicyForTransaction(ctx context.Context, tx pgx.Tx) (config.Policy, error) {
	return s.Identity.PolicyForTransaction(ctx, tx, s.Policy)
}
