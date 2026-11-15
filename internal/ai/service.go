package ai

import (
	"context"
	"crypto/cipher"
	"errors"
	"github.com/example-git/haunt-server/internal/ai/provider"
	aitools "github.com/example-git/haunt-server/internal/ai/tools"
	"github.com/example-git/haunt-server/internal/attachments"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/platform/postgres"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"sync"
	"time"
)

type Service struct {
	pool        postgres.Pool
	ids         *identity.Service
	chat        *chat.Store
	attachments *attachments.Store
	options     Options
	adapters    map[provider.ID]provider.Adapter
	tools       ToolRegistry
	aead        cipher.AEAD
	mu          sync.Mutex
	accounts    map[string]*sync.Mutex
	running     map[string]context.CancelFunc
	leases      map[string]string
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

func New(ctx context.Context, pool postgres.Pool, ids *identity.Service, ch *chat.Store, att *attachments.Store, o Options, adapters map[provider.ID]provider.Adapter) (*Service, error) {
	if o.ChannelMaxBytes == 0 {
		o.ChannelMaxBytes = DefaultChannelMaxBytes
	}
	if o.GlobalMaxBytes == 0 {
		o.GlobalMaxBytes = DefaultGlobalMaxBytes
	}
	if o.ChannelMaxBytes < 4096 || o.GlobalMaxBytes < 4096 {
		return nil, errors.New("AI storage budgets must be at least 4096 bytes")
	}
	s := &Service{pool: pool, ids: ids, chat: ch, attachments: att, options: o, adapters: adapters, tools: aitools.New(), accounts: map[string]*sync.Mutex{}, running: map[string]context.CancelFunc{}, leases: map[string]string{}}
	var err error
	s.aead, err = s.loadKey(ctx, o.DataDir)
	if err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Service) Start(parent context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return nil
	}
	recovery, done := context.WithTimeout(parent, 15*time.Second)
	err := s.recover(recovery)
	done()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	for i := 0; i < 4; i++ {
		s.wg.Add(1)
		go s.worker(ctx)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Maintain(ctx); err != nil {
					slog.Error("AI maintenance failed", "error_code", errorCode(err))
				}
				s.refreshAccounts(ctx)
			}
		}
	}()
	return nil
}
func (s *Service) Close() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
func (s *Service) transaction(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.chat.LockStorage(ctx, tx); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Service) channel(ctx context.Context, p identity.Principal, id string, manage bool) (identity.Channel, error) {
	p, err := s.ids.Refresh(ctx, p)
	if err != nil {
		return identity.Channel{}, err
	}
	if manage && !p.Can(identity.ManageChannels) {
		return identity.Channel{}, identity.ErrForbidden
	}
	ch, err := s.ids.Channel(ctx, p, id)
	if err != nil {
		return ch, err
	}
	if ch.Kind != "ai" {
		return ch, identity.ErrInvalid
	}
	return ch, nil
}
func (s *Service) authorizeAccounts(ctx context.Context, p identity.Principal) error {
	p, err := s.ids.Refresh(ctx, p)
	if err != nil {
		return err
	}
	if !p.Can(identity.ManageConfiguration) {
		return identity.ErrForbidden
	}
	return nil
}
func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.ErrNotFound
	}
	return err
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type ToolRegistry interface {
	Schemas() []provider.Tool
	Execute(context.Context, provider.ToolCall) (provider.ToolResult, error)
}
