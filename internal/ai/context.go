package ai

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"io"
	"strings"
)

func (s *Service) context(ctx context.Context, p identity.Principal, t claimedTurn, a provider.Adapter, c provider.Credentials) (provider.Request, error) {
	req := provider.Request{Credentials: c, Model: t.snapshot.Model, StructuralInstruction: provider.StructuralInstruction, Instructions: t.snapshot.Instructions + "\n" + t.snapshot.PresetInstructions + "\nSelected assistant identity: " + t.snapshot.Author.Name + " (" + t.snapshot.Author.ID + ").", Tools: s.tools.Schemas(), SessionID: t.ID, ThreadID: t.ChannelID, RequestID: identity.NewID(), InstallationID: s.options.InstanceID, WindowID: t.ChannelID, MaxOutputTokens: 8192}
	models, err := a.Models(ctx, c)
	if err != nil {
		return req, err
	}
	var selected *provider.Model
	for i := range models {
		if models[i].ID == req.Model {
			selected = &models[i]
			break
		}
	}
	if selected == nil {
		return req, &provider.Error{Kind: provider.InvalidRequest, Message: "selected model is unavailable"}
	}
	if selected.MaxOutputTokens > 0 {
		req.MaxOutputTokens = min(req.MaxOutputTokens, selected.MaxOutputTokens)
	}
	rows, err := s.pool.Query(ctx, `SELECT m.id,COALESCE(m.author_id::text,''),m.ai_author,m.body,m.attachment_ids,COALESCE(u.username,''),COALESCE(u.display_name,''),COALESCE(m.reply_to_id::text,''),COALESCE(r.author_id::text,''),COALESCE(ru.username,''),COALESCE(ru.display_name,''),r.ai_author FROM chatxo.messages m LEFT JOIN chatxo.users u ON u.id=m.author_id LEFT JOIN chatxo.messages r ON r.id=m.reply_to_id LEFT JOIN chatxo.users ru ON ru.id=r.author_id WHERE m.conversation_id=$1 AND NOT m.deleted AND (m.seq<=$2 OR (m.author_id IS NULL AND EXISTS(SELECT 1 FROM chatxo.ai_turns previous WHERE previous.response_request_id=m.request_id AND previous.boundary_seq<=$2 AND previous.id<>$3 AND previous.status='completed'))) ORDER BY m.seq DESC LIMIT 256`, t.ChannelID, t.boundary, t.ID)
	if err != nil {
		return req, err
	}
	type item struct {
		msg         provider.Message
		attachments []string
		size        int
	}
	var items []item
	size := 0
	contextLimit := 262144
	if selected.ContextWindow > 0 {
		contextLimit = min(contextLimit, max(16384, selected.ContextWindow*2))
	}
	for rows.Next() {
		var id, author, body, username, display, reply, replyAuthor, replyUser, replyDisplay string
		var ai, replyAI []byte
		var attachments []string
		if err = rows.Scan(&id, &author, &ai, &body, &attachments, &username, &display, &reply, &replyAuthor, &replyUser, &replyDisplay, &replyAI); err != nil {
			rows.Close()
			return req, err
		}
		if size+len(body) > contextLimit && len(items) > 0 {
			break
		}
		v := item{msg: provider.Message{Role: "user", Parts: []provider.Part{{Type: "text", Text: body}}}, attachments: attachments, size: len(body)}
		if author != "" {
			v.msg.Author = &provider.Attribution{UserID: author, Username: username, DisplayName: display, MessageID: id}
			if reply != "" {
				v.msg.Author.ReplyTo = &provider.Attribution{UserID: replyAuthor, Username: replyUser, DisplayName: replyDisplay, MessageID: reply}
				if replyAuthor == "" && len(replyAI) > 0 {
					var assistant chat.AIAuthor
					if json.Unmarshal(replyAI, &assistant) == nil {
						v.msg.Author.ReplyTo.UserID = "ai:" + assistant.ID
						v.msg.Author.ReplyTo.DisplayName = assistant.Name
					}
				}
			}
		} else {
			v.msg.Role = "assistant"
			var assistant chat.AIAuthor
			if err = json.Unmarshal(ai, &assistant); err != nil {
				rows.Close()
				return req, err
			}
			v.msg.Parts[0].Text = "<haunt_assistant id=\"" + assistant.ID + "\">" + assistant.Name + "</haunt_assistant>\n" + body
		}
		items = append(items, v)
		size += len(body)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return req, err
	}
	var imageBytes int64
	for i := len(items) - 1; i >= 0; i-- {
		v := items[i]
		for _, id := range v.attachments {
			if s.attachments == nil {
				return req, errors.New("attachment_reader_unavailable")
			}
			meta, file, e := s.attachments.Get(ctx, p, id)
			if e != nil {
				return req, e
			}
			if !strings.HasPrefix(meta.ContentType, "image/") {
				file.Close()
				return req, &provider.Error{Kind: provider.InvalidRequest, Message: "AI input requires a supported image attachment"}
			}
			if !selected.Vision {
				file.Close()
				return req, &provider.Error{Kind: provider.InvalidRequest, Message: "selected model does not support image input"}
			}
			if meta.Bytes > 8<<20 || imageBytes+meta.Bytes > 24<<20 {
				file.Close()
				return req, &provider.Error{Kind: provider.InvalidRequest, Message: "image input exceeds turn byte limit"}
			}
			data, e := io.ReadAll(io.LimitReader(file, (8<<20)+1))
			closeErr := file.Close()
			if e != nil {
				return req, e
			}
			if closeErr != nil {
				return req, closeErr
			}
			if len(data) > 8<<20 {
				return req, ErrStorage
			}
			imageBytes += int64(len(data))
			v.msg.Parts = append(v.msg.Parts, provider.Part{Type: "image", MIMEType: meta.ContentType, Data: data})
		}
		req.Messages = append(req.Messages, v.msg)
	}
	return req, provider.Validate(req)
}
