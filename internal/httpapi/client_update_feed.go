package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

func githubUpdateFeed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host != "github.com" || u.Scheme != "https" {
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	return len(parts) == 6 && parts[2] == "releases" && parts[5] == "haunt-updates.json" &&
		((parts[3] == "latest" && parts[4] == "download") ||
			(parts[3] == "download" && (strings.HasPrefix(parts[4], "client-v") ||
				strings.HasPrefix(parts[4], "v") && releaseVersion.MatchString(strings.TrimPrefix(parts[4], "v")))))
}

func publicUpdateURL(manifest, version, filename string) string {
	u, _ := url.Parse(manifest)
	if githubUpdateFeed(manifest) {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		prefix := "client-v"
		if parts[3] == "download" && strings.HasPrefix(parts[4], "v") ||
			parts[3] == "latest" && strings.EqualFold(parts[0]+"/"+parts[1], "example-git/haunt") {
			prefix = "v"
		}
		u.Path = "/" + parts[0] + "/" + parts[1] + "/releases/download/" + prefix + version + "/" + filename
	} else {
		u.Path = path.Join(path.Dir(u.Path), "client-v"+version, filename)
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}

// Only the small signed manifest is cached here. Artifact responses redirect to
// the configured public host, including for older launchers' relative URLs.
type publicUpdateFeed struct {
	url      string
	client   *http.Client
	gate     chan struct{}
	envelope ClientUpdateEnvelope
	payload  updatePayload
	expires  time.Time
}

func newPublicUpdateFeed(raw string) *publicUpdateFeed {
	return &publicUpdateFeed{url: raw, gate: make(chan struct{}, 1), client: &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.User != nil {
				return errors.New("invalid public update redirect")
			}
			return nil
		},
	}}
}

func (f *publicUpdateFeed) read(ctx context.Context) (ClientUpdateEnvelope, updatePayload, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case f.gate <- struct{}{}:
		defer func() { <-f.gate }()
	case <-ctx.Done():
		return ClientUpdateEnvelope{}, updatePayload{}, ctx.Err()
	}
	if time.Now().Before(f.expires) {
		return f.envelope, f.payload, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return ClientUpdateEnvelope{}, updatePayload{}, err
	}
	req.Header.Set("User-Agent", "Haunt-Public-Updates")
	response, err := f.client.Do(req)
	if err != nil {
		return ClientUpdateEnvelope{}, updatePayload{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ClientUpdateEnvelope{}, updatePayload{}, errors.New("public update manifest unavailable")
	}
	envelope, payload, err := decodeUpdateManifest(response.Body)
	if err == nil {
		f.envelope, f.payload, f.expires = envelope, payload, time.Now().Add(time.Minute)
	}
	return envelope, payload, err
}

func (f *publicUpdateFeed) manifest(w http.ResponseWriter, r *http.Request) {
	envelope, _, err := f.read(r.Context())
	if err != nil {
		writeJSON(w, 502, errorResponse{Error: "client_update_unavailable"})
		return
	}
	writeJSON(w, 200, envelope)
}

func (f *publicUpdateFeed) installers(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !enabled {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		envelope, payload, err := f.read(r.Context())
		if err != nil {
			writeJSON(w, 502, errorResponse{Error: "client_update_unavailable"})
			return
		}
		result := ClientInstallers{Manifest: envelope, Available: []string{}, Downloads: map[string]string{}}
		for target, entry := range payload.Targets {
			result.Available = append(result.Available, target)
			result.Downloads[target] = publicUpdateURL(f.url, payload.Version, entry.Installer.File)
		}
		writeJSON(w, 200, result)
	}
}

func (f *publicUpdateFeed) redirect(w http.ResponseWriter, r *http.Request) {
	version, filename := strings.TrimPrefix(r.PathValue("version"), "client-v"), r.PathValue("name")
	if !releaseVersion.MatchString(version) || !releaseFilename.MatchString(filename) {
		writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
		return
	}
	http.Redirect(w, r, publicUpdateURL(f.url, version, filename), http.StatusFound)
}
