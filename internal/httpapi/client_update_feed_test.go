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
	if got := publicUpdateURL("https://downloads.example.test/updates/haunt-updates.json", "0.2.0", "Haunt.AppImage"); got != "https://downloads.example.test/updates/client-v0.2.0/Haunt.AppImage" {
		t.Fatal(got)
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
		response = request(version + "/linux-x64.exe")
		if response.Code != http.StatusFound || response.Header().Get("Location") != upstream.URL+"/client-v0.2.0/linux-x64.exe" {
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
