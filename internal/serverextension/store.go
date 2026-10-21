package serverextension

import (
	"container/list"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var ErrInvalid = errors.New("invalid_server_extension")
var ErrConflict = errors.New("server_extension_conflict")
var ErrUnavailable = errors.New("server_extension_unavailable")
var ErrCredentials = errors.New("tts_credentials_rejected")
var ErrUpstream = errors.New("tts_provider_failed")
var ErrBusy = errors.New("tts_provider_busy")

type Extension struct {
	ID           string  `json:"id" required:"true"`
	Name         string  `json:"name" required:"true"`
	Description  string  `json:"description" required:"true"`
	Category     string  `json:"category" required:"true" enum:"tts"`
	Enabled      bool    `json:"enabled" required:"true"`
	Configured   bool    `json:"configured" required:"true"`
	Voices       []Voice `json:"voices" required:"true"`
	DefaultVoice string  `json:"default_voice" required:"true"`
	VoicePattern string  `json:"voice_pattern,omitempty"`
}

type Configuration struct {
	Extension
	Revision          int64             `json:"revision" required:"true" minimum:"0"`
	Fields            []Field           `json:"fields" required:"true"`
	Values            map[string]string `json:"values" required:"true"`
	ConfiguredSecrets []string          `json:"configured_secrets" required:"true"`
}

type SaveRequest struct {
	Revision     int64             `json:"revision" required:"true" minimum:"0"`
	Enabled      bool              `json:"enabled" required:"true"`
	Values       map[string]string `json:"values" required:"true"`
	ClearSecrets []string          `json:"clear_secrets,omitempty"`
}

type SpeechRequest struct {
	Text  string `json:"text" required:"true" maxLength:"500" description:"Text limited to 500 UTF-8 bytes."`
	Voice string `json:"voice" required:"true" maxLength:"64"`
}

type storedConfiguration struct {
	Revision int64             `json:"revision"`
	Enabled  bool              `json:"enabled"`
	Values   map[string]string `json:"values"`
}

type cacheEntry struct {
	key     [32]byte
	audio   []byte
	expires time.Time
}

type Store struct {
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	values     map[string]storedConfiguration
	packages   map[string]installedPackage
	generation uint64
	directory  string
	key        cipher.AEAD
	cache      map[[32]byte]*list.Element
	order      *list.List
	cacheBytes int
	slots      chan struct{}
	retryAt    map[string]time.Time
}

func New(dataDir string) (*Store, error) {
	directory, err := filepath.Abs(filepath.Join(dataDir, "server-extensions"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(directory, "packages"), 0700); err != nil {
		return nil, err
	}
	keyFile := filepath.Join(directory, "key")
	key, err := os.ReadFile(keyFile)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		file, createErr := os.OpenFile(keyFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if createErr != nil {
			return nil, createErr
		}
		_, err = file.Write(key)
		err = errors.Join(err, file.Close())
	}
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("cannot load server extension key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	store := &Store{directory: directory, key: aead, values: make(map[string]storedConfiguration), slots: make(chan struct{}, 2)}
	data, err := os.ReadFile(filepath.Join(directory, "configurations.encrypted"))
	if err == nil {
		if len(data) < aead.NonceSize() || len(data) > 8*1024*1024 {
			return nil, ErrInvalid
		}
		plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte("server-extensions-v1"))
		if err != nil || json.Unmarshal(plain, &store.values) != nil || store.values == nil {
			return nil, ErrInvalid
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := store.migrate(); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	for id, value := range store.values {
		if !identifier.MatchString(id) || value.Revision < 0 || value.Values == nil {
			return nil, ErrInvalid
		}
	}
	if err := store.Reload(); err != nil {
		return nil, err
	}
	store.ctx, store.cancel = context.WithCancel(context.Background())
	return store, nil
}

func (store *Store) migrate() error {
	data, err := os.ReadFile(filepath.Join(store.directory, "configuration.encrypted"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) < store.key.NonceSize() || len(data) > 8192 {
		return ErrInvalid
	}
	plain, err := store.key.Open(nil, data[:store.key.NonceSize()], data[store.key.NonceSize():], []byte("tiktok-tts"))
	var previous struct {
		Revision int64  `json:"revision"`
		Enabled  bool   `json:"enabled"`
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
	}
	if err != nil || json.Unmarshal(plain, &previous) != nil || previous.Revision < 0 {
		return ErrInvalid
	}
	store.values["tiktok-tts"] = storedConfiguration{previous.Revision, previous.Enabled, map[string]string{"endpoint": previous.Endpoint, "session_token": previous.Token}}
	return store.persist(store.values)
}

func (store *Store) Reload() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	packages, err := discover(filepath.Join(store.directory, "packages"))
	if err != nil {
		return err
	}
	store.packages = packages
	store.generation++
	store.cache = make(map[[32]byte]*list.Element)
	store.order = list.New()
	store.cacheBytes = 0
	store.retryAt = make(map[string]time.Time)
	return nil
}

func configurationReady(manifest Manifest, values map[string]string) bool {
	for _, field := range manifest.Fields {
		value := values[field.Key]
		if (field.Required && value == "") || !validField(field, value) {
			return false
		}
	}
	return true
}

func (store *Store) value(id string) storedConfiguration {
	value := store.values[id]
	values := make(map[string]string)
	for _, field := range store.packages[id].manifest.Fields {
		values[field.Key] = field.Default
	}
	for key, current := range value.Values {
		values[key] = current
	}
	value.Values = values
	return value
}

func (store *Store) status(id string) Configuration {
	manifest := store.packages[id].manifest
	value := store.value(id)
	result := Configuration{Extension: Extension{ID: id, Name: manifest.Name, Description: manifest.Description, Category: manifest.Category, Enabled: value.Enabled, Configured: configurationReady(manifest, value.Values), Voices: manifest.Voices, DefaultVoice: manifest.DefaultVoice, VoicePattern: manifest.VoicePattern}, Revision: value.Revision, Fields: manifest.Fields, Values: make(map[string]string), ConfiguredSecrets: []string{}}
	for _, field := range manifest.Fields {
		if field.Type == "password" {
			if value.Values[field.Key] != "" {
				result.ConfiguredSecrets = append(result.ConfiguredSecrets, field.Key)
			}
		} else {
			result.Values[field.Key] = value.Values[field.Key]
		}
	}
	return result
}

func (store *Store) Configuration(id string) (Configuration, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.packages[id]; !ok {
		return Configuration{}, ErrUnavailable
	}
	return store.status(id), nil
}

func (store *Store) Catalog() []Extension {
	store.mu.Lock()
	defer store.mu.Unlock()
	catalog := make([]Extension, 0, len(store.packages))
	for id := range store.packages {
		catalog = append(catalog, store.status(id).Extension)
	}
	sort.Slice(catalog, func(i, j int) bool { return catalog[i].ID < catalog[j].ID })
	return catalog
}

func (store *Store) persist(values map[string]storedConfiguration) error {
	plain, err := json.Marshal(values)
	if err != nil {
		return err
	}
	nonce := make([]byte, store.key.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	data := store.key.Seal(nonce, nonce, plain, []byte("server-extensions-v1"))
	file, err := os.CreateTemp(store.directory, ".configuration-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(store.directory, "configurations.encrypted"))
}

func (store *Store) Save(id string, request SaveRequest) (Configuration, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	extension, ok := store.packages[id]
	if !ok {
		return Configuration{}, ErrUnavailable
	}
	next := store.value(id)
	if request.Revision != next.Revision {
		return Configuration{}, ErrConflict
	}
	fields := make(map[string]Field)
	for _, field := range extension.manifest.Fields {
		fields[field.Key] = field
	}
	for key, value := range request.Values {
		field, ok := fields[key]
		if !ok || !validField(field, value) {
			return Configuration{}, ErrInvalid
		}
		if field.Type != "password" || value != "" {
			next.Values[key] = value
		}
	}
	for _, key := range request.ClearSecrets {
		if fields[key].Type != "password" || request.Values[key] != "" {
			return Configuration{}, ErrInvalid
		}
		next.Values[key] = ""
	}
	next.Enabled = request.Enabled
	if next.Enabled && !configurationReady(extension.manifest, next.Values) {
		return Configuration{}, ErrInvalid
	}
	next.Revision++
	values := make(map[string]storedConfiguration, len(store.values)+1)
	for key, value := range store.values {
		values[key] = value
	}
	values[id] = next
	if err := store.persist(values); err != nil {
		return Configuration{}, err
	}
	store.values = values
	store.cache = make(map[[32]byte]*list.Element)
	store.order.Init()
	store.cacheBytes = 0
	delete(store.retryAt, id)
	return store.status(id), nil
}

func (store *Store) Speech(ctx context.Context, id string, request SpeechRequest, test bool) ([]byte, error) {
	if len(request.Text) > 500 || !utf8.ValidString(request.Text) || strings.TrimSpace(request.Text) == "" || len(request.Voice) > 64 || !utf8.ValidString(request.Voice) {
		return nil, ErrInvalid
	}
	store.mu.Lock()
	extension, ok := store.packages[id]
	value := store.value(id)
	generation := store.generation
	if !ok || !configurationReady(extension.manifest, value.Values) || (!test && !value.Enabled) {
		store.mu.Unlock()
		return nil, ErrUnavailable
	}
	if request.Voice == "" {
		request.Voice = extension.manifest.DefaultVoice
	}
	if pattern := extension.manifest.VoicePattern; pattern != "" && !regexp.MustCompile(pattern).MatchString(request.Voice) {
		store.mu.Unlock()
		return nil, ErrInvalid
	}
	cacheKey := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d:%d:%s:%s", id, value.Revision, generation, len(request.Voice), request.Voice, request.Text)))
	if !test {
		if item := store.cache[cacheKey]; item != nil {
			entry := item.Value.(cacheEntry)
			if time.Now().Before(entry.expires) {
				store.order.MoveToBack(item)
				store.mu.Unlock()
				return append([]byte(nil), entry.audio...), nil
			}
			store.remove(item)
		}
		if time.Now().Before(store.retryAt[id]) {
			store.mu.Unlock()
			return nil, ErrBusy
		}
	}
	store.mu.Unlock()
	select {
	case store.slots <- struct{}{}:
		defer func() { <-store.slots }()
	default:
		return nil, ErrBusy
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(store.ctx, cancel)
	defer stop()
	audio, err := extension.speech(ctx, value.Values, request)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.values[id].Revision != value.Revision || store.generation != generation {
		return nil, ErrUnavailable
	}
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, ErrInvalid) {
			store.retryAt[id] = time.Now().Add(30 * time.Second)
		}
		return nil, err
	}
	if !test {
		if item := store.cache[cacheKey]; item != nil {
			store.remove(item)
		}
		store.cache[cacheKey] = store.order.PushBack(cacheEntry{cacheKey, append([]byte(nil), audio...), time.Now().Add(10 * time.Minute)})
		store.cacheBytes += len(audio)
		for store.order.Len() > 64 || store.cacheBytes > 16*1024*1024 {
			store.remove(store.order.Front())
		}
	}
	return audio, nil
}

func (store *Store) remove(item *list.Element) {
	entry := item.Value.(cacheEntry)
	delete(store.cache, entry.key)
	store.cacheBytes -= len(entry.audio)
	store.order.Remove(item)
}

func (store *Store) Close() { store.cancel() }
