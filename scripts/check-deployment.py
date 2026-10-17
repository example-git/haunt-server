#!/usr/bin/env python3
"""Compare public HTTP responses with the exact web assets in a release archive."""
import argparse
import concurrent.futures
import hashlib
import json
from pathlib import Path
import urllib.parse
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--origin', action='append', required=True)
    parser.add_argument('--web', type=Path, required=True, help='Extracted release web directory')
    parser.add_argument('--instance', required=True, help='Existing installation ID to preserve')
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    expected = json.loads((args.web / 'haunt-build.json').read_text())
    files = [(path.relative_to(args.web).as_posix(), path) for path in args.web.rglob('*') if path.is_file()]
    if not files or not (args.web / 'index.html').is_file():
        raise ValueError('Release web assets are missing')
    results = []
    for raw in args.origin:
        parsed = urllib.parse.urlsplit(raw)
        if parsed.scheme != 'https' or parsed.path not in ['', '/'] or parsed.query or parsed.fragment or parsed.username or parsed.password:
            raise ValueError('Provide a plain HTTPS origin')
        origin = raw.rstrip('/')
        def read(path):
            request = urllib.request.Request(origin + path, headers={'Cache-Control': 'no-cache'})
            with urllib.request.urlopen(request, timeout=30) as response:
                if response.status != 200:
                    raise ValueError(f'{origin}{path}: HTTP {response.status}')
                return response.read()
        result = {'origin': origin, 'passed': False, 'expected_build': expected, 'verified_assets': 0}
        results.append(result)
        try:
            for endpoint in ['/healthz', '/readyz']:
                if json.loads(read(endpoint)).get('status') != 'ok':
                    raise ValueError(f'{origin}{endpoint}: unavailable')
            if json.loads(read('/api/v1/instance'))['instance_id'] != args.instance:
                raise ValueError('Installation identity changed')
            if json.loads(read('/haunt-build.json')) != expected:
                raise ValueError('Public build marker differs from the release')
            def verify(entry):
                name, local = entry
                actual = read('/' if name == 'index.html' else '/' + urllib.parse.quote(name))
                if hashlib.sha256(actual).digest() != hashlib.sha256(local.read_bytes()).digest():
                    raise ValueError(f'Release asset differs: {origin}/{name}')
            with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
                for _ in pool.map(verify, files):
                    result['verified_assets'] += 1
            result['passed'] = True
        except Exception as error:
            result['error'] = str(error)
            raise
        finally:
            args.output.parent.mkdir(parents=True, exist_ok=True)
            args.output.write_text(json.dumps(results, indent=2) + '\n')
    print(json.dumps(results, indent=2))


if __name__ == '__main__':
    main()
