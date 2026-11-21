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
	params := json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","minLength":1,"maxLength":32000}},"required":["prompt"],"additionalProperties":false}`)
	return []provider.Tool{
		{Name: "image_generate", Description: "Generate an image from a prompt and post it to this channel.", Parameters: params},
		{Name: "image_edit", Description: "Edit images attached to the requesting message or its direct reply target, and post the result.", Parameters: params},
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
		Prompt string `json:"prompt"`
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
	var sources []provider.Part
	if call.Name == "image_edit" {
		tools.ReportActivity(ctx, call.Name, "Reading source images")
		sources, err = s.editImageInputs(ctx, p, t)
		if err != nil {
			return "", err
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
		m, err := s.chat.WriteAI(ctx, tx, t.ChannelID, generation, "", t.snapshot.Author, att.ID)
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

func (s *Service) editImageInputs(ctx context.Context, p identity.Principal, t claimedTurn) ([]provider.Part, error) {
	var ids []string
	err := s.pool.QueryRow(ctx, `SELECT m.attachment_ids || COALESCE(r.attachment_ids,'{}'::uuid[]) FROM chatxo.messages m LEFT JOIN chatxo.messages r ON r.id=m.reply_to_id AND r.conversation_id=m.conversation_id AND NOT r.deleted WHERE m.id=$1 AND m.conversation_id=$2 AND NOT m.deleted`, t.TriggerMessageID, t.ChannelID).Scan(&ids)
	if err != nil {
		return nil, err
	}
	var out []provider.Part
	remaining := int64(23 << 20)
	for _, id := range ids {
		meta, file, err := s.attachments.Get(ctx, p, id)
		if err != nil {
			return nil, err
		}
		if meta.ConversationID != t.ChannelID {
			file.Close()
			return nil, identity.ErrForbidden
		}
		if !strings.HasPrefix(meta.ContentType, "image/") {
			file.Close()
			continue
		}
		if meta.Bytes > remaining {
			file.Close()
			return nil, errors.New("source images exceed the Codex input limit")
		}
		data, err := io.ReadAll(io.LimitReader(file, remaining+1))
		closeErr := file.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		remaining -= int64(len(data))
		if remaining < 0 {
			return nil, errors.New("source images exceed the Codex input limit")
		}
		out = append(out, provider.Part{Type: "image", MIMEType: meta.ContentType, Data: data})
	}
	if len(out) == 0 {
		return nil, errors.New("Attach an image or reply directly to an image message to edit it.")
	}
	return out, nil
}
