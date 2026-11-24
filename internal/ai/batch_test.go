package ai

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"slices"
	"strings"
	"testing"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
)

func batchUser(t *testing.T, f *aiFixture) identity.Principal {
	t.Helper()
	ctx := context.Background()
	invite, err := f.ids.CreateInvite(ctx, f.p, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	login, err := f.ids.Register(ctx, "researcher", "valid test passphrase", invite.Token, "browser")
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.ids.Authenticate(ctx, login.Token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPendingMessagesBatchAcrossUsersAndSplitAtPersonalityChange(t *testing.T) {
	f := fixture(t, 8<<20)
	ctx := context.Background()
	secondUser := batchUser(t, f)
	alpha, err := f.s.SavePreset(ctx, f.p, f.channel.ID, "", PresetInput{Name: "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	beta, err := f.s.SavePreset(ctx, f.p, f.channel.ID, "", PresetInput{Name: "Beta"})
	if err != nil {
		t.Fatal(err)
	}
	f.send(t, "generate an image")
	busy := f.claim(t)
	first := f.send(t, "<@ai:"+alpha.ID+"> find the source")
	second, err := f.ch.Create(ctx, secondUser, f.channel.ID, identity.NewID(), "and publication date", first.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	third := f.send(t, "<@ai:"+alpha.ID+"> include the URL")
	fourth := f.send(t, "<@ai:"+beta.ID+"> your opinion?")
	fifth := f.send(t, "especially the ending")
	sixth := f.send(t, "<@ai:"+alpha.ID+"> one last source")
	f.s.execute(ctx, busy)
	turn := f.claim(t)
	if turn.snapshot.Author.ID != alpha.ID || !slices.Equal(turn.inputIDs(), []string{first.ID, second.ID, third.ID}) {
		t.Fatal("lost first group or selection", turn.inputIDs())
	}
	adapter, cred, err := f.s.credentials(ctx, turn.snapshot.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	req, err := f.s.context(ctx, f.p, turn, adapter, cred)
	if err != nil {
		t.Fatal(err)
	}
	active := req.Messages[len(req.Messages)-1]
	if active.Author.MessageID != first.ID || len(active.Parts) != 5 || active.Parts[0].Text != first.Body ||
		!strings.Contains(active.Parts[1].Text, secondUser.User.ID) || !strings.Contains(active.Parts[1].Text, first.ID) ||
		active.Parts[2].Text != second.Body || !strings.Contains(active.Parts[3].Text, third.ID) || active.Parts[4].Text != third.Body {
		t.Fatalf("lost sequence, authors, or reply relationship: %+v", active)
	}
	for _, msg := range req.Messages[:len(req.Messages)-1] {
		if msg.Author != nil && slices.Contains(turn.inputIDs(), msg.Author.MessageID) {
			t.Fatal("repeated active content in history")
		}
	}
	f.s.execute(ctx, turn)
	turn = f.claim(t)
	if turn.snapshot.Author.ID != beta.ID || !slices.Equal(turn.inputIDs(), []string{fourth.ID, fifth.ID}) {
		t.Fatal("lost second group")
	}
	f.s.execute(ctx, turn)
	turn = f.claim(t)
	if turn.snapshot.Author.ID != alpha.ID || !slices.Equal(turn.inputIDs(), []string{sixth.ID}) {
		t.Fatal("reordered third group")
	}
	f.s.execute(ctx, turn)
	if _, err = f.s.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("duplicate response", err)
	}
	f.accounting(t)
}

func TestHiddenPostNeverEntersTurnsHistoryOrImageInputs(t *testing.T) {
	f := fixture(t, 8<<20)
	ctx := context.Background()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	a, err := f.s.attachments.Upload(ctx, f.p, f.channel.ID, "private.png", int64(data.Len()), &data)
	if err != nil {
		t.Fatal(err)
	}
	// An unknown personality mention would fail a visible post.
	body := "members only <@ai:" + identity.NewID() + ">"
	request := identity.NewID()
	hidden, err := f.ch.CreateWithVisibility(ctx, f.p, f.channel.ID, request, body, "", []string{a.ID}, true)
	if err != nil || !hidden.HiddenFromAI {
		t.Fatal("hidden post", err)
	}
	again, err := f.ch.CreateWithVisibility(ctx, f.p, f.channel.ID, request, body, "", []string{a.ID}, true)
	if err != nil || again.ID != hidden.ID {
		t.Fatal("hidden retry", err)
	}
	if _, err = f.ch.Create(ctx, f.p, f.channel.ID, request, body, "", []string{a.ID}); !errors.Is(err, identity.ErrConflict) {
		t.Fatal("retry changed message visibility", err)
	}
	if _, err = f.s.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("hidden post triggered a turn", err)
	}
	visible, err := f.ch.Create(ctx, f.p, f.channel.ID, identity.NewID(), "visible reply", hidden.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn := f.claim(t)
	edited, err := f.ch.Edit(ctx, f.p, f.channel.ID, hidden.ID, "still hidden", false)
	if err != nil || !edited.HiddenFromAI {
		t.Fatal("edit lost visibility", err)
	}
	if _, err = f.s.checkExecution(ctx, turn); err != nil {
		t.Fatal("hidden edit interrupted the visible turn", err)
	}
	adapter, credentials, err := f.s.credentials(ctx, turn.snapshot.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	req, err := f.s.context(ctx, f.p, turn, adapter, credentials)
	if err != nil || len(req.Messages) != 1 {
		t.Fatal("hidden post in history", err)
	}
	m := req.Messages[0]
	if m.Author == nil || m.Author.MessageID != visible.ID || m.Author.ReplyTo != nil || len(m.Parts) != 1 || m.Parts[0].Text != visible.Body {
		t.Fatal("hidden reply metadata or image reached the provider")
	}
	sources, err := f.s.editImageInputs(ctx, f.p, turn, 10, 23<<20)
	if err != nil || len(sources) != 0 {
		t.Fatal("hidden image reached the edit tool", err)
	}
	f.accounting(t)
}

func TestBatchImagesPreferNewestCurrentSources(t *testing.T) {
	f := fixture(t, 8<<20)
	ctx := context.Background()
	sendImage := func(n int) []byte {
		im := image.NewRGBA(image.Rect(0, 0, 2, 2))
		im.SetRGBA(0, 0, color.RGBA{R: uint8(n), A: 255})
		var b bytes.Buffer
		if err := png.Encode(&b, im); err != nil {
			t.Fatal(err)
		}
		a, err := f.s.attachments.Upload(ctx, f.p, f.channel.ID, "photo.png", int64(b.Len()), bytes.NewReader(b.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.ch.Create(ctx, f.p, f.channel.ID, identity.NewID(), "photo", "", []string{a.ID}); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	sendImage(0)
	f.s.execute(ctx, f.claim(t))
	var sent [][]byte
	for i := 1; i <= 12; i++ {
		sent = append(sent, sendImage(i))
	}
	turn := f.claim(t)
	adapter, cred, err := f.s.credentials(ctx, turn.snapshot.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	req, err := f.s.context(ctx, f.p, turn, adapter, cred)
	if err != nil {
		t.Fatal(err)
	}
	var seen [][]byte
	for i, msg := range req.Messages {
		for _, p := range msg.Parts {
			if p.Type == "image" {
				if i != len(req.Messages)-1 {
					t.Fatal("history displaced an active image")
				}
				seen = append(seen, p.Data)
			}
		}
	}
	if len(seen) != 10 || !slices.EqualFunc(seen, sent[2:], bytes.Equal) {
		t.Fatal("image cap, priority or output order incorrect")
	}
	edits, err := f.s.editImageInputs(ctx, f.p, turn, 10, 23<<20)
	if err != nil || len(edits) != 10 {
		t.Fatal("edit cap", len(edits), err)
	}
	for i := range edits {
		if !bytes.Equal(edits[i].Data, seen[i]) {
			t.Fatal("edit/context selection disagrees")
		}
	}
	f.accounting(t)
}

func TestDeletedBatchHeadRetainsRemainingRequesterAndRetryFreezesMembers(t *testing.T) {
	f := fixture(t, 8<<20)
	ctx := context.Background()
	first := f.send(t, "old")
	second := f.send(t, "keep")
	if _, err := f.ch.Edit(ctx, f.p, f.channel.ID, first.ID, "", true); err != nil {
		t.Fatal(err)
	}
	turn := f.claim(t)
	if turn.TriggerMessageID != second.ID || !slices.Equal(turn.inputIDs(), []string{second.ID}) {
		t.Fatal("failed to advance deleted trigger", turn.inputIDs())
	}
	adapter, cred, err := f.s.credentials(ctx, turn.snapshot.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	req, err := f.s.context(ctx, f.p, turn, adapter, cred)
	if err != nil || req.Messages[len(req.Messages)-1].Author.MessageID != second.ID {
		t.Fatal("wrong surviving input", err)
	}
	if _, err := f.s.Cancel(ctx, f.p, f.channel.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	f.s.execute(ctx, turn)
	if _, err := f.s.Retry(ctx, f.p, f.channel.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	later := f.send(t, "another response")
	retry := f.claim(t)
	if retry.ID != turn.ID || slices.Contains(retry.inputIDs(), later.ID) {
		t.Fatal("later input folded into explicit retry")
	}
	f.adapter.generate = func(context.Context, provider.Request, func(provider.Event) error) (provider.Response, error) {
		return provider.Response{Text: "done"}, nil
	}
	f.s.execute(ctx, retry)
	if next := f.claim(t); next.TriggerMessageID != later.ID {
		t.Fatal("lost later request")
	}
	f.accounting(t)
}

func TestBatchContributorsCanCancelButRetryCannotBypassRevocation(t *testing.T) {
	f := fixture(t, 8<<20)
	ctx := context.Background()
	contributor := batchUser(t, f)
	f.send(t, "first")
	if _, err := f.ch.Create(ctx, contributor, f.channel.ID, identity.NewID(), "second", "", nil); err != nil {
		t.Fatal(err)
	}
	runs, err := f.s.Runs(ctx, contributor, f.channel.ID)
	if err != nil || len(runs) != 1 || !slices.Contains(runs[0].ParticipantIDs, contributor.User.ID) {
		t.Fatalf("missing shared requester: %v, %v", runs, err)
	}
	if _, err = f.s.Cancel(ctx, contributor, f.channel.ID, runs[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = f.ids.RevokeSession(ctx, contributor, contributor.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Retry(ctx, f.p, f.channel.ID, runs[0].ID); err != nil {
		t.Fatal(err)
	}
	retry := f.claim(t)
	f.s.execute(ctx, retry)
	runs, err = f.s.Runs(ctx, f.p, f.channel.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != "failed" || runs[0].ErrorCode != "permission_revoked" {
		t.Fatalf("retry with revoked contributor: %v %v", runs, err)
	}
	if len(f.adapter.requests) != 0 {
		t.Fatal("revoked request reached provider")
	}
	f.accounting(t)
}
