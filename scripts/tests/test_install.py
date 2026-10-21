import importlib.util
import os
from pathlib import Path
import stat
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("haunt_install", ROOT / "scripts/install.py")
INSTALL = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = INSTALL
SPEC.loader.exec_module(INSTALL)


class InstallerTests(unittest.TestCase):
    def test_container_profile_uses_password_file_and_rnnoise(self):
        with tempfile.TemporaryDirectory() as temporary:
            target = Path(temporary) / "profile"
            values = {field.key: field.default for _, field in INSTALL.fields_for("container") if field.default}
            values.update({
                "HAUNT_INSTANCE_ID": "test-installation",
                "HAUNT_NAME": "Night Owls",
                "HAUNT_PUBLIC_URL": "https://haunt.example.test",
                "HAUNT_MEDIA_PUBLIC_IP": "198.51.100.10",
                "HAUNT_DEFAULT_AUDIO_PLUGIN": "true",
            })
            created = INSTALL.write_install("container", values, "owner", "very long password", target, "./haunt")
            env = (target / "container.env").read_text()
            self.assertIn('HAUNT_DEFAULT_AUDIO_PLUGIN="true"', env)
            self.assertIn('HAUNT_BOOTSTRAP_PASSWORD_FILE="/run/secrets/haunt-owner-password"', env)
            self.assertNotIn("very long password", env)
            self.assertNotIn("HAUNT_MEDIA_TURN_PORT", env)
            self.assertIn("owner-password", (target / "compose.installer.yml").read_text())
            self.assertEqual(stat.S_IMODE((target / "owner-password").stat().st_mode), 0o600)
            self.assertEqual(len(created), 3)

    def test_bare_metal_profile_has_private_launcher(self):
        with tempfile.TemporaryDirectory() as temporary:
            target = Path(temporary) / "profile"
            values = {field.key: field.default for _, field in INSTALL.fields_for("bare-metal") if field.default}
            values.update({"HAUNT_INSTANCE_ID": "test", "HAUNT_PUBLIC_URL": "http://localhost:8080", "HAUNT_DATABASE_URL": "postgres://localhost/haunt", "HAUNT_NAME": "Haunt"})
            INSTALL.write_install("bare-metal", values, "owner", "very long password", target, "/opt/haunt/haunt")
            env = (target / "haunt.env").read_text()
            self.assertIn(str(target / "owner-password"), env)
            self.assertNotIn("very long password", env)
            self.assertEqual(stat.S_IMODE((target / "run-haunt.sh").stat().st_mode), 0o700)

    def test_profile_relationship_validation(self):
        values = {field.key: field.default for _, field in INSTALL.fields_for("container") if field.default}
        values.update({"HAUNT_MEDIA_PORT_MIN": "50100", "HAUNT_MEDIA_PORT_MAX": "50000"})
        self.assertTrue(any("maximum" in error for error in INSTALL.validate_profile("container", values)))

    def test_container_guidance_explains_frontend_and_backend_ports(self):
        self.assertIn("](docs/container.md)", (ROOT / "README.md").read_text())
        for relative in ("docs/container.md", "docs/first-run.md", "deploy/container.env.example"):
            with self.subTest(relative=relative):
                guidance = (ROOT / relative).read_text()
                self.assertIn("TCP 80/443", guidance)
                self.assertIn("8080", guidance)
        http_port = next(field for _, field in INSTALL.fields_for("container") if field.key == "HAUNT_HTTP_PORT")
        self.assertIn("container TCP 80", http_port.description)
        self.assertIn("8080 backend internal", http_port.description)


if __name__ == "__main__":
    unittest.main()
