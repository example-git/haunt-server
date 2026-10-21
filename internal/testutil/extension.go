package testutil

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/example-git/haunt-server/internal/serverextension"
)

func InstallSpeechExtension(t testing.TB, dataDir, id string) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("speech extension fixture requires python3")
	}
	path := filepath.Join(dataDir, "server-extensions", "packages", id)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := serverextension.Manifest{Schema: 1, ID: id, Name: "Example speech", Description: "Speech extension fixture", Category: "tts", Runtime: "stdio-v1", Entrypoint: "extension",
		DefaultVoice: "example_voice", VoicePattern: "^[a-z0-9_]{2,64}$", Voices: []serverextension.Voice{{ID: "example_voice", Name: "Example voice", Group: "Examples"}},
		Fields: []serverextension.Field{{Key: "endpoint", Label: "API region", Type: "select", Required: true, Default: "https://speech.example.test", Options: []serverextension.Option{{Value: "https://speech.example.test", Label: "Example"}}},
			{Key: "session_token", Label: "Session token", Type: "password", Required: true, Pattern: "^[a-f0-9]{32}$", MaxLength: 32}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!" + python + "\n" + `import base64, json, pathlib, sys, time
request = json.load(sys.stdin)
assert request["protocol"] == 1 and request["operation"] == "speech"
assert request["configuration"]["session_token"] == "a" * 32
root = pathlib.Path(__file__).parent
with (root / "calls").open("a") as calls:
    calls.write("1\n")
text = request["text"]
if text == "wait":
    time.sleep(30)
if text.startswith("error:"):
    response = {"protocol": 1, "error": text[6:]}
else:
    audio = (root / "audio.mp3").read_bytes() if (root / "audio.mp3").exists() else (text + ":" + request["voice"]).encode()
    response = {"protocol": 1, "audio": base64.b64encode(audio).decode()}
json.dump(response, sys.stdout)
`
	if err := os.WriteFile(filepath.Join(path, "extension"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
