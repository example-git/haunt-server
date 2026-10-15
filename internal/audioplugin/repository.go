package audioplugin

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var ErrIntegrity = errors.New("audio_plugin_hash_mismatch")
var ErrRepository = errors.New("audio_plugin_repository_unavailable")
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
var commitName = regexp.MustCompile(`^[a-f0-9]{40}$`)

// RepositoryInstall deliberately takes immutable identifiers, never branches,
// tags, download URLs, shell arguments or access tokens from a client.
type RepositoryInstall struct {
	Repository string `json:"repository" required:"true"`
	Commit     string `json:"commit" required:"true"`
	SHA256     string `json:"sha256" required:"true"`
}

func (r RepositoryInstall) URL() (string, error) {
	u, err := url.Parse(r.Repository)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !commitName.MatchString(r.Commit) || !hashName.MatchString(r.SHA256) {
		return "", ErrInvalid
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git"), "/")
	if len(parts) != 2 || !repositoryName.MatchString(parts[0]) || !repositoryName.MatchString(parts[1]) {
		return "", ErrInvalid
	}
	return "https://api.github.com/repos/" + parts[0] + "/" + parts[1] + "/contents/dist/haunt-plugin.zip?ref=" + r.Commit, nil
}

type RepositoryInstaller struct {
	client *http.Client
	token  string
	slots  chan struct{}
}

func NewRepositoryInstaller(token string) *RepositoryInstaller {
	transport := &http.Transport{Proxy: nil, MaxConnsPerHost: 2, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 10 * time.Second, TLSHandshakeTimeout: 5 * time.Second, MaxResponseHeaderBytes: 32 << 10}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host != "api.github.com" || port != "443" {
			return nil, ErrRepository
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, ErrRepository
		}
		for _, ip := range ips {
			if !repositoryIP(ip) {
				return nil, ErrRepository
			}
		}
		dialer := net.Dialer{Timeout: 5 * time.Second}
		for _, ip := range ips {
			if conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port)); err == nil {
				return conn, nil
			}
		}
		return nil, ErrRepository
	}
	return &RepositoryInstaller{client: &http.Client{Transport: transport, Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, token: token, slots: make(chan struct{}, 1)}
}

func repositoryIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is6() {
		return netip.MustParsePrefix("2000::/3").Contains(ip) && !netip.MustParsePrefix("2001::/23").Contains(ip) && !netip.MustParsePrefix("2002::/16").Contains(ip)
	}
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "168.63.129.16/32"} {
		if netip.MustParsePrefix(cidr).Contains(ip) {
			return false
		}
	}
	return true
}

func (i *RepositoryInstaller) Install(ctx context.Context, s *Store, r RepositoryInstall) (Plugin, error) {
	endpoint, err := r.URL()
	if err != nil {
		return Plugin{}, err
	}
	select {
	case i.slots <- struct{}{}:
		defer func() { <-i.slots }()
	default:
		return Plugin{}, ErrBusy
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Plugin{}, ErrInvalid
	}
	req.Header.Set("Accept", "application/vnd.github.raw+json")
	req.Header.Set("User-Agent", "Haunt-audio-plugin-installer/1")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if i.token != "" {
		req.Header.Set("Authorization", "Bearer "+i.token)
	}
	response, err := i.client.Do(req)
	if err != nil {
		return Plugin{}, ErrRepository
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > MaxBytes {
		return Plugin{}, ErrRepository
	}
	return s.InstallVerified(io.LimitReader(response.Body, MaxBytes+1), r.SHA256)
}

func (i *RepositoryInstaller) Close() { i.client.CloseIdleConnections() }
