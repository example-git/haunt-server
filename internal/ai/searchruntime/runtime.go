package searchruntime

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
)

//go:embed bundle/source.json bundle/COPYING serve.py
var assets embed.FS

type Runtime struct {
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	dir         string
	installed   string
	processDone chan struct{}
	client      *http.Client
	socketDir   string
}

func New(parent context.Context, dataDir string) *Runtime {
	ctx, cancel := context.WithCancel(parent)
	return &Runtime{ctx: ctx, cancel: cancel, dir: filepath.Join(dataDir, "ai-search")}
}

func (r *Runtime) Client() (*http.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	if r.processDone != nil {
		select {
		case <-r.processDone:
			r.cleanup()
		default:
			return r.client, nil
		}
	}
	if r.installed == "" {
		dir, err := r.install()
		if err != nil {
			return nil, err
		}
		r.installed = dir
	}
	socketDir, err := os.MkdirTemp("", "haunt-search-")
	if err != nil {
		return nil, err
	}
	r.socketDir = socketDir
	ok := false
	defer func() {
		if !ok {
			r.cleanup()
		}
	}()
	socketPath := filepath.Join(socketDir, "api.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	fd, err := listener.File()
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	config := filepath.Join(socketDir, "settings.yml")
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return nil, err
	}
	encodedSecret := hex.EncodeToString(secret[:])
	secretPath := filepath.Join(r.dir, "config-secret")
	if existing, err := os.ReadFile(secretPath); err == nil {
		if _, err = hex.DecodeString(string(existing)); err != nil || len(existing) != 64 {
			return nil, errors.New("invalid instance search secret")
		}
		encodedSecret = string(existing)
	} else if errors.Is(err, os.ErrNotExist) {
		f, err := os.OpenFile(secretPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		_, writeErr := io.WriteString(f, encodedSecret)
		if err = errors.Join(writeErr, f.Close()); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(socketDir, "limiter.toml"), nil, 0600); err != nil {
		return nil, err
	}
	// keep_only selects which engines to load. Upstream disables Brave for default queries.
	settings := fmt.Sprintf(`use_default_settings:
  engines:
    keep_only: [brave, duckduckgo]
engines:
  - name: brave
    disabled: false
  - name: duckduckgo
    disabled: false
server:
  secret_key: '%s'
  limiter: false
search:
  formats: [json]
outgoing:
  request_timeout: 8
  max_request_timeout: 18
`, encodedSecret)
	if err = os.WriteFile(config, []byte(settings), 0600); err != nil {
		return nil, err
	}
	dir := r.installed
	configDir := filepath.Join(r.dir, "cache")
	if err = os.MkdirAll(configDir, 0700); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(r.ctx, filepath.Join(dir, "bin/loader"), "--library-path", filepath.Join(dir, "lib"), filepath.Join(dir, "bin/python"), "-sB", filepath.Join(dir, "serve.py"))
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin"), "__SEARXNG_VERSION=2026.11.21-c6ee3e9fd", "HOME=" + configDir, "LANG=C.UTF-8", "PYTHONHOME=" + dir, "PYTHONPATH=" + filepath.Join(dir, "app") + ":" + filepath.Join(dir, "packages"),
		"SEARXNG_SETTINGS_PATH=" + config, "SEARXNG_DATA_PATH=" + configDir, "__SEARXNG_DATA_PATH=" + configDir,
		"XDG_CACHE_HOME=" + configDir, "SSL_CERT_FILE=" + filepath.Join(dir, "certs.pem"), "PYTHONDONTWRITEBYTECODE=1"}
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{fd}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		err := cmd.Wait()
		if err != nil && r.ctx.Err() == nil {
			slog.Warn("AI search backend exited", "error", err)
		}
		close(done)
	}()
	r.processDone = done
	r.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ok = true
	return r.client, nil
}

func (r *Runtime) Close() {
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.processDone != nil {
		<-r.processDone
	}
	r.cleanup()
}

func (r *Runtime) cleanup() {
	if r.client != nil {
		r.client.CloseIdleConnections()
		r.client = nil
	}
	if r.socketDir != "" {
		os.RemoveAll(r.socketDir)
		r.socketDir = ""
	}
	r.processDone = nil
}

func (r *Runtime) install() (string, error) {
	name := "bundle/" + runtime.GOARCH + ".tar.gz"
	data, err := payload.Open(name)
	if err != nil {
		return "", errors.New("SearXNG runtime missing from server build; run scripts/build-search-runtime.py for the target architecture before building")
	}
	defer data.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, data); err != nil {
		return "", err
	}
	launcher, err := assets.ReadFile("serve.py")
	if err != nil {
		return "", err
	}
	hash.Write(launcher)
	root, err := filepath.Abs(r.dir)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(root, fmt.Sprintf("runtime-%x", hash.Sum(nil)))
	if _, err = os.Stat(filepath.Join(dest, "serve.py")); err == nil {
		return dest, nil
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp(root, ".runtime-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	packed, err := payload.Open(name)
	if err != nil {
		return "", err
	}
	defer packed.Close()
	gz, err := gzip.NewReader(packed)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	for {
		if err = r.ctx.Err(); err != nil {
			return "", err
		}
		entry, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if entry.Typeflag != tar.TypeReg || !filepath.IsLocal(entry.Name) {
			return "", errors.New("invalid embedded search runtime entry")
		}
		p := filepath.Join(temp, entry.Name)
		if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return "", err
		}
		mode := os.FileMode(0600)
		if entry.Name == "bin/python" || entry.Name == "bin/loader" {
			mode = 0700
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(f, archive)
		if err = errors.Join(copyErr, f.Close()); err != nil {
			return "", err
		}
	}
	for _, metadata := range []string{"source.json", "COPYING"} {
		b, err := assets.ReadFile("bundle/" + metadata)
		if err != nil {
			return "", err
		}
		if err = os.WriteFile(filepath.Join(temp, metadata), b, 0600); err != nil {
			return "", err
		}
	}
	if err = os.WriteFile(filepath.Join(temp, "serve.py"), launcher, 0600); err != nil {
		return "", err
	}
	if err = os.Rename(temp, dest); err != nil {
		if _, existing := os.Stat(filepath.Join(dest, "serve.py")); existing != nil {
			return "", err
		}
	}
	// Each executable carries its runtime, including binaries restored during rollback.
	old, _ := filepath.Glob(filepath.Join(root, "runtime-*"))
	for _, p := range old {
		if p != dest {
			if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > 7*24*time.Hour {
				_ = os.RemoveAll(p)
			}
		}
	}
	return dest, nil
}
