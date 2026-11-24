package postgres

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

var migrationFiles = [...]string{
	"001_identity.sql",
	"002_chat.sql",
	"003_operations.sql",
	"004_indexes.sql",
	"005_customization.sql",
	"006_gateway_reads.sql",
	"007_emojis_animated_art.sql",
	"008_persistent_sessions.sql",
	"009_moderation.sql",
	"010_password_reset.sql",
	"011_channel_icons.sql",
	"012_runtime_storage.sql",
	"013_role_sections.sql",
	"014_dm_storage.sql",
	"015_dm_mls.sql",
	"016_message_replies.sql",
	"017_administrator.sql",
	"018_voice_moderation.sql",
	"019_history_pages.sql",
	"020_embed_cache.sql",
	"021_ai_channels.sql",
	"022_ai_research.sql",
	"023_ai_personality_handoff.sql",
	"024_message_visibility_attachment_rollover.sql",
}

const SchemaVersion = len(migrationFiles)

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
	for index, filename := range migrationFiles {
		nextVersion := index + 1
		if version >= nextVersion {
			continue
		}
		script, readErr := migrations.ReadFile("migrations/" + filename)
		if readErr != nil {
			return readErr
		}
		if _, err = tx.Exec(ctx, string(script)); err != nil {
			return fmt.Errorf("migration %d: %w", nextVersion, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO chatxo.schema_migrations(version) VALUES ($1)`, nextVersion); err != nil {
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
