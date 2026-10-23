package httpapi

import (
	"context"
	"sync"
	"testing"

	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/voice"
)

func TestVoiceRestrictionsPersistAndRestoreSelfState(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	member := f.register(t, "member")
	moderator := f.register(t, "moderator")
	peer := f.register(t, "peer")
	grantRole(t, f, moderator, identity.MuteMembers, 50)
	grantRole(t, f, peer, identity.MuteMembers, 50)
	var channel identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "voice", "kind": "voice"}, 201, &channel)
	base := "/api/v1/voice/" + channel.ID + "/restrictions"
	path := base + "/" + member.User.ID
	f.call(t, "GET", base, member.Token, nil, 403, nil)
	f.call(t, "PATCH", path, moderator.Token, map[string]bool{"deafened": true}, 403, nil)
	f.call(t, "PATCH", base+"/"+peer.User.ID, moderator.Token, map[string]bool{"muted": true}, 403, nil)
	f.call(t, "PATCH", base+"/"+f.owner.User.ID, moderator.Token, map[string]bool{"muted": true}, 403, nil)
	f.call(t, "PATCH", path, moderator.Token, map[string]bool{"muted": true}, 200, nil)
	conn, joined := joinVoice(t, f, member.Token, channel.ID)
	if !joined.State.Muted || !joined.Restriction.Muted || joined.SelfState.Muted {
		t.Fatal("join lost restriction or self state", joined)
	}
	second, other := joinVoice(t, f, member.Token, channel.ID)
	defer second.CloseNow()
	wsSend(t, conn, map[string]any{"type": "media.state", "state": voice.State{CameraOff: true, ScreenOff: true}})
	wsRead(t, conn, "media.state.accepted")
	var members participantsResponse
	f.call(t, "GET", "/api/v1/voice/"+channel.ID, member.Token, nil, 200, &members)
	for _, p := range members.Participants {
		if !p.State.Muted {
			t.Fatal("self-unmute bypassed restriction")
		}
	}
	f.call(t, "PATCH", path, f.owner.Token, map[string]bool{"muted": false, "deafened": true}, 200, nil)
	f.call(t, "GET", "/api/v1/voice/"+channel.ID, member.Token, nil, 200, &members)
	for _, p := range members.Participants {
		if !p.State.Muted || !p.State.Deafened || p.Restriction.Muted {
			t.Fatal("deafen did not preserve independent flags", p)
		}
	}
	wsSend(t, second, map[string]any{"type": "media.state", "state": voice.State{Muted: true, CameraOff: true, ScreenOff: true}})
	wsRead(t, second, "media.state.accepted")
	f.call(t, "PATCH", path, f.owner.Token, map[string]bool{"deafened": false}, 200, nil)
	f.call(t, "GET", "/api/v1/voice/"+channel.ID, member.Token, nil, 200, &members)
	for _, p := range members.Participants {
		if p.State.Deafened || p.State.Muted != (p.ID == other.ID) {
			t.Fatal("clearing restriction did not restore each session's own state", p)
		}
	}
	f.call(t, "PATCH", path, moderator.Token, map[string]bool{"muted": true}, 200, nil)
	wsSend(t, conn, map[string]any{"type": "media.leave"})
	wsRead(t, conn, "media.left")
	conn.CloseNow()
	conn, joined = joinVoice(t, f, member.Token, channel.ID)
	defer conn.CloseNow()
	if !joined.State.Muted {
		t.Fatal("restriction lost after leave")
	}
	restarted, err := voice.New(f.cfg, f.identities)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(context.Background())
	p, err := f.identities.Authenticate(context.Background(), member.Token)
	if err != nil {
		t.Fatal(err)
	}
	a, err := restarted.Admit(context.Background(), p, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := restarted.Join(context.Background(), p, identity.NewID(), a.ID, func(any) bool { return true })
	if err != nil || !v.State.Muted {
		t.Fatal("restart lost restriction", v, err)
	}
	var list []identity.VoiceRestriction
	f.call(t, "GET", base, moderator.Token, nil, 200, &list)
	if len(list) != 1 || !list[0].Muted {
		t.Fatal(list)
	}
	var audit auditResponse
	f.call(t, "GET", "/api/v1/admin/audit", f.owner.Token, nil, 200, &audit)
	found := false
	for _, entry := range audit.Entries {
		if entry.Action == "voice.mute" && entry.TargetID == member.User.ID && entry.Reason == channel.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("missing voice moderation audit")
	}
}

func TestVoiceRestrictionsSerializeConcurrentJoins(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	member := f.register(t, "member")
	var room identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "voice", "kind": "voice"}, 201, &room)
	ctx := context.Background()
	actor, err := f.identities.Authenticate(ctx, f.owner.Token)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := f.identities.Authenticate(ctx, member.Token)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	errors := make(chan error, 8)
	connections := make(chan string, 4)
	for index := range 4 {
		workers.Add(2)
		go func() {
			defer workers.Done()
			admission, err := features.Voice.Admit(ctx, principal, room.ID)
			if err == nil {
				connection := identity.NewID()
				_, err = features.Voice.Join(ctx, principal, connection, admission.ID, func(any) bool { return true })
				connections <- connection
			}
			errors <- err
		}()
		go func() {
			defer workers.Done()
			value := index%2 == 0
			_, err := features.Voice.Moderate(ctx, actor, room.ID, member.User.ID, &value, nil)
			errors <- err
		}()
	}
	workers.Wait()
	close(connections)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	value := true
	if _, err := features.Voice.Moderate(ctx, actor, room.ID, member.User.ID, &value, nil); err != nil {
		t.Fatal(err)
	}
	participants, err := features.Voice.Room(ctx, principal, room.ID)
	if err != nil || len(participants) != 4 {
		t.Fatal(len(participants), err)
	}
	for connection := range connections {
		if err := features.Voice.SetState(connection, voice.State{}); err != nil {
			t.Fatal(err)
		}
	}
	participants, err = features.Voice.Room(ctx, principal, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, participant := range participants {
		if !participant.State.Muted || !participant.Restriction.Muted {
			t.Fatal("join or self-state raced past restriction", participant)
		}
	}
}

func TestVoiceRestrictionsIdentifyAbsentAndDeletedMembers(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	member := f.register(t, "restricted")
	moderator := f.register(t, "muteonly")
	grantRole(t, f, moderator, identity.MuteMembers, 50)
	var channel identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "voice", "kind": "voice"}, 201, &channel)
	base := "/api/v1/voice/" + channel.ID + "/restrictions"
	path := base + "/" + member.User.ID
	f.call(t, "PATCH", path, f.owner.Token, map[string]bool{"muted": true, "deafened": true}, 200, nil)
	moderate(t, f, f.owner, member.User.ID, "kick", 204)
	var items []identity.VoiceRestrictionMember
	f.call(t, "GET", base, moderator.Token, nil, 200, &items)
	if len(items) != 1 || items[0].User.ID != member.User.ID || items[0].User.Username != "restricted" {
		t.Fatal("absent restriction lost its identity", items)
	}
	moderate(t, f, f.owner, member.User.ID, "delete", 204)
	f.call(t, "GET", base, moderator.Token, nil, 200, &items)
	if len(items) != 1 || items[0].User.DisplayName != "Deleted user" {
		t.Fatal("deleted restriction lost its identity", items)
	}
	f.call(t, "PATCH", path, moderator.Token, map[string]bool{"deafened": false}, 403, nil)
	f.call(t, "PATCH", path, moderator.Token, map[string]bool{"muted": true}, 404, nil)
	f.call(t, "PATCH", path, moderator.Token, map[string]bool{"muted": false}, 200, nil)
	f.call(t, "GET", base, moderator.Token, nil, 200, &items)
	if len(items) != 1 || items[0].Muted || !items[0].Deafened {
		t.Fatal("clearing mute changed the independent deafen restriction", items)
	}
	f.call(t, "PATCH", path, f.owner.Token, map[string]bool{"deafened": false}, 200, nil)
	f.call(t, "GET", base, moderator.Token, nil, 200, &items)
	if len(items) != 0 {
		t.Fatal("deleted restriction could not be cleared", items)
	}
	f.call(t, "PATCH", base+"/"+identity.NewID(), f.owner.Token, map[string]bool{"muted": false}, 404, nil)
}

func TestVoiceRestrictionPagesAndCurrentPermissions(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	moderator := f.register(t, "moderator")
	grantRole(t, f, moderator, identity.MuteMembers, 50)
	var channel identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "voice", "kind": "voice"}, 201, &channel)
	_, err := f.pool.Exec(context.Background(), `WITH members AS (
 INSERT INTO chatxo.users(id,instance_id,username,password_hash) SELECT gen_random_uuid(),$1,'restricted_'||n,'' FROM generate_series(1,101) n RETURNING id
) INSERT INTO chatxo.voice_restrictions(instance_id,channel_id,user_id,muted) SELECT $1,$2,id,true FROM members`, f.cfg.InstanceID, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/voice/" + channel.ID + "/restrictions"
	var first, next []identity.VoiceRestrictionMember
	f.call(t, "GET", base, moderator.Token, nil, 200, &first)
	if len(first) != 100 {
		t.Fatal("unbounded restriction page", len(first))
	}
	f.call(t, "GET", base+"?after="+first[99].UserID, moderator.Token, nil, 200, &next)
	if len(next) != 1 || next[0].UserID <= first[99].UserID {
		t.Fatal("invalid exclusive cursor", next)
	}
	f.call(t, "GET", base+"?after=invalid", moderator.Token, nil, 400, nil)
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.roles SET permissions=0 WHERE id IN (SELECT role_id FROM chatxo.user_roles WHERE user_id=$1)`, moderator.User.ID); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", base, moderator.Token, nil, 403, nil)
	f.call(t, "PATCH", base+"/"+first[0].UserID, moderator.Token, map[string]bool{"muted": false}, 403, nil)
}
