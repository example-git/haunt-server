package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/ai/tools"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
)

func imageToolSchemas() []provider.Tool {
	params := json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","minLength":1,"maxLength":32000},"target_id":{"type":"string","description":"For image_edit, an exact attachment ID or a private draft ID from this turn."},"reference_ids":{"type":"array","maxItems":10,"items":{"type":"string"},"description":"Ordered supporting attachment IDs; use resolve_media to disambiguate."},"reference_user_ids":{"type":"array","maxItems":10,"items":{"type":"string"},"description":"Selected avatar_user_id values from view_user_profile in this turn."}},"required":["prompt"],"additionalProperties":false}`)
	draft := json.RawMessage(`{"type":"object","properties":{"draft_id":{"type":"string"}},"required":["draft_id"],"additionalProperties":false}`)
	return []provider.Tool{
		{Name: "image_generate", Description: "Generate a private Codex draft. Inspect the returned pixels next round. For a satisfactory result, call attach_image with its draft_id before telling the user it is attached. Use image_edit to revise.", Parameters: params},
		{Name: "image_edit", Description: "Edit one exact image. The target is first; references only guide the edit. Omit target_id only for an unambiguous current attachment, reply image or sole selected avatar. Inspect the private result, then attach_image or revise.", Parameters: params},
		{Name: "preview_image", Description: "See a private draft from this turn again, including after a preview delivery error.", Parameters: draft},
		{Name: "attach_image", Description: "Attach an inspected, accepted private draft to your current reply. Reuses the exact original file. Only call after seeing its preview in an earlier provider step.", Parameters: draft},
	}
}

type imageDraft struct {
	id, mime, attached, message string
	data                        []byte
	preview                     provider.Part
	provenance                  provenance
	previewRound                int
}

func digest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func draftID(t claimedTurn, call string) string {
	d := sha256.Sum256([]byte(t.responseRequest + "\x00" + call))
	return fmt.Sprintf("%x-%x-%x-%x-%x", d[:4], d[4:6], d[6:8], d[8:10], d[10:16])
}

type imageInput struct {
	Prompt     string   `json:"prompt"`
	Target     string   `json:"target_id"`
	References []string `json:"reference_ids"`
	Users      []string `json:"reference_user_ids"`
}
type imageInputs struct {
	parts     []provider.Part
	bytes     int
	operation provenance
	spec      string
}

func (in *imageInputs) add(part provider.Part, src imageSource, target bool) error {
	if len(in.parts) >= maxContextImages || len(part.Data)+in.bytes > 23<<20 {
		return errors.New("selected images exceed the Codex input budget")
	}
	src.Digest = digest(part.Data)
	if target {
		in.operation.Target = &src
		if src.Kind == "attachment" {
			in.operation.Parent = src.ID
		}
	} else {
		in.operation.References = append(in.operation.References, src)
	}
	in.bytes += len(part.Data)
	in.parts = append(in.parts, part)
	return nil
}
func (s *Service) attachmentInput(ctx context.Context, p identity.Principal, t claimedTurn, id string, remaining int) (provider.Part, string, error) {
	var part provider.Part
	var spec string
	if !identity.ValidID(id) {
		return part, "", invalidImage()
	}
	err := s.pool.QueryRow(ctx, visibleHistory+`SELECT coalesce(d.spec,'') FROM visible m JOIN chatxo.attachments a ON a.message_id=m.id LEFT JOIN chatxo.ai_media d ON d.attachment_id=a.id WHERE a.id=$5 AND `+availableAttachment, append(s.historyArgs(t), id)...).Scan(&spec)
	if err != nil {
		return part, "", invalidImage()
	}
	meta, file, err := s.attachments.Get(ctx, p, id)
	if err != nil {
		return part, "", err
	}
	defer file.Close()
	if meta.ConversationID != t.ChannelID || !strings.HasPrefix(meta.ContentType, "image/") || meta.Bytes > int64(remaining) {
		return part, "", errors.New("selected attachment is not a supported image within the input budget")
	}
	data, err := io.ReadAll(io.LimitReader(file, meta.Bytes+1))
	if err != nil || int64(len(data)) != meta.Bytes {
		return part, "", errors.New("could not read complete image")
	}
	return provider.Part{Type: "image", MIMEType: meta.ContentType, Data: data}, spec, nil
}

func (s *Service) defaultTargets(ctx context.Context, t claimedTurn) ([]string, error) {
	rows, err := s.pool.Query(ctx, visibleHistory+`SELECT DISTINCT a.id::text FROM visible m LEFT JOIN visible r ON r.id=m.reply_to_id JOIN chatxo.attachments a ON a.message_id IN (m.id,r.id) WHERE m.id=ANY($5::uuid[]) AND a.content_type LIKE 'image/%' AND `+availableAttachment+` LIMIT 2`, append(s.historyArgs(t), t.inputIDs())...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
func (s *Service) imageInputs(ctx context.Context, p identity.Principal, t claimedTurn, kind string, args imageInput) (imageInputs, error) {
	var in imageInputs
	in.operation.Kind = kind
	in.operation.References = []imageSource{}
	target := args.Target
	if kind == "image_generate" && target != "" {
		return in, errors.New("target_id is only valid for edits")
	}
	profiles, err := t.profilePictures.references(args.Users)
	if err != nil {
		return in, err
	}
	users := []string{}
	for _, id := range args.Users {
		if !slices.Contains(users, id) {
			users = append(users, id)
		}
	}
	if kind == "image_edit" && target == "" {
		ids, err := s.defaultTargets(ctx, t)
		if err != nil {
			return in, err
		}
		if len(ids) == 1 {
			target = ids[0]
		} else if len(ids) == 0 && len(users) == 1 {
			if err = in.add(profiles[0], imageSource{Kind: "profile", ID: users[0]}, true); err != nil {
				return in, err
			}
			users = nil
			profiles = nil
		} else {
			return in, errors.New("Choose exactly one edit target with target_id. Use resolve_media or search_conversation for attachment IDs.")
		}
	}
	if target != "" {
		if draft := t.drafts[target]; draft != nil {
			if len(draft.data) == 0 {
				return in, errors.New("private draft has expired")
			}
			if len(draft.provenance.DraftSteps) >= 24 {
				return in, errors.New("private edit ancestry limit reached")
			}
			in.operation.DraftSteps = append(slices.Clone(draft.provenance.DraftSteps), draft.provenance.imageOperation)
			in.operation.Parent = draft.provenance.Parent
			id, kind := draft.id, "private_draft"
			if draft.attached != "" {
				id, kind = draft.attached, "attachment"
				in.operation.DraftSteps = nil
			}
			err = in.add(provider.Part{Type: "image", MIMEType: draft.mime, Data: draft.data}, imageSource{Kind: kind, ID: id}, true)
		} else {
			var part provider.Part
			part, in.spec, err = s.attachmentInput(ctx, p, t, target, 23<<20)
			if err == nil {
				err = in.add(part, imageSource{Kind: "attachment", ID: target}, true)
			}
		}
		if err != nil {
			return in, err
		}
	}
	for i, id := range args.References {
		if id == target || slices.Contains(args.References[:i], id) {
			return in, errors.New("duplicate target or reference")
		}
		part, _, err := s.attachmentInput(ctx, p, t, id, 23<<20-in.bytes)
		if err != nil {
			return in, err
		}
		if err = in.add(part, imageSource{Kind: "attachment", ID: id}, false); err != nil {
			return in, err
		}
	}
	for i, id := range users {
		if err = in.add(profiles[i], imageSource{Kind: "profile", ID: id}, false); err != nil {
			return in, err
		}
	}
	return in, nil
}

func (s *Service) makeDraft(ctx context.Context, p identity.Principal, t *claimedTurn, call provider.ToolCall) (*imageDraft, error) {
	id := draftID(*t, call.ID)
	if d := t.drafts[id]; d != nil {
		return d, nil
	}
	var args imageInput
	if !arguments(call, &args) || strings.TrimSpace(args.Prompt) == "" || len(args.Prompt) > 32000 || len(args.References)+len(args.Users) > maxContextImages {
		return nil, errors.New("invalid image prompt or reference list")
	}
	adapter, cred, err := s.credentials(ctx, t.snapshot.AccountID)
	if err != nil {
		return nil, err
	}
	generator, ok := adapter.(interface {
		Image(context.Context, provider.Credentials, string, []provider.Part, int64) ([]byte, error)
	})
	if !ok || adapter.ID() != provider.Codex {
		return nil, errors.New("selected account does not support Codex images")
	}
	inputs, err := s.imageInputs(ctx, p, *t, call.Name, args)
	if err != nil {
		return nil, err
	}
	prompt := args.Prompt
	if inputs.operation.Target != nil && len(inputs.parts) > 1 {
		prompt += "\nThe first supplied image is the edit target. All subsequent images are visual references only."
	}
	if inputs.spec != "" {
		prompt += "\nUser-saved edit instructions for this particular target: " + inputs.spec
	}
	tools.ReportActivity(ctx, call.Name, "Waiting for Codex")
	data, err := generator.Image(ctx, cred, prompt, inputs.parts, s.chat.CurrentPolicy().ImageMaxBytes)
	if err != nil {
		return nil, err
	}
	if _, err = s.checkExecution(ctx, *t); err != nil {
		return nil, err
	}
	preview, err := tools.GeneratedPreview(data)
	if err != nil {
		return nil, fmt.Errorf("private draft could not be visually inspected: %w", err)
	}
	if t.drafts == nil {
		t.drafts = make(map[string]*imageDraft)
	}
	inputs.operation.ID = id
	inputs.operation.Digest = digest(data)
	d := &imageDraft{id: id, mime: http.DetectContentType(data), data: data, preview: preview, provenance: inputs.operation}
	t.drafts[id] = d
	t.draftOrder = append(t.draftOrder, id)
	t.draftBytes += len(data) + len(preview.Data)
	for len(t.draftOrder) > 10 || t.draftBytes > 128<<20 {
		old := t.drafts[t.draftOrder[0]]
		t.draftOrder = t.draftOrder[1:]
		t.draftBytes -= len(old.data) + len(old.preview.Data)
		old.data = nil
		old.preview = provider.Part{}
		old.provenance = provenance{}
	}
	return d, nil
}

func (s *Service) runImageTool(ctx context.Context, p identity.Principal, t *claimedTurn, call provider.ToolCall) (provider.ToolResult, error) {
	var d *imageDraft
	var err error
	if s.attachments == nil {
		return toolFailure(call, "Image storage is unavailable."), nil
	}
	if call.Name == "image_generate" || call.Name == "image_edit" {
		d, err = s.makeDraft(ctx, p, t, call)
	} else {
		var args struct {
			ID string `json:"draft_id"`
		}
		if arguments(call, &args) {
			d = t.drafts[args.ID]
		}
	}
	if err != nil {
		return toolFailure(call, err.Error()), err
	}
	if d == nil {
		return toolFailure(call, "Unknown private draft for this response."), nil
	}
	if call.Name == "attach_image" {
		if d.attached != "" {
			return jsonResult(call, map[string]string{"attached_id": d.attached, "message_id": d.message})
		}
		if d.previewRound == 0 || d.previewRound >= t.round {
			return toolFailure(call, "Inspect the private draft in a previous provider step before attaching it."), nil
		}
		if len(d.data) == 0 {
			return toolFailure(call, "The draft has expired."), nil
		}
		ext := map[string]string{"image/png": "png", "image/webp": "webp", "image/jpeg": "jpg"}[d.mime]
		att, err := s.attachments.UploadGenerated(ctx, p, t.ChannelID, d.id, "image."+ext, d.data)
		if err != nil {
			return toolFailure(call, err.Error()), err
		}
		pending := &acceptedImage{claim: chat.GeneratedAttachment{ID: att.ID, Generation: d.id}, provenance: d.provenance}
		err = s.checkpointImage(ctx, *t, t.replyText, nil, pending)
		if err != nil {
			s.attachments.DiscardGenerated(ctx, att.ID, d.id)
			return toolFailure(call, "Could not attach draft to this reply."), err
		}
		d.attached, d.message = att.ID, pending.message
		return jsonResult(call, map[string]string{"attached_id": att.ID, "message_id": pending.message})
	}
	if len(d.preview.Data) == 0 {
		return toolFailure(call, "This private draft has expired."), nil
	}
	result, _ := jsonResult(call, map[string]string{"draft_id": d.id, "status": "private preview", "sha256": d.provenance.Digest})
	result.Media = []provider.Part{d.preview}
	return result, nil
}

type acceptedImage struct {
	claim      chat.GeneratedAttachment
	provenance provenance
	message    string
}
