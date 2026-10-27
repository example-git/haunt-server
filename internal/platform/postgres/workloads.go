package postgres

import (
	"context"
	"fmt"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	Config() *pgxpool.Config
	Stat() *pgxpool.Stat
}

type workloadKey struct{}

func WithWorkload(ctx context.Context, workload config.Workload) context.Context {
	return context.WithValue(ctx, workloadKey{}, workload)
}

type WorkloadPools struct {
	*pgxpool.Pool
	pools map[config.Workload]*pgxpool.Pool
}

func OpenWorkloads(ctx context.Context, base *pgxpool.Pool, c config.Config) (*WorkloadPools, error) {
	p := &WorkloadPools{Pool: base, pools: map[config.Workload]*pgxpool.Pool{config.Control: base}}
	for workload, limits := range c.WorkloadCapacities() {
		if workload == config.Control {
			continue
		}
		if limits.Connections < 1 {
			p.Close()
			return nil, fmt.Errorf("%s database pool requires at least one connection", workload)
		}
		cfg := base.Config()
		cfg.MaxConns = limits.Connections
		cfg.MinConns, cfg.MinIdleConns = 0, 0
		cfg.ConnConfig.RuntimeParams["application_name"] = "haunt_" + string(workload)
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("could not initialize %s database pool", workload)
		}
		p.pools[workload] = pool
		if err := pool.Ping(ctx); err != nil {
			p.Close()
			return nil, fmt.Errorf("%s database connectivity check failed", workload)
		}
	}
	return p, nil
}

func (p *WorkloadPools) Close() {
	for workload, pool := range p.pools {
		if workload != config.Control {
			pool.Close()
		}
	}
}

func (p *WorkloadPools) For(ctx context.Context) *pgxpool.Pool {
	workload, _ := ctx.Value(workloadKey{}).(config.Workload)
	if pool := p.pools[workload]; pool != nil {
		return pool
	}
	return p.Pool
}

func (p *WorkloadPools) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return p.For(ctx).Exec(ctx, sql, args...)
}
func (p *WorkloadPools) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.For(ctx).Query(ctx, sql, args...)
}
func (p *WorkloadPools) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.For(ctx).QueryRow(ctx, sql, args...)
}
func (p *WorkloadPools) Begin(ctx context.Context) (pgx.Tx, error) {
	return p.For(ctx).Begin(ctx)
}
func (p *WorkloadPools) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	return p.For(ctx).BeginTx(ctx, opts)
}
func (p *WorkloadPools) WorkloadStats() map[config.Workload]*pgxpool.Stat {
	stats := make(map[config.Workload]*pgxpool.Stat, len(p.pools))
	for workload, pool := range p.pools {
		stats[workload] = pool.Stat()
	}
	return stats
}
