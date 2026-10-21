package serverextension

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var identifier = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
var fieldKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type Option struct {
	Value string `json:"value" required:"true"`
	Label string `json:"label" required:"true"`
}

type Field struct {
	Key       string   `json:"key" required:"true"`
	Label     string   `json:"label" required:"true"`
	Type      string   `json:"type" required:"true" enum:"text,password,select"`
	Help      string   `json:"help,omitempty"`
	Default   string   `json:"default,omitempty"`
	Required  bool     `json:"required,omitempty"`
	MaxLength int      `json:"max_length,omitempty"`
	Pattern   string   `json:"pattern,omitempty"`
	Options   []Option `json:"options,omitempty"`
}

type Voice struct {
	ID    string `json:"id" required:"true"`
	Name  string `json:"name" required:"true"`
	Group string `json:"group,omitempty"`
}

type Manifest struct {
	Schema       int     `json:"schema" required:"true"`
	ID           string  `json:"id" required:"true"`
	Name         string  `json:"name" required:"true"`
	Description  string  `json:"description" required:"true"`
	Category     string  `json:"category" required:"true" enum:"tts"`
	Runtime      string  `json:"runtime" required:"true" enum:"stdio-v1"`
	Entrypoint   string  `json:"entrypoint" required:"true"`
	Fields       []Field `json:"fields" required:"true"`
	Voices       []Voice `json:"voices" required:"true"`
	DefaultVoice string  `json:"default_voice" required:"true"`
	VoicePattern string  `json:"voice_pattern,omitempty"`
}

type installedPackage struct {
	manifest Manifest
	path     string
}

func safeKey(key string) bool {
	return fieldKey.MatchString(key) && key != "constructor" && key != "prototype" && key != "__proto__"
}

func validManifest(manifest Manifest) bool {
	if manifest.Schema != 1 || !identifier.MatchString(manifest.ID) || manifest.ID == "device" || manifest.ID == "reload" ||
		manifest.Name == "" || len(manifest.Name) > 100 || len(manifest.Description) > 2000 || manifest.Category != "tts" || manifest.Runtime != "stdio-v1" ||
		(manifest.Entrypoint != "extension" && manifest.Entrypoint != "extension.exe") || manifest.Fields == nil || len(manifest.Fields) > 32 || manifest.Voices == nil || len(manifest.Voices) > 512 {
		return false
	}
	keys := map[string]bool{}
	for _, field := range manifest.Fields {
		if !safeKey(field.Key) || keys[field.Key] || field.Label == "" || len(field.Label) > 100 || len(field.Help) > 2000 ||
			field.MaxLength < 0 || field.MaxLength > 4096 || len(field.Pattern) > 512 || len(field.Options) > 128 ||
			(field.Type != "text" && field.Type != "password" && field.Type != "select") || (field.Type == "password" && field.Default != "") {
			return false
		}
		keys[field.Key] = true
		if field.Pattern != "" {
			if _, err := regexp.Compile(field.Pattern); err != nil {
				return false
			}
		}
		options := map[string]bool{}
		for _, option := range field.Options {
			if option.Value == "" || len(option.Value) > 4096 || option.Label == "" || len(option.Label) > 150 || options[option.Value] {
				return false
			}
			options[option.Value] = true
		}
		if field.Type == "select" && len(options) == 0 {
			return false
		}
		if field.Default != "" && !validField(field, field.Default) {
			return false
		}
	}
	if len(manifest.VoicePattern) > 512 {
		return false
	}
	var voicePattern *regexp.Regexp
	if manifest.VoicePattern != "" {
		var err error
		voicePattern, err = regexp.Compile(manifest.VoicePattern)
		if err != nil {
			return false
		}
	}
	voices := map[string]bool{}
	for _, voice := range manifest.Voices {
		if voice.ID == "" || len(voice.ID) > 64 || voices[voice.ID] || voice.Name == "" || len(voice.Name) > 150 || len(voice.Group) > 100 {
			return false
		}
		if voicePattern != nil && !voicePattern.MatchString(voice.ID) {
			return false
		}
		voices[voice.ID] = true
	}
	return manifest.DefaultVoice != "" && len(manifest.DefaultVoice) <= 64 && (voicePattern == nil || voicePattern.MatchString(manifest.DefaultVoice)) && (len(voices) == 0 || voices[manifest.DefaultVoice])
}

func validField(field Field, value string) bool {
	if !utf8.ValidString(value) || len(value) > 4096 || (field.MaxLength > 0 && utf8.RuneCountInString(value) > field.MaxLength) {
		return false
	}
	if value == "" {
		return true
	}
	if field.Pattern != "" && !regexp.MustCompile(field.Pattern).MatchString(value) {
		return false
	}
	if field.Type == "select" {
		for _, option := range field.Options {
			if option.Value == value {
				return true
			}
		}
		return false
	}
	return true
}

func discover(directory string) (map[string]installedPackage, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	packages := make(map[string]installedPackage)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		file, err := os.Open(filepath.Join(path, "manifest.json"))
		if err != nil {
			return nil, ErrInvalid
		}
		data, readErr := io.ReadAll(io.LimitReader(file, 128*1024+1))
		file.Close()
		var manifest Manifest
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if readErr != nil || len(data) > 128*1024 || decoder.Decode(&manifest) != nil || decoder.Decode(new(any)) != io.EOF || !validManifest(manifest) || entry.Name() != manifest.ID || len(packages) >= 32 {
			return nil, ErrInvalid
		}
		binary, err := os.Lstat(filepath.Join(path, manifest.Entrypoint))
		if err != nil || !binary.Mode().IsRegular() || binary.Mode().Perm()&0111 == 0 {
			return nil, ErrInvalid
		}
		packages[manifest.ID] = installedPackage{manifest: manifest, path: path}
	}
	return packages, nil
}

type invocation struct {
	Protocol      int               `json:"protocol"`
	Operation     string            `json:"operation"`
	Configuration map[string]string `json:"configuration"`
	Text          string            `json:"text"`
	Voice         string            `json:"voice"`
}

type result struct {
	Protocol int    `json:"protocol"`
	Audio    []byte `json:"audio,omitempty"`
	Error    string `json:"error,omitempty"`
}

type boundedOutput struct{ buffer bytes.Buffer }

func (output *boundedOutput) Write(data []byte) (int, error) {
	if output.buffer.Len()+len(data) > 12*1024*1024 {
		return 0, ErrUpstream
	}
	return output.buffer.Write(data)
}

func (extension installedPackage) speech(ctx context.Context, values map[string]string, request SpeechRequest) ([]byte, error) {
	input, err := json.Marshal(invocation{1, "speech", values, request.Text, request.Voice})
	if err != nil {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(extension.path, extension.manifest.Entrypoint))
	command.Dir = extension.path
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	for _, key := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"} {
		if value, ok := os.LookupEnv(key); ok {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	command.Stdin = bytes.NewReader(input)
	var output boundedOutput
	command.Stdout = &output
	command.Stderr = io.Discard
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		return nil, ErrUpstream
	}
	var response result
	decoder := json.NewDecoder(bytes.NewReader(output.buffer.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&response) != nil || decoder.Decode(new(any)) != io.EOF || response.Protocol != 1 {
		return nil, ErrUpstream
	}
	if response.Error != "" {
		switch response.Error {
		case "invalid_request":
			return nil, ErrInvalid
		case "credentials_rejected":
			return nil, ErrCredentials
		case "busy":
			return nil, ErrBusy
		default:
			return nil, ErrUpstream
		}
	}
	if len(response.Audio) == 0 || len(response.Audio) > 8*1024*1024 {
		return nil, ErrUpstream
	}
	return response.Audio, nil
}
