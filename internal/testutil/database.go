// Package testutil confines integration databases to the dedicated test container.
// It is imported by tests only; no production connection settings are consulted.
package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Database creates an isolated database and registers its cleanup. The Docker
// check happens before any PostgreSQL connection, so a mistaken URL fails closed.
func Database(t testing.TB) (*pgxpool.Pool, string) {
	t.Helper()
	uri := os.Getenv("HAUNT_TEST_DATABASE_URL")
	if uri == "" {
		t.Skip("database tests require HAUNT_TEST_DATABASE_URL for chatxo-identity-check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "--host=unix:///var/run/docker.sock", "inspect", "chatxo-identity-check").Output()
	if err != nil {
		t.Fatal("cannot verify dedicated test container; refusing database access")
	}
	var containers []struct {
		Name            string
		Config          struct{ Image string }
		State           struct{ Running bool }
		Mounts          []struct{ Type, Name string }
		NetworkSettings struct {
			Ports map[string][]struct{ HostIp, HostPort string }
		}
	}
	if json.Unmarshal(output, &containers) != nil || len(containers) != 1 {
		t.Fatal("invalid test container inspection")
	}
	c := containers[0]
	ports := c.NetworkSettings.Ports["5432/tcp"]
	if c.Name != "/chatxo-identity-check" || c.Config.Image != "postgres:16-alpine" || !c.State.Running || len(ports) != 1 || ports[0].HostIp != "127.0.0.1" || len(c.Mounts) != 1 || c.Mounts[0].Type != "volume" || len(c.Mounts[0].Name) != 64 {
		t.Fatal("test container does not match the isolated PostgreSQL setup")
	}
	expected := "postgres://postgres@127.0.0.1:" + ports[0].HostPort + "/chatxo_check?sslmode=disable"
	if uri != expected {
		t.Fatal("HAUNT_TEST_DATABASE_URL does not match the dedicated test container; refusing database access")
	}
	admin, err := pgxpool.New(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("chatxo_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := admin.Exec(cleanup, `DROP DATABASE `+name+` WITH (FORCE)`); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(uri)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, name
}
