"""Exercise install/update/rollback/uninstall in isolated directories with a simulated systemd host."""
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

DEPLOY = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('detector_update_tested', DEPLOY / 'update.py')
u = importlib.util.module_from_spec(spec)
spec.loader.exec_module(u)
d = u.d


def bundle(folder, ver, extra_license=False):
    folder.mkdir()
    files = {name: ('fixture for ' + name + ' ' + ver).encode() for name in d.ENTRY}
    files['VERSION'] = (ver + '\n').encode()
    files['install.sh'] = b'placeholder'
    files['LICENSES/base.txt'] = b'fixture'
    if extra_license:
        files['LICENSES/extra.txt'] = b'more licenses'
    sums = []
    for name, content in files.items():
        dest = folder / name
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_bytes(content)
        sums.append(hashlib.sha256(content).hexdigest() + '  ' + name)
    (folder / 'SHA256SUMS').write_text('\n'.join(sums) + '\n')
    return folder


class Lifecycle(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.tmp = Path(self.stack.enter_context(tempfile.TemporaryDirectory())).resolve()
        root = self.tmp / 'opt' / d.NAME
        root.parent.mkdir()
        unit = self.tmp / 'system' / (d.NAME + '.service')
        unit.parent.mkdir()
        self.vendor = bundle(self.tmp / 'v1', 'v0.1.0')
        self.successor = bundle(self.tmp / 'v2', 'v0.1.1', extra_license=True)
        self.stack.enter_context(patch.multiple(d, ROOT=root, UNIT=unit, RUNS=root/'data/runs',
                                                MARKER=root/'install-manifest.json', BUNDLE=self.vendor))
        self.user = None
        self.group = None
        self.commands = []
        self.properties = {'ActiveState': 'inactive', 'FragmentPath': str(unit), 'DropInPaths': ''}
        self.stack.enter_context(patch.object(d, 'run', side_effect=self.host))
        self.stack.enter_context(patch.object(d, 'identity', side_effect=lambda: (self.user, self.group)))
        self.stack.enter_context(patch.object(d, 'system_property', side_effect=lambda x: self.properties[x]))
        self.stack.enter_context(patch.object(d, 'mounts_inside', return_value=False))
        self.stack.enter_context(patch.object(d, 'root_owned', return_value=True))
        self.stack.enter_context(patch.object(d.os, 'chown', create=True))
        self.stack.enter_context(patch.object(d.shutil, 'which', return_value='/usr/sbin/nologin'))
        fake_pwd = SimpleNamespace(getpwall=lambda: [self.user] if self.user else [])
        self.stack.enter_context(patch.dict('sys.modules', {'pwd': fake_pwd}))
        self.reject_verify = False

    def host(self, *args, **kwargs):
        self.commands.append(args)
        if args[0] == 'groupadd':
            self.group = SimpleNamespace(gr_gid=4101, gr_mem=[])
        if args[0] == 'useradd':
            self.user = SimpleNamespace(pw_uid=4102, pw_gid=4101, pw_name=d.NAME, pw_dir='/nonexistent', pw_shell='/usr/sbin/nologin')
        if args[0] == 'userdel':
            self.user = None
        if args[0] == 'groupdel':
            self.group = None
        if args[0] == 'systemd-analyze' and self.reject_verify:
            raise ValueError('simulated unit verification failure')
        if args[:2] == ('systemctl', 'start'):
            self.properties['ActiveState'] = 'active'
        if args[:2] in (('systemctl', 'disable'), ('systemctl', 'stop')):
            self.properties['ActiveState'] = 'inactive'
        return SimpleNamespace(returncode=0, stdout='')

    def install(self):
        d.perform_install('http://127.0.0.1:8080', d.bundle_files(self.vendor))
        self.assertEqual(self.properties['ActiveState'], 'inactive')
        self.assertEqual(json.loads((d.ROOT/'config/config.json').read_text())['parallel'], 1)
        result = d.RUNS / 'b80311ce-0331-4fa2-9d82-9a94eb72e526.json'
        result.write_bytes(b'old answers')
        return result

    def apply(self):
        u.apply(self.successor, d.bundle_files(self.successor), d.verify_manifest(), d.ROOT/'.rollback')

    def test_failed_install_undoes_only_new_resources(self):
        neighbor = d.ROOT.parent / 'sub2api'
        neighbor.write_text('live site')
        self.reject_verify = True
        with self.assertRaisesRegex(ValueError, 'simulated'):
            d.perform_install('http://127.0.0.1:8080', d.bundle_files(self.vendor))
        self.assertEqual((self.user, self.group), (None, None))
        self.assertFalse(d.ROOT.exists())
        self.assertFalse(d.UNIT.exists())
        self.assertEqual(neighbor.read_text(), 'live site')

    def test_backup_before_uninstall_and_preserve_unrecognized_files(self):
        result = self.install()
        opts = SimpleNamespace(check=False, purge_results=False, backup_to=None)
        with self.assertRaisesRegex(ValueError, '--backup-to'):
            d.uninstall(opts)
        other = d.RUNS / 'operator-note.txt'
        other.write_text('retain')
        with self.assertRaises(ValueError):
            d.uninstall(SimpleNamespace(check=False, purge_results=True, backup_to=None))
        self.assertTrue(other.exists())
        other.unlink()
        backup = self.tmp / 'answers.tar.gz'
        opts.backup_to = str(backup)
        d.uninstall(opts)
        with tarfile.open(backup) as archive:
            self.assertEqual(archive.extractfile('runs/' + result.name).read(), b'old answers')
        self.assertFalse(d.ROOT.exists())
        self.assertFalse(d.UNIT.exists())
        self.assertEqual((self.user, self.group), (None,None))

    def test_inactive_update_keeps_config_state_and_results(self):
        result = self.install()
        config = d.ROOT/'config/config.json'
        config.write_text('{"parallel": 2, "local_override": true}')
        with patch.object(u,'health',side_effect=AssertionError('started inactive service')):
            self.apply()
        self.assertEqual(u.file_version(d.ROOT/'VERSION'), (0,1,1))
        self.assertTrue(json.loads(config.read_text())['local_override'])
        self.assertEqual(result.read_bytes(), b'old answers')
        self.assertEqual(self.properties['ActiveState'], 'inactive')
        self.assertFalse((d.ROOT/'.rollback').exists())
        d.verify_manifest()

    def test_failed_health_restores_previous_active_release(self):
        result = self.install()
        config = (d.ROOT/'config/config.json').read_bytes()
        manifest = d.MARKER.read_bytes()
        unit = d.UNIT.read_bytes()
        binary = (d.ROOT/'bin'/d.NAME).read_bytes()
        self.properties['ActiveState']='active'
        with patch.object(u,'health',side_effect=[ValueError('simulated wrong process'), None]) as check:
            with self.assertRaisesRegex(ValueError, 'simulated wrong'):
                self.apply()
        self.assertEqual(check.call_count, 2)
        self.assertEqual(self.properties['ActiveState'], 'active')
        self.assertEqual((d.ROOT/'bin'/d.NAME).read_bytes(), binary)
        self.assertEqual(d.UNIT.read_bytes(), unit)
        self.assertEqual(d.MARKER.read_bytes(),manifest)
        self.assertEqual((d.ROOT/'config/config.json').read_bytes(), config)
        self.assertEqual(result.read_bytes(), b'old answers')
        self.assertFalse((d.ROOT/'LICENSES/extra.txt').exists())
        self.assertFalse((d.ROOT/'.rollback').exists())
        d.verify_manifest()

    def test_recover_interrupted_update(self):
        result = self.install()
        folder = d.ROOT/'.rollback'
        before = d.MARKER.read_bytes()
        u.snapshot(folder, d.verify_manifest(), d.bundle_files(self.successor), False)
        (d.ROOT/'VERSION').write_text('v0.1.1')
        (d.ROOT/'LICENSES'/'extra.txt').write_text('interrupted')
        u.recover(folder)
        self.assertEqual(u.file_version(d.ROOT/'VERSION'), (0,1,0))
        self.assertFalse((d.ROOT/'LICENSES/extra.txt').exists())
        self.assertEqual(d.MARKER.read_bytes(), before)
        self.assertEqual(result.read_bytes(), b'old answers')
        self.assertFalse(folder.exists())

    def test_health_requires_expected_version_and_process(self):
        self.install()
        self.properties.update(ActiveState='active', MainPID='10')
        class Response(io.BytesIO):
            status = 200
        opener = SimpleNamespace(open=lambda *a, **kw: Response(json.dumps(self.served).encode()))
        with patch.object(u.request, 'build_opener', return_value=opener), patch.object(u.time,'sleep'), patch.object(u.os.path,'samefile', return_value=True) as same:
            self.served = {'status': 'ok', 'version':'v9.9.9'}
            with self.assertRaises(ValueError):
                u.health()
            self.served = {'status':'ok','version':'v0.1.0','additional':True}
            same.return_value = False
            with self.assertRaises(ValueError):
                u.health()
            same.return_value = True
            u.health()


class Releases(unittest.TestCase):
    def test_release_asset_source_and_checksum(self):
        name='sub2-fingerprint-linux-amd64.tar.gz'
        base='https://github.com/AuroraWhisperer/sub2-fingerprint/releases/download/v0.1.1/'
        digest = 'f'*64
        asset={'name':name,'state':'uploaded','size':321,'browser_download_url':base+name,'digest':'sha256:'+digest}
        sums=dict(name='SHA256SUMS.txt',state='uploaded',browser_download_url=base+'SHA256SUMS.txt')
        meta=dict(tag_name='v0.1.1',draft=False,prerelease=False,assets=[asset,sums])
        def fetch(url, limit):
            return json.dumps(meta).encode() if url==u.API else (digest+'  '+name+'\n').encode()
        with patch.object(u,'get_small',side_effect=fetch), patch.object(u.platform,'machine',return_value='x86_64'):
            self.assertEqual(u.release_info(),((0,1,1),base+name,digest,321))
            asset['digest']=''
            self.assertEqual(u.release_info()[2],digest)
            asset['browser_download_url']='https://example.invalid/'+name
            with self.assertRaises(ValueError):
                u.release_info()

    def test_unpack_and_verify_actual_contents(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp).resolve()
            b=bundle(root/'one','v0.1.0')
            archive=root/'release.tar.gz'
            with tarfile.open(archive,'w:gz') as result:
                result.add(b,arcname='release')
            plain=u.unpack(archive,root/'extracted')
            self.assertIn('VERSION', d.bundle_files(plain))
            (plain/'VERSION').write_text('v2.0.0')
            with self.assertRaises(ValueError):
                d.bundle_files(plain)
            with archive.open('rb') as source, self.assertRaises(ValueError):
                u.acquire(source,root/'copy','0'*64)

if __name__=='__main__':
    unittest.main()
