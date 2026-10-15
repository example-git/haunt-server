package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
)

type ClientUpdateSource struct {
	Installers    bool   `json:"installers"`
	ManifestURL   string `json:"manifest_url"`
	Authenticated bool   `json:"authenticated"`
}
type ClientUpdateEnvelope struct {
	Schema    int    `json:"schema"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}
type clientUpdateQuery struct {
	Version string `query:"version" required:"true" description:"Installed desktop version."`
	Target  string `query:"target" required:"true" description:"win32-x64, linux-x64, linux-arm64 or darwin-arm64."`
}

var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$`)
var releaseFilename = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,160}$`)
var releaseDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type updateArtifact struct {
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type updateTarget struct {
	ASAR      updateArtifact `json:"asar"`
	Installer updateArtifact `json:"installer"`
}
type updatePayload struct {
	Version string                  `json:"version"`
	Targets map[string]updateTarget `json:"targets"`
}

func clientUpdateSource(c config.Config) *ClientUpdateSource {
	if c.ClientUpdateManifestURL != "" {
		return &ClientUpdateSource{ManifestURL: c.ClientUpdateManifestURL}
	}
	if c.ClientUpdateLocal {
		return &ClientUpdateSource{ManifestURL: c.PublicURL + "/api/v1/client-updates/haunt-updates.json", Authenticated: true, Installers: c.ClientUpdateInstallers}
	}
	return nil
}

// The desktop's pinned release key, not a server-supplied key, determines which
// code may execute. The service distributes opaque signed release artifacts.
func readUpdateManifest(root *os.Root, name string) (ClientUpdateEnvelope, updatePayload, error) {
	var envelope ClientUpdateEnvelope
	var payload updatePayload
	file, err := root.Open(name)
	if err != nil {
		return envelope, payload, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 128*1024+1))
	if err != nil || len(data) > 128*1024 {
		return envelope, payload, errors.New("invalid update manifest size")
	}
	if err = json.Unmarshal(data, &envelope); err != nil || envelope.Schema != 1 {
		return envelope, payload, errors.New("invalid update envelope")
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != 64 {
		return envelope, payload, errors.New("invalid update signature encoding")
	}
	data, err = base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return envelope, payload, err
	}
	if err = json.Unmarshal(data, &payload); err != nil || !releaseVersion.MatchString(payload.Version) || len(payload.Targets) != 4 {
		return envelope, payload, errors.New("invalid release metadata")
	}
	for _, target := range []string{"win32-x64", "linux-x64", "linux-arm64", "darwin-arm64"} {
		entry, ok := payload.Targets[target]
		if !ok {
			return envelope, payload, errors.New("missing release target")
		}
		for _, asset := range []updateArtifact{entry.ASAR, entry.Installer} {
			if !releaseFilename.MatchString(asset.File) || !releaseDigest.MatchString(asset.SHA256) || asset.Size < 1 || asset.Size > 1<<30 {
				return envelope, payload, errors.New("invalid release file")
			}
		}
	}
	return envelope, payload, nil
}

func (a *api) clientUpdateRoutes(mux routeRegistrar, c config.Config) {
	local := c.ClientUpdateLocal && c.ClientUpdateManifestURL == ""
	directory := filepath.Join(c.DataDir, "client-updates")
	downloads := make(chan struct{}, 4)
	mux.HandleFunc("GET /api/v1/client-updates/installers", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !local || !c.ClientUpdateInstallers {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		if !a.limit(w, "client-installers:"+p.User.ID, 30, time.Hour) {
			return
		}
		root, err := os.OpenRoot(directory)
		if err != nil {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		defer root.Close()
		envelope, payload, err := readUpdateManifest(root, "haunt-updates.json")
		if err != nil {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		available := []string{}
		for target, entry := range payload.Targets {
			info, err := root.Stat("client-v" + payload.Version + "/" + entry.Installer.File)
			if err == nil && info.Mode().IsRegular() && info.Size() == entry.Installer.Size {
				available = append(available, target)
			}
		}
		writeJSON(w, 200, ClientInstallers{Manifest: envelope, Available: available})
	}))
	mux.HandleFunc("GET /api/v1/client-updates/haunt-updates.json", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !local {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		target := r.URL.Query().Get("target")
		if !releaseVersion.MatchString(r.URL.Query().Get("version")) || (target != "win32-x64" && target != "linux-x64" && target != "linux-arm64" && target != "darwin-arm64") {
			writeJSON(w, 400, errorResponse{Error: "invalid_update_client"})
			return
		}
		if !a.limit(w, "client-update-check:"+p.User.ID, 30, time.Hour) {
			return
		}
		root, err := os.OpenRoot(directory)
		if err != nil {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		defer root.Close()
		envelope, _, err := readUpdateManifest(root, "haunt-updates.json")
		if err != nil {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		writeJSON(w, 200, envelope)
	}))
	mux.HandleFunc("GET /api/v1/client-updates/{version}/{name}", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		if !local || !releaseVersion.MatchString(r.PathValue("version")) || !releaseFilename.MatchString(r.PathValue("name")) {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		if !a.limit(w, "client-update-file:"+p.User.ID, 12, time.Hour) {
			return
		}
		select {
		case downloads <- struct{}{}:
			defer func() { <-downloads }()
		default:
			w.Header().Set("Retry-After", "30")
			writeJSON(w, 503, errorResponse{Error: "update_download_capacity"})
			return
		}
		root, err := os.OpenRoot(directory)
		if err != nil {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		defer root.Close()
		version := r.PathValue("version")
		_, payload, err := readUpdateManifest(root, "client-v"+version+"/haunt-updates.json")
		if err != nil || payload.Version != version {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		var asset updateArtifact
		for _, target := range payload.Targets {
			for _, candidate := range []updateArtifact{target.ASAR, target.Installer} {
				if candidate.File == r.PathValue("name") {
					asset = candidate
				}
			}
		}
		if asset.File == "" {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		file, err := root.Open("client-v" + version + "/" + asset.File)
		if err != nil {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		defer file.Close()
		stat, err := file.Stat()
		if err != nil || !stat.Mode().IsRegular() || stat.Size() != asset.Size {
			writeJSON(w, 404, errorResponse{Error: "client_update_unavailable"})
			return
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Minute))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("ETag", `"`+asset.SHA256+`"`)
		http.ServeContent(w, r, asset.File, stat.ModTime(), file)
	}))
}

type ClientInstallers struct {
	Manifest  ClientUpdateEnvelope `json:"manifest"`
	Available []string             `json:"available"`
}
