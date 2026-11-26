package httpapi

import (
	"testing"

	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
)

func TestChannelModerationOverrides(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	moderator := f.register(t, "localmoderator")
	member := f.register(t, "localmember")
	role := grantRole(t, f, moderator, 0, 10)
	makeChannel := func(kind, name string) identity.Channel {
		var ch identity.Channel
		f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]string{"kind": kind, "name": name}, 201, &ch)
		return ch
	}
	voice := makeChannel("voice", "team")
	other := makeChannel("voice", "other")
	ai := makeChannel("ai", "assistant")
	override := func(ch identity.Channel, actor identity.Login, allow, deny int64, want int) {
		f.call(t, "PUT", "/api/v1/channels/"+ch.ID+"/permissions/"+role.ID, actor.Token, map[string]int64{"allow": allow, "deny": deny}, want, nil)
	}
	edit := func(ch identity.Channel, want int) {
		f.call(t, "PUT", "/api/v1/channels/"+ch.ID, moderator.Token, map[string]string{"kind": ch.Kind, "name": "edited"}, want, nil)
	}
	restriction := func(ch identity.Channel, change map[string]bool, want int) {
		f.call(t, "PATCH", "/api/v1/voice/"+ch.ID+"/restrictions/"+member.User.ID, moderator.Token, change, want, nil)
	}
	override(voice, f.owner, identity.ManageChannels|identity.MuteMembers, 0, 204)
	override(voice, moderator, identity.ManageChannels|identity.MuteMembers|identity.DeafenMembers, 0, 403)
	edit(voice, 200)
	edit(other, 403)
	restriction(voice, map[string]bool{"muted": true}, 200)
	restriction(other, map[string]bool{"muted": true}, 403)
	restriction(voice, map[string]bool{"muted": false, "deafened": true}, 403)
	override(voice, f.owner, identity.DeafenMembers, identity.MuteMembers, 204)
	restriction(voice, map[string]bool{"deafened": true}, 200)
	restriction(voice, map[string]bool{"muted": false}, 403)
	edit(voice, 403)
	override(ai, f.owner, identity.MuteMembers, 0, 400)
	override(ai, f.owner, identity.Administrator, 0, 400)

	f.call(t, "GET", "/api/v1/conversations/"+ai.ID+"/messages", member.Token, nil, 200, nil)
	// Write the same AI-authored row the provider writes, with no human owner.
	ctx := t.Context()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	request := identity.NewID()
	if err := features.Chat.LockStorage(ctx, tx); err != nil {
		t.Fatal(err)
	}
	reply, err := features.Chat.WriteAI(ctx, tx, ai.ID, request, request, "A response", chat.AIAuthor{ID: identity.NewID(), Name: "Helper"})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/conversations/" + ai.ID + "/messages/" + reply.ID
	f.call(t, "DELETE", path, moderator.Token, nil, 403, nil)
	override(ai, f.owner, identity.ManageMessages, 0, 204)
	var deleted chat.Message
	f.call(t, "DELETE", path, moderator.Token, nil, 200, &deleted)
	if !deleted.Deleted || deleted.ID != reply.ID {
		t.Fatal("AI reply was not deleted")
	}

	// A channel deny must also take precedence over a server role grant.
	global := grantRole(t, f, member, identity.ManageChannels|identity.ManageMessages, 20)
	f.call(t, "PUT", "/api/v1/channels/"+voice.ID+"/permissions/"+global.ID, f.owner.Token,
		map[string]int64{"allow": 0, "deny": identity.ManageChannels | identity.ManageMessages}, 204, nil)
	f.call(t, "DELETE", "/api/v1/channels/"+voice.ID, member.Token, nil, 403, nil)
	post := sendMessage(t, f, moderator.Token, voice.ID, "voice chat")
	path = "/api/v1/conversations/" + voice.ID + "/messages/" + post.ID
	f.call(t, "DELETE", path, member.Token, nil, 403, nil)
	f.call(t, "DELETE", path, moderator.Token, nil, 200, nil)
	own := sendMessage(t, f, member.Token, voice.ID, "my own message")
	f.call(t, "DELETE", "/api/v1/conversations/"+voice.ID+"/messages/"+own.ID, member.Token, nil, 200, nil)

}
