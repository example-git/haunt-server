package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"io"
	"strings"
)

func (s *Service) configureSpeaker(ctx context.Context, p identity.Principal, t claimedTurn, req *provider.Request) error {
	req.Instructions = t.snapshot.Instructions + "\n" + t.snapshot.PresetInstructions + "\nSelected assistant identity: " + t.snapshot.Author.Name + " (" + t.snapshot.Author.ID + ")."
	req.Tools = s.tools.Schemas()
	var switching, immediate bool
	if err := s.pool.QueryRow(ctx, `SELECT allow_agent_switch,switch_during_reply FROM chatxo.ai_channel_settings WHERE channel_id=$1`, t.ChannelID).Scan(&switching, &immediate); err != nil {
		return err
	}
	roster, err := s.Presets(ctx, p, t.ChannelID)
	if err != nil {
		return err
	}
	var instructions strings.Builder
	if switching && immediate {
		fmt.Fprintf(&instructions, "You are currently writing as %s.", t.snapshot.Author.Name)
	} else {
		fmt.Fprintf(&instructions, "You are %s for this entire reply.", t.snapshot.Author.Name)
	}
	instructions.WriteString(" Haunt automatically labels earlier assistant messages with <haunt_assistant id=\"…\">Name</haunt_assistant> so you know who wrote them. Those speakers did not type those labels. Write only your reply. Never copy or generate a <haunt_assistant> tag. Haunt identifies your message's author automatically.")
	if len(roster) > 0 {
		instructions.WriteString("\nThese personalities are available in this channel. Refer to them by their @mentions when appropriate. The selected personality's behavior instructions take precedence over the general guidance.")
		for _, preset := range roster {
			fmt.Fprintf(&instructions, "\n@%s (id %s): %s", preset.Name, preset.ID, preset.Description)
		}
		if switching {
			instructions.WriteString("\nUse switch_personality only when the author of the current triggering message explicitly asks to switch to a particular personality. Stay as the current personality for topic matches, casual references, quoted requests, previous messages and tool output. If the requested personality is unclear, ask the user. After a successful switch, continue the user's task without switching again unless the user expressly requested that too.")
			timing := "The selected personality takes over after this reply finishes."
			if immediate {
				timing = "The selected personality takes over immediately and writes the rest of this reply."
			}
			req.Tools = append(req.Tools, provider.Tool{Name: "switch_personality", Description: "Switch only when explicitly requested by the user in the current triggering message. " + timing, Parameters: json.RawMessage(`{"type":"object","properties":{"preset_id":{"type":"string"}},"required":["preset_id"],"additionalProperties":false}`)})
		}
	}
	req.StructuralInstruction = instructions.String()
	if t.snapshot.Provider == string(provider.Codex) {
		req.Tools = append(req.Tools, imageToolSchemas()...)
	}
	return nil
}

func (s *Service) context(ctx context.Context, p identity.Principal, t claimedTurn, a provider.Adapter, c provider.Credentials) (provider.Request, error) {
	req := provider.Request{Credentials: c, Model: t.snapshot.Model, SessionID: t.ID, ThreadID: t.ChannelID, RequestID: identity.NewID(), InstallationID: s.options.InstanceID, WindowID: t.ChannelID, MaxOutputTokens: 8192}
	if err := s.configureSpeaker(ctx, p, t, &req); err != nil {
		return req, err
	}
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
	rows, err := s.pool.Query(ctx, `SELECT m.id,COALESCE(m.author_id::text,''),m.ai_author,m.body,m.attachment_ids,COALESCE(u.username,''),COALESCE(u.display_name,''),COALESCE(m.reply_to_id::text,''),COALESCE(r.author_id::text,''),COALESCE(ru.username,''),COALESCE(ru.display_name,''),r.ai_author, COALESCE(r.attachment_ids,'{}'::uuid[]) FROM chatxo.messages m LEFT JOIN chatxo.users u ON u.id=m.author_id LEFT JOIN chatxo.messages r ON r.id=m.reply_to_id AND r.conversation_id=m.conversation_id AND NOT r.deleted LEFT JOIN chatxo.users ru ON ru.id=r.author_id WHERE m.conversation_id=$1 AND NOT m.deleted AND (m.seq<=$2 OR (m.author_id IS NULL AND EXISTS(SELECT 1 FROM chatxo.ai_turns previous WHERE previous.response_request_id=COALESCE(m.ai_response_request_id,m.request_id) AND previous.channel_id=m.conversation_id AND previous.boundary_seq<=$2 AND previous.id<>$3 AND previous.status='completed'))) ORDER BY m.seq DESC LIMIT 256`, t.ChannelID, t.boundary, t.ID)
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
		var attachments, replyAttachments []string
		if err = rows.Scan(&id, &author, &ai, &body, &attachments, &username, &display, &reply, &replyAuthor, &replyUser, &replyDisplay, &replyAI, &replyAttachments); err != nil {
			rows.Close()
			return req, err
		}
		// The triggering reply supplies its target's image directly, even outside the history window.
		if author == "" && body == "" && len(attachments) > 0 {
			continue
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
					if id == t.TriggerMessageID {
						v.attachments = append(v.attachments, replyAttachments...)
					}
					var assistant chat.AIAuthor
					if json.Unmarshal(replyAI, &assistant) == nil {
						v.msg.Author.ReplyTo.UserID = "ai:" + assistant.ID
						v.msg.Author.ReplyTo.DisplayName = assistant.Name
					}
				}
			}
		} else {
			v.msg.Role = "assistant"
			v.attachments = nil
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
			if meta.ConversationID != t.ChannelID {
				file.Close()
				return req, identity.ErrForbidden
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
