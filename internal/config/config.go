// Package config loads and validates per-instance configuration.
// A Config is immutable after startup and may be shared by request handlers.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Policy struct {
	DM                      DMPolicy `json:"dm,omitempty"`
	TextMaxBytes            int64    `json:"text_max_bytes"`
	TextRetentionDays       int64    `json:"text_retention_days"`
	ImageMaxBytes           int64    `json:"image_max_bytes"`
	VideoMaxBytes           int64    `json:"video_max_bytes"`
	AttachmentMaxBytes      int64    `json:"attachment_max_bytes"`
	AttachmentRetentionDays int64    `json:"attachment_retention_days"`
}

type Config struct {
	InstanceID              string
	Name                    string
	PublicURL               string
	ListenAddr              string
	DatabaseURL             string `json:"-"`
	DBMaxConns              int32
	ClientOrigins           []string
	ClientUpdateManifestURL string
	ClientUpdateInstallers  bool
	ClientUpdateLocal       bool
	TrustedProxies          []netip.Prefix
	Policy                  Policy
	BootstrapUsername       string
	BootstrapPassword       string `json:"-"`
	SessionDays             int64
	AuthParallel            int
	DataDir                 string
	FFprobe                 string
	AudioPluginGitHubToken  string `json:"-"`
	DefaultAudioPlugin      bool
	ICE                     []ICEServer `json:"-"`
	Cloudflared             bool
	MediaRelayOnly          bool
	CloudflareTURNKeyID     string
	CloudflareTURNToken     string `json:"-"`
	MediaPublicIP           string
	MediaPortMin            uint16
	MediaPortMax            uint16
	MediaMaxPeers           int
	MediaRoomLimit          int
	TURN                    TURN
}

type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Load applies defaults only to omitted values. Explicit empty, malformed, or
// out-of-range values fail rather than silently enabling a different behavior.
func Load() (Config, error) {
	var c Config
	var err error
	for _, item := range []struct {
		key string
		dst *string
	}{
		{"HAUNT_INSTANCE_ID", &c.InstanceID},
		{"HAUNT_PUBLIC_URL", &c.PublicURL},
		{"HAUNT_DATABASE_URL", &c.DatabaseURL},
	} {
		if *item.dst, err = required(item.key); err != nil {
			return Config{}, err
		}
	}
	if c.Name, err = textDefault("HAUNT_NAME", "Haunt"); err != nil {
		return Config{}, err
	}
	if c.ListenAddr, err = textDefault("HAUNT_LISTEN_ADDR", "127.0.0.1:8080"); err != nil {
		return Config{}, err
	}
	_, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return Config{}, fmt.Errorf("HAUNT_LISTEN_ADDR must have host:port form")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return Config{}, fmt.Errorf("HAUNT_LISTEN_ADDR requires a port from 1 to 65535")
	}
	if c.PublicURL, err = origin(c.PublicURL); err != nil {
		return Config{}, fmt.Errorf("HAUNT_PUBLIC_URL: %w", err)
	}
	maxConns, err := number("HAUNT_DB_MAX_CONNS", 16, 2, 1024)
	if err != nil {
		return Config{}, err
	}
	c.DBMaxConns = int32(maxConns)
	c.ClientOrigins = []string{c.PublicURL}
	if raw, present := os.LookupEnv("HAUNT_TRUSTED_PROXIES"); present {
		for _, value := range strings.Split(raw, ",") {
			prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
			if err != nil || prefix.Bits() == 0 || len(c.TrustedProxies) >= 32 {
				return Config{}, fmt.Errorf("HAUNT_TRUSTED_PROXIES requires at most 32 specific CIDR ranges")
			}
			c.TrustedProxies = append(c.TrustedProxies, prefix.Masked())
		}
	}
	if raw, present := os.LookupEnv("HAUNT_CLIENT_ORIGINS"); present {
		for _, value := range strings.Split(raw, ",") {
			allowed, err := origin(strings.TrimSpace(value))
			if err != nil {
				return Config{}, fmt.Errorf("HAUNT_CLIENT_ORIGINS: %w", err)
			}
			c.ClientOrigins = append(c.ClientOrigins, allowed)
		}
	}
	for _, item := range []struct {
		key      string
		fallback int64
		max      int64
		dst      *int64
	}{
		{"HAUNT_TEXT_MAX_BYTES", 256_000_000, 1 << 50, &c.Policy.TextMaxBytes},
		{"HAUNT_TEXT_RETENTION_DAYS", 30, 36500, &c.Policy.TextRetentionDays},
		{"HAUNT_IMAGE_MAX_BYTES", 2 << 20, 1 << 50, &c.Policy.ImageMaxBytes},
		{"HAUNT_VIDEO_MAX_BYTES", 10 << 20, 1 << 50, &c.Policy.VideoMaxBytes},
		{"HAUNT_ATTACHMENT_MAX_BYTES", 1 << 30, 1 << 50, &c.Policy.AttachmentMaxBytes},
		{"HAUNT_ATTACHMENT_RETENTION_DAYS", 7, 36500, &c.Policy.AttachmentRetentionDays},
	} {
		if *item.dst, err = number(item.key, item.fallback, 1, item.max); err != nil {
			return Config{}, err
		}
	}
	if c.Policy.ImageMaxBytes > c.Policy.AttachmentMaxBytes || c.Policy.VideoMaxBytes > c.Policy.AttachmentMaxBytes {
		return Config{}, fmt.Errorf("attachment budget must accommodate each configured per-file limit")
	}
	if len(c.InstanceID) > 128 || strings.TrimSpace(c.InstanceID) != c.InstanceID {
		return Config{}, fmt.Errorf("HAUNT_INSTANCE_ID must contain 1 to 128 bytes without surrounding whitespace")
	}
	c.BootstrapUsername, _ = os.LookupEnv("HAUNT_BOOTSTRAP_USERNAME")
	c.BootstrapPassword, _ = os.LookupEnv("HAUNT_BOOTSTRAP_PASSWORD")
	_, usernameSet := os.LookupEnv("HAUNT_BOOTSTRAP_USERNAME")
	_, passwordSet := os.LookupEnv("HAUNT_BOOTSTRAP_PASSWORD")
	if usernameSet != passwordSet || (usernameSet && (c.BootstrapUsername == "" || c.BootstrapPassword == "")) {
		return Config{}, fmt.Errorf("bootstrap username and password must both be set to non-empty values or both omitted")
	}
	if c.SessionDays, err = number("HAUNT_SESSION_DAYS", 30, 1, 365); err != nil {
		return Config{}, err
	}
	parallel, err := number("HAUNT_AUTH_PARALLEL", 4, 1, 32)
	if err != nil {
		return Config{}, err
	}
	c.AuthParallel = int(parallel)
	if err := loadClientUpdates(&c); err != nil {
		return Config{}, err
	}
	if err := loadAudioPlugins(&c); err != nil {
		return Config{}, err
	}
	if c.DataDir, err = textDefault("HAUNT_DATA_DIR", "data"); err != nil {
		return Config{}, err
	}
	if c.FFprobe, err = textDefault("HAUNT_FFPROBE", "ffprobe"); err != nil {
		return Config{}, err
	}
	if c.Policy.TextMaxBytes < 4096 {
		return Config{}, fmt.Errorf("HAUNT_TEXT_MAX_BYTES must be at least 4096")
	}
	if err := loadMedia(&c); err != nil {
		return Config{}, err
	}
	if err := loadExternalMedia(&c); err != nil {
		return Config{}, err
	}
	if err := loadTURN(&c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func required(key string) (string, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must be set to a non-empty value", key)
	}
	return value, nil
}

func textDefault(key, fallback string) (string, error) {
	if _, ok := os.LookupEnv(key); !ok {
		return fallback, nil
	}
	return required(key)
}

func number(key string, fallback, min, max int64) (int64, error) {
	raw, present := os.LookupEnv(key)
	if !present {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("%s must be an integer from %d to %d", key, min, max)
	}
	return value, nil
}

func origin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Hostname() == "" || u.User != nil ||
		strings.Contains(u.Host, "*") || strings.HasSuffix(u.Host, ":") || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("expected an origin without credentials, path, query, or fragment")
	}
	local := strings.EqualFold(u.Hostname(), "localhost")
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		local = ip.IsLoopback() || ip.IsPrivate()
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return "", fmt.Errorf("HTTPS is required except for localhost, loopback, or private-network IP origins")
	}
	port := u.Port()
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", fmt.Errorf("origin port must be from 1 to 65535")
		}
	}
	host := strings.ToLower(u.Host)
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		// Browsers omit default ports from the Origin header.
		host = strings.ToLower(u.Hostname())
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	return u.Scheme + "://" + host, nil
}
