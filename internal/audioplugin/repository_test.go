package audioplugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func repositoryFixture(t *testing.T) (RepositoryInstall, []byte) {
	b := fixture(t)
	h := sha256.Sum256(b)
	return RepositoryInstall{Repository: "https://github.com/example/audio.git", Commit: strings.Repeat("a", 40), SHA256: hex.EncodeToString(h[:])}, b
}

func TestRepositoryPinnedHashAndUnchangedOnFailure(t *testing.T) {
	s, _ := New(t.TempDir())
	request, data := repositoryFixture(t)
	i := NewRepositoryInstaller("test-token")
	defer i.Close()
	i.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.github.com/repos/example/audio/contents/dist/haunt-plugin.zip?ref="+request.Commit || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Accept") != "application/vnd.github.raw+json" {
			t.Fatal("unexpected request")
		}
		return &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})
	p, err := i.Install(context.Background(), s, request)
	if err != nil || p.Digest != request.SHA256 {
		t.Fatalf("install: %v", err)
	}
	data = append(data, 1)
	if _, err := i.Install(context.Background(), s, request); !errors.Is(err, ErrIntegrity) {
		t.Fatal("accepted modified package", err)
	}
	if len(s.List()) != 1 || s.List()[0].Digest != p.Digest {
		t.Fatal("failed update changed installation")
	}
	i.slots <- struct{}{}
	if _, err := i.Install(context.Background(), s, request); !errors.Is(err, ErrBusy) {
		t.Fatal("unbounded repository requests", err)
	}
	<-i.slots
}

func TestRepositoryRejectsMutableRefsAndUntrustedDestinations(t *testing.T) {
	r, _ := repositoryFixture(t)
	for _, value := range []string{"main", "v1.0.0", "1234", strings.Repeat("a", 41), "../../HEAD"} {
		bad := r
		bad.Commit = value
		if _, err := bad.URL(); err == nil {
			t.Fatalf("accepted ref %q", value)
		}
	}
	for _, value := range []string{"http://github.com/x/y", "https://github.com.evil/x/y", "https://user@github.com/x/y", "https://github.com:443/x/y", "https://127.0.0.1/x/y", "https://github.com/x/y?ref=main", "https://github.com/x/y#fragment", "https://github.com/x/y/..", "https://github.com/x/%79"} {
		bad := r
		bad.Repository = value
		if _, err := bad.URL(); err == nil {
			t.Fatalf("accepted URL %q", value)
		}
	}
	for _, value := range []string{"", "sha256", strings.Repeat("a", 63)} {
		bad := r
		bad.SHA256 = value
		if _, err := bad.URL(); err == nil {
			t.Fatal("missing expected integrity")
		}
	}
	for _, value := range []string{"127.0.0.1", "::1", "10.0.0.1", "169.254.169.254", "100.100.100.200", "168.63.129.16", "::ffff:127.0.0.1", "fd00::1", "64:ff9b::a00:1"} {
		if repositoryIP(netip.MustParseAddr(value)) {
			t.Fatalf("accepted private address %s", value)
		}
	}
}

func TestRepositoryRedirectAndDownloadRejection(t *testing.T) {
	s, _ := New(t.TempDir())
	r, _ := repositoryFixture(t)
	i := NewRepositoryInstaller("")
	defer i.Close()
	if i.client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects can leak credentials")
	}
	for _, status := range []int{302, 403, 404, 500} {
		i.client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
		})
		if _, err := i.Install(context.Background(), s, r); !errors.Is(err, ErrRepository) {
			t.Fatal("accepted failure", status, err)
		}
	}
	i.client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: MaxBytes + 1, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if _, err := i.Install(context.Background(), s, r); !errors.Is(err, ErrRepository) {
		t.Fatal("accepted oversize", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("failed download installed a plugin")
	}
}
