#!/usr/bin/env python3
"""Bundle the pinned SearXNG distribution for a Haunt release executable."""

import argparse
from contextlib import contextmanager
import gzip
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
BUNDLES = ROOT / "internal/ai/searchruntime/bundle"
LOADER = {"amd64": "ld-linux-x86-64.so.2", "arm64": "ld-linux-aarch64.so.1"}


@contextmanager
def files_from_image(image, platform):
    subprocess.run(["docker", "pull", "--platform", platform, image], check=True)
    container = subprocess.check_output(["docker", "create", "--platform", platform, image], text=True).strip()
    try:
        with tempfile.TemporaryDirectory(prefix="haunt-search-") as temporary:
            root = Path(temporary)
            export = root / "image.tar"
            subprocess.run(["docker", "export", "-o", export, container], check=True)
            with tarfile.open(export) as layers:
                for entry in layers:
                    name = entry.name.removeprefix("./")
                    if not (name.startswith(("usr/lib/", "usr/local/searxng/")) or name in ("usr/bin/python3.14", "etc/ssl/certs/ca-certificates.crt")):
                        continue
                    if entry.isdir() or Path(name).is_absolute() or ".." in Path(name).parts:
                        continue
                    if entry.issym() and entry.linkname.startswith("/"):
                        continue
                    try:
                        src = layers.extractfile(entry)
                    except (KeyError, OSError):
                        if entry.issym():
                            continue
                        raise
                    if src:
                        path = root / name
                        path.parent.mkdir(parents=True, exist_ok=True)
                        with src, path.open("wb") as out:
                            shutil.copyfileobj(src, out)
            yield root
    finally:
        subprocess.run(["docker", "rm", "-v", container], check=True, stdout=subprocess.DEVNULL)


def package(root, target, arch):
    version = "python3.14"
    lib = root / "usr/lib"
    searx = root / "usr/local/searxng"
    executable = root / "usr/bin" / version
    with executable.open("rb") as data:
        header = data.read(20)
    if header[:6] != b"\x7fELF\x02\x01" or int.from_bytes(header[18:20], "little") != {"amd64": 62, "arm64": 183}[arch]:
        raise ValueError(f"Unexpected {arch} search runtime architecture")
    entries = [(executable, "bin/python"), (lib / LOADER[arch], "bin/loader"),
               (root / "etc/ssl/certs/ca-certificates.crt", "certs.pem")]
    for source in sorted(lib.glob("*.so*")):
        if source.is_file():
            entries.append((source, "lib/" + source.name))
    for base, prefix in ((lib / version, "lib/" + version), (searx / ".venv/lib" / version / "site-packages", "packages"),
                         (searx / "searx", "app/searx")):
        if not base.is_dir():
            raise ValueError(f"Required runtime directory is missing: {base}")
        for path in sorted(base.rglob("*")):
            if path.is_file() and "__pycache__" not in path.parts and path.suffix not in (".pyc", ".pyo"):
                entries.append((path, prefix + "/" + path.relative_to(base).as_posix()))
    with target.open("wb") as raw, gzip.GzipFile(filename="", fileobj=raw, mode="wb", mtime=0, compresslevel=5) as compressed, tarfile.open(mode="w|", fileobj=compressed) as out:
        for source, name in entries:
            entry = tarfile.TarInfo(name)
            entry.size = source.stat().st_size
            entry.mode = 0o755 if name.startswith("bin/") else 0o644
            with source.open("rb") as data:
                out.addfile(entry, data)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("arch", choices=LOADER)
    parser.add_argument("--root", type=Path)
    parser.add_argument("--output", type=Path, default=BUNDLES)
    args = parser.parse_args()
    pins = json.loads((BUNDLES / "source.json").read_text())
    platform = "linux/" + args.arch
    dest = args.output / (args.arch + ".tar.gz")
    dest.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=dest.parent) as temporary:
        archive = Path(temporary) / dest.name
        if args.root:
            package(args.root, archive, args.arch)
        else:
            image = pins["image"].split("@")[0] + "@" + pins[platform]
            with files_from_image(image, platform) as root:
                package(root, archive, args.arch)
        with archive.open("rb") as data:
            digest = hashlib.sha256()
            for block in iter(lambda: data.read(1024 * 1024), b""):
                digest.update(block)
            digest = digest.hexdigest()
        os.replace(archive, dest)
        print(f"{dest.name} {dest.stat().st_size} bytes sha256:{digest}")


if __name__ == "__main__":
    main()
