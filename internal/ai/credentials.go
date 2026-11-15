package ai

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"os"
	"path/filepath"
)

func (s *Service) loadKey(ctx context.Context, dir string) (cipher.AEAD, error) {
	path := filepath.Join(dir, "ai-provider.key")
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		var used bool
		if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.ai_accounts WHERE credentials IS NOT NULL) OR EXISTS(SELECT 1 FROM chatxo.ai_oauth_sessions)`).Scan(&used); err != nil {
			return nil, err
		}
		if used {
			return nil, errors.New("restore ai-provider.key alongside the database before starting")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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
		return nil, errors.New("invalid ai-provider.key")
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (s *Service) seal(id string, raw []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, raw, []byte(s.options.InstanceID+":"+id)), nil
}
func (s *Service) open(id string, raw []byte) ([]byte, error) {
	if len(raw) < s.aead.NonceSize() {
		return nil, errors.New("invalid encrypted AI credentials")
	}
	v, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], []byte(s.options.InstanceID+":"+id))
	if err != nil {
		return nil, errors.New("AI credentials cannot be decrypted with ai-provider.key")
	}
	return v, nil
}
func (s *Service) encryptCredentials(id string, c provider.Credentials) ([]byte, error) {
	raw, err := json.Marshal(c)
	if len(raw) > 65536 {
		return nil, errors.New("AI credentials exceed byte limit")
	}
	if err != nil {
		return nil, err
	}
	return s.seal(id, raw)
}
func (s *Service) decryptCredentials(id string, raw []byte) (provider.Credentials, error) {
	var c provider.Credentials
	data, err := s.open(id, raw)
	if err == nil {
		err = json.Unmarshal(data, &c)
	}
	return c, err
}
