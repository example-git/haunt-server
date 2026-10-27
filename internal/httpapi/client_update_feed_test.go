package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/haunt-server/internal/config"
)

func TestPublicClientUpdateURLs(t *testing.T) {
	for _, manifest := range []string{
		"https://github.com/example-git/haunt-client/releases/latest/download/haunt-updates.json",
		"https://github.com/example-git/haunt-client/releases/download/client-v0.2.0/haunt-updates.json",
	} {
		if got := publicUpdateURL(manifest, "0.2.0", "Haunt.AppImage"); got != "https://github.com/example-git/haunt-client/releases/download/client-v0.2.0/Haunt.AppImage" {
			t.Fatal(got)
		}
	}
	for _, manifest := range []string{
		"https://github.com/example-git/haunt/releases/latest/download/haunt-updates.json",
		"https://github.com/example-git/haunt/releases/download/v0.2.0/haunt-updates.json",
		"https://github.com/community/release-hub/releases/download/v0.2.0/haunt-updates.json",
	} {
		if !githubUpdateFeed(manifest) {
			t.Fatal("Hub manifest was not recognized", manifest)
		}
		if got := publicUpdateURL(manifest, "0.2.0", "Haunt.AppImage"); got != manifest[:strings.Index(manifest, "/releases/")]+"/releases/download/v0.2.0/Haunt.AppImage" {
			t.Fatal(got)
		}
	}
	if got := publicUpdateURL("https://downloads.example.test/updates/haunt-updates.json", "0.2.0", "Haunt.AppImage"); got != "https://downloads.example.test/updates/client-v0.2.0/Haunt.AppImage" {
		t.Fatal(got)
	}
}

type updateFeedTransport func(*http.Request) (*http.Response, error)

func (f updateFeedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPublicHubClientUpdateRoutes(t *testing.T) {
	envelope := updateFixtureFiles(t, t.TempDir())
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	manifest := "https://github.com/example-git/haunt/releases/download/v0.2.0/haunt-updates.json"
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = updateFeedTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("Unexpected upstream request", r.URL)
		}
		if r.Method == http.MethodGet && r.URL.String() == manifest {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data)))}, nil
		}
		if r.Method != http.MethodHead || r.URL.String() != "https://github.com/example-git/haunt/releases/download/v0.2.0/linux-x64.rpm" {
			t.Fatal("Unexpected artifact request", r.Method, r.URL)
		}
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://release-assets.githubusercontent.com/fixture/linux-x64.rpm?token=fixture"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	mux := http.NewServeMux()
	configuration := config.Config{PublicURL: "https://community.example.test", ClientUpdateInstallers: true, ClientUpdateManifestURL: manifest}
	source := clientUpdateSource(configuration)
	if source.Authenticated || !source.Installers || source.ManifestURL != configuration.PublicURL+"/api/v1/client-updates/haunt-updates.json" {
		t.Fatal("Incorrect advertised hub feed", source)
	}
	(&api{}).clientUpdateRoutes(mux, configuration)
	for _, path := range []string{"haunt-updates.json", "installers", "0.2.0/linux-x64.rpm", "client-v0.2.0/linux-x64.rpm"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/client-updates/"+path, nil))
		switch path {
		case "haunt-updates.json":
			var actual ClientUpdateEnvelope
			if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil || response.Code != 200 || actual != envelope {
				t.Fatal(response.Code, actual, err)
			}
		case "installers":
			var actual ClientInstallers
			if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil || response.Code != 200 || actual.Manifest != envelope || len(actual.Available) != 4 {
				t.Fatal(response.Code, actual, err)
			}
			for target, link := range actual.Downloads {
				if link != "https://github.com/example-git/haunt/releases/download/v0.2.0/"+target+".exe" {
					t.Fatal(link)
				}
			}
		default:
			if response.Code != http.StatusFound || response.Header().Get("Location") != "https://release-assets.githubusercontent.com/fixture/linux-x64.rpm?token=fixture" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(response.Code, response.Header())
			}
		}
	}
}

func TestPublicGitHubArtifactRedirects(t *testing.T) {
	for _, test := range []struct {
		name, destination string
		status            int
	}{
		{"official", "https://objects.githubusercontent.com/fixture/asset?token=fixture", http.StatusFound},
		{"untrusted", "https://tracker.example.test/asset", http.StatusBadGateway},
		{"insecure", "http://release-assets.githubusercontent.com/asset", http.StatusBadGateway},
		{"credentials", "https://user:password@release-assets.githubusercontent.com/asset", http.StatusBadGateway},
		{"port", "https://release-assets.githubusercontent.com:444/asset", http.StatusBadGateway},
		{"loop", "https://github.com/example-git/haunt-client/releases/download/client-v0.2.0/linux-x64.rpm", http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			feed := newPublicUpdateFeed("https://github.com/example-git/haunt-client/releases/latest/download/haunt-updates.json")
			requests := 0
			feed.client.Transport = updateFeedTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodHead || r.URL.String() != "https://github.com/example-git/haunt-client/releases/download/client-v0.2.0/linux-x64.rpm" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Fatal("Unexpected upstream request", r.Method, r.URL)
				}
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {test.destination}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			req := httptest.NewRequest("GET", "/api/v1/client-updates/client-v0.2.0/linux-x64.rpm", nil)
			req.SetPathValue("version", "client-v0.2.0")
			req.SetPathValue("name", "linux-x64.rpm")
			req.Header.Set("Authorization", "Bearer private-session")
			req.Header.Set("Cookie", "private=cookie")
			response := httptest.NewRecorder()
			feed.redirect(response, req)
			if response.Code != test.status || requests > 5 {
				t.Fatal(response.Code, requests, response.Body.String())
			}
			if test.status == http.StatusFound {
				if response.Header().Get("Location") != test.destination {
					t.Fatal(response.Header())
				}
			} else if response.Header().Get("Location") != "" {
				t.Fatal("Untrusted redirect was exposed", response.Header())
			}
		})
	}
}

func TestPublicClientUpdateRoutes(t *testing.T) {
	envelope := updateFixtureFiles(t, t.TempDir())
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/haunt-updates.json" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("Unexpected upstream request: %s, credentials present: %v", r.URL.Path, r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "")
		}
		json.NewEncoder(w).Encode(envelope)
	}))
	defer upstream.Close()
	configuration := config.Config{ClientUpdateLocal: true, ClientUpdateInstallers: true, ClientUpdateManifestURL: upstream.URL + "/haunt-updates.json"}
	mux := http.NewServeMux()
	(&api{}).clientUpdateRoutes(mux, configuration)
	request := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/client-updates/"+path, nil)
		// These must never be forwarded to the public upstream.
		req.Header.Set("Authorization", "Bearer private-session")
		req.Header.Set("Cookie", "private=cookie")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, req)
		return response
	}
	response := request("haunt-updates.json")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var actual ClientUpdateEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil || actual != envelope {
		t.Fatal("Signed envelope changed", actual, err)
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/client-updates/installers", nil))
	var installers ClientInstallers
	if err := json.Unmarshal(response.Body.Bytes(), &installers); err != nil || response.Code != 200 || len(installers.Available) != 4 || installers.Manifest != envelope {
		t.Fatal(response.Code, installers, err)
	}
	for _, target := range installers.Available {
		if installers.Downloads[target] != upstream.URL+"/client-v0.2.0/"+target+".exe" {
			t.Fatal(installers.Downloads)
		}
	}
	for _, version := range []string{"0.2.0", "client-v0.2.0"} {
		response = request(version + "/linux-x64.rpm")
		if response.Code != http.StatusFound || response.Header().Get("Location") != upstream.URL+"/client-v0.2.0/linux-x64.rpm" {
			t.Fatal(response.Code, response.Header())
		}
	}
	if requests.Load() != 1 {
		t.Fatal("Metadata was not cached or artifact bytes were proxied", requests.Load())
	}
	if response = request("invalid/linux-x64.exe"); response.Code != 404 {
		t.Fatal(response.Code)
	}
	configuration.ClientUpdateInstallers = false
	disabled := http.NewServeMux()
	(&api{}).clientUpdateRoutes(disabled, configuration)
	response = httptest.NewRecorder()
	disabled.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/client-updates/installers", nil))
	if response.Code != 404 {
		t.Fatal("Disabled installer discovery remained available", response.Code)
	}
}

func TestPublicClientUpdateFeedFailures(t *testing.T) {
	for _, body := range []string{"{}", strings.Repeat("x", 128*1024+1)} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		mux := http.NewServeMux()
		(&api{}).clientUpdateRoutes(mux, config.Config{ClientUpdateInstallers: true, ClientUpdateManifestURL: upstream.URL + "/haunt-updates.json"})
		for _, endpoint := range []string{"haunt-updates.json", "installers"} {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/client-updates/"+endpoint, nil))
			if response.Code != 502 {
				t.Fatal(endpoint, response.Code)
			}
		}
		upstream.Close()
	}
}
