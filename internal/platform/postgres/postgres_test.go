package postgres

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/example-git/haunt-server/internal/testutil"
)

func TestMigrationConcurrencyIdentityAndUpgrade(t *testing.T) {
	pool, name := testutil.Database(t)
	ctx := context.Background()
	// An unrelated table in this disposable database survives every migration.
	if _, err := pool.Exec(ctx, `CREATE TABLE public.sentinel(value text); INSERT INTO public.sentinel VALUES('preserve')`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); failures <- Migrate(ctx, pool, name) }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, pool, "different-instance"); err == nil {
		t.Fatal("changed stored identity")
	}
	var value string
	if err := pool.QueryRow(ctx, `SELECT value FROM public.sentinel`).Scan(&value); err != nil || value != "preserve" {
		t.Fatal("migration touched unrelated data")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO chatxo.schema_migrations VALUES(999)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, name); err == nil {
		t.Fatal("newer schema accepted")
	}

	older, oldName := testutil.Database(t)
	sql, err := migrations.ReadFile("migrations/001_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = older.Exec(ctx, `CREATE SCHEMA chatxo; CREATE TABLE chatxo.schema_migrations(version integer PRIMARY KEY); INSERT INTO chatxo.schema_migrations VALUES(1);`+string(sql)); err != nil {
		t.Fatal(err)
	}
	if _, err = older.Exec(ctx, `INSERT INTO chatxo.instances(id) VALUES($1)`, oldName); err != nil {
		t.Fatal(err)
	}
	if _, err = older.Exec(ctx, `INSERT INTO chatxo.users(id,instance_id,username,password_hash,is_owner) VALUES(gen_random_uuid(),$1,'owner','preserved-hash',true)`, oldName); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, older, oldName); err != nil {
		t.Fatal(err)
	}
	var disabled bool
	if err = older.QueryRow(ctx, `SELECT password_hash,disabled FROM chatxo.users WHERE is_owner`).Scan(&value, &disabled); err != nil || value != "preserved-hash" || disabled {
		t.Fatal("upgrade failed to preserve owner")
	}
}

func TestChannelIconUpgradePreservesExistingChannels(t *testing.T) {
	pool, instance := testutil.Database(t)
	ctx := context.Background()
	// Build the historical v10 schema directly so later migrations do not make
	// this fixture pretend that a newer database is still v10.
	if _, err := pool.Exec(ctx, `CREATE SCHEMA chatxo; CREATE TABLE chatxo.schema_migrations(version integer PRIMARY KEY); INSERT INTO chatxo.schema_migrations VALUES(10)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() > "010_password_reset.sql" {
			continue
		}
		raw, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO chatxo.instances(id) VALUES($1)`, instance); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO chatxo.channels(id,instance_id,name,kind,topic) VALUES(gen_random_uuid(),$1,'existing','voice','Preserve topic') RETURNING id`, instance).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, instance); err != nil {
		t.Fatal(err)
	}
	var name, kind, topic, icon string
	if err := pool.QueryRow(ctx, `SELECT name,kind,topic,icon FROM chatxo.channels WHERE id=$1`, id).Scan(&name, &kind, &topic, &icon); err != nil {
		t.Fatal(err)
	}
	if name != "existing" || kind != "voice" || topic != "Preserve topic" || icon != "" {
		t.Fatal("upgrade changed existing channel metadata")
	}
}

func TestPoolConfigurationAndSecretRedaction(t *testing.T) {
	for _, input := range []string{"postgres://user:never-log-this@[/db", "never-log-this"} {
		_, err := Open(context.Background(), input, 2)
		if err == nil || strings.Contains(err.Error(), "never-log-this") {
			t.Fatal("invalid configuration or secret redaction failed")
		}
	}
	if _, err := Open(context.Background(), "unused", 1); err == nil {
		t.Fatal("single-connection pool accepted")
	}
	pool, _ := testutil.Database(t)
	cfg := pool.Config().ConnConfig.ConnString()
	checked, err := Open(context.Background(), cfg, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer checked.Close()
	if checked.Config().MaxConns != 3 {
		t.Fatal("pool maximum not authoritative")
	}
}

func TestGatewayMigrationBackfillsMentionsAndAccounting(t *testing.T) {
	pool, name := testutil.Database(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE SCHEMA chatxo; CREATE TABLE chatxo.schema_migrations(version integer PRIMARY KEY); INSERT INTO chatxo.schema_migrations VALUES(5)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_identity.sql", "002_chat.sql", "003_operations.sql", "004_indexes.sql", "005_customization.sql"} {
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO chatxo.instances(id) VALUES($1)`, name); err != nil {
		t.Fatal(err)
	}
	var user, conv string
	if err := pool.QueryRow(ctx, `INSERT INTO chatxo.users(id,instance_id,username,password_hash,is_owner) VALUES(gen_random_uuid(),$1,'alice','hash',true) RETURNING id`, name).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO chatxo.conversations(id,instance_id,kind) VALUES(gen_random_uuid(),$1,'dm') RETURNING id`, name).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	bodies := []string{"Hello @Alice! @alice", "mail@alice.test https://x/@alice @alice_longer", "(@alice), @ALICE", "@alice"}
	for i, body := range bodies {
		if _, err := pool.Exec(ctx, `INSERT INTO chatxo.messages(id,conversation_id,author_id,seq,body,created_at,request_id,request_hash,bytes,deleted) VALUES(gen_random_uuid(),$1,$2,$3,$4,now(),gen_random_uuid(),'hash',100,$5)`, conv, user, i+1, body, i == 3); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE chatxo.storage SET text_bytes=400`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, name); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT seq,mention_user_ids,bytes FROM chatxo.messages ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	total := int64(0)
	for rows.Next() {
		var seq int
		var ids []string
		var size int64
		if err := rows.Scan(&seq, &ids, &size); err != nil {
			t.Fatal(err)
		}
		if seq == 1 || seq == 3 {
			if len(ids) != 1 || ids[0] != user || size != int64(100+len(`,"mention_user_ids":["`+user+`"]`)) {
				t.Fatalf("mention backfill %d %v %d", seq, ids, size)
			}
		} else if len(ids) != 0 || size != 100 {
			t.Fatalf("false mention %d %v", seq, ids)
		}
		total += size
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var stored int64
	if err := pool.QueryRow(ctx, `SELECT text_bytes FROM chatxo.storage`).Scan(&stored); err != nil || stored != total {
		t.Fatalf("accounting %d != %d: %v", stored, total, err)
	}
}

func TestRoleSectionUpgradePreservesRoles(t *testing.T) {
	pool, instance := testutil.Database(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE SCHEMA chatxo; CREATE TABLE chatxo.schema_migrations(version integer PRIMARY KEY); INSERT INTO chatxo.schema_migrations VALUES(12)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() > "012_runtime_storage.sql" {
			continue
		}
		raw, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO chatxo.instances(id) VALUES($1)`, instance); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO chatxo.roles(id,instance_id,name,permissions,position) VALUES(gen_random_uuid(),$1,'Existing hosts',71,12) RETURNING id`, instance).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, instance); err != nil {
		t.Fatal(err)
	}
	var name string
	var bits, rank int
	var display bool
	if err := pool.QueryRow(ctx, `SELECT name,permissions,position,display_separately FROM chatxo.roles WHERE id=$1`, id).Scan(&name, &bits, &rank, &display); err != nil {
		t.Fatal(err)
	}
	if name != "Existing hosts" || bits != 71 || rank != 12 || display {
		t.Fatal("upgrade changed an existing role or enabled grouping")
	}
	if _, err := pool.Exec(ctx, `UPDATE chatxo.roles SET display_separately=true WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, instance); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT display_separately FROM chatxo.roles WHERE id=$1`, id).Scan(&display); err != nil || !display {
		t.Fatal("repeat migration lost the saved setting")
	}
}
