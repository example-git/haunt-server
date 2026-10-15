import base64
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('release_importer', Path(__file__).parents[1] / 'install-client-release.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

class ReleaseImport(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        subprocess.run(['openssl','genpkey','-algorithm','ed25519','-out',str(self.root/'key')],check=True,capture_output=True)
        subprocess.run(['openssl','pkey','-in',str(self.root/'key'),'-pubout','-out',str(self.root/'public')],check=True,capture_output=True)
        self.files = {}
        targets = {}
        for target in module.TARGETS:
            targets[target] = {}
            for kind in ('asar','installer'):
                name = target + ('.asar' if kind == 'asar' else '.exe')
                data = ('fixture-'+name).encode()
                self.files[name] = data
                targets[target][kind] = {'file':name,'size':len(data),'sha256':hashlib.sha256(data).hexdigest()}
        payload = json.dumps({'schema':1,'repository':'example-git/haunt-client','version':'0.2.0','targets':targets}).encode()
        (self.root/'payload').write_bytes(payload)
        subprocess.run(['openssl','pkeyutl','-sign','-inkey',str(self.root/'key'),'-rawin','-in',str(self.root/'payload'),'-out',str(self.root/'signature')],check=True,capture_output=True)
        self.files['haunt-updates.json'] = json.dumps({'schema':1,'payload':base64.b64encode(payload).decode(),'signature':base64.b64encode((self.root/'signature').read_bytes()).decode()}).encode()

    def archive(self, extra=None):
        output = io.BytesIO()
        with tarfile.open(fileobj=output,mode='w:gz') as tar:
            for name,data in self.files.items():
                info=tarfile.TarInfo('./'+name);info.size=len(data)
                tar.addfile(info,io.BytesIO(data))
            if extra: tar.addfile(extra)
        output.seek(0)
        return output

    def test_install_and_immutable_versions(self):
        target=self.root/'updates'
        module.install(target,self.root/'public',self.archive())
        self.assertEqual((target/'haunt-updates.json').read_bytes(),self.files['haunt-updates.json'])
        self.assertTrue((target/'client-v0.2.0/win32-x64.asar').is_file())
        with self.assertRaises(ValueError): module.install(target,self.root/'public',self.archive())

    def test_changed_file_cannot_replace_manifest(self):
        self.files['win32-x64.asar']=b'corrupt'
        target=self.root/'updates'
        with self.assertRaises(ValueError): module.install(target,self.root/'public',self.archive())
        self.assertFalse((target/'haunt-updates.json').exists())

    def test_symlinks_and_traversal_rejected(self):
        for name,kind in (('../outside',tarfile.REGTYPE),('link',tarfile.SYMTYPE)):
            entry=tarfile.TarInfo(name);entry.type=kind;entry.linkname='/etc/passwd'
            with self.assertRaises(ValueError): module.install(self.root/'updates',self.root/'public',self.archive(entry))

    def test_signature_required(self):
        envelope=json.loads(self.files['haunt-updates.json']);envelope['signature']=base64.b64encode(bytes(64)).decode()
        self.files['haunt-updates.json']=json.dumps(envelope).encode()
        with self.assertRaises(subprocess.CalledProcessError): module.install(self.root/'updates',self.root/'public',self.archive())

if __name__ == '__main__': unittest.main()
