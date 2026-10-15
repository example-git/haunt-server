package voice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/turnclient"
	"github.com/pion/webrtc/v4"
)

type credentialSource func(context.Context, string) (turnclient.Credentials, error)

func (s credentialSource) Get(ctx context.Context, key string) (turnclient.Credentials, error) {
	return s(ctx, key)
}

func TestProviderCredentialsSeparateClientAndServer(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	fail := false
	source := credentialSource(func(_ context.Context, key string) (turnclient.Credentials, error) {
		if fail {
			return turnclient.Credentials{}, turnclient.ErrUnavailable
		}
		deadline := expires.Add(time.Hour)
		if key == "server" {
			deadline = expires
		}
		return turnclient.Credentials{Servers: []config.ICEServer{{URLs: []string{"turn:127.0.0.1:3478?transport=tcp"}, Username: key, Credential: "temporary-" + key}}, ExpiresAt: deadline}, nil
	})
	m, err := New(config.Config{MediaRelayOnly: true}, &identity.Service{}, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(context.Background()) })
	credentials, err := m.ConnectionICE(context.Background(), identity.Principal{SessionID: "device"})
	if err != nil || len(credentials.Servers) != 1 || credentials.Servers[0].Username != "device" || !credentials.ExpiresAt.Equal(expires) {
		t.Fatal("client credential separation/earliest expiry failed", err)
	}
	pc, err := m.newPeer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	c := pc.GetConfiguration()
	if c.ICETransportPolicy != webrtc.ICETransportPolicyRelay || len(c.ICEServers) != 1 || c.ICEServers[0].Username != "server" {
		t.Fatal("SFU did not use its own relay credentials")
	}
	before := m.peers.Load()
	fail = true
	if _, err := m.newPeer(context.Background()); !errors.Is(err, turnclient.ErrUnavailable) || m.peers.Load() != before {
		t.Fatal("provider failure leaked capacity or allowed a peer", err)
	}
	if _, err := m.ConnectionICE(context.Background(), identity.Principal{SessionID: "device"}); !errors.Is(err, turnclient.ErrUnavailable) {
		t.Fatal("provider failure was not propagated")
	}
}
