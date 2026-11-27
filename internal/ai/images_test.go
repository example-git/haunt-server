package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/identity"
)

type accountImageFixture struct {
	*testAdapter
	t                *testing.T
	inputCounts      []int
	output, revision []byte
}

func (a *accountImageFixture) ID() provider.ID { return provider.Codex }
func (a *accountImageFixture) Image(_ context.Context, c provider.Credentials, prompt string, images []provider.Part, _ int64) ([]byte, error) {
	if c.AccessToken != "selected-test-access" || c.AccountID != "selected-test-account" || prompt != "fixture prompt" {
		a.t.Fatal("image call used incorrect account or prompt")
	}
	a.inputCounts = append(a.inputCounts, len(images))
	if len(images) > 0 && !bytes.Equal(images[0].Data, a.output) && !bytes.Equal(images[0].Data, a.revision) {
		a.t.Fatal("edit source bytes changed")
	}
	if len(a.inputCounts) == 2 {
		return a.revision, nil
	}
	return a.output, nil
}
func TestChannelCodexImagesAndReplyScope(t *testing.T) {
	ctx := context.Background()
	f := fixture(t, 2<<20)
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(0, 0, color.White)
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	adapter := &accountImageFixture{testAdapter: &testAdapter{}, t: t, output: b.Bytes()}
	img.Set(1, 1, color.White)
	var revised bytes.Buffer
	if err := png.Encode(&revised, img); err != nil {
		t.Fatal(err)
	}
	adapter.revision = revised.Bytes()
	f.s.adapters[provider.Codex] = adapter
	credentials := provider.Credentials{AccessToken: "selected-test-access", AccountID: "selected-test-account"}
	id := identity.NewID()
	raw, err := f.s.encryptCredentials(id, credentials)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.pool.Exec(ctx, `INSERT INTO chatxo.ai_accounts(id,instance_id,created_by,provider,status,credentials,expires_at) VALUES($1,$2,$3,'codex','connected',$4,now()+interval '1 day')`, id, f.cfg.InstanceID, f.p.User.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.SaveSettings(ctx, f.p, f.channel.ID, SettingsInput{AccountID: id, Provider: "codex", Model: "vision", Name: "Image author", MaxBytes: 2 << 20})
	if err != nil {
		t.Fatal(err)
	}
	f.send(t, "draw")
	turn := f.claim(t)
	call := provider.ToolCall{ID: "image-call", Name: "image_generate", Arguments: json.RawMessage(`{"prompt":"fixture prompt"}`)}
	// Drive the actual provider loop. Both previews must remain private until the
	// agent selects the revision, even with text already checkpointed.
	var msgID, accepted string
	stage := 0
	adapter.generate = func(_ context.Context, r provider.Request, emit func(provider.Event) error) (provider.Response, error) {
		stage++
		switch stage {
		case 1:
			return provider.Response{Text: "Working. ", Calls: []provider.ToolCall{call}}, nil
		case 2, 3:
			if len(r.ToolMedia) != 2 || r.ToolMedia[1].Type != "image" {
				t.Fatal("missing visual preview", r.ToolResults)
			}
			if _, err := png.Decode(bytes.NewReader(r.ToolMedia[1].Data)); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM chatxo.attachments WHERE conversation_id=$1`, turn.ChannelID).Scan(&count); err != nil || count != 0 {
				t.Fatal("draft published before acceptance", count, err)
			}
			var receipt struct {
				ID string `json:"draft_id"`
			}
			if json.Unmarshal([]byte(r.ToolResults[0].Content), &receipt) != nil || receipt.ID == "" {
				t.Fatal(r.ToolResults)
			}
			if stage == 2 {
				return provider.Response{Calls: []provider.ToolCall{{Name: "image_edit", ID: "revise", Arguments: json.RawMessage(`{"prompt":"fixture prompt","target_id":"` + receipt.ID + `"}`)}}}, nil
			}
			return provider.Response{Calls: []provider.ToolCall{{Name: "attach_image", ID: "accept", Arguments: json.RawMessage(`{"draft_id":"` + receipt.ID + `"}`)}}}, nil
		case 4:
			if r.ToolResults[0].IsError {
				t.Fatal(r.ToolResults)
			}
			var receipt struct {
				ID      string `json:"attached_id"`
				Message string `json:"message_id"`
			}
			if err := json.Unmarshal([]byte(r.ToolResults[0].Content), &receipt); err != nil {
				t.Fatal(err)
			}
			msgID, accepted = receipt.Message, receipt.ID
			return provider.Response{Text: "Here it is."}, nil
		}
		t.Fatal("unexpected provider step")
		return provider.Response{}, nil
	}
	f.s.execute(ctx, turn)
	adapter.generate = nil
	var body string
	var images []string
	var storedDigest string
	var messages int
	if err = f.s.pool.QueryRow(ctx, `SELECT m.body,m.attachment_ids,d.provenance->>'output_sha256',(SELECT count(*) FROM chatxo.messages WHERE conversation_id=m.conversation_id AND ai_author IS NOT NULL) FROM chatxo.messages m JOIN chatxo.ai_media d ON d.message_id=m.id WHERE m.id=$1 AND NOT m.ai_streaming`, msgID).Scan(&body, &images, &storedDigest, &messages); err != nil || body != "Working. Here it is." || len(images) != 1 || images[0] != accepted || messages != 1 || storedDigest != digest(adapter.revision) {
		t.Fatal("accepted draft not finalized on same message", body, images, err)
	}
	_, file, err := f.s.attachments.Get(ctx, f.p, accepted)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(file)
	file.Close()
	if err != nil || !bytes.Equal(actual, adapter.revision) {
		t.Fatal("accepted file changed", err)
	}
	check := func(reply string, wantImages int) claimedTurn {
		t.Helper()
		if _, err := f.ch.Create(ctx, f.p, f.channel.ID, identity.NewID(), "look", reply, nil); err != nil {
			t.Fatal(err)
		}
		next := f.claim(t)
		req, err := f.s.context(ctx, f.p, next, adapter, credentials)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range req.Messages {
			for _, p := range m.Parts {
				if m.Role == "assistant" && strings.HasSuffix(strings.TrimSpace(p.Text), "</haunt_assistant>") {
					t.Fatal("image-only channel message appeared as a historical assistant turn")
				}
				if p.Type == "image" {
					n++
				}
			}
		}
		if n != wantImages {
			t.Fatalf("reply=%q got %d images, want %d", reply, n, wantImages)
		}
		return next
	}
	unrelated := check("", 0)
	f.s.execute(ctx, unrelated)
	reply := check(msgID, 1)
	stored, err := f.s.resolveMedia(ctx, reply, provider.ToolCall{Name: "resolve_media", Arguments: json.RawMessage(`{"attachment_id":"` + accepted + `"}`)})
	if err != nil || stored.IsError || !strings.Contains(stored.Content, digest(adapter.revision)) {
		t.Fatal(stored, err)
	}
	search, err := f.s.searchConversation(ctx, reply, provider.ToolCall{Name: "search_conversation", Arguments: json.RawMessage(`{"query":"Working.","media_only":true}`)})
	if err != nil || search.IsError || !strings.Contains(search.Content, accepted) {
		t.Fatal(search, err)
	}
	chain, err := f.s.searchConversation(ctx, reply, provider.ToolCall{Name: "search_conversation", Arguments: json.RawMessage(`{"reply_chain":"` + reply.TriggerMessageID + `"}`)})
	if err != nil || chain.IsError || !strings.Contains(chain.Content, msgID) {
		t.Fatal(chain, err)
	}
	spec, err := f.s.ImageSpec(ctx, f.p, reply.ChannelID, accepted, &EditSpecInput{Instructions: "Keep framing"})
	if err != nil || spec.EditorID != f.p.User.ID {
		t.Fatal(spec, err)
	}
	input, err := f.s.imageInputs(ctx, f.p, reply, "image_edit", imageInput{Target: accepted})
	if err != nil || input.spec != "Keep framing" || input.operation.Parent != accepted {
		t.Fatal(input.spec, err)
	}
	_, err = f.s.ImageSpec(ctx, f.p, reply.ChannelID, accepted, &EditSpecInput{})
	if err != nil {
		t.Fatal(err)
	}
	call.Name = "image_edit"
	if _, err = f.s.makeDraft(ctx, f.p, &reply, call); err != nil {
		t.Fatal(err)
	}
	if len(adapter.inputCounts) != 3 || adapter.inputCounts[2] != 1 {
		t.Fatal("missing edit source")
	}
	f.s.execute(ctx, reply)
	next := check("", 0)
	if _, err := f.ids.SaveArt(ctx, f.p, "avatar", b.Bytes()); err != nil {
		t.Fatal(err)
	}
	profileCall := provider.ToolCall{ID: "profile", Name: "view_user_profile", Arguments: json.RawMessage(`{"user":"@OWNER"}`)}
	result, err := f.s.dispatchTool(ctx, f.p, &next, profileCall, newRunActivity())
	if err != nil || result.IsError || !strings.Contains(result.Content, `"avatar_user_id":"`+f.p.User.ID+`"`) {
		t.Fatalf("profile lookup: %+v, %v", result, err)
	}
	req, err := f.s.context(ctx, f.p, next, adapter, credentials)
	if err != nil {
		t.Fatal(err)
	}
	continued := next.profilePictures.visualContext(req)
	if len(continued.ToolMedia) != 2 || !bytes.Equal(continued.ToolMedia[1].Data, b.Bytes()) {
		t.Fatal("profile picture did not reach the model")
	}
	for _, tool := range []string{"image_generate", "image_edit"} {
		call := provider.ToolCall{
			ID: tool, Name: tool,
			Arguments: json.RawMessage(`{"prompt":"fixture prompt","reference_user_ids":["` + f.p.User.ID + `"]}`),
		}
		if _, err := f.s.makeDraft(ctx, f.p, &next, call); err != nil {
			t.Fatal(err)
		}
	}
	if len(adapter.inputCounts) != 5 || adapter.inputCounts[3] != 1 || adapter.inputCounts[4] != 1 {
		t.Fatal("selected profile picture missing from a Codex operation")
	}
	f.s.execute(ctx, next)
	later := check("", 0)
	if _, err := later.profilePictures.references([]string{f.p.User.ID}); err == nil {
		t.Fatal("profile reference carried over to the next response")
	}
	f.accounting(t)
}
func TestCreatePresetWithPortraitRetry(t *testing.T) {
	f := fixture(t, 2<<20)
	img := image.NewRGBA(image.Rect(0, 0, 1600, 800))
	var raw bytes.Buffer
	if err := png.Encode(&raw, img); err != nil {
		t.Fatal(err)
	}
	input := PresetInput{Name: "Full Name", Description: "A test personality", AvatarData: raw.Bytes(), RequestID: identity.NewID()}
	ctx := context.Background()
	for range 2 {
		saved, err := f.s.SavePreset(ctx, f.p, f.channel.ID, "", input)
		if err != nil || saved.ID != input.RequestID || saved.AvatarURL == "" {
			t.Fatalf("preset create: %v", err)
		}
	}
	items, err := f.s.Presets(ctx, f.p, f.channel.ID)
	if err != nil || len(items) != 1 || items[0].Description != input.Description {
		t.Fatalf("retry duplicated or damaged preset: %v", err)
	}
	picture, err := f.s.Avatar(ctx, f.p, f.channel.ID, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(picture))
	if err != nil || cfg.Width != 512 || cfg.Height != 256 {
		t.Fatalf("resize: %+v %v", cfg, err)
	}
}
