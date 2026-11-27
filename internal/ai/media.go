package ai

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
)

type imageSource struct {
	Kind   string `json:"kind"`
	ID     string `json:"id,omitempty"`
	Digest string `json:"sha256"`
}
type imageOperation struct {
	ID         string        `json:"operation_id"`
	Kind       string        `json:"operation"`
	Target     *imageSource  `json:"target,omitempty"`
	References []imageSource `json:"references"`
	Parent     string        `json:"parent_id,omitempty"`
	Digest     string        `json:"output_sha256"`
}
type provenance struct {
	imageOperation
	DraftSteps []imageOperation `json:"private_parent_operations,omitempty"`
}
type EditSpecInput struct {
	Instructions string `json:"instructions"`
}
type EditSpec struct {
	Instructions string     `json:"instructions"`
	EditorID     string     `json:"editor_id,omitempty"`
	UpdatedAt    *time.Time `json:"updated_at,omitempty"`
	CanEdit      bool       `json:"can_edit"`
}

func (s *Service) ImageSpec(ctx context.Context, p identity.Principal, channel, id string, in *EditSpecInput) (EditSpec, error) {
	var out EditSpec
	if !identity.ValidID(id) || !identity.ValidID(channel) || in != nil && (len(in.Instructions) > 4096 || !validInstructions(in.Instructions)) {
		return out, identity.ErrInvalid
	}
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		ch, err := s.ids.ChannelForTransaction(ctx, tx, p, channel)
		if err != nil {
			return err
		}
		if ch.Kind != "ai" || ch.Permissions&identity.ViewChannel == 0 {
			return identity.ErrForbidden
		}
		var owner, message string
		err = tx.QueryRow(ctx, `SELECT m.id,coalesce(m.author_id::text,''),coalesce(d.spec,''),coalesce(d.editor_id::text,''),d.edited_at FROM chatxo.attachments a JOIN chatxo.messages m ON m.id=a.message_id LEFT JOIN chatxo.ai_media d ON d.attachment_id=a.id WHERE a.id=$1 AND m.conversation_id=$2 AND m.ai_visible AND NOT m.deleted AND a.content_type LIKE 'image/%' AND `+availableAttachment+` AND chatxo.message_retained(m.conversation_id,m.created_at,$3,0)`, id, channel, s.chat.CurrentPolicy().TextRetentionDays).Scan(&message, &owner, &out.Instructions, &out.EditorID, &out.UpdatedAt)
		if err != nil {
			return translate(err)
		}
		out.CanEdit = ch.Permissions&identity.ManageChannels != 0 || ch.Permissions&(identity.SendMessages|identity.UseAI) == identity.SendMessages|identity.UseAI && (owner == "" || owner == p.User.ID)
		if in == nil {
			return nil
		}
		if !out.CanEdit {
			return identity.ErrForbidden
		}
		out.Instructions = strings.TrimSpace(in.Instructions)
		if out.Instructions == "" {
			_, err = tx.Exec(ctx, `UPDATE chatxo.ai_media SET spec='',editor_id=NULL,edited_at=NULL WHERE attachment_id=$1`, id)
			out.EditorID = ""
			out.UpdatedAt = nil
			return err
		}
		out.EditorID = p.User.ID
		err = tx.QueryRow(ctx, `INSERT INTO chatxo.ai_media(attachment_id,message_id,spec,editor_id,edited_at) VALUES($1,$2,$3,$4,now()) ON CONFLICT(attachment_id) DO UPDATE SET spec=excluded.spec,editor_id=excluded.editor_id,edited_at=excluded.edited_at RETURNING edited_at`, id, message, out.Instructions, p.User.ID).Scan(&out.UpdatedAt)
		if err != nil {
			return err
		}
		return s.enforce(ctx, tx, channel)
	})
	return out, err
}

func (s *Service) resolveMedia(ctx context.Context, t claimedTurn, call provider.ToolCall) (provider.ToolResult, error) {
	var in struct {
		Attachment string `json:"attachment_id"`
		Message    string `json:"message_id"`
	}
	if !arguments(call, &in) || (in.Attachment == "") == (in.Message == "") || in.Attachment != "" && !identity.ValidID(in.Attachment) || in.Message != "" && !identity.ValidID(in.Message) {
		return toolFailure(call, "Specify one attachment_id or message_id."), nil
	}
	ids, err := s.mediaIDs(ctx, t, in.Message, in.Attachment)
	if err != nil {
		return toolFailure(call, "Media lookup failed."), err
	}
	if len(ids) == 0 {
		return toolFailure(call, "No retained, visible attachment found."), nil
	}
	results := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		var msg, kind, name, spec, editor, author string
		var size int64
		var raw json.RawMessage
		var children []string
		err = s.pool.QueryRow(ctx, visibleHistory+`SELECT m.id,coalesce(m.author_id::text,'ai:'||(m.ai_author->>'id')),a.content_type,a.filename,a.bytes,d.provenance,coalesce(d.spec,''),coalesce(d.editor_id::text,''),
  ARRAY(SELECT child.id::text FROM chatxo.ai_media c JOIN visible cm ON cm.id=c.message_id JOIN chatxo.attachments child ON child.id=c.attachment_id WHERE c.provenance->>'parent_id'=a.id::text AND child.status='ready' AND child.claimed AND (child.expires_at IS NULL OR child.expires_at>now()) ORDER BY cm.seq LIMIT 64)
  FROM visible m JOIN chatxo.attachments a ON a.message_id=m.id LEFT JOIN chatxo.ai_media d ON d.attachment_id=a.id WHERE a.id=$5 AND `+availableAttachment, append(s.historyArgs(t), id)...).Scan(&msg, &author, &kind, &name, &size, &raw, &spec, &editor, &children)
		if err != nil {
			return toolFailure(call, "Media is unavailable."), err
		}
		var record any = "unknown"
		if len(raw) > 0 {
			var pr provenance
			if err = json.Unmarshal(raw, &pr); err != nil {
				return toolFailure(call, "Invalid provenance."), err
			}
			if err = s.filterSources(ctx, t, &pr); err != nil {
				return toolFailure(call, "Could not verify source visibility."), err
			}
			record = pr
		}
		results = append(results, map[string]any{"attachment_id": id, "message_id": msg, "author_id": author, "content_type": kind, "filename": name, "bytes": size, "provenance": record, "child_ids": children, "user_edit_instructions": spec, "instructions_author_id": editor})
	}
	return jsonResult(call, results)
}

// Validate all published input references in one snapshot. Uploaded sources may
// be deleted, hidden or expire independently of their retained derivatives.
func (pr *provenance) sources() (refs []*imageSource, parents []*string, ids []string) {
	add := func(op *imageOperation) {
		if op.Parent != "" {
			parents = append(parents, &op.Parent)
			ids = append(ids, op.Parent)
		}
		ref := func(r *imageSource) {
			if r != nil && r.Kind == "attachment" {
				refs = append(refs, r)
				ids = append(ids, r.ID)
			}
		}
		ref(op.Target)
		for i := range op.References {
			ref(&op.References[i])
		}
	}
	add(&pr.imageOperation)
	for i := range pr.DraftSteps {
		add(&pr.DraftSteps[i])
	}
	return
}

type mediaQuery interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (s *Service) availableSources(ctx context.Context, q mediaQuery, t claimedTurn, ids []string) (map[string]bool, error) {
	available := map[string]bool{}
	if len(ids) == 0 {
		return available, nil
	}
	rows, err := q.Query(ctx, visibleHistory+`SELECT a.id::text FROM visible m JOIN chatxo.attachments a ON a.message_id=m.id WHERE a.id=ANY($5::uuid[]) AND `+availableAttachment, append(s.historyArgs(t), ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		available[id] = true
	}
	return available, rows.Err()
}
func (s *Service) filterSources(ctx context.Context, t claimedTurn, pr *provenance) error {
	refs, parents, ids := pr.sources()
	available, err := s.availableSources(ctx, s.pool, t, ids)
	if err != nil {
		return err
	}
	for _, src := range refs {
		if !available[src.ID] {
			*src = imageSource{Kind: "unavailable"}
		}
	}
	for _, parent := range parents {
		if !available[*parent] {
			*parent = ""
		}
	}
	return nil
}
