package httpapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/turnclient"
	"github.com/example-git/haunt-server/internal/voice"
)

type testICEProvider func(context.Context, string) (turnclient.Credentials, error)

func (s testICEProvider) Get(ctx context.Context, key string) (turnclient.Credentials, error) {
	return s(ctx, key)
}

func TestManagedTURNAdmissionExpiryAndFailure(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	var room identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "call", "kind": "voice"}, 201, &room)
	expires := time.Now().Add(5 * time.Second)
	source := testICEProvider(func(_ context.Context, key string) (turnclient.Credentials, error) {
		if time.Now().After(expires) {
			return turnclient.Credentials{}, turnclient.ErrUnavailable
		}
		return turnclient.Credentials{Servers: []config.ICEServer{{URLs: []string{"turn:127.0.0.1:3478"}, Username: key, Credential: "temporary"}}, ExpiresAt: expires}, nil
	})
	features.Voice.Close(context.Background())
	manager, err := voice.New(config.Config{MediaRelayOnly: true}, f.identities, source)
	if err != nil {
		t.Fatal(err)
	}
	features.Voice, features.Gateway.media = manager, manager
	var admission voice.Admission
	f.call(t, "POST", "/api/v1/voice/"+room.ID+"/admissions", f.owner.Token, nil, 201, &admission)
	if admission.ICEPolicy != "relay" || admission.Transport != "webrtc" || admission.ICEExpiresAt == nil || !admission.ICEExpiresAt.Equal(expires) || len(admission.ICE) != 1 || admission.ICE[0].Username != f.owner.Session.ID {
		t.Fatal("managed admission invalid")
	}
	events := wsConnect(t, f, f.owner.Token)
	wsSend(t, events, map[string]string{"type": "media.join", "admission_id": admission.ID})
	wsRead(t, events, "media.joined")
	message := wsRead(t, events, "media.reconnect_required")
	var reason string
	if json.Unmarshal(message["reason"], &reason) != nil || reason != "ice_credentials_expiring" {
		t.Fatal("missing automatic reconnect notice")
	}
	wsRead(t, events, "media.revoked")
	if manager.Stats()["participants"].(int) != 0 {
		t.Fatal("expired relay participant retained")
	}
	var failure map[string]string
	f.call(t, "POST", "/api/v1/voice/"+room.ID+"/admissions", f.owner.Token, nil, 503, &failure)
	if failure["error"] != "media_relay_unavailable" {
		t.Fatal("provider failure was not sanitized")
	}
}
