package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
)

func TestAccountFeedAllRoomsNewDMReplayAndReadPositions(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	alice, bob := f.register(t, "alice"), f.register(t, "bob")
	owner, _ := f.identities.Authenticate(context.Background(), f.owner.Token)
	channels := []identity.Channel{}
	for i := 0; i < 70; i++ {
		channel, err := f.identities.SaveChannel(context.Background(), owner, "", fmt.Sprintf("room-%03d", i), "text", nil)
		if err != nil {
			t.Fatal(err)
		}
		channels = append(channels, channel)
	}
	ws := wsConnect(t, f, alice.Token)
	wsSend(t, ws, map[string]any{"type": "events.watch"})
	wsRead(t, ws, "events.watching")
	last := channels[69].ID
	m := sendMessage(t, f, f.owner.Token, last, "Hi @Alice, mail@alice.test doesn't count twice")
	event := wsRead(t, ws, "message.created")
	var got chat.Message
	if json.Unmarshal(event["data"], &got) != nil || got.ID != m.ID || len(got.MentionUserIDs) != 1 || got.MentionUserIDs[0] != alice.User.ID {
		t.Fatalf("account event or mentions: %+v", got)
	}
	var eventID int64
	_ = json.Unmarshal(event["id"], &eventID)
	stateEvent := wsRead(t, ws, "read.state")
	var states []chat.ReadState
	_ = json.Unmarshal(stateEvent["states"], &states)
	if len(states) != 1 || states[0].Unread != 1 || states[0].Mentions != 1 {
		t.Fatal(states)
	}
	// A second session/device sees read markers without rereading history.
	other := wsConnect(t, f, alice.Token)
	wsSend(t, other, map[string]any{"type": "events.watch"})
	wsRead(t, other, "events.watching")
	var state chat.ReadState
	f.call(t, "PUT", "/api/v1/conversations/"+last+"/read", alice.Token, map[string]any{"seq": m.Seq}, 200, &state)
	if state.Unread != 0 || state.ReadSeq != m.Seq {
		t.Fatal(state)
	}
	changed := wsRead(t, other, "read.state")
	_ = json.Unmarshal(changed["states"], &states)
	if len(states) != 1 || states[0].ReadSeq != m.Seq || states[0].Unread != 0 {
		t.Fatal(states)
	}
	f.call(t, "PUT", "/api/v1/conversations/"+last+"/read", alice.Token, map[string]any{"seq": 0}, 200, &state)
	if state.ReadSeq != m.Seq {
		t.Fatal("read position moved backward")
	}
	other.CloseNow()
	ws.CloseNow()
	replay := sendMessage(t, f, f.owner.Token, last, "@alice while disconnected")
	ws = wsConnect(t, f, alice.Token)
	wsSend(t, ws, map[string]any{"type": "events.watch", "after_event": eventID})
	wsRead(t, ws, "events.watching")
	event = wsRead(t, ws, "message.created")
	_ = json.Unmarshal(event["data"], &got)
	if got.ID != replay.ID {
		t.Fatalf("replay: %+v", got)
	}
	// A newly created DM is admitted automatically, with no channel subscribe.
	var dm chat.Conversation
	f.call(t, "POST", "/api/v1/dms", bob.Token, map[string]any{"members": []string{alice.User.ID}}, 201, &dm)
	direct := sendMessage(t, f, bob.Token, dm.ID, "new direct message")
	event = wsRead(t, ws, "message.created")
	_ = json.Unmarshal(event["data"], &got)
	if got.ID != direct.ID {
		t.Fatal("new DM not delivered")
	}
	f.call(t, "PUT", "/api/v1/conversations/"+dm.ID+"/read", f.owner.Token, map[string]any{"seq": 1}, 404, nil)
	// Full read snapshots recover after process/socket loss and keep DM counts.
	var page chat.ReadPage
	f.call(t, "GET", "/api/v1/read-state", alice.Token, nil, 200, &page)
	foundDM, foundChannel := false, false
	for _, s := range page.States {
		if s.ConversationID == dm.ID {
			foundDM = s.Kind == "dm" && s.Unread == 1
		}
		if s.ConversationID == last {
			foundChannel = s.Unread == 1 && s.Mentions == 1 && s.ReadSeq == m.Seq
		}
	}
	if !foundDM || !foundChannel {
		t.Fatalf("snapshot %+v", page)
	}
	// Permission loss removes the room from the feed and all read APIs.
	var roles rolesResponse
	f.call(t, "GET", "/api/v1/roles", f.owner.Token, nil, 200, &roles)
	f.call(t, "PUT", "/api/v1/channels/"+last+"/permissions/"+roles.Roles[0].ID, f.owner.Token, map[string]any{"allow": 0, "deny": 1}, 204, nil)
	wsRead(t, ws, "subscription.revoked")
	f.call(t, "PUT", "/api/v1/conversations/"+last+"/read", alice.Token, map[string]any{"seq": 99}, 404, nil)
	f.call(t, "GET", "/api/v1/read-state", alice.Token, nil, 200, &page)
	for _, s := range page.States {
		if s.ConversationID == last {
			t.Fatal("hidden read state leaked")
		}
	}
	// A non-member owner does not gain DM access through the all-account feed.
	p, _ := f.identities.Authenticate(context.Background(), f.owner.Token)
	ids, err := features.Chat.VisibleIDs(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == dm.ID {
			t.Fatal("owner saw private DM")
		}
	}
	accounting(t, f)
}

func TestUnreadCountsFollowEditsDeletesAndExpiredHistory(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	alice := f.register(t, "alice")
	channel := textChannel(t, f)
	m := sendMessage(t, f, f.owner.Token, channel.ID, "ordinary")
	sendMessage(t, f, alice.Token, channel.ID, "own @alice message")
	read := func() chat.ReadState {
		var page chat.ReadPage
		f.call(t, "GET", "/api/v1/read-state", alice.Token, nil, 200, &page)
		if len(page.States) != 1 {
			t.Fatal(page)
		}
		return page.States[0]
	}
	if s := read(); s.Unread != 1 || s.Mentions != 0 {
		t.Fatal(s)
	}
	f.call(t, "PUT", "/api/v1/conversations/"+channel.ID+"/messages/"+m.ID, f.owner.Token, map[string]any{"body": "edited @alice"}, 200, nil)
	if s := read(); s.Unread != 1 || s.Mentions != 1 {
		t.Fatal(s)
	}
	f.call(t, "DELETE", "/api/v1/conversations/"+channel.ID+"/messages/"+m.ID, f.owner.Token, nil, 200, nil)
	if s := read(); s.Unread != 0 || s.Mentions != 0 {
		t.Fatal(s)
	}
	expired := sendMessage(t, f, f.owner.Token, channel.ID, "aged @alice")
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.messages SET created_at=now()-interval '31 days' WHERE id=$1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	if s := read(); s.Unread != 0 || s.Mentions != 0 {
		t.Fatal(s)
	}
	f.call(t, "PUT", "/api/v1/conversations/"+channel.ID+"/read", alice.Token, map[string]any{"seq": -1}, 400, nil)
	accounting(t, f)
}

func TestReadStatePaginationAndConcurrentMonotonicReads(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	ctx := context.Background()
	alice := f.register(t, "alice")
	reader, err := f.identities.Authenticate(ctx, alice.Token)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := f.identities.Authenticate(ctx, f.owner.Token)
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for i := 0; i < 106; i++ {
		c, err := f.identities.SaveChannel(ctx, owner, "", fmt.Sprintf("paged-%03d", i), "text", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = features.Chat.Create(ctx, owner, c.ID, identity.NewID(), "@alice unread", nil); err != nil {
			t.Fatal(err)
		}
		last = c.ID
	}
	var page chat.ReadPage
	f.call(t, "GET", "/api/v1/read-state", alice.Token, nil, 200, &page)
	if len(page.States) != 100 || page.Next == "" {
		t.Fatalf("first page %d %q", len(page.States), page.Next)
	}
	seen := map[string]bool{}
	for _, state := range page.States {
		seen[state.ConversationID] = true
		if state.Unread != 1 || state.Mentions != 1 {
			t.Fatal(state)
		}
	}
	nextPage := page.Next
	page = chat.ReadPage{}
	f.call(t, "GET", "/api/v1/read-state?after="+nextPage, alice.Token, nil, 200, &page)
	if len(page.States) != 6 || page.Next != "" {
		t.Fatalf("second page %d %q", len(page.States), page.Next)
	}
	for _, state := range page.States {
		if seen[state.ConversationID] {
			t.Fatal("repeated page row")
		}
	}
	f.call(t, "GET", "/api/v1/read-state?after=bad", alice.Token, nil, 400, nil)
	for i := 1; i < 20; i++ {
		if _, err := features.Chat.Create(ctx, owner, last, identity.NewID(), "message", nil); err != nil {
			t.Fatal(err)
		}
	}
	failures := make(chan error, 30)
	for i := 1; i <= 30; i++ {
		go func(seq int) { _, err := features.Chat.MarkRead(ctx, reader, last, int64(seq)); failures <- err }(i)
	}
	for i := 0; i < 30; i++ {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	states, err := features.Chat.ReadStates(ctx, reader, []string{last})
	if err != nil || len(states) != 1 || states[0].ReadSeq != 20 || states[0].Unread != 0 {
		t.Fatalf("monotonic/clamped read %v %v", states, err)
	}
	accounting(t, f)
}
