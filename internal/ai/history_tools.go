package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
)

func arguments(call provider.ToolCall, out any) bool {
	if len(call.Arguments) > 65536 {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(call.Arguments))
	d.DisallowUnknownFields()
	return d.Decode(out) == nil && d.Decode(new(any)) == io.EOF
}
func jsonResult(call provider.ToolCall, value any) (provider.ToolResult, error) {
	b, err := json.Marshal(value)
	return provider.ToolResult{ID: call.ID, Name: call.Name, Content: string(b)}, err
}
func historyTools() []provider.Tool {
	return []provider.Tool{
		{Name: "search_conversation", Description: "Search retained AI-visible messages in this channel. Use author for an exact author ID or username, after/before for RFC3339 timestamps. Pass reply_chain with a message ID to follow its actual ancestors. Results include excerpts, links, and live attachment IDs.", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","maxLength":256},"author":{"type":"string"},"after":{"type":"string"},"before":{"type":"string"},"media_only":{"type":"boolean"},"reply_chain":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":24}},"additionalProperties":false}`)},
		{Name: "resolve_media", Description: "Resolve an exact retained attachment or list attachments of a message in this channel. Returns recorded edit target, references actually sent, explicit image-specific instructions and direct children. Follow parent_id for earlier edits. An unavailable source cannot be used.", Parameters: json.RawMessage(`{"type":"object","properties":{"attachment_id":{"type":"string"},"message_id":{"type":"string"}},"additionalProperties":false}`)},
		{Name: "clock", Description: "Convert an explicit instant or the current server time into an IANA timezone, including its daylight-saving status. Apply calendar_days in that zone, then elapsed_seconds.", Parameters: json.RawMessage(`{"type":"object","properties":{"at":{"type":"string","description":"RFC3339 with offset; omit for now"},"timezone":{"type":"string"},"calendar_days":{"type":"integer"},"elapsed_seconds":{"type":"integer"}},"required":["timezone"],"additionalProperties":false}`)},
	}
}

// The running turn has a fixed boundary. A preceding queued answer may have been
// written after that sequence, while the triggering human posts were waiting.
const visibleHistory = `WITH RECURSIVE visible AS NOT MATERIALIZED (
 SELECT m.* FROM chatxo.messages m WHERE m.conversation_id=$1 AND m.ai_visible AND NOT m.deleted
 AND chatxo.message_retained(m.conversation_id,m.created_at,$4,0)
 AND (m.seq<=$2 OR (m.ai_author IS NOT NULL AND EXISTS(SELECT 1 FROM chatxo.ai_turns prev
 WHERE prev.channel_id=$1 AND prev.response_request_id=COALESCE(m.ai_response_request_id,m.request_id)
 AND prev.boundary_seq<=$2 AND (prev.status='completed' OR (prev.id=$3 AND prev.status='running')))))) `
const availableAttachment = `a.status='ready' AND a.claimed AND (a.expires_at IS NULL OR a.expires_at>now())`

type historyHit struct {
	ID          string    `json:"message_id"`
	AuthorID    string    `json:"author_id"`
	Username    string    `json:"username,omitempty"`
	Name        string    `json:"display_name"`
	Excerpt     string    `json:"excerpt"`
	Sent        time.Time `json:"created_at"`
	Reply       string    `json:"reply_to,omitempty"`
	Attachments []string  `json:"attachment_ids"`
	Link        string    `json:"link,omitempty"`
}

func (s *Service) historyArgs(t claimedTurn) []any {
	return []any{t.ChannelID, t.boundary, t.ID, s.chat.CurrentPolicy().TextRetentionDays}
}
func (s *Service) searchConversation(ctx context.Context, t claimedTurn, call provider.ToolCall) (provider.ToolResult, error) {
	var in struct {
		Query, Author, After, Before string
		Media                        bool   `json:"media_only"`
		Reply                        string `json:"reply_chain"`
		Limit                        *int   `json:"limit"`
	}
	if !arguments(call, &in) || len([]rune(in.Query)) > 256 || len(in.Author) > 128 || in.Reply != "" && !identity.ValidID(in.Reply) {
		return toolFailure(call, "Invalid search or reply-chain arguments."), nil
	}
	limit := 12
	if in.Limit != nil {
		limit = *in.Limit
	}
	if limit < 1 || limit > 24 {
		return toolFailure(call, "limit must be between 1 and 24"), nil
	}
	for _, date := range []string{in.After, in.Before} {
		if date != "" {
			if _, err := time.Parse(time.RFC3339, date); err != nil {
				return toolFailure(call, "Dates must be RFC3339 timestamps."), nil
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	args := s.historyArgs(t)
	query := visibleHistory
	from := "visible m"
	where := `WHERE strpos(lower(m.body),lower($5))>0 AND ($6='' OR COALESCE(m.author_id::text,'ai:' || (m.ai_author->>'id'))=$6 OR lower(u.username)=lower($6) OR lower(m.ai_author->>'name')=lower($6))
 AND ($7::timestamptz IS NULL OR m.created_at>=$7) AND ($8::timestamptz IS NULL OR m.created_at<$8)
 AND (NOT $9 OR cardinality(media.ids)>0) ORDER BY m.seq DESC LIMIT $10`
	if in.Reply != "" {
		query += `, chain(id,depth) AS (SELECT id,1 FROM visible WHERE id=$5 UNION ALL SELECT parent.id,c.depth+1 FROM chain c JOIN visible cur ON cur.id=c.id JOIN visible parent ON parent.id=cur.reply_to_id AND parent.seq<cur.seq WHERE c.depth<$6) `
		from += " JOIN chain c ON c.id=m.id"
		where = " ORDER BY c.depth"
		args = append(args, in.Reply, limit+1)
	} else {
		args = append(args, in.Query, strings.TrimPrefix(in.Author, "@"), nullable(in.After), nullable(in.Before), in.Media, limit+1)
	}
	query += ` SELECT m.id,COALESCE(m.author_id::text,'ai:' || (m.ai_author->>'id')),coalesce(u.username,''),COALESCE(NULLIF(u.display_name,''),u.username,m.ai_author->>'name',''),left(m.body,600),m.created_at,COALESCE(r.id::text,''),media.ids FROM ` + from + ` LEFT JOIN chatxo.users u ON u.id=m.author_id LEFT JOIN visible r ON r.id=m.reply_to_id
 CROSS JOIN LATERAL (SELECT ARRAY(SELECT a.id::text FROM chatxo.attachments a WHERE a.message_id=m.id AND ` + availableAttachment + ` ORDER BY array_position(m.attachment_ids,a.id)) AS ids) media ` + where
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return toolFailure(call, "Channel history search unavailable."), err
	}
	hits, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (historyHit, error) {
		var h historyHit
		err := row.Scan(&h.ID, &h.AuthorID, &h.Username, &h.Name, &h.Excerpt, &h.Sent, &h.Reply, &h.Attachments)
		if s.options.PublicURL != "" {
			q := url.Values{"server": {s.options.PublicURL}, "channel": {t.ChannelID}, "message": {h.ID}}
			h.Link = "haunt://open?" + q.Encode()
		}
		return h, err
	})
	if err != nil {
		return toolFailure(call, "Could not read channel history."), err
	}
	return jsonResult(call, map[string]any{"messages": hits[:min(len(hits), limit)], "more": len(hits) > limit, "scope": "retained AI-visible channel history"})
}

func (s *Service) mediaIDs(ctx context.Context, t claimedTurn, msg, attachment string) ([]string, error) {
	rows, err := s.pool.Query(ctx, visibleHistory+`SELECT a.id::text FROM visible m JOIN chatxo.attachments a ON a.message_id=m.id AND a.conversation_id=m.conversation_id WHERE `+availableAttachment+` AND ($5::uuid IS NULL OR m.id=$5) AND ($6::uuid IS NULL OR a.id=$6) ORDER BY array_position(m.attachment_ids,a.id)`, append(s.historyArgs(t), nullable(msg), nullable(attachment))...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
func invalidImage() error {
	return errors.New("image is unavailable in retained AI-visible channel history")
}
