import importlib.util
import json
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("package_container_app", Path(__file__).resolve().parents[1] / "package-container-app.py")
package = importlib.util.module_from_spec(spec)
spec.loader.exec_module(package)


class PackageTargetTests(unittest.TestCase):
    def test_selected_architecture_and_default_complete_release(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "deploy/container").mkdir(parents=True)
            (root / "deploy/container/runtime-version").write_text("runtime")
            (root / "client/dist").mkdir(parents=True)
            (root / "client/dist/haunt-build.json").write_text('{"version":1,"id":"fixture"}')
            (root / "client/dist/index.html").write_text("web")
            def build(command, **kwargs):
                Path(command[command.index("-o") + 1]).write_bytes(b"binary")
            for selected, expected in [(["--arch", "arm64"], {"linux/arm64"}), ([], {"linux/amd64", "linux/arm64"})]:
                output = root / ("selected" if selected else "complete")
                with patch.object(package, "ROOT", root), patch.object(sys, "argv", ["package-container-app.py", "0.17.9", str(output), *selected]), patch.object(package.subprocess, "run", side_effect=build):
                    package.main()
                manifest = json.loads((output / "haunt-app.json").read_text())
                self.assertEqual(set(manifest["platforms"]), expected)
                for platform in expected:
                    asset = manifest["platforms"][platform]
                    with tarfile.open(output / asset["asset"]) as archive:
                        self.assertEqual(json.load(archive.extractfile("release.json"))["platform"], platform)
                        self.assertEqual(archive.extractfile("web/index.html").read(), b"web")


if __name__ == "__main__":
    unittest.main()
