package postgres

import (
	"context"
	"testing"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/testutil"
	"github.com/jackc/pgx/v5"
)

func TestWorkloadConnectionOwnership(t *testing.T) {
	base, _ := testutil.Database(t)
	c := config.Config{Capacity: map[config.Workload]config.WorkloadCapacity{config.History: {Active: 1, Queued: 1, Connections: 1}}}
	pools, err := OpenWorkloads(context.Background(), base, c)
	if err != nil {
		t.Fatal(err)
	}
	defer pools.Close()
	ctx := WithWorkload(context.Background(), config.History)
	if pools.For(ctx).Config().MaxConns != 1 {
		t.Fatal("explicit connection limit ignored")
	}
	tx, err := pools.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var application string
	if err := tx.QueryRow(WithWorkload(ctx, config.Media), `SELECT current_setting('application_name')`).Scan(&application); err != nil || application != "haunt_history" {
		t.Fatal("transaction moved between pools", application, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for workload, stats := range pools.WorkloadStats() {
		if stats.AcquiredConns() != 0 {
			t.Fatal(workload, "connection leaked")
		}
	}
	pools.Close()
	if err := base.Ping(context.Background()); err != nil {
		t.Fatal("feature shutdown closed caller-owned control pool", err)
	}
}
