// Package postgres owns a concurrent connection pool, not a single shared
// connection or a process-wide serialized database worker.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open validates configuration and verifies initial connectivity. The caller
// owns the returned pool and closes it after all dependent work has stopped.
func Open(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	if maxConns < 2 {
		return nil, fmt.Errorf("PostgreSQL pool requires at least two connections")
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// Parse errors can contain the original connection string. Keep secrets
		// out of startup logs while failing the explicit value visibly.
		return nil, fmt.Errorf("invalid HAUNT_DATABASE_URL")
	}
	cfg.MaxConns = maxConns
	if cfg.MinConns > cfg.MaxConns {
		return nil, fmt.Errorf("database pool minimum exceeds HAUNT_DB_MAX_CONNS")
	}
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("could not initialize PostgreSQL pool")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("PostgreSQL connectivity check failed; verify database address, credentials, and TLS settings")
	}
	return pool, nil
}
