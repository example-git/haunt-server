#!/usr/bin/env python3
"""Check that local links in repository Markdown resolve."""

from __future__ import annotations

from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import unquote, urlsplit


ROOT = Path(__file__).resolve().parents[1]
INLINE_LINK = re.compile(
    r"""!?\[[^\]]*\]\(\s*(?:<([^>]+)>|([^\s)]+))(?:\s+["'][^)]*["'])?\s*\)"""
)
REFERENCE_LINK = re.compile(
    r"""^\s{0,3}\[[^\]]+\]:\s*(?:<([^>]+)>|([^\s]+))""", re.MULTILINE
)
HTML_LINK = re.compile(r"""(?:href|src)=["']([^"']+)["']""", re.IGNORECASE)


def output(*args: str) -> str:
    return subprocess.check_output(args, cwd=ROOT, text=True)


def markdown_files() -> list[Path]:
    names = subprocess.check_output(
        [
            "git",
            "ls-files",
            "-z",
            "--cached",
            "--others",
            "--exclude-standard",
            "--",
            "*.md",
        ],
        cwd=ROOT,
    )
    return sorted(ROOT / name.decode() for name in names.split(b"\0") if name)


def submodule_roots() -> tuple[Path, ...]:
    roots = []
    for line in output("git", "ls-files", "--stage").splitlines():
        if not line.startswith("160000 "):
            continue
        _, name = line.split("\t", 1)
        roots.append((ROOT / name).resolve())
    return tuple(roots)


def targets(text: str):
    for pattern in (INLINE_LINK, REFERENCE_LINK):
        for match in pattern.finditer(text):
            yield match.group(1) or match.group(2), match.start()
    for match in HTML_LINK.finditer(text):
        yield match.group(1), match.start()


def local_path(source: Path, target: str) -> Path | None:
    target = target.strip()
    parsed = urlsplit(target)
    if (
        not target
        or target.startswith(("#", "/"))
        or parsed.scheme
        or parsed.netloc
    ):
        return None
    path = unquote(parsed.path)
    if not path:
        return None
    return (source.parent / path).resolve()


def main() -> int:
    submodules = submodule_roots()
    failures = []
    checked = 0
    for source in markdown_files():
        text = source.read_text(encoding="utf-8")
        for target, offset in targets(text):
            destination = local_path(source, target)
            if destination is None:
                continue
            checked += 1
            line = text.count("\n", 0, offset) + 1
            try:
                destination.relative_to(ROOT)
            except ValueError:
                failures.append((source, line, target, "leaves the repository"))
                continue
            if destination.exists() or any(
                destination == root or destination.is_relative_to(root)
                for root in submodules
            ):
                continue
            failures.append((source, line, target, "does not exist"))

    if failures:
        for source, line, target, reason in failures:
            print(f"{source.relative_to(ROOT)}:{line}: {target}: {reason}", file=sys.stderr)
        return 1
    print(f"Documentation links passed: {len(markdown_files())} Markdown files; {checked} local targets checked.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

