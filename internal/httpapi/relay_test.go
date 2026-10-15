package httpapi

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/relay"
	"github.com/example-git/haunt-server/internal/voice"
)

func TestServerManagedRelayCredentials(t *testing.T) {
	f, features := featureFixture(t, 256_000_000)
	alice := f.register(t, "alice")
	var room identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "relayed", "kind": "voice"}, 201, &room)
	f.call(t, "GET", "/api/v1/voice/"+room.ID+"/ice", alice.Token, nil, 404, nil)
	probe, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	features.Relay, err = relay.New(config.TURN{Enabled: true, PublicIP: "127.0.0.1", ListenAddr: "127.0.0.1:0", RelayBindIP: "127.0.0.1", RelayMin: uint16(port), RelayMax: uint16(port + 16), CredentialTTL: time.Minute, MaxAllocations: 8, AllowedPeers: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	if err != nil {
		t.Fatal(err)
	}
	var credentials relay.Credentials
	f.call(t, "GET", "/api/v1/voice/"+room.ID+"/ice", "", nil, 401, nil)
	f.call(t, "GET", "/api/v1/voice/"+room.ID+"/ice", alice.Token, nil, 200, &credentials)
	if len(credentials.Servers) != 1 || credentials.Servers[0].Username == "" || credentials.Servers[0].Credential == "" || time.Until(credentials.ExpiresAt) < time.Second {
		t.Fatal("relay credentials missing")
	}
	var admission voice.Admission
	f.call(t, "POST", "/api/v1/voice/"+room.ID+"/admissions", alice.Token, nil, 201, &admission)
	if len(admission.ICE) != 1 || admission.ICEExpiresAt == nil {
		t.Fatal("admission did not automatically include relay path")
	}
	roles, err := f.identities.Roles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.call(t, "PUT", "/api/v1/channels/"+room.ID+"/permissions/"+roles[0].ID, f.owner.Token, map[string]any{"allow": 0, "deny": 4}, 204, nil)
	f.call(t, "GET", "/api/v1/voice/"+room.ID+"/ice", alice.Token, nil, 403, nil)
}
