#!/usr/bin/python3
"""Stage verified application releases; keep the container runtime immutable."""
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.parse
import urllib.request

RUNTIME = Path(__file__).resolve().with_name("runtime-version").read_text().strip()
APPS = Path("/data/apps")
CURRENT = APPS / "current"
MAX_ARCHIVE = 256 << 20
MAX_UNPACKED = 512 << 20
PLATFORM = "linux/" + {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine(), "unsupported")


def version(value):
    if not isinstance(value, str) or not re.fullmatch(r"\d{1,8}\.\d{1,8}\.\d{1,8}", value):
        raise ValueError("Release version must be major.minor.patch")
    return tuple(map(int, value.split(".")))


def switch(target):
    link = APPS / (".current-" + str(os.getpid()))
    link.unlink(missing_ok=True)
    link.symlink_to(target.relative_to(APPS))
    link.replace(CURRENT)


def seed():
    (APPS / "releases").mkdir(parents=True, exist_ok=True)
    os.chmod(APPS, 0o755)
    os.chmod(APPS / "releases", 0o755)
    if CURRENT.exists():
        return  # Never overwrite a downloaded release on container restart.
    source = Path("/usr/local/bin/haunt")
    digest = hashlib.sha256(source.read_bytes() + Path("/usr/share/haunt/web/haunt-build.json").read_bytes()).hexdigest()
    target = APPS / "releases" / ("bundled-" + digest[:16])
    raw = Path("/opt/haunt/bundled-version").read_text().strip().removeprefix("server-v")
    try:
        version(raw)
    except ValueError:
        raw = "0.0.0"
    with tempfile.TemporaryDirectory(prefix=".seed-", dir=APPS) as temporary:
        stage = Path(temporary) / "release"
        stage.mkdir(mode=0o755)
        os.chmod(stage, 0o755)
        shutil.copy2(source, stage / "haunt")
        shutil.copytree("/usr/share/haunt/web", stage / "web")
        (stage / "release.json").write_text(json.dumps({"schema": 1, "runtime": RUNTIME, "version": raw, "platform": PLATFORM}))
        os.chmod(stage / "release.json", 0o644)
        if not target.exists():
            stage.rename(target)
    switch(target)
    print("Seeded the bundled server and web client into /data/apps", flush=True)


def https_url(value):
    if not isinstance(value, str):
        raise ValueError("Missing release URL")
    url = urllib.parse.urlsplit(value)
    if url.scheme != "https" or not url.hostname or url.username or url.password or url.fragment:
        raise ValueError("Release downloads require HTTPS without embedded credentials")
    return url


class Redirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        destination = https_url(newurl)
        redirected = super().redirect_request(req, fp, code, msg, headers, newurl)
        if urllib.parse.urlsplit(req.full_url).netloc != destination.netloc:
            redirected.remove_header("Authorization")
        return redirected


def fetch(url, limit, output=None, octets=False):
    parsed = https_url(url)
    headers = {"User-Agent": "Haunt-container-updater/1", "Accept": "application/octet-stream" if octets else "application/json"}
    token_file = os.environ.get("HAUNT_APP_UPDATE_TOKEN_FILE")
    source = os.environ.get("HAUNT_APP_UPDATE_MANIFEST_URL", "")
    allowed_auth = {"api.github.com", urllib.parse.urlsplit(source).netloc}
    if token_file and parsed.netloc in allowed_auth:
        token = Path(token_file).read_text().strip()
        if not token or len(token) > 4096 or "\n" in token or "\r" in token:
            raise ValueError("Invalid update token file")
        headers["Authorization"] = "Bearer " + token
    opener = urllib.request.build_opener(Redirects())
    total, chunks, digest = 0, [], hashlib.sha256()
    deadline = time.monotonic() + 300
    with opener.open(urllib.request.Request(url, headers=headers), timeout=20) as response:
        while True:
            block = response.read(65536)
            if not block:
                break
            total += len(block)
            if total > limit or time.monotonic() > deadline:
                raise ValueError("Release download exceeds its size or time limit")
            digest.update(block)
            if output:
                output.write(block)
            else:
                chunks.append(block)
    return (total, digest.hexdigest()) if output else b"".join(chunks)


def manifest():
    source = os.environ.get("HAUNT_APP_UPDATE_MANIFEST_URL")
    repo = os.environ.get("HAUNT_APP_UPDATE_REPOSITORY")
    assets = {}
    if source and repo:
        raise ValueError("Choose a manifest URL or a GitHub repository, not both")
    if repo:
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
            raise ValueError("Invalid update repository")
        release = json.loads(fetch("https://api.github.com/repos/" + repo + "/releases/latest", 1 << 20))
        assets = {asset["name"]: asset["url"] for asset in release["assets"]}
        source = assets.get("haunt-app.json")
    if not source:
        raise ValueError("Set HAUNT_APP_UPDATE_MANIFEST_URL or HAUNT_APP_UPDATE_REPOSITORY")
    document = json.loads(fetch(source, 1 << 20, octets=bool(repo)))
    if document.get("schema") != 1 or document.get("runtime") != RUNTIME:
        raise ValueError("This release needs a different container runtime; update the image")
    version(document.get("version"))
    artifact = document["platforms"][PLATFORM]
    if not isinstance(artifact.get("bytes"), int) or not 1 <= artifact["bytes"] <= MAX_ARCHIVE:
        raise ValueError("Invalid release size")
    if not re.fullmatch(r"[a-f0-9]{64}", artifact.get("sha256", "")):
        raise ValueError("Invalid release checksum")
    url = assets.get(artifact.get("asset")) if repo else artifact.get("url")
    https_url(url)
    return document, artifact, url


def extract(archive, destination, expected):
    total, seen = 0, set()
    with tarfile.open(archive, "r:gz") as bundle:
        for member in bundle:
            path = PurePosixPath(member.name)
            if (path.is_absolute() or ".." in path.parts or "\\" in member.name or
                    not path.parts or path.parts[0] not in {"haunt", "web", "release.json"} or
                    path.as_posix() != member.name.rstrip("/") or member.size < 0 or
                    member.name in seen or len(seen) >= 20000 or
                    not (member.isfile() or member.isdir())):
                raise ValueError("Unsafe release archive entry")
            seen.add(member.name)
            total += member.size
            if total > MAX_UNPACKED:
                raise ValueError("Unpacked release exceeds its limit")
            target = destination.joinpath(*path.parts)
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True, mode=0o755)
            else:
                target.parent.mkdir(parents=True, exist_ok=True, mode=0o755)
                with bundle.extractfile(member) as source, target.open("xb") as output:
                    shutil.copyfileobj(source, output)
                os.chmod(target, 0o755 if member.name == "haunt" else 0o644)
    for directory in [destination, *(p for p in destination.rglob("*") if p.is_dir())]:
        os.chmod(directory, 0o755)
    metadata = json.loads((destination / "release.json").read_text())
    if metadata != {"schema": 1, "version": expected["version"], "runtime": RUNTIME, "platform": PLATFORM}:
        raise ValueError("Release contents do not match the manifest or platform")
    if not (destination / "haunt").is_file() or not (destination / "web/index.html").is_file() or not (destination / "web/haunt-build.json").is_file():
        raise ValueError("Release is missing the server, web client or build marker")


def command(*args, output=None, timeout=None):
    result = subprocess.run(args, stdout=output or subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    if result.returncode:
        raise RuntimeError("Update step failed: " + args[0])
    return result.stdout


def control(*args):
    return command("supervisorctl", "-c", "/run/haunt/supervisord.conf", *args)


def ready():
    for _ in range(90):
        result = subprocess.run(["curl", "-fsS", "--max-time", "2", "http://127.0.0.1:8081/readyz"], capture_output=True)
        if result.returncode == 0:
            return
        time.sleep(1)
    raise RuntimeError("The updated application did not become ready")


def apply_release(document, artifact, url):
    if not Path("/run/haunt/local-database").exists():
        raise ValueError("Application updates currently require the bundled database for backup/recovery")
    old = CURRENT.resolve()
    target = APPS / "releases" / (document["version"] + "-" + artifact["sha256"][:16])
    with tempfile.TemporaryDirectory(prefix=".update-", dir=APPS) as temporary:
        temporary = Path(temporary)
        archive = temporary / "release.tar.gz"
        if shutil.disk_usage(APPS).free < MAX_UNPACKED + artifact["bytes"]:
            raise ValueError("Insufficient free space to stage the update")
        with archive.open("wb") as output:
            size, digest = fetch(url, artifact["bytes"], output, octets=True)
        if size != artifact["bytes"] or digest != artifact["sha256"]:
            raise ValueError("Release size/checksum verification failed; running installation unchanged")
        stage = temporary / "release"
        stage.mkdir(mode=0o755)
        extract(archive, stage, document)
        # Never run downloaded code until the entire package was verified.
        command(str(stage / "haunt"), "-version", timeout=10)
        data = Path("/data/haunt")
        data_bytes = sum(p.stat().st_size for p in data.rglob("*") if p.is_file())
        database_bytes = int(command("su-exec", "postgres", "psql", "-XAt", "-h", "/run/postgresql", "-d", "haunt", "-c", "SELECT pg_database_size(current_database())"))
        if shutil.disk_usage(APPS).free < data_bytes + 2 * database_bytes + MAX_UNPACKED:
            raise ValueError("Insufficient free space for the update recovery backup")
        backup = Path("/data/update-backups") / ("before-" + document["version"] + "-" + str(time.time_ns()))
        backup.mkdir(parents=True, mode=0o700)
        # SIGTERM must not interrupt a schema update halfway through recovery.
        termination_requested = []
        previous_signal = signal.signal(signal.SIGTERM, lambda *_: termination_requested.append(True))
        stopped, backed_up, switched = False, False, False
        Path("/run/haunt/updating").touch()
        try:
            stopped = True
            control("stop", "nginx", "haunt")
            with (backup / "database.dump").open("wb") as dump:
                command("su-exec", "postgres", "pg_dump", "-h", "/run/postgresql", "-Fc", "-d", "haunt", output=dump)
            command("tar", "-C", str(data), "-czf", str(backup / "files.tar.gz"), ".")
            (backup / "release.json").write_text((old / "release.json").read_text())
            backed_up = True
            if target.exists():
                shutil.rmtree(target)
            stage.rename(target)
            switch(target)
            switched = True
            control("start", "haunt", "nginx")
            ready()
        except BaseException:
            if switched and backed_up:
                # Restore the database as well as files: swapping binaries alone
                # is unsafe if the failed release applied a schema migration.
                subprocess.run(["supervisorctl", "-c", "/run/haunt/supervisord.conf", "stop", "nginx", "haunt"], capture_output=True)
                command("su-exec", "postgres", "dropdb", "-h", "/run/postgresql", "--force", "haunt")
                command("su-exec", "postgres", "createdb", "-h", "/run/postgresql", "-O", "haunt", "haunt")
                with (backup / "database.dump").open("rb") as dump:
                    result = subprocess.run(["su-exec", "postgres", "pg_restore", "-h", "/run/postgresql", "--exit-on-error", "-d", "haunt"], stdin=dump, capture_output=True)
                    if result.returncode:
                        raise RuntimeError("Recovery needs operator attention; backup retained at " + str(backup))
                shutil.rmtree(data)
                data.mkdir(mode=0o700)
                os.chown(data, 10001, 10001)
                command("tar", "-C", str(data), "-xzf", str(backup / "files.tar.gz"))
                switch(old)
            if stopped:
                control("start", "haunt", "nginx")
                ready()
            raise
        finally:
            Path("/run/haunt/updating").unlink(missing_ok=True)
            signal.signal(signal.SIGTERM, previous_signal)
            if termination_requested:
                raise SystemExit(0)
        # Retain the previous release and latest recovery backup; never accumulate
        # an unbounded history on the installation volume.
        for release in (APPS / "releases").iterdir():
            if release not in {old, target} and release.is_dir():
                shutil.rmtree(release)
        for saved in backup.parent.glob("before-*"):
            if saved != backup and saved.is_dir():
                shutil.rmtree(saved)
        print("Installed application release " + document["version"] + "; recovery backup: " + str(backup), flush=True)


def update(apply):
    with (APPS / "update.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        document, artifact, url = manifest()
        current = json.loads((CURRENT / "release.json").read_text())
        if version(document["version"]) <= version(current["version"]):
            print("Application release " + current["version"] + " is current", flush=True)
            return
        print("Application update available: " + current["version"] + " -> " + document["version"], flush=True)
        if apply:
            apply_release(document, artifact, url)


def main():
    action = sys.argv[1] if len(sys.argv) == 2 else "help"
    if action == "seed":
        seed()
    elif action in {"check", "apply"}:
        update(action == "apply")
    elif action == "watch":
        interval = int(os.environ.get("HAUNT_APP_UPDATE_INTERVAL", "21600"))
        if interval < 300:
            raise ValueError("Update interval must be at least 300 seconds")
        while not Path("/run/haunt/started").exists():
            time.sleep(1)
        while True:
            try:
                update(os.environ.get("HAUNT_APP_UPDATE_AUTO", "false") == "true")
            except Exception as error:
                print("Application update deferred: " + type(error).__name__ + ": " + str(error), file=sys.stderr, flush=True)
            time.sleep(interval)
    else:
        raise ValueError("Usage: haunt-update check|apply|watch|seed")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print("Application updater: " + str(error), file=sys.stderr)
        sys.exit(1)
