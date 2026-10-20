// Package relay owns an optional authenticated TURN service. It relays opaque
// WebRTC packets between clients and does not decode or terminate their DTLS.
package relay

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/pion/turn/v5"
	"golang.org/x/net/netutil"
)

type Credentials struct {
	Servers   []config.ICEServer `json:"ice_servers"`
	ExpiresAt time.Time          `json:"expires_at"`
}
type Server struct {
	server    *turn.Server
	cfg       config.TURN
	secret    [32]byte
	urls      []string
	allocator *allocator
	closed    atomic.Bool
}

func New(c config.TURN) (*Server, error) {
	if !c.Enabled {
		return nil, nil
	}
	s := &Server{cfg: c}
	if _, err := rand.Read(s.secret[:]); err != nil {
		return nil, err
	}
	if c.MaxAllocations < 1 || c.CredentialTTL <= 0 || c.RelayMin == 0 || c.RelayMin > c.RelayMax || c.RelayMax == 65535 {
		return nil, errors.New("invalid TURN resource configuration")
	}
	generator := &turn.RelayAddressGeneratorPortRange{RelayAddress: net.ParseIP(c.PublicIP), Address: c.RelayBindIP, MinPort: c.RelayMin, MaxPort: c.RelayMax, MaxRetries: 64}
	s.allocator = &allocator{generator: generator, max: int64(c.MaxAllocations), connections: map[*countedPacketConn]struct{}{}, users: map[string]int{}}
	udp, err := net.ListenPacket("udp", c.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("TURN UDP listen: %w", err)
	}
	listeners := []net.Listener{}
	success := false
	defer func() {
		if !success {
			udp.Close()
			for _, listener := range listeners {
				listener.Close()
			}
		}
	}()
	tcp, err := net.Listen("tcp", c.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("TURN TCP listen: %w", err)
	}
	tcp = ownListener(netutil.LimitListener(tcp, c.MaxAllocations*2))
	listeners = append(listeners, tcp)
	publicHost := c.PublicHost
	if publicHost == "" {
		publicHost = c.PublicIP
	}
	_, udpPort, _ := net.SplitHostPort(udp.LocalAddr().String())
	_, tcpPort, _ := net.SplitHostPort(tcp.Addr().String())
	s.urls = []string{"turn:" + net.JoinHostPort(publicHost, udpPort) + "?transport=udp", "turn:" + net.JoinHostPort(publicHost, tcpPort) + "?transport=tcp"}
	if c.TLSAddr != "" {
		certificate, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
		if err != nil {
			return nil, errors.New("could not load TURN TLS certificate/key")
		}
		listener, err := tls.Listen("tcp", c.TLSAddr, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
		if err != nil {
			return nil, fmt.Errorf("TURN TLS listen: %w", err)
		}
		listener = ownListener(netutil.LimitListener(listener, c.MaxAllocations*2))
		listeners = append(listeners, listener)
		_, port, _ := net.SplitHostPort(listener.Addr().String())
		s.urls = append(s.urls, "turns:"+net.JoinHostPort(publicHost, port)+"?transport=tcp")
	}
	settings := turn.ServerConfig{Realm: "haunt", AuthHandler: s.authenticate, AllocationLifetime: 10 * time.Minute, PacketConnConfigs: []turn.PacketConnConfig{{PacketConn: udp, RelayAddressGenerator: s.allocator, PermissionHandler: s.permission}}}
	for _, listener := range listeners {
		settings.ListenerConfigs = append(settings.ListenerConfigs, turn.ListenerConfig{Listener: listener, RelayAddressGenerator: s.allocator, PermissionHandler: s.permission})
	}
	s.server, err = turn.NewServer(settings)
	if err != nil {
		return nil, err
	}
	success = true
	return s, nil
}
func (s *Server) password(username string) string {
	// TURN REST time-windowed credentials use HMAC-SHA1; the shared signing key
	// remains server-local and only the expiring derived password is returned.
	mac := hmac.New(sha1.New, s.secret[:])
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Server) Credentials(p identity.Principal) (Credentials, error) {
	if s == nil || s.closed.Load() {
		return Credentials{}, identity.ErrNotFound
	}
	expires := time.Now().Add(s.cfg.CredentialTTL).Truncate(time.Second)
	username := strconv.FormatInt(expires.Unix(), 10) + ":" + p.SessionID
	return Credentials{Servers: []config.ICEServer{{URLs: append([]string{}, s.urls...), Username: username, Credential: s.password(username)}}, ExpiresAt: expires}, nil
}
func (s *Server) authenticate(a *turn.RequestAttributes) (string, []byte, bool) {
	if s.closed.Load() || a.Realm != "haunt" || len(a.Username) > 128 {
		return "", nil, false
	}
	raw, id, ok := strings.Cut(a.Username, ":")
	expiry, err := strconv.ParseInt(raw, 10, 64)
	if !ok || err != nil || !identity.ValidID(id) || expiry <= time.Now().Unix() || expiry > time.Now().Add(s.cfg.CredentialTTL+time.Minute).Unix() {
		return "", nil, false
	}
	return id, turn.GenerateAuthKey(a.Username, a.Realm, s.password(a.Username)), true
}
func (s *Server) permission(_ net.Addr, peer net.IP) bool {
	ip, ok := netip.AddrFromSlice(peer)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	for _, prefix := range s.cfg.AllowedPeers {
		if prefix.Contains(ip) {
			return true
		}
	}
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback()
}
func (s *Server) Close() error {
	if s == nil || s.closed.Swap(true) {
		return nil
	}
	err := s.server.Close()
	s.allocator.close()
	return err
}
func (s *Server) Stats() map[string]any {
	if s == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{"enabled": true, "allocations": s.allocator.active.Load(), "max_allocations": s.cfg.MaxAllocations, "allocation_rejections": s.allocator.rejected.Load()}
}

// The allocator provides a strict concurrent allocation bound, checks requested
// ports, and limits relayed traffic to UDP (including clients using TURN TCP/TLS).
type allocator struct {
	generator   *turn.RelayAddressGeneratorPortRange
	max         int64
	active      atomic.Int64
	rejected    atomic.Uint64
	mu          sync.Mutex
	closed      bool
	connections map[*countedPacketConn]struct{}
	users       map[string]int
	pending     sync.WaitGroup
}

func (a *allocator) Validate() error { return a.generator.Validate() }
func (a *allocator) AllocatePacketConn(c turn.AllocateListenerConfig) (net.PacketConn, net.Addr, error) {
	if c.RequestedPort != 0 && (c.RequestedPort < int(a.generator.MinPort) || c.RequestedPort > int(a.generator.MaxPort)) {
		return nil, nil, errors.New("relay port outside configured range")
	}
	a.mu.Lock()
	if a.closed || a.active.Load() >= a.max || a.users[c.UserID] >= 64 {
		a.mu.Unlock()
		a.rejected.Add(1)
		return nil, nil, errors.New("relay capacity reached")
	}
	a.active.Add(1)
	a.users[c.UserID]++
	a.pending.Add(1)
	a.mu.Unlock()
	defer a.pending.Done()
	releaseLocked := func() {
		a.active.Add(-1)
		a.users[c.UserID]--
		if a.users[c.UserID] == 0 {
			delete(a.users, c.UserID)
		}
	}
	conn, address, err := a.generator.AllocatePacketConn(c)
	if err != nil {
		a.mu.Lock()
		releaseLocked()
		a.mu.Unlock()
		return nil, nil, err
	}
	owned := &countedPacketConn{PacketConn: conn}
	owned.release = func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		// Shutdown must not observe a removed socket with an outstanding count.
		delete(a.connections, owned)
		releaseLocked()
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		owned.Close()
		return nil, nil, net.ErrClosed
	}
	a.connections[owned] = struct{}{}
	a.mu.Unlock()
	return owned, address, nil
}
func (a *allocator) AllocateListener(turn.AllocateListenerConfig) (net.Listener, net.Addr, error) {
	return nil, nil, errors.New("TCP relay allocations are unsupported")
}
func (a *allocator) AllocateConn(turn.AllocateConnConfig) (net.Conn, error) {
	return nil, errors.New("TCP relay allocations are unsupported")
}

type countedPacketConn struct {
	net.PacketConn
	once    sync.Once
	release func()
}

func (c *countedPacketConn) Close() error {
	err := c.PacketConn.Close()
	c.once.Do(c.release)
	return err
}

func (a *allocator) close() {
	a.mu.Lock()
	a.closed = true
	connections := make([]*countedPacketConn, 0, len(a.connections))
	for conn := range a.connections {
		connections = append(connections, conn)
	}
	a.mu.Unlock()
	for _, conn := range connections {
		conn.Close()
	}
	a.pending.Wait()
}

// Closing a TURN listener must close accepted connections too. Pion manages
// protocol state; this layer owns socket lifetime and bounded idle/write waits.
type ownedListener struct {
	net.Listener
	mu          sync.Mutex
	closed      bool
	connections map[*ownedConn]struct{}
}

func ownListener(listener net.Listener) *ownedListener {
	return &ownedListener{Listener: listener, connections: map[*ownedConn]struct{}{}}
}
func (l *ownedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	owned := &ownedConn{Conn: conn, listener: l}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		conn.Close()
		return nil, net.ErrClosed
	}
	l.connections[owned] = struct{}{}
	l.mu.Unlock()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	return owned, nil
}
func (l *ownedListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	connections := make([]*ownedConn, 0, len(l.connections))
	for conn := range l.connections {
		connections = append(connections, conn)
	}
	l.mu.Unlock()
	err := l.Listener.Close()
	for _, conn := range connections {
		conn.Close()
	}
	return err
}

type ownedConn struct {
	net.Conn
	listener *ownedListener
	once     sync.Once
}

func (c *ownedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err == nil {
		c.Conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
	}
	return n, err
}
func (c *ownedConn) Write(p []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.Conn.Write(p)
}
func (c *ownedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.listener.mu.Lock(); delete(c.listener.connections, c); c.listener.mu.Unlock() })
	return err
}
