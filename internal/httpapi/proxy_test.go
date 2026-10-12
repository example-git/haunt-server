package httpapi

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestTrustedProxyBoundary(t *testing.T) {
	a := &api{trustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128")}}
	for _, tc := range []struct{ peer, header, want string }{
		{"127.0.0.1:50", "198.51.100.8", "198.51.100.8"},
		{"[::1]:50", "2001:db8::5", "2001:db8::5"},
		{"198.51.100.2:50", "198.51.100.8", "198.51.100.2"},
		{"127.0.0.1:50", "198.51.100.8, 198.51.100.9", "127.0.0.1"},
		{"127.0.0.1:50", "bad", "127.0.0.1"},
		{"127.0.0.1:50", "fe80::1%eth0", "127.0.0.1"},
		{"[::ffff:127.0.0.1]:50", "::ffff:198.51.100.8", "198.51.100.8"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("X-Forwarded-For", tc.header)
		if got := a.remoteIP(r); got != tc.want {
			t.Errorf("%q %q => %q, want %q", tc.peer, tc.header, got, tc.want)
		}
	}
}

func TestCloudflaredProxyBoundary(t *testing.T) {
	a := &api{cloudflared: true, trustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	for _, tc := range []struct{ peer, header, want string }{
		{"127.0.0.1:50", "198.51.100.8", "198.51.100.8"},
		{"198.51.100.2:50", "198.51.100.8", "198.51.100.2"},
		{"127.0.0.1:50", "", "127.0.0.1"},
		{"127.0.0.1:50", "198.51.100.8, 198.51.100.9", "127.0.0.1"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("CF-Connecting-IP", tc.header)
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
		if got := a.remoteIP(r); got != tc.want {
			t.Errorf("%q %q => %q, want %q", tc.peer, tc.header, got, tc.want)
		}
	}
}
