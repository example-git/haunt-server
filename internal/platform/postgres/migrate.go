package postgres

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

const SchemaVersion = 17

// Migrate serializes schema changes at startup only. Normal requests use the
// pool independently. The schema and installation identity commit together.
func Migrate(ctx context.Context, pool *pgxpool.Pool, instanceID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(831684291)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS chatxo;
        CREATE TABLE IF NOT EXISTS chatxo.schema_migrations (version integer PRIMARY KEY)`); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM chatxo.schema_migrations`).Scan(&version); err != nil {
		return err
	}
	if version > SchemaVersion {
		return fmt.Errorf("database schema is newer than this server")
	}
	for _, migration := range []struct {
		version int
		name    string
	}{{1, "001_identity.sql"}, {2, "002_chat.sql"}, {3, "003_operations.sql"}, {4, "004_indexes.sql"}, {5, "005_customization.sql"}, {6, "006_gateway_reads.sql"}, {7, "007_emojis_animated_art.sql"}, {8, "008_persistent_sessions.sql"}, {9, "009_moderation.sql"}, {10, "010_password_reset.sql"}, {11, "011_channel_icons.sql"}, {12, "012_runtime_storage.sql"}, {13, "013_role_sections.sql"}, {14, "014_dm_storage.sql"}, {15, "015_dm_mls.sql"}, {16, "016_message_replies.sql"}, {17, "017_administrator.sql"}} {
		if version >= migration.version {
			continue
		}
		script, readErr := migrations.ReadFile("migrations/" + migration.name)
		if readErr != nil {
			return readErr
		}
		if _, err = tx.Exec(ctx, string(script)); err != nil {
			return fmt.Errorf("migration %d: %w", migration.version, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO chatxo.schema_migrations(version) VALUES ($1)`, migration.version); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chatxo.instances(id) VALUES ($1) ON CONFLICT (singleton) DO NOTHING`, instanceID); err != nil {
		return err
	}
	var storedID string
	if err = tx.QueryRow(ctx, `SELECT id FROM chatxo.instances WHERE singleton`).Scan(&storedID); err != nil {
		return err
	}
	if storedID != instanceID {
		return fmt.Errorf("HAUNT_INSTANCE_ID differs from the persisted installation identity")
	}
	return tx.Commit(ctx)
}
