package httpapi

import (
	"context"
	"testing"

	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
)

func TestVoiceChannelChatUsesMessagePermissionsWithoutJoining(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	alice, bob := f.register(t, "alice"), f.register(t, "bob")
	var channel identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "lounge", "kind": "voice"}, 201, &channel)
	var roles rolesResponse
	f.call(t, "GET", "/api/v1/roles", f.owner.Token, nil, 200, &roles)
	everyone := roles.Roles[0].ID
	permission := "/api/v1/channels/" + channel.ID + "/permissions/" + everyone
	// Chat access is independent of permission to join the voice call.
	f.call(t, "PUT", permission, f.owner.Token, map[string]any{"allow": 0, "deny": 4}, 204, nil)
	var conversations conversationsResponse
	f.call(t, "GET", "/api/v1/conversations", alice.Token, nil, 200, &conversations)
	if len(conversations.Conversations) != 1 || conversations.Conversations[0].ID != channel.ID {
		t.Fatal("voice channel chat missing from directory")
	}
	ws := wsConnect(t, f, bob.Token)
	wsSend(t, ws, map[string]any{"type": "subscribe", "conversations": []string{channel.ID}})
	wsRead(t, ws, "subscribed")
	m := sendMessage(t, f, alice.Token, channel.ID, "A note alongside the call")
	wsRead(t, ws, "message.created")
	var history chat.History
	f.call(t, "GET", "/api/v1/conversations/"+channel.ID+"/messages", bob.Token, nil, 200, &history)
	if len(history.Messages) != 1 || history.Messages[0].ID != m.ID {
		t.Fatal("voice channel chat history did not persist")
	}
	p, err := f.identities.Authenticate(context.Background(), alice.Token)
	if err != nil {
		t.Fatal(err)
	}
	rooms, err := features.Voice.Presence(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 0 {
		t.Fatal("messaging unexpectedly admitted a voice participant")
	}
	f.call(t, "PUT", permission, f.owner.Token, map[string]any{"allow": 0, "deny": 2}, 204, nil)
	f.call(t, "POST", "/api/v1/conversations/"+channel.ID+"/messages", alice.Token, map[string]any{"request_id": identity.NewID(), "body": "forbidden"}, 403, nil)
	f.call(t, "GET", "/api/v1/conversations/"+channel.ID+"/messages", bob.Token, nil, 200, &history)
	f.call(t, "PUT", permission, f.owner.Token, map[string]any{"allow": 0, "deny": 1}, 204, nil)
	f.call(t, "GET", "/api/v1/conversations/"+channel.ID+"/messages", bob.Token, nil, 404, nil)
	f.call(t, "GET", "/api/v1/conversations", bob.Token, nil, 200, &conversations)
	if len(conversations.Conversations) != 0 {
		t.Fatal("hidden voice chat leaked into directory")
	}
	f.call(t, "DELETE", "/api/v1/channels/"+channel.ID, f.owner.Token, nil, 204, nil)
	f.call(t, "GET", "/api/v1/conversations/"+channel.ID+"/messages", f.owner.Token, nil, 404, nil)
	accounting(t, f)
}
