package embeds

import (
	"context"
	"errors"
	"github.com/example-git/haunt-server/internal/publichttp"
	"net"
	"net/http"
	"net/netip"
	"net/url"
)

var ErrUnavailable = errors.New("embed_unavailable")
var ErrBusy = errors.New("embed_busy")

func ParseURL(raw string) (*url.URL, error) {
	u, err := publichttp.ParseURL(raw)
	if err != nil {
		return nil, ErrUnavailable
	}
	return u, nil
}
func publicIP(ip netip.Addr) bool { return publichttp.PublicIP(ip) }
func publicHost(host string) bool { return publichttp.PublicHost(host) }
func safeClient() *http.Client {
	c := publichttp.NewClient()
	redirect := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := redirect(req, via); err != nil {
			return ErrUnavailable
		}
		return nil
	}
	return c
}
func dialPublic(ctx context.Context, network, address string, lookup func(context.Context, string, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	conn, err := publichttp.DialPublic(ctx, network, address, lookup, dial)
	if err != nil {
		return nil, ErrUnavailable
	}
	return conn, nil
}
