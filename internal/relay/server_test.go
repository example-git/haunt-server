package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/pion/stun/v4"
	"github.com/pion/turn/v5"
	"github.com/pion/webrtc/v4"
)

func relayConfig(t *testing.T) config.TURN {
	t.Helper()
	probe, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	return config.TURN{Enabled: true, PublicIP: "127.0.0.1", ListenAddr: "127.0.0.1:0", RelayBindIP: "127.0.0.1", RelayMin: uint16(port), RelayMax: uint16(port + 16), CredentialTTL: time.Minute, MaxAllocations: 8, AllowedPeers: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
}
func TestRelayWebRTCBetweenClients(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			service, err := New(relayConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			credentials, err := service.Credentials(identity.Principal{SessionID: identity.NewID()})
			if err != nil {
				t.Fatal(err)
			}
			server := credentials.Servers[0]
			selected := ""
			for _, url := range server.URLs {
				if strings.HasSuffix(url, "transport="+transport) {
					selected = url
				}
			}
			cfg := webrtc.Configuration{ICETransportPolicy: webrtc.ICETransportPolicyRelay, ICEServers: []webrtc.ICEServer{{URLs: []string{selected}, Username: server.Username, Credential: server.Credential}}}
			a, err := webrtc.NewPeerConnection(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			b, err := webrtc.NewPeerConnection(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			delivered := make(chan string, 2)
			b.OnDataChannel(func(channel *webrtc.DataChannel) {
				channel.OnMessage(func(message webrtc.DataChannelMessage) { channel.SendText("echo:" + string(message.Data)) })
			})
			channel, err := a.CreateDataChannel("relay-test", nil)
			if err != nil {
				t.Fatal(err)
			}
			channel.OnOpen(func() { channel.SendText("encrypted peer payload") })
			channel.OnMessage(func(message webrtc.DataChannelMessage) { delivered <- string(message.Data) })
			offer, err := a.CreateOffer(nil)
			if err != nil {
				t.Fatal(err)
			}
			gathered := webrtc.GatheringCompletePromise(a)
			if err := a.SetLocalDescription(offer); err != nil {
				t.Fatal(err)
			}
			select {
			case <-gathered:
			case <-time.After(8 * time.Second):
				t.Fatal("relay offer gather timed out")
			}
			if err := b.SetRemoteDescription(*a.LocalDescription()); err != nil {
				t.Fatal(err)
			}
			answer, err := b.CreateAnswer(nil)
			if err != nil {
				t.Fatal(err)
			}
			gathered = webrtc.GatheringCompletePromise(b)
			if err := b.SetLocalDescription(answer); err != nil {
				t.Fatal(err)
			}
			select {
			case <-gathered:
			case <-time.After(8 * time.Second):
				t.Fatal("relay answer gather timed out")
			}
			if err := a.SetRemoteDescription(*b.LocalDescription()); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-delivered:
				if got != "echo:encrypted peer payload" {
					t.Fatal("peer payload changed")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("relay-only peer exchange timed out")
			}
			if service.Stats()["allocations"].(int64) < 2 {
				t.Fatal("WebRTC did not allocate both relay paths")
			}
			a.Close()
			b.Close()
			service.Close()
			if service.Stats()["allocations"].(int64) != 0 {
				t.Fatal("relay leaked allocations")
			}
		})
	}
}
func TestRelayAuthenticationPermissionsAndAllocationBounds(t *testing.T) {
	cfg := relayConfig(t)
	cfg.MaxAllocations = 1
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, username := range []string{"invalid", "0:" + identity.NewID(), strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + ":" + identity.NewID()} {
		if _, _, ok := s.authenticate(&turn.RequestAttributes{Username: username, Realm: "haunt"}); ok {
			t.Fatal("invalid/expired credentials accepted")
		}
	}
	for _, ip := range []string{"169.254.169.254", "10.0.0.1", "192.168.0.1", "::1", "224.0.0.1", "0.0.0.0"} {
		if s.permission(nil, net.ParseIP(ip)) {
			t.Fatal("private/special relay peer permitted", ip)
		}
	}
	if !s.permission(nil, net.ParseIP("198.51.100.1")) || !s.permission(nil, net.ParseIP("127.0.0.1")) {
		t.Fatal("public/explicit peer denied")
	}
	conn, _, err := s.allocator.AllocatePacketConn(turn.AllocateListenerConfig{Network: "udp4"})
	if err != nil {
		t.Fatal(err)
	}
	if extra, _, err := s.allocator.AllocatePacketConn(turn.AllocateListenerConfig{Network: "udp4"}); err == nil {
		extra.Close()
		t.Fatal("allocation bound exceeded")
	}
	conn.Close()
	conn.Close()
	if s.allocator.active.Load() != 0 {
		t.Fatal("allocation release was not idempotent")
	}
	if conn, _, err := s.allocator.AllocatePacketConn(turn.AllocateListenerConfig{Network: "udp4", RequestedPort: 1}); err == nil {
		conn.Close()
		t.Fatal("escaped relay port range")
	}
	// Check the real network authentication path with a forged password.
	credentials, _ := s.Credentials(identity.Principal{SessionID: identity.NewID()})
	url, err := stun.ParseURI(credentials.Servers[0].URLs[0])
	if err != nil {
		t.Fatal(err)
	}
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	client, err := turn.NewClient(&turn.ClientConfig{Conn: socket, TURNServerAddr: net.JoinHostPort(url.Host, strconv.Itoa(url.Port)), Username: credentials.Servers[0].Username, Password: "forged", RTO: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if allocated, err := client.AllocateWithContext(ctx); err == nil {
		allocated.Close()
		t.Fatal("forged relay password accepted")
	}
}
func TestRelayTLSListener(t *testing.T) {
	cfg := relayConfig(t)
	cfg.TLSAddr = "127.0.0.1:0"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.TLSCert = filepath.Join(dir, "cert.pem")
	cfg.TLSKey = filepath.Join(dir, "key.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	os.WriteFile(cfg.TLSCert, cert, 0600)
	os.WriteFile(cfg.TLSKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600)
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	credentials, _ := server.Credentials(identity.Principal{SessionID: identity.NewID()})
	value := credentials.Servers[0]
	url, err := stun.ParseURI(value.URLs[2])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	address := net.JoinHostPort(url.Host, strconv.Itoa(url.Port))
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", address, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client, err := turn.NewClient(&turn.ClientConfig{Conn: turn.NewSTUNConn(conn), TURNServerAddr: address, Username: value.Username, Password: value.Credential})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	allocation, err := client.AllocateWithContext(ctx)
	if err != nil {
		t.Fatal("authenticated TLS relay allocation failed", err)
	}
	allocation.Close()
}
