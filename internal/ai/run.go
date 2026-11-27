package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/ai/tools"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
)

type activityState struct {
	action   string
	detail   string
	progress time.Time
}

type runActivity struct {
	mu        sync.Mutex
	current   activityState
	sent      activityState
	published time.Time
}

func newRunActivity() *runActivity {
	return &runActivity{current: activityState{action: "provider", progress: time.Now()}}
}

func (a *runActivity) set(action, detail string, progress bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if progress || action != a.current.action || detail != a.current.detail {
		a.current.progress = time.Now()
	}
	a.current.action = action
	a.current.detail = detail
}

func (a *runActivity) take(now time.Time) (activityState, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.current
	if current.action == "provider" && now.Sub(current.progress) >= 2*time.Minute {
		current.action = "waiting_for_provider"
	}
	if now.Sub(a.published) < time.Second || current == a.sent {
		return current, false
	}
	if current.action == a.sent.action && current.detail == a.sent.detail && now.Sub(a.published) < 15*time.Second {
		return current, false
	}
	a.published, a.sent = now, current
	return current, true
}

func (s *Service) publishActivity(ctx context.Context, t claimedTurn, state activityState) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `UPDATE chatxo.ai_turns SET action=$2,detail=$3,last_progress_at=$4,revision=revision+1,updated_at=now() WHERE id=$1 AND status='running' AND attempt=$5 AND response_request_id=$6 AND history_epoch=$7`, t.ID, state.action, state.detail, state.progress, t.attempt, t.responseRequest, t.epoch)
		if err != nil || result.RowsAffected() == 0 {
			return err
		}
		return s.emitTurn(ctx, tx, t.ID)
	})
}

func supersedeEvents(ctx context.Context, tx pgx.Tx, messageID, kind string) error {
	_, err := tx.Exec(ctx, `WITH matching AS (
		SELECT id FROM chatxo.events WHERE message_id=$1 AND convert_from(payload,'UTF8')::jsonb->>'type'=$2 ORDER BY id DESC OFFSET 1
	), removed AS (
		DELETE FROM chatxo.events e USING matching WHERE e.id=matching.id RETURNING e.bytes
	) UPDATE chatxo.storage SET text_bytes=text_bytes-(SELECT sum(bytes) FROM removed) WHERE singleton AND EXISTS(SELECT 1 FROM removed)`, messageID, kind)
	return err
}

func toolDetail(call provider.ToolCall) string {
	var args struct {
		URL   string `json:"url"`
		Query string `json:"query"`
		User  string `json:"user"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	detail := call.Name
	if call.Name == "read_url" {
		if endpoint, err := url.Parse(args.URL); err == nil && endpoint.Hostname() != "" {
			detail = endpoint.Hostname()
		}
	}
	if call.Name == "web_search" && args.Query != "" {
		detail = args.Query
	}
	if call.Name == "view_user_profile" && args.User != "" {
		detail = args.User
	}
	return shortText(detail, 128)
}

func shortText(value string, n int) string {
	chars := []rune(strings.Join(strings.Fields(value), " "))
	if len(chars) > n {
		chars = chars[:n]
	}
	return string(chars)
}

func (s *Service) dispatchTool(ctx context.Context, p identity.Principal, t *claimedTurn, call provider.ToolCall, a *runActivity) (provider.ToolResult, error) {
	detail := toolDetail(call)
	action := "tool_running"
	switch call.Name {
	case "attach_image", "preview_image", "search_conversation", "resolve_media", "clock", "image_generate", "image_edit", "switch_personality", "web_search", "read_url", "view_user_profile":
		action = call.Name
	}
	a.set(action, detail, true)
	ctx = tools.WithActivity(ctx, func(state, description string) {
		if state == "tool_running" {
			state = action
		}
		if description == "" {
			description = detail
		}
		a.set(state, shortText(description, 128), false)
	})
	switch call.Name {
	case "search_conversation":
		return s.searchConversation(ctx, *t, call)
	case "resolve_media":
		return s.resolveMedia(ctx, *t, call)
	case "clock":
		return clockTool(call)
	case "view_user_profile":
		return s.viewUserProfile(ctx, t, call)
	case "image_generate", "image_edit", "attach_image", "preview_image":
		if t.snapshot.Provider == string(provider.Codex) {
			return s.runImageTool(ctx, p, t, call)
		}
		return toolFailure(call, "Image tools require the Codex provider."), nil
	case "switch_personality":
		var args struct {
			PresetID string `json:"preset_id"`
		}
		if json.Unmarshal(call.Arguments, &args) != nil || !identity.ValidID(args.PresetID) {
			return toolFailure(call, "Choose a personality ID from this channel’s roster."), nil
		}
		return s.requestSwitch(ctx, t, args.PresetID, call)
	default:
		return s.tools.Execute(tools.WithPageMemory(tools.WithChannel(ctx, t.ChannelID), &t.pages), call)
	}
}

func toolFailure(call provider.ToolCall, content string) provider.ToolResult {
	return provider.ToolResult{ID: call.ID, Name: call.Name, Content: content, IsError: true}
}

type switchTarget struct {
	author       chat.AIAuthor
	instructions string
	lastHuman    int64
	immediate    bool
}

func loadSwitchTarget(ctx context.Context, tx pgx.Tx, channel, preset string) (switchTarget, error) {
	var target switchTarget
	err := tx.QueryRow(ctx, `SELECT p.id,p.name,p.avatar_url,p.instructions,s.last_human_selection_seq,s.switch_during_reply
  FROM chatxo.ai_channel_settings s JOIN chatxo.ai_presets p ON p.channel_id=s.channel_id
  WHERE s.channel_id=$1 AND s.allow_agent_switch AND p.id=$2 FOR UPDATE OF s,p`, channel, preset).
		Scan(&target.author.ID, &target.author.Name, &target.author.AvatarURL, &target.instructions, &target.lastHuman, &target.immediate)
	return target, err
}

func (s *Service) requestSwitch(ctx context.Context, t *claimedTurn, preset string, call provider.ToolCall) (provider.ToolResult, error) {
	var target switchTarget
	next := t.snapshot
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT t.status='running' AND t.history_epoch=$2 AND s.history_epoch=$2
   FROM chatxo.ai_turns t JOIN chatxo.ai_channel_settings s ON t.channel_id=s.channel_id
   WHERE t.id=$1 AND t.attempt=$3 AND t.response_request_id=$4 FOR UPDATE OF t,s`, t.ID, t.epoch, t.attempt, t.responseRequest).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrHistoryChanged
		}
		var err error
		target, err = loadSwitchTarget(ctx, tx, t.ChannelID, preset)
		if err != nil || !target.immediate || target.author.ID == t.snapshot.Author.ID {
			return err
		}
		next.Author, next.PresetInstructions = target.author, target.instructions
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE chatxo.ai_turns SET snapshot=$2,partial_text='',revision=revision+1 WHERE id=$1`, t.ID, raw); err != nil {
			return err
		}
		if err = s.applySwitch(ctx, tx, *t, target); err != nil {
			return err
		}
		if err = s.emitTurn(ctx, tx, t.ID); err != nil {
			return err
		}
		return s.enforceProtected(ctx, tx, t.ChannelID, t.ID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return toolFailure(call, "Personality switching is unavailable for that identity in this channel."), nil
	}
	if err != nil {
		return provider.ToolResult{}, err
	}
	result := provider.ToolResult{ID: call.ID, Name: call.Name}
	if target.immediate {
		t.snapshot = next
		t.pendingPreset = ""
		result.Content = "You are now " + target.author.Name + ". Continue the current user's request as this personality."
	} else {
		t.pendingPreset = preset
		result.Content = "Requested " + target.author.Name + " for subsequent turns after this reply completes."
	}
	return result, nil
}

func (s *Service) commitSwitch(ctx context.Context, tx pgx.Tx, t claimedTurn) error {
	target, err := loadSwitchTarget(ctx, tx, t.ChannelID, t.pendingPreset)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.applySwitch(ctx, tx, t, target)
}

func (s *Service) applySwitch(ctx context.Context, tx pgx.Tx, t claimedTurn, target switchTarget) error {
	raw, err := json.Marshal(target.author)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE chatxo.ai_turns SET snapshot=snapshot || jsonb_build_object('author',$3::jsonb,'preset_instructions',$4::text)
  WHERE channel_id=$1 AND status='queued' AND attempt=0 AND boundary_seq>$2 AND COALESCE((snapshot->>'selection_seq')::bigint,0)<=$2`, t.ChannelID, t.boundary, raw, target.instructions)
	if err != nil || target.lastHuman > t.boundary {
		return err
	}
	return s.selectPreset(ctx, tx, t.ChannelID, target.author.ID, nil)
}

func (s *Service) selectPreset(ctx context.Context, tx pgx.Tx, channel, preset string, humanSeq *int64) error {
	var revision int64
	if err := tx.QueryRow(ctx, `UPDATE chatxo.ai_channel_settings SET active_preset_id=$2,last_human_selection_seq=coalesce($3::bigint,last_human_selection_seq),revision=revision+1 WHERE channel_id=$1 RETURNING revision`, channel, preset, humanSeq).Scan(&revision); err != nil {
		return err
	}
	return s.chat.Emit(ctx, tx, channel, nil, "ai.settings.updated", map[string]int64{"revision": revision})
}

type runMemory struct {
	notes []string
}

func (m *runMemory) advance(req *provider.Request, response provider.Response) {
	req.Replay = response.Replay
	size := 0
	for _, item := range req.Replay {
		size += len(item)
	}

	notes := make([]string, 0, len(req.ToolResults))
	for i, tool := range req.ToolResults {
		source := tool.Name
		if i < len(response.Calls) {
			if detail := toolDetail(response.Calls[i]); detail != tool.Name {
				source += " " + detail
			}
		}
		notes = append(notes, source+": "+shortText(tool.Content, 768))
	}

	if (size >= 96<<10 || len(req.Replay) >= 128) && response.RoundStart > 0 && response.RoundStart <= len(req.Replay) && len(req.ToolResults) > 0 {
		req.Replay = append([]json.RawMessage(nil), req.Replay[response.RoundStart:]...)
		if len(m.notes) > 0 {
			content, _ := json.Marshal(struct {
				Current string   `json:"current_tool_result"`
				Earlier []string `json:"earlier_tool_result_excerpts"`
			}{req.ToolResults[0].Content, m.notes})
			req.ToolResults[0].Content = string(content)
		}
	}
	m.notes = append(m.notes, notes...)
	if len(m.notes) > 64 {
		m.notes = append([]string(nil), m.notes[len(m.notes)-64:]...)
	}
}
