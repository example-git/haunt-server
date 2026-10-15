"""Run only inside the disposable integration-test container."""
import hashlib
import io
import json
from pathlib import Path
import sys
import tarfile

current = Path('/data/apps/current')
metadata = json.loads((current / 'release.json').read_text())
parts = list(map(int, metadata['version'].split('.')))
parts[2] += 1
metadata['version'] = '.'.join(map(str, parts))
destination = current / 'web/test-updates'
destination.mkdir(exist_ok=True)
archive = destination / 'release.tar.gz'
with tarfile.open(archive, 'w:gz') as output:
    raw = json.dumps(metadata).encode()
    member = tarfile.TarInfo('release.json')
    member.size = len(raw)
    output.addfile(member, io.BytesIO(raw))
    if sys.argv[1] == 'broken':
        raw = b'#!/bin/sh\n[ "$1" = "-version" ] && exit 0\nexit 1\n'
        member = tarfile.TarInfo('haunt')
        member.size = len(raw)
        member.mode = 0o755
        output.addfile(member, io.BytesIO(raw))
    else:
        output.add(current / 'haunt', arcname='haunt')
    output.add(current / 'web', arcname='web', filter=lambda entry: None if entry.name.startswith('web/test-updates') else entry)
manifest = {key: metadata[key] for key in ('schema', 'runtime', 'version')}
manifest['platforms'] = {metadata['platform']: {
    'bytes': archive.stat().st_size,
    'sha256': hashlib.sha256(archive.read_bytes()).hexdigest(),
    'url': 'https://localhost/test-updates/release.tar.gz',
}}
(destination / 'haunt-app.json').write_text(json.dumps(manifest))
