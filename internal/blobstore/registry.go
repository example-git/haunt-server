package blobstore

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/platform/postgres"
	"github.com/jackc/pgx/v5"
)

type Provider struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Configuration  Configuration `json:"configuration"`
	Active         bool          `json:"active"`
	HasCredentials bool          `json:"has_credentials"`
	Attachments    int64         `json:"attachments"`
	Bytes          int64         `json:"bytes"`
}
type RegisterRequest struct {
	Name          string        `json:"name" required:"true" minLength:"1" maxLength:"80"`
	Configuration Configuration `json:"configuration" required:"true"`
	Credentials   Credentials   `json:"credentials"`
}
type Registry struct {
	pool     postgres.Pool
	ids      *identity.Service
	instance string
	aead     cipher.AEAD
	slots    chan struct{}
}

func New(ctx context.Context, pool postgres.Pool, ids *identity.Service, dir, instance string) (*Registry, error) {
	keyPath := filepath.Join(dir, "storage-provider.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		var used bool
		if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.storage_providers WHERE credentials IS NOT NULL)`).Scan(&used); err != nil {
			return nil, err
		}
		if used {
			return nil, errors.New("restore storage-provider.key alongside the database before starting")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(keyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		_, e = f.Write(key)
		err = errors.Join(e, f.Sync(), f.Close())
		if err == nil {
			d, e := os.Open(dir)
			if e != nil {
				return nil, e
			}
			err = errors.Join(d.Sync(), d.Close())
		}
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid storage-provider.key")
	}
	if err = os.Chmod(keyPath, 0600); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Registry{pool: pool, ids: ids, instance: instance, aead: aead, slots: make(chan struct{}, 2)}, nil
}
func (r *Registry) encrypt(id string, k Credentials) []byte {
	raw, _ := json.Marshal(k)
	nonce := make([]byte, r.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return r.aead.Seal(nonce, nonce, raw, []byte(id))
}
func (r *Registry) backend(ctx context.Context, id string) (*backend, error) {
	var raw, secret []byte
	err := r.pool.QueryRow(ctx, `SELECT configuration,credentials FROM chatxo.storage_providers WHERE id=$1`, id).Scan(&raw, &secret)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, identity.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var c Configuration
	var k Credentials
	if err = json.Unmarshal(raw, &c); err != nil {
		return nil, ErrUnavailable
	}
	if secret != nil {
		n := r.aead.NonceSize()
		if len(secret) < n {
			return nil, ErrUnavailable
		}
		decoded, e := r.aead.Open(nil, secret[:n], secret[n:], []byte(id))
		if e != nil || json.Unmarshal(decoded, &k) != nil {
			return nil, ErrUnavailable
		}
	}
	return makeBackend(c, k, r.instance, id), nil
}
func (r *Registry) List(ctx context.Context) ([]Provider, error) {
	rows, err := r.pool.Query(ctx, `SELECT p.id,p.name,p.configuration,p.active,p.credentials IS NOT NULL,count(a.id),COALESCE(sum(a.bytes),0) FROM chatxo.storage_providers p LEFT JOIN chatxo.attachments a ON a.storage_provider_id=p.id GROUP BY p.id ORDER BY p.created_at,p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Provider{}
	for rows.Next() {
		var p Provider
		var raw []byte
		if err = rows.Scan(&p.ID, &p.Name, &raw, &p.Active, &p.HasCredentials, &p.Attachments, &p.Bytes); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &p.Configuration); err != nil {
			return nil, err
		}
		items = append(items, p)
	}
	return items, rows.Err()
}
func (r *Registry) transaction(ctx context.Context, p identity.Principal) (pgx.Tx, error) {
	// The accounting row already serializes reservations; provider activation and
	// removal use the same order so a newly admitted upload always pins its store.
	if !p.Can(identity.ManageConfiguration) {
		return nil, identity.ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT singleton FROM chatxo.storage WHERE singleton FOR UPDATE`); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	// A fresh session lookup on this transaction avoids nesting pool acquisitions.
	if err = r.ids.AuthorizeConfiguration(ctx, tx, p); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}
func (r *Registry) Register(ctx context.Context, p identity.Principal, v RegisterRequest) (Provider, error) {
	if !p.Can(identity.ManageConfiguration) {
		return Provider{}, identity.ErrForbidden
	}
	v.Name = strings.TrimSpace(v.Name)
	if len(v.Name) < 1 || len(v.Name) > 80 || strings.ContainsAny(v.Name, "\x00\r\n") || v.Configuration.validate(v.Credentials) != nil {
		return Provider{}, ErrInvalid
	}
	id := identity.NewID()
	b := makeBackend(v.Configuration, v.Credentials, r.instance, id)
	if v.Configuration.Kind == "local" {
		if err := os.MkdirAll(b.local, 0700); err != nil {
			return Provider{}, ErrUnavailable
		}
	}
	if err := r.check(ctx, b); err != nil {
		return Provider{}, err
	}
	tx, err := r.transaction(ctx, p)
	if err != nil {
		return Provider{}, err
	}
	defer tx.Rollback(ctx)
	var n int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.storage_providers`).Scan(&n); err != nil {
		return Provider{}, err
	}
	if n >= 16 {
		return Provider{}, identity.ErrConflict
	}
	raw, _ := json.Marshal(v.Configuration)
	var secret []byte
	if v.Configuration.Kind == "s3" {
		secret = r.encrypt(id, v.Credentials)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chatxo.storage_providers(id,name,configuration,credentials) VALUES($1,$2,$3,$4)`, id, v.Name, raw, secret); err != nil {
		return Provider{}, err
	}
	if err = r.ids.AuditTransaction(ctx, tx, p, id, "storage.registered"); err != nil {
		return Provider{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Provider{}, err
	}
	return Provider{ID: id, Name: v.Name, Configuration: v.Configuration, HasCredentials: secret != nil}, nil
}
func (r *Registry) check(ctx context.Context, b *backend) error {
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return identity.ErrBusy
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	id := identity.NewID()
	payload := make([]byte, 64)
	if _, err := rand.Read(payload); err != nil {
		return err
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = b.remove(cleanup, id)
	}()
	if err := b.put(ctx, id, bytes.NewReader(payload)); err != nil {
		return ErrUnavailable
	}
	f, err := b.open(ctx, id, int64(len(payload)))
	if err != nil {
		return ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65))
	closeErr := f.Close()
	if err != nil || closeErr != nil || !bytes.Equal(raw, payload) {
		return ErrUnavailable
	}
	if err = b.remove(ctx, id); err != nil {
		return ErrUnavailable
	}
	return nil
}
func (r *Registry) Check(ctx context.Context, id string) error {
	if !identity.ValidID(id) {
		return ErrInvalid
	}
	b, err := r.backend(ctx, id)
	if err != nil {
		return err
	}
	return r.check(ctx, b)
}
func (r *Registry) Activate(ctx context.Context, p identity.Principal, id string) error {
	if !p.Can(identity.ManageConfiguration) {
		return identity.ErrForbidden
	}
	if id != "" {
		if err := r.Check(ctx, id); err != nil {
			return err
		}
	}
	tx, err := r.transaction(ctx, p)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if id != "" {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.storage_providers WHERE id=$1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return identity.ErrNotFound
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.storage_providers SET active=false WHERE active`); err != nil {
		return err
	}
	if id != "" {
		if _, err = tx.Exec(ctx, `UPDATE chatxo.storage_providers SET active=true WHERE id=$1`, id); err != nil {
			return err
		}
	}
	if err = r.ids.AuditTransaction(ctx, tx, p, id, "storage.activated"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (r *Registry) Delete(ctx context.Context, p identity.Principal, id string) error {
	if !identity.ValidID(id) {
		return ErrInvalid
	}
	tx, err := r.transaction(ctx, p)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var used bool
	err = tx.QueryRow(ctx, `SELECT active OR EXISTS(SELECT 1 FROM chatxo.attachments WHERE storage_provider_id=$1) FROM chatxo.storage_providers WHERE id=$1`, id).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.ErrNotFound
	}
	if err != nil {
		return err
	}
	if used {
		return ErrInUse
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.storage_providers WHERE id=$1`, id); err != nil {
		return err
	}
	if err = r.ids.AuditTransaction(ctx, tx, p, id, "storage.removed"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (r *Registry) RotateCredentials(ctx context.Context, p identity.Principal, id string, k Credentials) error {
	if !p.Can(identity.ManageConfiguration) {
		return identity.ErrForbidden
	}
	if !identity.ValidID(id) {
		return ErrInvalid
	}
	b, err := r.backend(ctx, id)
	if err != nil {
		return err
	}
	if b.client == nil {
		return ErrInvalid
	}
	var c Configuration
	var raw []byte
	if err = r.pool.QueryRow(ctx, `SELECT configuration FROM chatxo.storage_providers WHERE id=$1`, id).Scan(&raw); err != nil {
		return err
	}
	if json.Unmarshal(raw, &c) != nil || c.validate(k) != nil {
		return ErrInvalid
	}
	if err = r.check(ctx, makeBackend(c, k, r.instance, id)); err != nil {
		return err
	}
	tx, err := r.transaction(ctx, p)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE chatxo.storage_providers SET credentials=$2 WHERE id=$1`, id, r.encrypt(id, k))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return identity.ErrNotFound
	}
	if err = r.ids.AuditTransaction(ctx, tx, p, id, "storage.credentials.updated"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (r *Registry) Put(ctx context.Context, provider, id string, reader io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()
	b, err := r.backend(ctx, provider)
	if err != nil {
		return err
	}
	if b.put(ctx, id, reader) != nil {
		return ErrUnavailable
	}
	return nil
}
func (r *Registry) Open(ctx context.Context, provider, id string, size int64) (ReadSeekCloser, error) {
	b, err := r.backend(ctx, provider)
	if err != nil {
		return nil, err
	}
	f, err := b.open(ctx, id, size)
	if err != nil {
		return nil, ErrUnavailable
	}
	return f, nil
}
func (r *Registry) Remove(ctx context.Context, provider, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	b, err := r.backend(ctx, provider)
	if err != nil {
		return err
	}
	if b.remove(ctx, id) != nil {
		return ErrUnavailable
	}
	return nil
}
