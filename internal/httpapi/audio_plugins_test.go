package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/example-git/haunt-server/internal/audioplugin"
	"io"
	"net/http"
	"os"
	"testing"
)

func TestAudioPluginAuthorizationUploadIntegrityAndRemoval(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	member := f.register(t, "plugin_member")
	data, err := os.ReadFile("../../client/tests/fixtures/audio-plugin/test-audio.zip")
	if err != nil {
		t.Fatal(err)
	}
	upload := func(token string, want int) audioplugin.Plugin {
		t.Helper()
		r, _ := http.NewRequest("POST", f.server.URL+"/api/v1/admin/audio-plugins", bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/zip")
		resp, e := f.server.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("upload %d want %d: %s", resp.StatusCode, want, b)
		}
		var p audioplugin.Plugin
		if want == 201 {
			if e = json.Unmarshal(b, &p); e != nil {
				t.Fatal(e)
			}
		}
		return p
	}
	upload(member.Token, 403)
	p := upload(f.owner.Token, 201)
	var catalog AudioPluginsResponse
	f.call(t, "GET", "/api/v1/audio-plugins", member.Token, nil, 200, &catalog)
	if len(catalog.Plugins) != 1 || catalog.Plugins[0].Digest != p.Digest || catalog.MaxPackageBytes != 50<<20 {
		t.Fatal("bad catalog")
	}
	path := "/api/v1/audio-plugins/" + p.Digest + "/files/" + p.Files[0].Name
	f.call(t, "GET", path, "", nil, 401, nil)
	req, _ := http.NewRequest("GET", f.server.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+member.Token)
	resp, e := f.server.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || int64(len(b)) != p.Files[0].Bytes || resp.Header.Get("Content-Type") != "application/wasm" {
		t.Fatal("bad plugin file")
	}
	f.call(t, "DELETE", "/api/v1/admin/audio-plugins/"+p.Manifest.ID, member.Token, nil, 403, nil)
	f.call(t, "DELETE", "/api/v1/admin/audio-plugins/"+p.Manifest.ID, f.owner.Token, nil, 204, nil)
	f.call(t, "GET", path, member.Token, nil, 404, nil)
	var audits int
	if e = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM chatxo.audit_log WHERE action IN ('audio_plugin.installed','audio_plugin.removed')`).Scan(&audits); e != nil || audits != 2 {
		t.Fatalf("missing plugin audit %d %v", audits, e)
	}
}
