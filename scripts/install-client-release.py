#!/usr/bin/env python3
"""Import a signed desktop release tar.gz from stdin into a server's mirror.

The public key is installed by the host administrator, never read from the upload.
Suitable for an SSH authorized_keys forced command; accepts no paths from SSH.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

TARGETS = ('win32-x64', 'linux-x64', 'linux-arm64', 'darwin-arm64')
NAME = re.compile(r'[A-Za-z0-9][A-Za-z0-9._-]{0,160}')
VERSION = re.compile(r'(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})')


def install(directory, public_key, stream):
    directory.mkdir(mode=0o750, parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='.incoming-', dir=directory) as temporary:
        stage = Path(temporary)
        total, count = 0, 0
        with tarfile.open(fileobj=stream, mode='r|gz') as archive:
            for member in archive:
                if member.isdir() and member.name in ('.', './'):
                    continue
                name = member.name.removeprefix('./')
                count += 1
                total += member.size
                if not member.isfile() or not NAME.fullmatch(name) or count > 20 or member.size > 1024**3 or total > 5 * 1024**3:
                    raise ValueError('Invalid release archive entry or size')
                with (stage / name).open('xb') as output, archive.extractfile(member) as source:
                    shutil.copyfileobj(source, output, 1024 * 1024)
                    output.flush()
                    os.fsync(output.fileno())
                (stage / name).chmod(0o640)
        manifest_file = stage / 'haunt-updates.json'
        if manifest_file.stat().st_size > 128 * 1024:
            raise ValueError('Manifest too large')
        envelope = json.loads(manifest_file.read_text())
        payload = base64.b64decode(envelope['payload'], validate=True)
        signature = base64.b64decode(envelope['signature'], validate=True)
        if envelope['schema'] != 1 or len(signature) != 64:
            raise ValueError('Invalid signature envelope')
        (stage / '.payload').write_bytes(payload)
        (stage / '.signature').write_bytes(signature)
        subprocess.run(['openssl', 'pkeyutl', '-verify', '-pubin', '-inkey', str(public_key), '-rawin', '-in', str(stage / '.payload'), '-sigfile', str(stage / '.signature')], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        manifest = json.loads(payload)
        version = manifest['version']
        if manifest['schema'] != 1 or manifest['repository'] != 'example-git/haunt-client' or not VERSION.fullmatch(version) or set(manifest['targets']) != set(TARGETS):
            raise ValueError('Unexpected release identity')
        keep = {'haunt-updates.json'}
        for target in TARGETS:
            for kind in ('asar', 'installer'):
                entry = manifest['targets'][target][kind]
                name = entry['file']
                if not NAME.fullmatch(name) or not re.fullmatch('[a-f0-9]{64}', entry['sha256']) or not isinstance(entry['size'], int) or not 0 < entry['size'] <= 1024**3:
                    raise ValueError('Invalid signed artifact')
                filename = stage / name
                if filename.stat().st_size != entry['size']:
                    raise ValueError('Artifact size mismatch')
                with filename.open('rb') as source:
                    digest = hashlib.file_digest(source, 'sha256').hexdigest()
                if digest != entry['sha256']:
                    raise ValueError('Artifact checksum mismatch')
                keep.add(name)
        for filename in stage.iterdir():
            if filename.name not in keep:
                filename.unlink()
        destination = directory / ('client-v' + version)
        if destination.exists():
            raise ValueError('Release version already installed; publish a new version')
        # Rename the complete version first, then publish the manifest last.
        stage.chmod(0o750)
        os.rename(stage, destination)
        latest = directory / '.haunt-updates.next'
        shutil.copyfile(destination / 'haunt-updates.json', latest)
        latest.chmod(0o640)
        with latest.open('rb') as handle:
            os.fsync(handle.fileno())
        os.replace(latest, directory / 'haunt-updates.json')
        descriptor = os.open(directory, os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
        print('Installed desktop release ' + version)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--directory', type=Path, required=True)
    parser.add_argument('--public-key', type=Path, required=True)
    args = parser.parse_args()
    try:
        install(args.directory, args.public_key, sys.stdin.buffer)
    except Exception as error:
        print('Release import rejected: ' + str(error), file=sys.stderr)
        sys.exit(1)
