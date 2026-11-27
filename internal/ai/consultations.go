package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
)

type consultationRun struct {
	results map[string]provider.ToolResult
	targets map[string]bool
}

func consultTool() provider.Tool {
	return provider.Tool{Name: "consult_personality", Description: "Ask another personality in this channel one focused question when their perspective materially helps the reply. Choose distinct consultants, one at a time. Their advice appears in a card in your message. You remain responsible for answering the user.", Parameters: json.RawMessage(`{"type":"object","properties":{"preset_id":{"type":"string"},"question":{"type":"string","maxLength":1000}},"required":["preset_id","question"],"additionalProperties":false}`)}
}

func (s *Service) consult(ctx context.Context, t claimedTurn, call provider.ToolCall, adapter provider.Adapter, input provider.Request, run *consultationRun, notes []string, recent []provider.ToolResult, draft string, activity *runActivity) (provider.ToolResult, error) {
	if result, ok := run.results[call.ID]; ok {
		return result, nil
	}
	var args struct {
		PresetID string `json:"preset_id"`
		Question string `json:"question"`
	}
	if call.ID == "" || len(call.ID) > 256 || strings.ContainsRune(call.ID, 0) || json.Unmarshal(call.Arguments, &args) != nil || !identity.ValidID(args.PresetID) {
		return toolFailure(call, "Choose a personality from this channel and supply a focused question."), nil
	}
	args.Question = strings.TrimSpace(args.Question)
	if args.Question == "" || utf8.RuneCountInString(args.Question) > 1000 || strings.ContainsRune(args.Question, 0) {
		return toolFailure(call, "Use a question of 1 to 1000 characters."), nil
	}
	if args.PresetID == t.snapshot.Author.ID || run.targets[args.PresetID] {
		return toolFailure(call, "Choose a different personality that has not already been consulted in this reply."), nil
	}
	var target Preset
	var maximum int
	err := s.pool.QueryRow(ctx, `SELECT p.id,p.name,p.instructions,s.max_consultations FROM chatxo.ai_channel_settings s JOIN chatxo.ai_presets p ON p.channel_id=s.channel_id WHERE s.channel_id=$1 AND s.allow_consultations AND p.id=$2 AND EXISTS (SELECT 1 FROM chatxo.ai_presets caller WHERE caller.channel_id=s.channel_id AND caller.id<>p.id)`, t.ChannelID, args.PresetID).Scan(&target.ID, &target.Name, &target.Instructions, &maximum)
	if errors.Is(err, pgx.ErrNoRows) {
		return toolFailure(call, "Consultations are unavailable for this personality."), nil
	}
	if err != nil {
		return provider.ToolResult{}, err
	}
	if len(run.targets) >= maximum {
		return toolFailure(call, "The channel consultation limit has been reached for this reply."), nil
	}
	run.targets[target.ID] = true
	card := chat.Consultation{CallID: call.ID, PresetID: target.ID, Status: "waiting", Question: args.Question}
	if err = s.checkpointCard(ctx, t, draft, &card); err != nil {
		if errors.Is(err, ErrStorage) || errors.Is(err, chat.ErrAIStorageFull) {
			result := toolFailure(call, "There is not enough channel storage for this consultation.")
			run.results[call.ID] = result
			return result, nil
		}
		return provider.ToolResult{}, err
	}
	activity.set("consult_personality", target.Name, true)
	input.Instructions = t.snapshot.Instructions + "\n" + target.Instructions
	runtime, _, _ := strings.Cut(input.StructuralInstruction, "\n\n")
	input.StructuralInstruction = runtime + "\n\n" + fmt.Sprintf("You are %s (id %s), advising %s in one consultation. The final question comes from that assistant. Give a concise answer to the question. The exchange will appear inside the caller's reply. Historical <haunt_assistant> labels are added automatically; never reproduce them.", target.Name, target.ID, t.snapshot.Author.Name)
	input.SessionID = identity.NewID()
	input.RequestID = identity.NewID()
	input.Tools = nil
	input.Replay = nil
	input.ToolResults = nil
	input.MaxOutputTokens = min(input.MaxOutputTokens, 800)
	var evidence strings.Builder
	for _, note := range notes {
		fmt.Fprintln(&evidence, note)
	}
	for _, r := range recent {
		fmt.Fprintf(&evidence, "%s: %s\n", r.Name, shortText(r.Content, 768))
	}
	prompt := fmt.Sprintf("Question from %s: %s", t.snapshot.Author.Name, args.Question)
	if evidence.Len() > 0 {
		prompt += "\nTool evidence from the caller:\n" + evidence.String()
	}
	if draft != "" {
		prompt += "\nCaller’s reply so far:\n" + shortText(draft, 2000)
	}
	// Each adapter encodes ToolMedia as a separate internal input after the attributed conversation.
	input.ToolMedia = append([]provider.Part{{Type: "text", Text: prompt}}, input.ToolMedia...)
	child, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var reply strings.Builder
	full := errors.New("consultation output limit reached")
	response, err := adapter.Generate(child, input, func(event provider.Event) error {
		if event.Type == "text" || event.Type == "text_delta" {
			if reply.Len()+len(event.Text) > 16000 {
				reply.WriteString(shortText(event.Text, 4000))
				return full
			}
			reply.WriteString(event.Text)
		}
		activity.set("consult_personality", target.Name, true)
		return nil
	})
	if ctx.Err() != nil {
		return provider.ToolResult{}, ctx.Err()
	}
	answer := response.Text
	if answer == "" {
		answer = reply.String()
	}
	answer = strings.TrimSpace(strings.ReplaceAll(answer, "\x00", ""))
	chars := []rune(answer)
	if len(chars) > 4000 {
		answer = string(chars[:4000])
	}
	result := provider.ToolResult{ID: call.ID, Name: call.Name, Content: target.Name + ": " + answer}
	if (err != nil && !errors.Is(err, full)) || answer == "" || len(response.Calls) > 0 {
		card.Status = "unavailable"
		result = toolFailure(call, "The consultant could not answer. Continue with the available evidence.")
	} else {
		card.Status = "answered"
		card.Answer = answer
	}
	if err = s.checkpointCard(ctx, t, draft, &card); err != nil {
		return provider.ToolResult{}, err
	}
	run.results[call.ID] = result
	return result, nil
}

func (s *Service) finalizeMessages(ctx context.Context, tx pgx.Tx, t Turn) error {
	var request string
	if err := tx.QueryRow(ctx, `SELECT response_request_id FROM chatxo.ai_turns WHERE id=$1`, t.ID).Scan(&request); err != nil {
		return err
	}
	ids, err := s.chat.FinalizeAI(ctx, tx, t.ChannelID, request)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = supersedeEvents(ctx, tx, id, "message.updated"); err != nil {
			return err
		}
	}
	return nil
}
