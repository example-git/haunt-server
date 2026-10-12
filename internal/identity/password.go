package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// RFC 9106's second recommended Argon2id profile: 64 MiB, 3 passes, 4 lanes.
// A per-service semaphore bounds memory/CPU. Admission fails immediately rather
// than accumulating an unbounded queue; no database connection is held hashing.
type Passwords struct {
	slots chan struct{}
	dummy string
}

func NewPasswords(parallel int) (*Passwords, error) {
	if parallel < 1 || parallel > 32 {
		return nil, fmt.Errorf("password hashing concurrency must be between 1 and 32")
	}
	p := &Passwords{slots: make(chan struct{}, parallel)}
	dummy, err := p.Hash(context.Background(), "unmatchable-"+newToken())
	p.dummy = dummy
	return p, err
}

func (p *Passwords) admit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case p.slots <- struct{}{}:
		return nil
	default:
		return ErrBusy
	}
}

func (p *Passwords) Hash(ctx context.Context, password string) (string, error) {
	if err := p.admit(ctx); err != nil {
		return "", err
	}
	defer func() { <-p.slots }()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "$argon2id$v=19$m=65536,t=3,p=4$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

func (p *Passwords) Verify(ctx context.Context, password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=4" {
		return false, fmt.Errorf("unsupported password hash")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		return false, fmt.Errorf("invalid password salt")
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) != 32 {
		return false, fmt.Errorf("invalid password hash")
	}
	if err := p.admit(ctx); err != nil {
		return false, err
	}
	defer func() { <-p.slots }()
	key := argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(key, expected) == 1, nil
}
