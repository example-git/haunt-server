package tools

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/example-git/haunt-server/internal/publichttp"
)

var proxySources = []string{
	"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt",
	"https://api.proxyscrape.com/v4/free-proxy-list/get?request=display_proxies&proxy_format=ipport&format=text&protocol=socks5&timeout=10000",
}

type proxyPool struct {
	mu      sync.Mutex
	entries []netip.AddrPort
	next    int
}

func (p *proxyPool) refresh(ctx context.Context, cl *http.Client) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		var entries []netip.AddrPort
		seen := map[netip.AddrPort]bool{}
		for _, source := range proxySources {
			list := loadProxies(ctx, cl, source)
			for _, addr := range list {
				if !seen[addr] {
					entries = append(entries, addr)
					seen[addr] = true
				}
			}
		}
		if len(entries) > 0 {
			p.mu.Lock()
			p.entries = entries
			p.next = 0
			p.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func loadProxies(parent context.Context, cl *http.Client, source string) []netip.AddrPort {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", source, nil)
	if err != nil {
		return nil
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	var result []netip.AddrPort
	for line := range strings.Lines(string(b)) {
		address, err := netip.ParseAddrPort(strings.TrimPrefix(strings.TrimSpace(line), "socks5://"))
		if err == nil && publichttp.PublicIP(address.Addr()) && address.Port() != 0 {
			result = append(result, address)
			if len(result) == 512 {
				break
			}
		}
	}
	return result
}
func (p *proxyPool) candidates() []netip.AddrPort {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := min(2, len(p.entries))
	var result []netip.AddrPort
	for range count {
		result = append(result, p.entries[p.next%len(p.entries)])
		p.next = (p.next + 1) % len(p.entries)
	}
	return result
}
