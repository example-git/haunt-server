package embeds

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("embed_unavailable")
var ErrBusy = errors.New("embed_busy")

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

func publicIP(ip netip.Addr) bool {
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
func publicHost(host string) bool {
	host = strings.ToLower(host)
	for _, suffix := range []string{"localhost", "local", "internal", "test", "invalid", "example", "onion"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return false
		}
	}
	return !strings.Contains(host, "%") && (strings.Contains(host, ".") || strings.Contains(host, ":"))
}
func safeClient() *http.Client {
	transport := &http.Transport{
		Proxy: nil, MaxIdleConns: 16, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 4,
		IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 3 * time.Second,
		MaxResponseHeaderBytes: 32 << 10,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialer := net.Dialer{Timeout: 3 * time.Second}
		return dialPublic(ctx, network, address, net.DefaultResolver.LookupNetIP, dialer.DialContext)
	}

	return &http.Client{Transport: transport, Timeout: 6 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return ErrUnavailable
		}
		_, err := ParseURL(req.URL.String())
		return err
	}}
}

func dialPublic(ctx context.Context, network, address string, lookup func(context.Context, string, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !publicHost(host) {
		return nil, ErrUnavailable
	}
	ips, err := lookup(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, ErrUnavailable
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, ErrUnavailable
		}
	}
	// The only dial target is an already-validated IP; no second DNS resolution.
	for _, ip := range ips {
		conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, ErrUnavailable
}
