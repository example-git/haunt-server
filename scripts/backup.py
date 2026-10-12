#!/usr/bin/env python3
"""Offline PostgreSQL + attachment backups with checksums and bounded rotation.

Requires HAUNT_DATABASE_URL, HAUNT_INSTANCE_ID, HAUNT_DATA_DIR (default data),
Python 3, and matching PostgreSQL client tools. Stop the server before use.
"""
import argparse
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import uuid
from urllib.parse import urlsplit, unquote, parse_qsl

UUID_NAME = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(?:\.part)?$")
BACKUP_NAME = re.compile(r"^\d{8}T\d{6}\.\d{6}Z-[0-9a-f]{8}$")
KIND = "chatxo-backup-v1"


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1 << 20), b""):
            value.update(chunk)
    return value.hexdigest()


def database(command, *arguments):
    # Connection secrets stay out of command-line arguments and logs.
    # Inherited libpq options such as PGHOSTADDR/PGSERVICE must never redirect
    # an explicitly configured backup/restore to another PostgreSQL server.
    env = {key: value for key, value in os.environ.items() if not key.startswith("PG")}
    connection = urlsplit(os.environ["HAUNT_DATABASE_URL"])
    if connection.scheme not in ("postgres", "postgresql"):
        raise RuntimeError("backup tooling requires a PostgreSQL connection URI")
    env["PGDATABASE"] = unquote(connection.path.removeprefix("/"))
    if not env["PGDATABASE"]:
        raise RuntimeError("database URI requires a database name")
    if connection.hostname:
        env["PGHOST"] = unquote(connection.hostname)
    if connection.port:
        env["PGPORT"] = str(connection.port)
    if connection.username is not None:
        env["PGUSER"] = unquote(connection.username)
    if connection.password is not None:
        env["PGPASSWORD"] = unquote(connection.password)
    parameters = {"sslmode":"PGSSLMODE", "sslrootcert":"PGSSLROOTCERT", "sslcert":"PGSSLCERT", "sslkey":"PGSSLKEY", "sslpassword":"PGSSLPASSWORD", "channel_binding":"PGCHANNELBINDING", "target_session_attrs":"PGTARGETSESSIONATTRS", "connect_timeout":"PGCONNECT_TIMEOUT", "host":"PGHOST", "hostaddr":"PGHOSTADDR", "port":"PGPORT", "user":"PGUSER", "password":"PGPASSWORD", "dbname":"PGDATABASE"}
    for key, value in parse_qsl(connection.query):
        if key in parameters:
            env[parameters[key]] = value
        elif not key.startswith("pool_"):
            raise RuntimeError("unsupported database URI option for backup tooling")
    if "=" in env["PGDATABASE"] or env["PGDATABASE"].startswith(("postgres:", "postgresql:")):
        raise RuntimeError("backup tooling requires a plain database name")
    env.setdefault("PGCONNECT_TIMEOUT", "5")
    if command == "pg_restore":
        arguments = ("--dbname=" + env["PGDATABASE"], *arguments)
    result = subprocess.run([command, *arguments], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode:
        raise RuntimeError(command + " failed; verify database permissions and client/server versions")
    return result.stdout.decode().strip()


def sql(query):
    return database("psql", "-X", "-At", "-v", "ON_ERROR_STOP=1", "-c", query)


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def backup(root, attachments, keep):
    stored_id = sql("SELECT id FROM chatxo.instances WHERE singleton")
    if stored_id != os.environ["HAUNT_INSTANCE_ID"]:
        raise RuntimeError("configured instance ID differs from the database")
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    timestamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    destination = root / (timestamp + "-" + uuid.uuid4().hex[:8])
    staging = Path(tempfile.mkdtemp(prefix=".chatxo-backup-", dir=root))
    try:
        database("pg_dump", "--format=custom", "--no-owner", "--no-acl", "--file=" + str(staging / "database.dump"))
        os.chmod(staging / "database.dump", 0o600)
        files = {"database.dump": digest(staging / "database.dump")}
        (staging / "attachments").mkdir(mode=0o700)
        for source in attachments.iterdir():
            if source.name == ".lock":
                continue
            if source.is_symlink() or not source.is_file() or not UUID_NAME.fullmatch(source.name):
                raise RuntimeError("unexpected entry in attachment directory")
            target = staging / "attachments" / source.name
            shutil.copyfile(source, target)
            os.chmod(target, 0o600)
            files["attachments/" + source.name] = digest(target)
        key = attachments.parent / "storage-provider.key"
        if key.exists():
            if key.is_symlink() or not key.is_file() or key.stat().st_size != 32:
                raise RuntimeError("invalid storage provider encryption key")
            shutil.copyfile(key, staging / key.name)
            os.chmod(staging / key.name, 0o600)
            files[key.name] = digest(staging / key.name)
        plugins = attachments.parent / "audio-plugins"
        if plugins.exists():
            if plugins.is_symlink() or not plugins.is_dir():
                raise RuntimeError("invalid audio plugin directory")
            (staging / "audio-plugins").mkdir(mode=0o700)
            for source in plugins.iterdir():
                if source.name.startswith(".upload-"):
                    continue
                if source.is_symlink() or not source.is_file() or not re.fullmatch(r"[a-z][a-z0-9_-]{0,63}\.zip", source.name) or source.stat().st_size > 50 * 1024 * 1024:
                    raise RuntimeError("invalid audio plugin package")
                target = staging / "audio-plugins" / source.name
                shutil.copyfile(source, target)
                os.chmod(target, 0o600)
                files["audio-plugins/" + source.name] = digest(target)
            sync_directory(staging / "audio-plugins")
        external = []
        if sql("SELECT to_regclass('chatxo.storage_providers') IS NOT NULL") == "t":
            if sql("SELECT EXISTS(SELECT 1 FROM chatxo.storage_providers WHERE credentials IS NOT NULL)") == "t" and key.name not in files:
                raise RuntimeError("storage provider credentials require storage-provider.key in the backup")
            external = json.loads(sql("SELECT COALESCE(json_agg(json_build_object('id',id,'name',name,'configuration',configuration)), '[]'::json) FROM chatxo.storage_providers"))
        manifest = {"external_storage": external, "kind": KIND, "instance_id": stored_id, "created_at": timestamp, "files": files}
        (staging / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        os.chmod(staging / "manifest.json", 0o600)
        for path in [staging / name for name in files] + [staging / "manifest.json"]:
            with path.open("rb") as handle:
                os.fsync(handle.fileno())
        sync_directory(staging / "attachments")
        sync_directory(staging)
        staging.rename(destination)
        sync_directory(root)
        candidates = []
        for item in root.iterdir():
            if item.is_symlink() or not item.is_dir() or not BACKUP_NAME.fullmatch(item.name):
                continue
            try:
                info = json.loads((item / "manifest.json").read_text())
            except (OSError, ValueError):
                continue
            if isinstance(info, dict) and info.get("kind") == KIND and info.get("instance_id") == stored_id and isinstance(info.get("created_at"), str):
                candidates.append((info["created_at"], item))
        candidates.sort(reverse=True)
        for _, expired in candidates[keep:]:
            shutil.rmtree(expired)
        sync_directory(root)
        print(str(destination))
    finally:
        if staging.exists():
            shutil.rmtree(staging)


def restore(source, attachments):
    if (source / "manifest.json").is_symlink() or (source / "attachments").is_symlink() or (source / "audio-plugins").is_symlink():
        raise RuntimeError("backup entries must not be symbolic links")
    manifest = json.loads((source / "manifest.json").read_text())
    if not isinstance(manifest, dict) or manifest.get("kind") != KIND or manifest.get("instance_id") != os.environ["HAUNT_INSTANCE_ID"]:
        raise RuntimeError("backup kind/instance ID mismatch")
    files = manifest.get("files", {})
    if not isinstance(files, dict) or "database.dump" not in files:
        raise RuntimeError("backup has no database dump")
    if sum(name.startswith("audio-plugins/") for name in files) > 4:
        raise RuntimeError("too many audio plugins in backup")
    for name, checksum in files.items():
        if name not in ("database.dump", "storage-provider.key") and not (name.startswith("attachments/") and UUID_NAME.fullmatch(name.removeprefix("attachments/"))) and not re.fullmatch(r"audio-plugins/[a-z][a-z0-9_-]{0,63}\.zip", name):
            raise RuntimeError("unsafe backup path")
        path = source / name
        if name.startswith("audio-plugins/") and path.stat().st_size > 50 * 1024 * 1024:
            raise RuntimeError("oversized audio plugin in backup")
        if path.is_symlink() or not path.is_file() or digest(path) != checksum:
            raise RuntimeError("backup checksum/type validation failed")
    if "storage-provider.key" in files and (source / "storage-provider.key").stat().st_size != 32:
        raise RuntimeError("invalid storage provider encryption key")
    if (attachments.parent / "storage-provider.key").exists() or (attachments.parent / "storage-provider.key").is_symlink():
        raise RuntimeError("restore requires a data directory without an existing storage provider key")
    if sql("""SELECT (SELECT count(*) FROM pg_namespace WHERE nspname NOT IN ('public','information_schema') AND nspname !~ '^pg_')
        + (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public')
        + (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public')
        + (SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname='public')
        + (SELECT count(*) FROM pg_extension WHERE extname<>'plpgsql')""") != "0":
        raise RuntimeError("restore requires an empty target database; no existing data will be overwritten")
    if any(path.name != ".lock" for path in attachments.iterdir()):
        raise RuntimeError("restore requires an empty attachment directory")
    plugins = attachments.parent / "audio-plugins"
    if plugins.is_symlink() or (plugins.exists() and any(plugins.iterdir())):
        raise RuntimeError("restore requires an empty audio plugin directory")
    staging = Path(tempfile.mkdtemp(prefix=".chatxo-restore-", dir=attachments.parent))
    committed = False
    try:
        for name in files:
            if name == "storage-provider.key" or name.startswith(("attachments/", "audio-plugins/")):
                target = staging / name.removeprefix("attachments/")
                target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                shutil.copyfile(source / name, target)
                os.chmod(target, 0o600)
                with target.open("rb") as handle:
                    os.fsync(handle.fileno())
        database("pg_restore", "--single-transaction", "--no-owner", "--no-acl", str(source / "database.dump"))
        committed = True
        for path in staging.iterdir():
            destination = attachments.parent if path.name in ("storage-provider.key", "audio-plugins") else attachments
            if path.name == "audio-plugins" and plugins.exists():
                plugins.rmdir()
            path.rename(destination / path.name)
        sync_directory(attachments)
        sync_directory(attachments.parent)
        if manifest.get("external_storage"):
            print("External storage objects are not inside this backup; restore the listed providers and their object snapshots before using their attachments.")
        print("Restored installation; start the server to reconcile storage.")
    except Exception:
        if committed:
            raise RuntimeError("database restored, but file finalization failed; recover attachment files from " + str(staging))
        raise
    finally:
        if staging.exists() and (not committed or not any(staging.iterdir())):
            shutil.rmtree(staging)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=["backup", "restore"])
    parser.add_argument("path", type=Path, help="backup root, or a specific backup directory to restore")
    parser.add_argument("--keep", type=int, default=3, help="keep this many backups of this installation (default 3)")
    args = parser.parse_args()
    if not 1 <= args.keep <= 365:
        parser.error("--keep must be between 1 and 365")
    for key in ("HAUNT_DATABASE_URL", "HAUNT_INSTANCE_ID"):
        if not os.environ.get(key):
            parser.error(key + " is required")
    attachments = Path(os.environ.get("HAUNT_DATA_DIR", "data")).resolve() / "attachments"
    if attachments.is_symlink():
        raise RuntimeError("attachment directory must not be a symbolic link")
    target = args.path.resolve()
    if target == attachments or attachments in target.parents:
        raise RuntimeError("backup destination/source must be outside the attachment directory")
    attachments.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (attachments / ".lock").open("a+b") as lock:
        try:
            fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise RuntimeError("stop the running server before backing up or restoring")
        if args.operation == "backup":
            backup(target, attachments, args.keep)
        else:
            restore(target, attachments)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, KeyError) as exc:
        print("Backup/restore failed: " + str(exc), file=sys.stderr)
        sys.exit(1)
