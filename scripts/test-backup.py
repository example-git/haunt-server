"""File backup regression tests. Database tools are mocked; no PostgreSQL access."""
import contextlib
import importlib.util
import io
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
spec = importlib.util.spec_from_file_location("backup", Path(__file__).with_name("backup.py"))
backup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(backup)

class PluginBackupTests(unittest.TestCase):
    def test_plugin_defaults_and_removed_bundle_marker_roundtrip(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            attachments = root / "data/attachments"
            attachments.mkdir(parents=True)
            plugins = attachments.parent / "audio-plugins"
            plugins.mkdir()
            content = {"defaults.json": b'{"id":"rnnoise","variant":"little","values":{"strength":75}}', ".default-seeded": b"1\n", **{f"plugin-{i}.zip": b"fixture" for i in range(4)}}
            for name, data in content.items(): (plugins / name).write_bytes(data)
            def database(command, *args):
                if command == "pg_dump":
                    Path(next(a.removeprefix("--file=") for a in args if a.startswith("--file="))).write_bytes(b"mock database")
            def sql(query):
                if query.startswith("SELECT id"): return "test-installation"
                if "to_regclass" in query: return "f"
                return "0"
            target = root / "restored/attachments"
            target.mkdir(parents=True)
            with patch.dict(os.environ, {"HAUNT_INSTANCE_ID": "test-installation"}), patch.object(backup, "database", database), patch.object(backup, "sql", sql), contextlib.redirect_stdout(io.StringIO()):
                backup.backup(root / "backups", attachments, 3)
                saved = next((root / "backups").iterdir())
                backup.restore(saved, target)
            for name, data in content.items(): self.assertEqual((target.parent / "audio-plugins" / name).read_bytes(), data)

    def test_metadata_is_bounded_and_unknown_files_rejected(self):
        self.assertEqual(backup.plugin_file_limit("defaults.json"), 32768)
        self.assertEqual(backup.plugin_file_limit(".default-seeded"), 16)
        for path in ("../defaults.json", "custom.js", ".ssh", "other.json"):
            with self.assertRaises(RuntimeError): backup.plugin_file_limit(path)

class AIKeyBackupTests(unittest.TestCase):
    def database(self, command, *args):
        if command == "pg_dump":
            Path(next(a.removeprefix("--file=") for a in args if a.startswith("--file="))).write_bytes(b"mock database")

    def sql(self, query):
        if query.startswith("SELECT id"): return "test-installation"
        if "to_regclass('chatxo.ai_accounts')" in query: return "t"
        if "to_regclass" in query: return "f"
        if "chatxo.ai_accounts WHERE credentials" in query: return "t"
        return "0"

    def test_ai_key_roundtrip(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            attachments = root / "data/attachments"
            attachments.mkdir(parents=True)
            key = bytes(range(32))
            (attachments.parent / "ai-provider.key").write_bytes(key)
            target = root / "restored/attachments"
            target.mkdir(parents=True)
            with patch.dict(os.environ, {"HAUNT_INSTANCE_ID": "test-installation"}), patch.object(backup, "database", self.database), patch.object(backup, "sql", self.sql), contextlib.redirect_stdout(io.StringIO()):
                backup.backup(root / "backups", attachments, 3)
                saved = next((root / "backups").iterdir())
                backup.restore(saved, target)
            restored = target.parent / "ai-provider.key"
            self.assertEqual(restored.read_bytes(), key)
            self.assertEqual(restored.stat().st_mode & 0o777, 0o600)

    def test_missing_ai_key_blocks_backup(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            attachments = root / "data/attachments"
            attachments.mkdir(parents=True)
            with patch.dict(os.environ, {"HAUNT_INSTANCE_ID": "test-installation"}), patch.object(backup, "database", self.database), patch.object(backup, "sql", self.sql):
                with self.assertRaisesRegex(RuntimeError, "AI credentials require ai-provider.key"):
                    backup.backup(root / "backups", attachments, 3)
            self.assertEqual(list((root / "backups").iterdir()), [])

    def test_missing_or_existing_ai_key_blocks_restore_before_database(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            attachments = root / "data/attachments"
            attachments.mkdir(parents=True)
            (attachments.parent / "ai-provider.key").write_bytes(bytes(32))
            target = root / "restored/attachments"
            target.mkdir(parents=True)
            with patch.dict(os.environ, {"HAUNT_INSTANCE_ID": "test-installation"}), patch.object(backup, "database", self.database), patch.object(backup, "sql", self.sql), contextlib.redirect_stdout(io.StringIO()):
                backup.backup(root / "backups", attachments, 3)
                saved = next((root / "backups").iterdir())
                (target.parent / "ai-provider.key").write_bytes(bytes(32))
                with self.assertRaisesRegex(RuntimeError, "existing provider key"):
                    backup.restore(saved, target)
                (target.parent / "ai-provider.key").unlink()
                manifest = backup.json.loads((saved / "manifest.json").read_text())
                del manifest["files"]["ai-provider.key"]
                (saved / "manifest.json").write_text(backup.json.dumps(manifest))
                with self.assertRaisesRegex(RuntimeError, "AI credentials require ai-provider.key"):
                    backup.restore(saved, target)

if __name__ == "__main__": unittest.main()
