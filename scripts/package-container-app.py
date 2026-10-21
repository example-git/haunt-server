#!/usr/bin/env python3
"""Build small server + static web update archives for the fixed runtime image."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def main():
    version = sys.argv[1].removeprefix("server-v")
    if not re.fullmatch(r"\d+\.\d+\.\d+", version):
        raise ValueError("Usage: package-container-app.py MAJOR.MINOR.PATCH [release-directory]")
    output = Path(sys.argv[2] if len(sys.argv) > 2 else ROOT / "release").resolve()
    output.mkdir(parents=True, exist_ok=True)
    runtime = (ROOT / "deploy/container/runtime-version").read_text().strip()
    web = ROOT / "client/dist"
    if not (web / "haunt-build.json").is_file():
        raise ValueError("Build the web client first")
    manifest = {"schema": 1, "version": version, "runtime": runtime, "platforms": {}}
    for arch in ["amd64", "arm64"]:
        with tempfile.TemporaryDirectory(prefix="haunt-app-") as temporary:
            stage = Path(temporary)
            subprocess.run(["go", "build", "-p", "2", "-trimpath", "-ldflags=-s -w -X main.version=" + version + " -X github.com/example-git/haunt-server/internal/config.HostedClientOrigin=" + os.environ.get("HAUNT_WEB_CLIENT_ORIGIN", ""),
                            "-o", str(stage / "haunt"), "./cmd/haunt"], cwd=ROOT,
                           env={**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch, "GOMAXPROCS": "2"}, check=True)
            metadata = {"schema": 1, "version": version, "runtime": runtime, "platform": "linux/" + arch}
            (stage / "release.json").write_text(json.dumps(metadata))
            name = "haunt-app-linux-" + arch + ".tar.gz"
            archive = output / name
            with tarfile.open(archive, "w:gz") as bundle:
                def clean(info):
                    if not (info.isfile() or info.isdir()):
                        raise ValueError("Release inputs must contain only normal files/directories")
                    info.uid = info.gid = 0
                    info.uname = info.gname = "root"
                    info.mtime = 0
                    info.mode = 0o755 if info.isdir() or info.name == "haunt" else 0o644
                    return info
                bundle.add(stage / "haunt", arcname="haunt", filter=clean)
                bundle.add(stage / "release.json", arcname="release.json", filter=clean)
                bundle.add(web, arcname="web", filter=clean)
            manifest["platforms"]["linux/" + arch] = {
                "asset": name, "bytes": archive.stat().st_size,
                "sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
                "url": "https://github.com/example-git/haunt-server/releases/download/server-v" + version + "/" + name,
            }
    (output / "haunt-app.json").write_text(json.dumps(manifest, indent=2) + "\n")


if __name__ == "__main__":
    main()
