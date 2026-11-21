package publichttp

import (
	"context"
	"errors"
	"golang.org/x/net/proxy"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("public_http_unavailable")

func ParseURL(raw string) (*url.URL, error) {
	if len(raw) > 2048 || strings.ContainsAny(raw, "\\\x00\r\n\t") {
		return nil, ErrUnavailable
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, ErrUnavailable
	}
	if u.Port() != "" && !((u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443")) {
		return nil, ErrUnavailable
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	if u.Path == "" {
		u.Path = "/"
	}
	if strings.HasSuffix(u.Hostname(), ".") {
		return nil, ErrUnavailable
	}
	return u, nil
}

var blocked = func() []netip.Prefix {
	values := []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "168.63.129.16/32", "2001::/23", "2001:db8::/32", "3fff::/20", "2002::/16", "64:ff9b::/96", "64:ff9b:1::/48"}
	out := []netip.Prefix{}
	for _, s := range values {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func PublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range blocked {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
func PublicHost(host string) bool {
	host = strings.ToLower(host)
	for _, suffix := range []string{"localhost", "local", "internal", "test", "invalid", "example", "onion"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return false
		}
	}
	return !strings.Contains(host, "%") && (strings.Contains(host, ".") || strings.Contains(host, ":"))
}
func NewClient() *http.Client {
	return client(3*time.Second, 3*time.Second, 6*time.Second, nil)
}

func PageClient(relay *netip.AddrPort) *http.Client {
	return client(10*time.Second, 15*time.Second, 60*time.Second, relay)
}

func client(connect, headers, total time.Duration, relay *netip.AddrPort) *http.Client {
	transport := &http.Transport{
		Proxy: nil, MaxIdleConns: 16, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 4,
		IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: connect, ResponseHeaderTimeout: headers,
		MaxResponseHeaderBytes: 32 << 10,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, connect)
		defer cancel()
		dialer := &net.Dialer{Timeout: connect}
		dial := dialer.DialContext
		if relay != nil {
			if !PublicIP(relay.Addr()) || relay.Port() == 0 {
				return nil, ErrUnavailable
			}
			socks, err := proxy.SOCKS5("tcp", relay.String(), nil, dialer)
			if err != nil {
				return nil, err
			}
			dial = socks.(proxy.ContextDialer).DialContext
		}
		return DialPublic(dialCtx, network, address, net.DefaultResolver.LookupNetIP, dial)
	}

	return &http.Client{Transport: transport, Timeout: total, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return ErrUnavailable
		}
		_, err := ParseURL(req.URL.String())
		return err
	}}
}

func DialPublic(ctx context.Context, network, address string, lookup func(context.Context, string, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !PublicHost(host) {
		return nil, ErrUnavailable
	}
	ips, err := lookup(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, ErrUnavailable
	}
	for _, ip := range ips {
		if !PublicIP(ip) {
			return nil, ErrUnavailable
		}
	}
	var last error
	// The only dial target is an already-validated IP; no second DNS resolution.
	for _, ip := range ips {
		conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, errors.Join(ErrUnavailable, last)
}
