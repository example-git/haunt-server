package clientidentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/example-git/haunt-server/internal/ai/provider"
)

type Snapshot struct {
	Version    string
	UserAgent  string
	Originator string
}
type Manager struct {
	mu       sync.RWMutex
	path     string
	versions map[provider.ID]string
	client   *http.Client
}

var versionPattern = regexp.MustCompile(`^\d+\.\d+[\w.-]*$`)
var fallbacks = map[provider.ID]string{provider.Claude: "2.1.170", provider.Codex: "0.146.0", provider.Antigravity: "1.0.9"}
var environments = map[provider.ID]string{provider.Claude: "CLAUDE_CODE_VERSION", provider.Codex: "CODEX_VERSION", provider.Antigravity: "ANTIGRAVITY_CLI_VERSION"}

func New(path string) (*Manager, error) {
	m := &Manager{path: path, versions: map[provider.ID]string{}, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if len(b) > 0 {
			if err = json.Unmarshal(b, &m.versions); err != nil {
				return nil, fmt.Errorf("invalid provider version cache: %w", err)
			}
		}
	}
	if m.versions == nil {
		m.versions = map[provider.ID]string{}
	}
	for id, v := range fallbacks {
		if !versionPattern.MatchString(m.versions[id]) {
			m.versions[id] = v
		}
		if _, err := m.Snapshot(id); err != nil {
			return nil, err
		}
	}
	return m, nil
}
func (m *Manager) Snapshot(id provider.ID) (Snapshot, error) {
	m.mu.RLock()
	v, ok := m.versions[id]
	m.mu.RUnlock()
	if !ok {
		return Snapshot{}, fmt.Errorf("unknown provider %q", id)
	}
	if explicit, exists := os.LookupEnv(environments[id]); exists {
		if !versionPattern.MatchString(explicit) {
			return Snapshot{}, fmt.Errorf("%s contains an invalid version", environments[id])
		}
		v = explicit
	}
	s := Snapshot{Version: v}
	switch id {
	case provider.Claude:
		s.UserAgent = "claude-cli/" + v + " (external, cli)"
	case provider.Codex:
		s.Originator = "codex_cli_rs"
		if o := os.Getenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE"); o != "" {
			if !safeHeader(o) {
				return s, errors.New("invalid CODEX_INTERNAL_ORIGINATOR_OVERRIDE")
			}
			s.Originator = o
		}
		platform := map[string]string{"linux": "Linux", "darwin": "Mac OS", "windows": "Windows"}[runtime.GOOS]
		if platform == "" {
			platform = runtime.GOOS
		}
		release := "14.0"
		if runtime.GOOS == "darwin" {
			if b, err := os.ReadFile("/System/Library/CoreServices/SystemVersion.plist"); err == nil {
				if match := regexp.MustCompile(`<key>ProductVersion</key>\s*<string>([^<]+)</string>`).FindSubmatch(b); len(match) == 2 {
					release = string(match[1])
				}
			}
		}
		terminal := os.Getenv("TERM_PROGRAM")
		if terminal != "" && os.Getenv("TERM_PROGRAM_VERSION") != "" {
			terminal += "/" + os.Getenv("TERM_PROGRAM_VERSION")
		}
		if terminal == "" {
			terminal = os.Getenv("TERM")
		}
		if terminal == "" {
			terminal = "unknown"
		}
		if !safeHeader(terminal) || !safeHeader(release) {
			return s, errors.New("invalid native terminal identity")
		}
		s.UserAgent = fmt.Sprintf("%s/%s (%s %s; %s) %s", s.Originator, v, platform, release, runtime.GOARCH, terminal)
	case provider.Antigravity:
		s.UserAgent = fmt.Sprintf("antigravity/cli/%s %s/%s", v, runtime.GOOS, runtime.GOARCH)
	}
	return s, nil
}
func safeHeader(s string) bool {
	if len(s) > 512 {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}
func (m *Manager) Refresh(ctx context.Context) error {
	var failures []error
	for _, id := range []provider.ID{provider.Claude, provider.Codex, provider.Antigravity} {
		if _, set := os.LookupEnv(environments[id]); set {
			if _, err := m.Snapshot(id); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		v, err := m.discover(ctx, id)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		m.mu.Lock()
		m.versions[id] = v
		m.mu.Unlock()
	}
	m.mu.RLock()
	b, err := json.Marshal(m.versions)
	m.mu.RUnlock()
	if err == nil && m.path != "" {
		err = os.MkdirAll(filepath.Dir(m.path), 0700)
		if err == nil {
			var f *os.File
			f, err = os.CreateTemp(filepath.Dir(m.path), ".provider-versions-*")
			if err == nil {
				name := f.Name()
				defer os.Remove(name)
				if err = f.Chmod(0600); err == nil {
					_, err = f.Write(b)
				}
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
				if err == nil {
					err = os.Rename(name, m.path)
				}
			}
		}
	}
	if err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
func (m *Manager) Run(ctx context.Context) {
	_ = m.Refresh(ctx)
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = m.Refresh(ctx)
		}
	}
}
func (m *Manager) discover(ctx context.Context, id provider.ID) (string, error) {
	method := http.MethodGet
	endpoint := "https://downloads.claude.ai/claude-code-releases/latest"
	if id == provider.Codex {
		method = http.MethodHead
		endpoint = "https://github.com/openai/codex/releases/latest"
	}
	if id == provider.Antigravity {
		platform := runtime.GOOS + "_" + runtime.GOARCH
		if runtime.GOOS == "linux" {
			for _, p := range []string{"/lib/libc.musl-x86_64.so.1", "/lib/libc.musl-aarch64.so.1"} {
				if _, err := os.Stat(p); err == nil {
					platform += "_musl"
					break
				}
			}
		}
		endpoint = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/" + platform + ".json"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s version discovery unavailable", id)
	}
	defer resp.Body.Close()
	candidate := ""
	if id == provider.Codex {
		u, err := url.Parse(resp.Header.Get("Location"))
		if err == nil && resp.StatusCode >= 300 && resp.StatusCode < 400 && u.Hostname() == "github.com" && u.Scheme == "https" {
			candidate = strings.TrimPrefix(u.Path, "/openai/codex/releases/tag/rust-v")
		}
	} else if resp.StatusCode == 200 {
		b, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
		if err == nil && len(b) <= 64<<10 {
			if id == provider.Claude {
				candidate = strings.TrimSpace(string(b))
			} else {
				var d struct {
					Version string `json:"version"`
				}
				if json.Unmarshal(b, &d) == nil {
					candidate = d.Version
				}
			}
		}
	}
	if !versionPattern.MatchString(candidate) {
		return "", fmt.Errorf("%s version metadata is invalid or unavailable", id)
	}
	return candidate, nil
}
