package ai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/ai/tools"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
)

func imageToolSchemas() []provider.Tool {
	params := json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","minLength":1,"maxLength":32000},"reference_user_ids":{"type":"array","maxItems":10,"items":{"type":"string"},"description":"Optional avatar_user_id values returned by view_user_profile during this response. Include only pictures requested as references."}},"required":["prompt"],"additionalProperties":false}`)
	return []provider.Tool{
		{Name: "image_generate", Description: "Generate an image from a prompt, optionally using selected profile pictures as visual references, and post it to this channel.", Parameters: params},
		{Name: "image_edit", Description: "Edit images attached to the requesting messages, their direct reply targets, or selected profile pictures, and post the result.", Parameters: params},
	}
}

func (s *Service) runImageTool(ctx context.Context, p identity.Principal, t claimedTurn, call provider.ToolCall) (provider.ToolResult, error) {
	result := provider.ToolResult{ID: call.ID, Name: call.Name}
	id, err := s.publishToolImage(ctx, p, t, call)
	if err != nil {
		result.IsError = true
		result.Content = err.Error()
		return result, err
	}
	result.Content = "Image posted to this channel in message " + id + "."
	return result, nil
}

func (s *Service) publishToolImage(ctx context.Context, p identity.Principal, t claimedTurn, call provider.ToolCall) (string, error) {
	if t.snapshot.Provider != string(provider.Codex) || s.attachments == nil {
		return "", errors.New("Codex image tool is unavailable")
	}
	var input struct {
		Prompt         string   `json:"prompt"`
		ReferenceUsers []string `json:"reference_user_ids"`
	}
	dec := json.NewDecoder(bytes.NewReader(call.Arguments))
	dec.DisallowUnknownFields()
	if dec.Decode(&input) != nil || dec.Decode(&struct{}{}) != io.EOF || strings.TrimSpace(input.Prompt) == "" || len(input.Prompt) > 32000 {
		return "", errors.New("invalid image prompt")
	}
	digest := sha256.Sum256([]byte(t.responseRequest + "\x00" + call.ID))
	generation := fmt.Sprintf("%x-%x-%x-%x-%x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
	var msgID string
	err := s.pool.QueryRow(ctx, `SELECT id FROM chatxo.messages WHERE conversation_id=$1 AND request_id=$2 AND ai_author IS NOT NULL`, t.ChannelID, generation).Scan(&msgID)
	if err == nil {
		return msgID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	adapter, cred, err := s.credentials(ctx, t.snapshot.AccountID)
	if err != nil {
		return "", err
	}
	imageProvider, ok := adapter.(interface {
		Image(context.Context, provider.Credentials, string, []provider.Part, int64) ([]byte, error)
	})
	if !ok || adapter.ID() != provider.Codex {
		return "", errors.New("selected account does not support Codex images")
	}
	sources, err := t.profilePictures.references(input.ReferenceUsers)
	if err != nil {
		return "", err
	}
	remaining := int64(23 << 20)
	for _, source := range sources {
		remaining -= int64(len(source.Data))
	}
	if remaining < 0 {
		return "", errors.New("Selected profile pictures exceed the Codex image input limit.")
	}
	if call.Name == "image_edit" {
		tools.ReportActivity(ctx, call.Name, "Reading source images")
		attachments, err := s.editImageInputs(ctx, p, t, maxContextImages-len(sources), remaining)
		if err != nil {
			return "", err
		}
		sources = append(attachments, sources...)
		if len(sources) == 0 {
			return "", errors.New("Attach an image, reply to an image, or select a profile picture to edit.")
		}
	} else if call.Name != "image_generate" {
		return "", errors.New("unknown image tool")
	}
	tools.ReportActivity(ctx, call.Name, "Waiting for Codex")
	data, err := imageProvider.Image(ctx, cred, input.Prompt, sources, s.chat.CurrentPolicy().ImageMaxBytes)
	if err != nil {
		return "", err
	}
	if _, err = s.checkExecution(ctx, t); err != nil {
		return "", err
	}
	ext, ok := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif"}[http.DetectContentType(data)]
	if !ok {
		return "", errors.New("Codex returned an unsupported image format")
	}
	tools.ReportActivity(ctx, call.Name, "Saving image to the channel")
	att, err := s.attachments.UploadGenerated(ctx, p, t.ChannelID, generation, "image."+ext, data)
	if err != nil {
		return "", err
	}
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		var running bool
		if err := tx.QueryRow(ctx, `SELECT status='running' AND history_epoch=$3 AND attempt=$4 AND response_request_id=$5 FROM chatxo.ai_turns WHERE id=$1 AND channel_id=$2 FOR UPDATE`, t.ID, t.ChannelID, t.epoch, t.attempt, t.responseRequest).Scan(&running); err != nil {
			return err
		}
		if !running {
			return context.Canceled
		}
		m, err := s.chat.WriteAI(ctx, tx, t.ChannelID, generation, t.responseRequest, "", t.snapshot.Author, att.ID)
		if err != nil {
			return err
		}
		msgID = m.ID
		return s.enforceProtected(ctx, tx, t.ChannelID, t.ID)
	})
	if err != nil {
		s.attachments.DiscardGenerated(ctx, att.ID, generation)
	}
	return msgID, err
}

func (s *Service) editImageInputs(ctx context.Context, p identity.Principal, t claimedTurn, maxCount int, maxBytes int64) ([]provider.Part, error) {
	maxCount = min(maxCount, maxContextImages)
	if maxCount <= 0 || maxBytes <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT m.attachment_ids || COALESCE(r.attachment_ids,'{}'::uuid[])
		FROM chatxo.messages m LEFT JOIN chatxo.messages r ON r.id=m.reply_to_id AND r.conversation_id=m.conversation_id AND NOT r.deleted
		WHERE m.id=ANY($1::uuid[]) AND m.conversation_id=$2 AND NOT m.deleted ORDER BY m.seq DESC`, t.inputIDs(), t.ChannelID)
	if err != nil {
		return nil, err
	}
	groups, err := pgx.CollectRows(rows, pgx.RowTo[[]string])
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var sources [][]provider.Part
	for _, ids := range groups {
		var parts []provider.Part
		for _, id := range ids {
			if len(seen) >= maxCount {
				break
			}
			if seen[id] {
				continue
			}
			meta, file, err := s.attachments.Get(ctx, p, id)
			if err != nil {
				return nil, err
			}
			if meta.ConversationID != t.ChannelID {
				file.Close()
				return nil, identity.ErrForbidden
			}
			if !strings.HasPrefix(meta.ContentType, "image/") || meta.Bytes > maxBytes {
				file.Close()
				continue
			}
			data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
			closeErr := file.Close()
			if err != nil {
				return nil, err
			}
			if closeErr != nil {
				return nil, closeErr
			}
			if int64(len(data)) != meta.Bytes || int64(len(data)) > maxBytes {
				return nil, errors.New("source images exceed the Codex input limit")
			}
			maxBytes -= int64(len(data))
			seen[id] = true
			parts = append(parts, provider.Part{Type: "image", MIMEType: meta.ContentType, Data: data})
		}
		sources = append(sources, parts)
	}
	var out []provider.Part
	for i := len(sources) - 1; i >= 0; i-- {
		out = append(out, sources[i]...)
	}
	return out, nil
}
