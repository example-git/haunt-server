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
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const TikTokID = "tiktok-tts"
const PrimaryEndpoint = "https://api16-normal-useast5.us.tiktokv.com"
const AlternateEndpoint = "https://api16-normal-useast5.tiktokv.us"

var ErrInvalid = errors.New("invalid_server_extension")
var ErrConflict = errors.New("server_extension_conflict")
var ErrUnavailable = errors.New("server_extension_unavailable")
var ErrCredentials = errors.New("tts_credentials_rejected")
var ErrUpstream = errors.New("tts_provider_failed")
var ErrBusy = errors.New("tts_provider_busy")

type Extension struct {
	ID         string `json:"id" required:"true"`
	Name       string `json:"name" required:"true"`
	Category   string `json:"category" required:"true" enum:"tts"`
	Enabled    bool   `json:"enabled" required:"true"`
	Configured bool   `json:"configured" required:"true"`
}

type Configuration struct {
	Extension
	Revision int64  `json:"revision" required:"true" minimum:"0"`
	Endpoint string `json:"endpoint" required:"true"`
}

type SaveRequest struct {
	Revision     int64  `json:"revision" required:"true" minimum:"0"`
	Enabled      bool   `json:"enabled" required:"true"`
	Endpoint     string `json:"endpoint" required:"true"`
	SessionToken string `json:"session_token,omitempty" description:"Write-only TikTok sessionid. Omit or leave blank to keep the saved token."`
	ClearToken   bool   `json:"clear_token,omitempty"`
}

type SpeechRequest struct {
	Text  string `json:"text" required:"true" maxLength:"500"`
	Voice string `json:"voice" required:"true" maxLength:"64"`
}

type storedConfiguration struct {
	Revision int64  `json:"revision"`
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
}

type cacheEntry struct {
	key     [32]byte
	audio   []byte
	expires time.Time
}

type Store struct {
	mu         sync.Mutex
	value      storedConfiguration
	directory  string
	key        cipher.AEAD
	client     *http.Client
	cache      map[[32]byte]*list.Element
	order      *list.List
	cacheBytes int
	slots      chan struct{}
	retryAt    time.Time
}

func New(dataDir string, clients ...*http.Client) (*Store, error) {
	directory := filepath.Join(dataDir, "server-extensions")
	if err := os.MkdirAll(directory, 0700); err != nil {
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
	client := &http.Client{Timeout: 8 * time.Second}
	if len(clients) > 0 {
		client = clients[0]
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	store := &Store{directory: directory, key: aead, client: &copyClient,
		value: storedConfiguration{Endpoint: PrimaryEndpoint}, cache: make(map[[32]byte]*list.Element), order: list.New(), slots: make(chan struct{}, 2)}
	data, err := os.ReadFile(filepath.Join(directory, "configuration.encrypted"))
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil || len(data) < aead.NonceSize() || len(data) > 8192 {
		return nil, fmt.Errorf("cannot load server extension configuration")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(TikTokID))
	if err != nil || json.Unmarshal(plain, &store.value) != nil || !validConfiguration(store.value) {
		return nil, fmt.Errorf("invalid server extension configuration")
	}
	return store, nil
}

func validConfiguration(value storedConfiguration) bool {
	return value.Revision >= 0 && (value.Endpoint == PrimaryEndpoint || value.Endpoint == AlternateEndpoint) &&
		(value.Token == "" || sessionPattern.MatchString(value.Token)) && (!value.Enabled || value.Token != "")
}

func (store *Store) status() Configuration {
	return Configuration{Extension: Extension{ID: TikTokID, Name: "TikTok TTS", Category: "tts", Enabled: store.value.Enabled, Configured: store.value.Token != ""},
		Revision: store.value.Revision, Endpoint: store.value.Endpoint}
}

func (store *Store) Configuration(id string) (Configuration, error) {
	if id != TikTokID {
		return Configuration{}, ErrInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.status(), nil
}

func (store *Store) Catalog() []Extension {
	store.mu.Lock()
	defer store.mu.Unlock()
	return []Extension{store.status().Extension}
}

func (store *Store) Save(id string, request SaveRequest) (Configuration, error) {
	if id != TikTokID {
		return Configuration{}, ErrInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if request.Revision != store.value.Revision {
		return Configuration{}, ErrConflict
	}
	next := store.value
	next.Enabled, next.Endpoint = request.Enabled, request.Endpoint
	if request.ClearToken && request.SessionToken != "" {
		return Configuration{}, ErrInvalid
	}
	if request.ClearToken {
		next.Token = ""
	}
	if request.SessionToken != "" {
		next.Token = request.SessionToken
	}
	next.Revision++
	if !validConfiguration(next) {
		return Configuration{}, ErrInvalid
	}
	plain, err := json.Marshal(next)
	if err != nil {
		return Configuration{}, err
	}
	nonce := make([]byte, store.key.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return Configuration{}, err
	}
	data := store.key.Seal(nonce, nonce, plain, []byte(TikTokID))
	file, err := os.CreateTemp(store.directory, ".configuration-")
	if err != nil {
		return Configuration{}, err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return Configuration{}, err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return Configuration{}, err
	}
	if err = file.Close(); err != nil {
		return Configuration{}, err
	}
	if err = os.Rename(file.Name(), filepath.Join(store.directory, "configuration.encrypted")); err != nil {
		return Configuration{}, err
	}
	store.value = next
	store.cache = make(map[[32]byte]*list.Element)
	store.order.Init()
	store.cacheBytes = 0
	store.retryAt = time.Time{}
	return store.status(), nil
}

func (store *Store) Speech(ctx context.Context, id string, request SpeechRequest, test bool) ([]byte, error) {
	if id != TikTokID || len(request.Text) == 0 || len(request.Text) > 500 || !voicePattern.MatchString(request.Voice) {
		return nil, ErrInvalid
	}
	store.mu.Lock()
	value := store.value
	if value.Token == "" || (!test && !value.Enabled) {
		store.mu.Unlock()
		return nil, ErrUnavailable
	}
	cacheKey := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s", value.Revision, request.Voice, request.Text)))
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
		if time.Now().Before(store.retryAt) {
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
	audio, err := synthesizeTikTok(ctx, store.client, value.Endpoint, value.Token, request)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.value.Revision != value.Revision {
		return nil, ErrUnavailable
	}
	if err != nil {
		if ctx.Err() == nil {
			store.retryAt = time.Now().Add(30 * time.Second)
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

func (store *Store) Close() { store.client.CloseIdleConnections() }
