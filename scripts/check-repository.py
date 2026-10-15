"""Check tracked files and ignore boundaries without reading local secrets.

Each repository keeps a standalone copy so a client checkout needs no server.
Secret content is checked separately by Gitleaks in repository-hygiene CI.
"""
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parent.parent


def git(*args, input=None):
    result = subprocess.run(
        ["git", *args], cwd=ROOT, input=input, capture_output=True, check=False
    )
    if result.returncode not in (0, 1):
        raise RuntimeError(result.stderr.decode("utf-8", "replace"))
    return result.stdout


def ignored(paths):
    if not paths:
        return set()
    # --no-index checks even already tracked files against the current rules.
    data = git("check-ignore", "--no-index", "-z", "--stdin",
               input=b"\0".join(path.encode() for path in paths) + b"\0")
    return {path.decode() for path in data.split(b"\0") if path}


def main():
    files = [p.decode() for p in git("ls-files", "-z").split(b"\0") if p]
    problems = [f"Tracked local/output file: {p}" for p in sorted(ignored(files))]
    private = [
        ".env", ".env.local", ".env.production", "deploy/.env",
        ".envrc", ".npmrc", ".netrc", ".ssh/id_ed25519", ".aws/credentials",
        "credentials.json", "service-account-prod.json", "keys/release.pem",
        "keys/signing.p12", "keys/apple.p8", "private/plugin/index.js",
        "var/sessions.json", "data/uploads/image.webp", "backups/client.json",
        "plugins/local/manifest.json", "models/filter.onnx", "download.ort",
        "AGENTS.md", ".codex/config.toml", "mutagen.yml", "mutagen.yml.lock",
        "state.sqlite3", "state.sqlite3-wal", "test.db-shm", "database.dump",
        "release/installer.zip", "Archive.zip", "build/output.bin",
        "dist/index.html", "out/report.txt", "tmp/trace.json",
        "test-results/trace.zip", "playwright-report/index.html",
        "coverage/lcov.info", "debug.log", "cpu.pprof", "node_modules/mod/a.js",
        "scripts/__pycache__/test.pyc", ".DS_Store", "._icon.png",
        ".idea/workspace.xml", ".vscode/settings.json", "file.swp", "file~",
    ]
    source = [
        "README.md", ".env.example", ".env.production.example", ".gitignore",
        ".github/workflows/repository-hygiene.yml", "scripts/check-repository.py",
    ]
    if (ROOT / "package.json").is_file():
        private += [
            "native/dm-crypto/target/out.wasm", "native/media-worker/media-worker",
            "electron-app/apps/desktop/main.cjs", "electron-app/resources/icon.png",
            "electron-app/update-runtime.json", "packages/core/dist/index.js",
            "tsconfig.tsbuildinfo", "Haunt.AppImage", "Haunt.app/Contents/Info.plist",
            "Haunt.dmg", "Haunt.exe", "latest.asar", "heap.heapsnapshot",
            "branding/haunt/HANDOFF.md",
        ]
        source += [
            "package-lock.json", "native/dm-crypto/Cargo.lock",
            "static/icon.icns", "static/native/mac/SwiftCaptureWorker",
            "static/native/mac/HauntMediaWorker", "static/updates.json",
            "static/dm-crypto/haunt_dm_crypto_bg.wasm", "static/api/openapi.json",
            "tests/fixtures/audio-plugin/test-audio.zip",
            "tests/fixtures/audio-plugin/default.wasm", "electron-app/main.cjs",
            "electron-app/package.json", "electron-app/entitlements.mac.plist",
            "apps/web/src/emoji-sprites/index.json", "vendor/twemoji/LICENSE",
        ]
    else:
        private += ["haunt", "haunt-server", "bin/haunt", "docs/oracle-test.md"]
        source += [
            "go.mod", "go.sum", "client", "deploy/haunt.service",
            "internal/platform/postgres/migrations/001_identity.sql",
            "docs/openapi.json", "cmd/haunt/main.go",
        ]
    problems += [f"Missing ignore rule: {p}" for p in sorted(set(private) - ignored(private))]
    problems += [f"Required source is ignored: {p}" for p in sorted(ignored(source))]
    if problems:
        print("\n".join(problems), file=sys.stderr)
        return 1
    print(f"Repository hygiene passed: {len(files)} tracked paths; "
          f"{len(private)} private/output and {len(source)} source boundaries checked.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
