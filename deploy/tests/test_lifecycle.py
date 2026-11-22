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
import time
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
        self.vendor = bundle(self.tmp / 'v1', 'v0.2.0')
        self.successor = bundle(self.tmp / 'v2', 'v0.2.1', extra_license=True)
        sock = unit.with_suffix('.socket')
        state = self.tmp/'var/lib'/d.NAME
        state.parent.mkdir(parents=True)
        self.stack.enter_context(patch.multiple(d, ROOT=root, UNIT=unit, SOCKET=sock, STATE=state, RUNS=root/'data/runs',
                                                MARKER=root/'install-manifest.json', BUNDLE=self.vendor))
        self.user = None
        self.group = None
        self.commands = []
        self.active = set()
        self.enabled = set()
        self.stack.enter_context(patch.object(d, 'run', side_effect=self.host))
        self.stack.enter_context(patch.object(d, 'identity', side_effect=lambda: (self.user, self.group)))
        self.stack.enter_context(patch.object(d, 'system_property', side_effect=self.prop))
        self.stack.enter_context(patch.object(d, 'mounts_inside', return_value=False))
        self.stack.enter_context(patch.object(d, 'root_owned', return_value=True))
        self.stack.enter_context(patch.object(d, 'state_permissions', return_value=True))
        if os.name == 'nt':
            # The actual installer runs on Linux. Windows scanners can briefly hold a fixture's rename handle.
            def sharing_retry(operation):
                def retry(*args, **kwargs):
                    for attempt in range(8):
                        try:
                            return operation(*args, **kwargs)
                        except PermissionError as exc:
                            if exc.winerror not in (5, 32, 33) or attempt == 7:
                                raise
                            time.sleep(0.04 * (attempt + 1))
                return retry
            for name in ('rename', 'replace'):
                self.stack.enter_context(patch.object(os, name, sharing_retry(getattr(os, name))))
        self.stack.enter_context(patch.object(d, 'check_port'))
        self.stack.enter_context(patch.object(d.os, 'chown', create=True))
        self.stack.enter_context(patch.object(d.shutil, 'which', return_value='/usr/sbin/nologin'))
        self.stack.enter_context(patch.dict('sys.modules', {'pwd': SimpleNamespace(getpwall=lambda: [self.user] if self.user else [])}))
        self.reject_verify = False

    def prop(self, prop, unit=None):
        unit = unit or d.UNIT
        return {'ActiveState': 'active' if unit in self.active else 'inactive',
                'UnitFileState':'enabled' if unit in self.enabled else 'disabled',
                'LoadState':'loaded' if unit.is_file() else 'not-found',
                'FragmentPath':str(unit) if unit.is_file() else '', 'MainPID':'17', 'DropInPaths':''}[prop]

    def host(self, *args, **kwargs):
        self.commands.append(args)
        if args[0] == 'groupadd':
            self.group = SimpleNamespace(gr_gid=4101, gr_mem=[])
        elif args[0] == 'useradd':
            self.user = SimpleNamespace(pw_uid=4102, pw_gid=4101, pw_name=d.NAME, pw_dir='/nonexistent', pw_shell='/usr/sbin/nologin')
        elif args[0] == 'userdel':
            self.user = None
        elif args[0] == 'groupdel':
            self.group = None
        elif args[0] == 'systemd-analyze' and self.reject_verify:
            raise ValueError('simulated unit verification failure')
        elif args[0] == 'systemctl' and args[1] != 'daemon-reload':
            unit = d.UNIT.parent / args[-1]
            if args[1] == 'enable':
                self.enabled.add(unit)
            if args[1] == 'disable':
                self.enabled.discard(unit)
            if args[1] == 'start' or (args[1]=='enable' and '--now' in args):
                self.active.add(unit)
            if args[1] == 'stop' or (args[1]=='disable' and '--now' in args):
                self.active.discard(unit)
        return SimpleNamespace(returncode=0, stdout='')

    def install(self):
        d.perform_install('http://127.0.0.1:8080', 'fixture-probe-key', 'Probe', d.bundle_files(self.vendor))
        self.assertEqual(self.active,{d.SOCKET, d.update_units()[0]})
        self.assertEqual(self.enabled,{d.SOCKET, d.update_units()[0]})
        self.assertFalse(d.RUNS.exists())
        config = json.loads((d.ROOT/'config/config.json').read_text())
        self.assertEqual(config['parallel'], 1)
        self.assertEqual(config['probe_api_key'], 'fixture-probe-key')
        self.assertEqual(config['probe_group_name'], 'Probe')
        session = d.STATE/('a'*64+'.session')
        session.write_bytes(b'fixture renewable session')
        return session

    def legacy(self, active=False):
        self.host('groupadd'); self.host('useradd')
        d.ROOT.mkdir()
        owned=set(d.ENTRY.values()) - {'docs/'+d.SOCKET.name}
        for name in owned:
            dest=d.ROOT/name
            dest.parent.mkdir(parents=True,exist_ok=True)
            dest.write_text('v0.1.0' if name=='VERSION' else 'old '+name)
        (d.ROOT/'config').mkdir()
        (d.ROOT/'config/config.json').write_text(
            '{"parallel":1,"data_dir":"/previous","probe_api_key":"fixture-probe-key","probe_group_name":"Probe"}')
        d.UNIT.write_bytes((d.ROOT/'docs'/d.UNIT.name).read_bytes())
        d.jwrite(d.MARKER, dict(format=1,root=str(d.ROOT),dev=d.ROOT.stat().st_dev,ino=d.ROOT.stat().st_ino,
            uid=self.user.pw_uid,gid=self.group.gr_gid,service=str(d.UNIT),unit_sha256=d.digest(d.UNIT),
            files=sorted(owned|{'config/config.json',d.MARKER.name})))
        self.enabled.add(d.UNIT)
        if active:
            self.active.add(d.UNIT)
        d.RUNS.mkdir(parents=True)
        path=d.RUNS/'b80311ce-0331-4fa2-9d82-9a94eb72e526.json'
        path.write_bytes(b'v0.1 answers')
        return path

    def apply(self):
        u.apply(self.successor, d.bundle_files(self.successor), d.verify_manifest(), d.ROOT/'.rollback')

    def test_failed_fresh_install_removes_only_new_resources(self):
        neighbor = d.ROOT.parent/'sub2api'
        neighbor.write_text('live site')
        self.reject_verify = True
        with self.assertRaisesRegex(ValueError, 'simulated'):
            d.perform_install('http://127.0.0.1:8080', 'fixture-probe-key', 'Probe', d.bundle_files(self.vendor))
        self.assertEqual((self.user,self.group),(None,None))
        self.assertFalse(any(p.exists() for p in (d.ROOT,d.STATE,d.UNIT,d.SOCKET)))
        self.assertEqual(neighbor.read_text(),'live site')

    def test_uninstall_removes_session_without_backup_and_preserves_unknown_files(self):
        session = self.install()
        extra = d.STATE/'personal-file'
        extra.write_bytes(b'keep')
        with self.assertRaises(ValueError):
            d.uninstall(SimpleNamespace(check=False))
        self.assertEqual(extra.read_bytes(),b'keep')
        self.assertTrue(session.exists())
        extra.unlink()
        d.uninstall(SimpleNamespace(check=False))
        self.assertFalse(any(p.exists() for p in (d.ROOT,d.STATE,d.UNIT,d.SOCKET)))
        self.assertFalse(list(self.tmp.rglob('*.gz')))

    def test_inactive_update_keeps_config_session_and_listener_state(self):
        session=self.install()
        self.active.clear(); self.enabled.clear()
        config=d.ROOT/'config/config.json'
        existing = {"parallel": 1, "local_override": True}
        config.write_text(json.dumps(existing))
        with patch.object(u,'health',side_effect=AssertionError('started inactive socket')):
            self.apply()
        self.assertEqual(u.file_version(d.ROOT/'VERSION'),(0,2,1))
        self.assertEqual(json.loads(config.read_text()), existing)
        self.assertEqual(session.read_bytes(),b'fixture renewable session')
        self.assertFalse(self.active or self.enabled or (d.ROOT/'.rollback').exists())
        d.verify_manifest()

    def test_migration_commits_only_after_healthy_wakeup(self):
        run=self.legacy(active=True)
        cfg=(d.ROOT/'config/config.json').read_bytes()
        def live_test():
            self.assertTrue(run.exists())
            self.assertFalse(list((d.ROOT/'.rollback').rglob(run.name)))
            self.assertIn(d.SOCKET,self.active)
        with patch.object(u,'health',side_effect=live_test), patch.object(u,'owned_process',return_value=True):
            self.apply()
        self.assertEqual(self.active,{d.SOCKET, d.update_units()[0]})
        self.assertEqual(self.enabled,{d.SOCKET, d.update_units()[0]})
        self.assertFalse(d.RUNS.parent.exists())
        self.assertEqual((d.ROOT/'config/config.json').read_bytes(),cfg)
        self.assertTrue(d.STATE.is_dir())
        self.assertEqual(d.verify_manifest()['format'],2)
        self.assertLess(self.commands.index(('systemctl','stop',d.UNIT.name)),self.commands.index(('systemctl','start',d.SOCKET.name)))

    def test_failed_migration_restores_old_service_and_answers(self):
        run=self.legacy(active=True)
        manifest=d.MARKER.read_bytes()
        unit=d.UNIT.read_bytes()
        with patch.object(u,'health',side_effect=[ValueError('bad new health'),None]) as live, patch.object(u,'owned_process',return_value=True):
            with self.assertRaisesRegex(ValueError,'bad new health'):
                self.apply()
        self.assertEqual(live.call_count,2)
        self.assertEqual(self.active,{d.UNIT})
        self.assertEqual(self.enabled,{d.UNIT})
        self.assertEqual(run.read_bytes(),b'v0.1 answers')
        self.assertEqual(d.MARKER.read_bytes(),manifest)
        self.assertEqual(d.UNIT.read_bytes(),unit)
        self.assertFalse(d.STATE.exists() or d.SOCKET.exists() or (d.ROOT/'.rollback').exists())
        self.assertEqual(d.verify_manifest()['format'],1)

    def test_migration_rejects_foreign_directory_unit_or_port_before_stopping(self):
        run=self.legacy()
        d.STATE.mkdir(); other=d.STATE/'original'; other.write_text('keep')
        with self.assertRaises(ValueError): self.apply()
        self.assertEqual(other.read_text(),'keep')
        other.unlink(); d.STATE.rmdir()
        d.SOCKET.write_text('other unit')
        with self.assertRaises(ValueError): self.apply()
        self.assertEqual(d.SOCKET.read_text(),'other unit')
        d.SOCKET.unlink()
        with patch.object(d,'check_port',side_effect=OSError('occupied')),self.assertRaises(OSError): self.apply()
        self.assertTrue(run.exists())
        self.assertFalse(any(c[:2]==('systemctl','stop') for c in self.commands))

    def test_failed_socket_update_preserves_remembered_session(self):
        session=self.install()
        stamp=d.MARKER.read_bytes()
        with patch.object(u,'health',side_effect=[ValueError('bad new health'),None]), self.assertRaises(ValueError):
            self.apply()
        self.assertEqual(d.MARKER.read_bytes(),stamp)
        self.assertEqual(session.read_bytes(),b'fixture renewable session')
        self.assertEqual(self.active,{d.SOCKET, d.update_units()[0]})
        self.assertEqual(self.enabled,{d.SOCKET, d.update_units()[0]})
        self.assertFalse((d.ROOT/'LICENSES/extra.txt').exists())

    def test_committed_migration_resumes_interrupted_purge(self):
        run=self.legacy()
        with patch.object(u,'health'), patch.object(d,'purge_legacy_runs',side_effect=OSError('interrupted')):
            with self.assertRaisesRegex(OSError,'interrupted'): self.apply()
        self.assertTrue((d.ROOT/'.rollback/verified-migration').exists() and run.exists())
        u.recover(d.ROOT/'.rollback')
        self.assertFalse((d.ROOT/'.rollback').exists() or run.exists())
        self.assertEqual(d.verify_manifest()['format'],2)

    def test_health_requires_expected_version_and_process(self):
        self.install(); self.active.add(d.UNIT)
        class Response(io.BytesIO): status=200
        opener=SimpleNamespace(open=lambda *a,**kw: Response(json.dumps(self.served).encode()))
        with patch.object(u.request,'build_opener',return_value=opener), patch.object(u.time,'sleep'), patch.object(u,'owned_process',return_value=True) as same:
            self.served={'status':'ok','version':'v9.9.9'}
            with self.assertRaises(ValueError): u.health()
            self.served={'status':'ok','version':'v0.2.0','additional':True}
            same.return_value=False
            with self.assertRaises(ValueError): u.health()
            same.return_value=True
            u.health()

    def test_existing_install_gains_update_units_and_preserves_config(self):
        session = self.install()
        meta = d.verify_manifest()
        for unit in d.update_units():
            unit.unlink()
            self.active.discard(unit)
            self.enabled.discard(unit)
            doc = 'docs/' + unit.name
            (d.ROOT / doc).unlink()
            meta['files'].remove(doc)
        del meta['update_units']
        d.MARKER.write_text(json.dumps(meta))
        config = (d.ROOT / 'config/config.json').read_bytes()
        with patch.object(u, 'health'):
            self.apply()
        self.assertEqual(set(d.verify_manifest()['update_units']), {unit.name for unit in d.update_units()})
        self.assertIn(d.update_units()[0], self.active)
        self.assertEqual(config, (d.ROOT / 'config/config.json').read_bytes())
        self.assertTrue(session.exists())

    def test_same_version_repairs_units_omitted_by_old_updater(self):
        session = self.install()
        meta = d.verify_manifest()
        for unit in d.update_units():
            unit.unlink()
            self.active.discard(unit)
            self.enabled.discard(unit)
            doc = 'docs/' + unit.name
            (d.ROOT / doc).unlink()
            meta['files'].remove(doc)
        del meta['update_units']
        d.MARKER.write_text(json.dumps(meta))
        # The old updater copied the new VERSION and scripts using its old ENTRY.
        (d.ROOT / 'VERSION').write_text('v0.2.1\n')
        original = d.MARKER.read_bytes()
        config = (d.ROOT / 'config/config.json').read_bytes()
        archive = self.tmp / 'release.tar.gz'
        with tarfile.open(archive, 'w:gz') as out:
            out.add(self.successor, arcname='release')
        payload = archive.read_bytes()
        args = SimpleNamespace(recover=False, migrate=False, archive=None, check=False)
        with patch.object(u, 'release_info', return_value=((0, 2, 1), 'fixture', hashlib.sha256(payload).hexdigest(), len(payload))), \
                patch.object(u, 'http', side_effect=lambda _: io.BytesIO(payload)), patch.object(d, 'check_arch'):
            args.check = True
            u.update(args)
            self.assertEqual(d.MARKER.read_bytes(), original)
            args.check = False
            with patch.object(u, 'health', side_effect=[ValueError('bad repair'), None]), self.assertRaisesRegex(ValueError, 'bad repair'):
                u.update(args)
            self.assertEqual(d.MARKER.read_bytes(), original)
            self.assertFalse(any(unit.exists() for unit in d.update_units()))
            self.assertEqual(self.active, {d.SOCKET})
            with patch.object(u, 'health'):
                u.update(args)
            self.assertEqual(set(d.verify_manifest()['update_units']), {unit.name for unit in d.update_units()})
            self.assertIn(d.update_units()[0], self.active)
            self.assertEqual(config, (d.ROOT / 'config/config.json').read_bytes())
            self.assertTrue(session.exists())
            # Once repaired, the same version must remain a no-op.
            with patch.object(u, 'apply', side_effect=AssertionError('reapplied current version')):
                u.update(args)

    def test_failed_update_restores_update_units_and_disabled_watcher(self):
        self.install()
        watcher, worker = d.update_units()
        self.active.discard(watcher)
        self.enabled.discard(watcher)
        originals = {unit: unit.read_bytes() for unit in (watcher, worker)}
        with patch.object(u, 'health', side_effect=[ValueError('failed health'), None]), self.assertRaises(ValueError):
            self.apply()
        for unit, data in originals.items():
            self.assertEqual(unit.read_bytes(), data)
        self.assertNotIn(watcher, self.active)
        self.assertNotIn(watcher, self.enabled)
        d.verify_manifest()

    def test_uninstall_removes_update_units(self):
        self.install()
        d.uninstall(SimpleNamespace(check=False))
        self.assertFalse(any(unit.exists() for unit in d.update_units()))
        self.assertFalse(self.active or self.enabled)


class WebUpdates(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.trigger = self.root / 'update.request'
        self.status = self.root / 'status.json'
        real_path = Path
        paths = {'/run/sub2-fingerprint/update.request': self.trigger,
                 '/run/sub2-fingerprint-update': self.root,
                 '/run/sub2-fingerprint-update/status.json': self.status}
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.object(u, 'Path', side_effect=lambda p: paths.get(str(p), real_path(p))))
        self.stack.enter_context(patch.object(d, 'root_owned', return_value=True))
        self.stack.enter_context(patch.object(d, 'maintenance_lock', side_effect=contextlib.nullcontext))

    def test_fixed_request_records_success_and_is_consumed(self):
        self.trigger.write_bytes(b'')
        with patch.object(u, 'update') as update:
            u.web_update(SimpleNamespace())
        update.assert_called_once()
        self.assertFalse(self.trigger.exists())
        self.assertEqual(json.loads(self.status.read_text()), {'state': 'done'})
        u.web_finished()
        self.assertEqual(json.loads(self.status.read_text())['state'], 'done')

    def test_failure_and_abnormal_exit_record_sanitized_status(self):
        self.trigger.write_bytes(b'')
        with patch.object(u, 'update', side_effect=ValueError('private upstream details')), self.assertRaises(ValueError):
            u.web_update(SimpleNamespace())
        self.assertEqual(json.loads(self.status.read_text()), {'state': 'failed'})
        u.web_status('running')
        u.web_finished()
        self.assertEqual(json.loads(self.status.read_text()), {'state': 'failed'})

    def test_request_cannot_supply_commands(self):
        for content in (None, b'arbitrary command'):
            if content is not None:
                self.trigger.write_bytes(content)
            with patch.object(u, 'update') as update, self.assertRaises(ValueError):
                u.web_update(SimpleNamespace())
            update.assert_not_called()


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
