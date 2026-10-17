#!/usr/bin/env python3
"""Archive tracked server/client source and revision hashes for an external review."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('output', type=Path)
    args = parser.parse_args()
    output = args.output.resolve()
    manifest = {'review_status': 'not independently reviewed', 'components': {}}
    for name, source in [('server', ROOT), ('client', ROOT / 'client')]:
        revision = subprocess.check_output(['git', '-C', str(source), 'rev-parse', 'HEAD'], text=True).strip()
        if subprocess.check_output(['git', '-C', str(source), 'status', '--porcelain', '--untracked-files=no'], text=True).strip():
            raise ValueError(f'Commit tracked {name} changes before preparing the review snapshot')
        manifest['components'][name] = {'revision': revision}
    embedded = subprocess.check_output(['git', '-C', str(ROOT), 'ls-tree', 'HEAD', 'client'], text=True).split()[2]
    if embedded != manifest['components']['client']['revision']:
        raise ValueError('Client checkout differs from server pin')
    output.mkdir(parents=True, exist_ok=False)
    for name, source in [('server', ROOT), ('client', ROOT / 'client')]:
        archive = output / f'{name}-source.tar.gz'
        subprocess.run(['git', '-C', str(source), 'archive', '--format=tar.gz', '--output', str(archive), manifest['components'][name]['revision']], check=True)
        manifest['components'][name].update({'archive': archive.name, 'sha256': hashlib.sha256(archive.read_bytes()).hexdigest()})
    (output / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    (output / 'review-scope.md').write_bytes((ROOT / 'docs/security-review.md').read_bytes())
    print(f'Prepared exact source snapshots in {output}; independent review remains pending.')


if __name__ == '__main__':
    main()
