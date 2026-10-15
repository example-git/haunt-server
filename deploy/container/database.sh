#!/bin/bash
# Kept separate so initialization can be exercised inside an isolated database
# fixture without starting another application or touching a host database.
set -Eeuo pipefail
export PGDATA=${PGDATA:-/data/postgres}
export PGHOST=${PGHOST:-/run/postgresql}
export PGPORT=${PGPORT:-5432}

case "${1:-}" in
    init)
        install -d -m 2775 -o postgres -g postgres "$PGHOST"
        if [[ -f "$PGDATA/PG_VERSION" ]]; then
            [[ $(cat "$PGDATA/PG_VERSION") == 16 ]] || {
                echo 'PostgreSQL major version differs; migrate a backup, never reuse this data directory with a different major.' >&2
                exit 1
            }
            exit 0
        fi
        # An interrupted init never makes a partial cluster look usable. Refuse
        # unknown populated directories; only our disposable staging is reset.
        if [[ -d "$PGDATA" ]] && [[ -n $(ls -A "$PGDATA") ]]; then
            echo 'Refusing to initialize a nonempty PostgreSQL data directory.' >&2
            exit 1
        fi
        rm -rf -- "${PGDATA}.initializing"
        install -d -m 700 -o postgres -g postgres "${PGDATA}.initializing"
        su-exec postgres initdb -D "${PGDATA}.initializing" --encoding=UTF8 --locale=C \
            --auth-local=peer --auth-host=scram-sha-256 --data-checksums
        rmdir "$PGDATA" 2>/dev/null || true
        mv "${PGDATA}.initializing" "$PGDATA"
        ;;
    provision)
        # No TCP listener or shared database password. OS identity authenticates
        # the application, whose role cannot create roles/databases or bypass RLS.
        su-exec postgres psql -X -v ON_ERROR_STOP=1 -d postgres <<'SQL'
SELECT 'CREATE ROLE haunt LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS'
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'haunt') \gexec
SELECT 'CREATE DATABASE haunt OWNER haunt'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'haunt') \gexec
SQL
        ;;
    *) echo 'Usage: database.sh init|provision' >&2; exit 2 ;;
esac
