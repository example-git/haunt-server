import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('container_update', Path(__file__).resolve().parents[2] / 'deploy/container/update.py')
updater = importlib.util.module_from_spec(spec)
spec.loader.exec_module(updater)


class ReleaseValidation(unittest.TestCase):
    def unpack(self, extra=(), metadata=None):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = root / 'release.tar.gz'
            document = {'schema': 1, 'version': '1.2.3', 'runtime': updater.RUNTIME, 'platform': updater.PLATFORM}
            entries = [('haunt', b'binary'), ('web/index.html', b'page'), ('web/haunt-build.json', b'{}'), ('release.json', json.dumps(metadata or document).encode()), *extra]
            with tarfile.open(archive, 'w:gz') as bundle:
                for name, data in entries:
                    member = tarfile.TarInfo(name)
                    if data is None:
                        member.type = tarfile.SYMTYPE
                        member.linkname = '/etc/passwd'
                        bundle.addfile(member)
                    else:
                        member.size = len(data)
                        bundle.addfile(member, io.BytesIO(data))
            destination = root / 'stage'
            destination.mkdir()
            updater.extract(archive, destination, document)
            self.assertEqual((destination / 'haunt').stat().st_mode & 0o777, 0o755)

    def test_valid(self):
        self.unpack()

    def test_unsafe_paths_links_and_duplicates(self):
        for name, data in [('../outside', b''), ('/web/a', b''), ('web/../a', b''), ('web//a', b''), ('web/a', None), ('haunt', b'replaced'), ('install.sh', b'')]:
            with self.subTest(name=name), self.assertRaises(ValueError):
                self.unpack([(name, data)])

    def test_incompatible_metadata(self):
        for key, value in [('runtime', 'unknown'), ('platform', 'other'), ('version', '1.2.4')]:
            metadata = {'schema': 1, 'version': '1.2.3', 'runtime': updater.RUNTIME, 'platform': updater.PLATFORM, key: value}
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.unpack(metadata=metadata)

    def test_transport_and_versions(self):
        for url in ['http://example.com/a', 'https://user:pass@example.com/a', 'file:///etc/passwd', 'https://example.com/a#fragment']:
            with self.assertRaises(ValueError):
                updater.https_url(url)
        self.assertEqual(updater.version('1.2.3'), (1, 2, 3))
        for value in ['latest', '1.0', '-1.2.3', '1.2.3-rc', None]:
            with self.assertRaises(ValueError):
                updater.version(value)


if __name__ == '__main__':
    unittest.main()
