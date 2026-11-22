#!/usr/bin/python3
"""On-demand release update. Run the installed copy, e.g. sudo sh /opt/sub2-fingerprint/update.sh."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
from urllib.parse import urlsplit
from urllib import request

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location('detector_deploy', Path(__file__).with_name('deploy.py'))
d = importlib.util.module_from_spec(spec)
spec.loader.exec_module(d)
REPO = 'AuroraWhisperer/sub2-fingerprint'
API = 'https://api.github.com/repos/' + REPO + '/releases/latest'
ARCHIVE_LIMIT = 64 << 20
PLAIN_LIMIT = 96 << 20

def version(value):
    m = re.fullmatch(r'v?(\d+)\.(\d+)\.(\d+)', value.strip())
    d.require(m, '需要稳定版本号，格式如 v1.2.3')
    return tuple(map(int, m.groups()))

def file_version(path):
    # The release VERSION contains a version line and may include source metadata.
    for line in path.read_text('utf-8').splitlines():
        m = re.fullmatch(r'(?:version\s*[:=]\s*)?(v?\d+\.\d+\.\d+)', line.strip(), re.I)
        if m:
            return version(m[1])
    raise ValueError('VERSION 缺少有效版本号')

def release_host(url):
    p = urlsplit(url)
    return (p.scheme == 'https' and p.port in (443, None) and '@' not in p.netloc and
            p.hostname in ('api.github.com', 'github.com', 'release-assets.githubusercontent.com', 'objects.githubusercontent.com'))

class ReleaseRedirect(request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        d.require(release_host(newurl), '下载跳转来源无效')
        return super().redirect_request(req, fp, code, msg, headers, newurl)

def http(url):
    d.require(release_host(url), '下载地址无效')
    opener = request.build_opener(request.ProxyHandler({}), ReleaseRedirect())
    return opener.open(request.Request(url, headers={'User-Agent': 'sub2-fingerprint-updater'}), timeout=20)

def get_small(url, limit):
    with http(url) as r:
        data = r.read(limit + 1)
    d.require(len(data) <= limit, '发布元数据超过上限')
    return data

def trusted_asset(meta, name):
    matches = [a for a in meta['assets'] if a['name'] == name and a['state'] == 'uploaded']
    prefix = f"https://github.com/{REPO}/releases/download/{meta['tag_name']}/"
    d.require(len(matches) == 1 and matches[0]['browser_download_url'] == prefix + name, '缺少可信发布附件 ' + name)
    return matches[0]

def release_info():
    meta = json.loads(get_small(API, 512 << 10))
    d.require(not meta.get('draft', True) and not meta.get('prerelease', True), '未找到正式发布')
    ver = version(meta['tag_name'])
    arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(platform.machine())
    d.require(arch, '当前架构缺少发布包')
    name = 'sub2-fingerprint-linux-' + arch + '.tar.gz'
    asset = trusted_asset(meta, name)
    d.require(0 < asset['size'] <= ARCHIVE_LIMIT, '发布包大小无效')
    sha = asset.get('digest') or ''
    if re.fullmatch(r'sha256:[0-9a-f]{64}', sha):
        return ver, asset['browser_download_url'], sha[7:], asset['size']
    sums = get_small(trusted_asset(meta, 'SHA256SUMS.txt')['browser_download_url'], 32 << 10).decode('utf-8')
    entries = re.findall(r'^([0-9a-fA-F]{64}) [ *](?:\./)?' + re.escape(name) + r'\r?$', sums, re.M)
    d.require(len(entries) == 1, '未找到该架构的 SHA256 值')
    return ver, asset['browser_download_url'], entries[0].lower(), asset['size']

def acquire(src, target, sha, size=None):
    h = hashlib.sha256()
    count = 0
    with target.open('xb') as out:
        for block in iter(lambda: src.read(256 << 10), b''):
            count += len(block)
            d.require(count <= ARCHIVE_LIMIT and (size is None or count <= size), '发布包超过大小限制')
            h.update(block)
            out.write(block)
    d.require(h.hexdigest() == sha and (size is None or count == size), '整包 SHA256 或长度不匹配')

def unpack(archive, root):
    """Only create ordinary files and directories. Permissions come from our own installer."""
    root.mkdir(mode=0o700)
    total = 0
    names = set()
    with tarfile.open(archive, 'r|gz') as tar:
        for i, item in enumerate(tar):
            name = item.name[2:] if item.name.startswith('./') else item.name
            if name in ('', '.') and item.isdir():
                continue
            d.require(i < 256 and len(name) < 240 and not name.startswith('/') and '\\' not in name and
                      not any(p in ('', '.', '..') for p in name.split('/')) and name not in names,
                      '发布包目录结构无效')
            names.add(name)
            dest = root / name
            if item.isdir():
                dest.mkdir(mode=0o700, parents=True, exist_ok=True)
                continue
            total += item.size
            d.require(item.isreg() and not item.issparse() and 0 <= item.size and total <= PLAIN_LIMIT, '发布包含特殊文件或内容过大')
            dest.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            with tar.extractfile(item) as source, dest.open('xb') as out:
                shutil.copyfileobj(source, out, length=256 << 10)
    if (root / 'SHA256SUMS').is_file():
        return root
    entries = list(root.iterdir())
    d.require(len(entries) == 1 and entries[0].is_dir(), '发布包缺少 SHA256SUMS')
    return entries[0]

def atomic_install(src, dest, mode):
    dest.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix='.update-', dir=dest.parent)
    try:
        with os.fdopen(fd, 'wb') as to, src.open('rb') as f:
            shutil.copyfileobj(f, to, length=256 << 10)
            os.chmod(tmp, mode)
            to.flush()
            os.fsync(to.fileno())
        os.replace(tmp, dest)
        d.sync_dir(dest.parent)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)

def running(unit=None):
    state = d.system_property('ActiveState', unit)
    d.require(state in ('active', 'inactive', 'failed'), '服务正在切换状态，请稍后再试')
    return state == 'active'

def owned_process(pid):
    return pid > 0 and os.path.samefile(f'/proc/{pid}/exe', d.ROOT / 'bin' / d.NAME)

def health():
    expected = file_version(d.ROOT / 'VERSION')
    opener = request.build_opener(request.ProxyHandler({}), NoRedirect())
    for _ in range(12):
        try:
            with opener.open('http://127.0.0.1:17861/healthz', timeout=2) as resp:
                payload = json.loads(resp.read(512))
                ok = (resp.status == 200 and isinstance(payload, dict) and payload.get('status') == 'ok' and
                      isinstance(payload.get('version'), str) and version(payload['version']) == expected)
            if ok and running():
                pid = int(d.system_property('MainPID'))
                if owned_process(pid):
                    return
        except (ValueError, OSError):
            pass
        time.sleep(0.5)
    raise ValueError('检测器健康检查失败')

class NoRedirect(request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None

def snapshot(folder, meta, files, active, enabled, socket_sum, update_sums):
    # Publish a complete rollback record before changing any installed file.
    with tempfile.TemporaryDirectory(prefix='.sub2-fingerprint-backup-', dir=d.ROOT.parent) as tmp:
        stage = Path(tmp) / 'snapshot'
        stage.mkdir(0o700)
        previous = set(meta['files']) - {'config/config.json'}
        for name in previous:
            original = d.ROOT / name
            d.require(d.regular(original), '现有程序文件缺失：' + name)
            backup = stage / 'old' / name
            backup.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(original, backup)
        shutil.copy2(d.UNIT, stage / 'old.service')
        if meta['format'] == 2:
            shutil.copy2(d.SOCKET, stage / 'old.socket')
        for unit in d.update_units():
            if unit.name in meta.get('update_units', {}):
                shutil.copy2(unit, stage / ('old.' + unit.name))
        watcher = d.update_units()[0]
        journal = {'dev': meta['dev'], 'ino': meta['ino'], 'active': active, 'enabled': enabled,
                   'socket_sum': socket_sum,
                   'update_sums': update_sums,
                   'watcher_active': running(watcher) if meta.get('update_units') else False,
                   'watcher_enabled': d.system_property('UnitFileState', watcher) == 'enabled' if meta.get('update_units') else False,
                   'previous': sorted(previous), 'new': sorted(set(files.values()) - previous)}
        d.jwrite(stage / 'journal.json', journal, 0o600)
        if os.name == 'posix':
            for entry in sorted(stage.rglob('*'), key=lambda p: len(p.parts), reverse=True):
                if entry.is_file():
                    with entry.open('rb') as source:
                        os.fsync(source.fileno())
                elif entry.is_dir():
                    d.sync_dir(entry)
            d.sync_dir(stage)
        os.rename(stage, folder)
        d.sync_dir(folder.parent)

def finish_migration(folder):
    d.purge_legacy_runs()
    shutil.rmtree(folder)

def enable_as(unit, enabled):
    d.run('systemctl', 'enable' if enabled else 'disable', unit.name)

def recover(folder):
    d.require(folder.resolve() == folder and d.root_owned(folder, 0o077) and
              not d.mounts_inside(folder), '恢复目录权限或挂载状态无效')
    j = json.loads((folder / 'journal.json').read_text())
    d.require((j['dev'], j['ino']) == (d.ROOT.stat().st_dev, d.ROOT.stat().st_ino), '恢复记录属于另一安装目录')
    if (folder / 'verified-migration').exists():
        finish_migration(folder)
        return
    old = json.loads((folder / 'old' / d.MARKER.name).read_text())
    legacy = old['format'] == 1
    for unit in d.update_units():
        if os.path.lexists(unit):
            d.require(d.regular(unit) and d.digest(unit) in
                      (old.get('update_units', {}).get(unit.name), j.get('update_sums', {}).get(unit.name)),
                      '恢复时发现来源不明的更新单元：' + unit.name)
    watcher = d.update_units()[0]
    if watcher.exists():
        d.run('systemctl', 'disable', '--now', watcher.name)
    for name in j['previous'] + j['new']:
        safe = name in d.ENTRY.values() or name == d.MARKER.name or name.startswith('LICENSES/')
        dest = d.ROOT / name
        d.require(safe and dest == dest.resolve() and d.ROOT in dest.parents, '恢复清单含无效程序路径')
    if os.path.lexists(d.SOCKET):
        d.require(d.regular(d.SOCKET) and d.digest(d.SOCKET) in (old.get('socket_sha256'), j['socket_sum']),
                  '恢复时发现来源不明的 socket 单元')
        d.run('systemctl', 'disable', '--now', d.SOCKET.name)
    d.run('systemctl', 'stop', d.UNIT.name)
    for name in j['previous']:
        source = folder / 'old' / name
        d.require(d.regular(source) and (folder / 'old') in source.parents, '恢复文件路径无效')
        atomic_install(source, d.ROOT / name, source.stat().st_mode & 0o777)
    for name in j['new']:
        dest = d.ROOT / name
        d.require(name != d.MARKER.name, '恢复目标无效')
        if dest.exists():
            dest.unlink()
            parent = dest.parent
            while parent != d.ROOT and not any(parent.iterdir()):
                parent.rmdir()
                parent = parent.parent
    atomic_install(folder / 'old.service', d.UNIT, 0o644)
    if legacy:
        if d.SOCKET.exists():
            d.SOCKET.unlink()
        state_mark = folder / 'created-state.json'
        if state_mark.exists() and os.path.lexists(d.STATE):
            d.remove_state(json.loads(state_mark.read_text()), old['uid'], old['gid'])
    else:
        atomic_install(folder / 'old.socket', d.SOCKET, 0o644)
    for unit in d.update_units():
        if unit.name in old.get('update_units', {}):
            atomic_install(folder / ('old.' + unit.name), unit, 0o644)
        elif unit.exists():
            unit.unlink()
    d.run('systemctl', 'daemon-reload')
    if old.get('update_units'):
        enable_as(watcher, j['watcher_enabled'])
        if j['watcher_active']:
            d.run('systemctl', 'start', watcher.name)
    restored = d.UNIT if legacy else d.SOCKET
    enable_as(restored, j['enabled'])
    if j['active']:
        d.run('systemctl', 'start', restored.name)
        health()
    shutil.rmtree(folder)

def preflight_migration(meta):
    d.require(meta['format'] == 1 and file_version(d.ROOT / 'VERSION') == (0, 1, 0),
              '只有 v0.1.0 使用一次性迁移入口')
    d.unused_unit(d.SOCKET)
    d.vacant(d.STATE)
    if running(d.UNIT):
        pid = int(d.system_property('MainPID', d.UNIT))
        d.require(owned_process(pid),
                  '旧版正在运行的进程身份与安装目录不符')
    else:
        d.check_port()

def apply(bundle, files, meta, folder):
    legacy = meta['format'] == 1
    old = d.UNIT if legacy else d.SOCKET
    active = running(old)
    unit_state = d.system_property('UnitFileState', old)
    d.require(unit_state in ('enabled', 'disabled'), '单元有特殊启用状态，请先检查')
    enabled = unit_state == 'enabled'
    if legacy:
        preflight_migration(meta)
    if not meta.get('update_units'):
        for unit in d.update_units():
            d.unused_unit(unit)
    service_src, socket_src = ('units/' + unit.name for unit in (d.UNIT, d.SOCKET))
    socket_sum = d.digest(bundle / socket_src)
    update_sums = {unit.name: d.digest(bundle / 'units' / unit.name) for unit in d.update_units()}
    snapshot(folder, meta, files, active, enabled, socket_sum, update_sums)
    watcher = d.update_units()[0]
    watcher_enabled = d.system_property('UnitFileState', watcher) == 'enabled' if meta.get('update_units') else True
    watcher_active = running(watcher) if meta.get('update_units') else True
    try:
        if meta.get('update_units'):
            d.run('systemctl', 'stop', watcher.name)
        if legacy:
            state = d.create_state(meta['uid'], meta['gid'])
            try:
                d.jwrite(folder / 'created-state.json', state, 0o600)
            except BaseException:
                d.remove_state(state, meta['uid'], meta['gid'])
                raise
            enable_as(d.UNIT, False)
        else:
            state = meta['state']
            d.run('systemctl', 'stop', d.SOCKET.name)
        d.run('systemctl', 'stop', d.UNIT.name)
        d.check_port()
        for src, dest in files.items():
            atomic_install(bundle / src, d.ROOT / dest, 0o755 if src in (d.NAME, 'uninstall.sh', 'update.sh') else 0o644)
        unit = d.ROOT / files[service_src]
        listener = d.ROOT / files[socket_src]
        update_sources = [(d.ROOT / files['units/' + target.name], target) for target in d.update_units()]
        d.run('systemd-analyze', '--man=no', 'verify', str(unit), str(listener), *(str(source) for source, _ in update_sources))
        atomic_install(unit, d.UNIT, 0o644)
        if legacy:
            with d.SOCKET.open('xb') as out, listener.open('rb') as inp:
                shutil.copyfileobj(inp, out)
            os.chmod(d.SOCKET, 0o644)
        else:
            atomic_install(listener, d.SOCKET, 0o644)
        for source, target in update_sources:
            atomic_install(source, target, 0o644)
        updated = dict(meta, format=2, state=state, unit_sha256=d.digest(d.UNIT), socket_sha256=socket_sum,
                       update_units=update_sums,
                       files=sorted(set(meta['files']) | set(files.values())))
        temp_marker = folder / 'new-manifest.json'
        d.jwrite(temp_marker, updated)
        atomic_install(temp_marker, d.MARKER, 0o644)
        d.run('systemctl', 'daemon-reload')
        if legacy:
            enable_as(d.SOCKET, True)
        if active or legacy:
            d.run('systemctl', 'start', d.SOCKET.name)
            health()
        enable_as(watcher, watcher_enabled)
        if watcher_active:
            d.run('systemctl', 'start', watcher.name)
        if legacy:
            d.write_new(folder / 'verified-migration', 'ok', 0o600)
    except BaseException:
        print('新版本未通过。正在恢复原版本。', file=sys.stderr)
        recover(folder)
        raise
    if legacy:
        finish_migration(folder)
    else:
        shutil.rmtree(folder)
    print('已切换到按需监听。' if legacy else '已完成手动更新。')

def update(args):
    folder = d.ROOT / '.rollback'
    if args.recover:
        recover(folder)
        print('已处理未完成的维护操作。')
        return
    d.require(not os.path.lexists(folder), '存在未完成的更新。请先运行 update.sh --recover')
    meta = d.verify_manifest()
    current = file_version(d.ROOT / 'VERSION')
    if args.migrate:
        files = d.bundle_files(d.BUNDLE)
        d.check_arch(d.BUNDLE / d.NAME)
        d.require(file_version(d.BUNDLE / 'VERSION') > current, '迁移包版本必须高于已安装版本')
        preflight_migration(meta)
        if args.check:
            print('v0.1.0 迁移预检通过。')
        else:
            apply(d.BUNDLE, files, meta, folder)
        return
    d.require(meta['format'] == 2, '请在新版解压目录执行 sudo sh ./install.sh --migrate')
    # Older updaters copy the new scripts but omit units absent from their ENTRY.
    repair_units = not meta.get('update_units')
    newest = None
    if not args.archive:
        newest, url, sha, size = release_info()
        print('已安装', '.'.join(map(str, current)), 'GitHub 最新', '.'.join(map(str, newest)))
        if newest < current or (newest == current and not repair_units) or args.check:
            return
    else:
        d.require(bool(re.fullmatch('[0-9a-fA-F]{64}', args.sha256 or '')), '本地发布包需要提供可信的 --sha256')
        sha, size = args.sha256.lower(), None
    with tempfile.TemporaryDirectory(prefix='.sub2-fingerprint-download-', dir=d.ROOT.parent) as temp:
        scratch = Path(temp)
        archive = scratch / 'download.tar.gz'
        with (open(args.archive, 'rb') if args.archive else http(url)) as src:
            acquire(src, archive, sha, size)
        bundle = unpack(archive, scratch / 'expanded')
        files = d.bundle_files(bundle)
        d.check_arch(bundle / d.NAME)
        candidate = file_version(bundle / 'VERSION')
        d.require(newest is None or newest == candidate, '程序版本与 GitHub 发布标签不一致')
        d.require(candidate >= current, '选择的发布版本低于已安装版本')
        if (candidate == current and not repair_units) or args.check:
            print('本地发布包已验证。')
            return
        apply(bundle, files, meta, folder)

def web_status(state):
    folder = Path('/run/sub2-fingerprint-update')
    d.require(folder.is_dir() and folder.resolve() == folder and d.root_owned(folder), '更新状态目录无效')
    fd, name = tempfile.mkstemp(prefix='.status-', dir=folder)
    try:
        with os.fdopen(fd, 'w', encoding='utf-8') as out:
            json.dump({'state': state}, out)
            out.flush()
            os.fsync(out.fileno())
        os.chmod(name, 0o644)
        os.replace(name, folder / 'status.json')
    finally:
        if os.path.exists(name):
            os.unlink(name)

def web_update(args):
    trigger = Path('/run/sub2-fingerprint/update.request')
    d.require(trigger.exists() and not trigger.is_symlink() and trigger.is_file() and trigger.stat().st_size == 0,
              '未找到有效的页面更新请求')
    web_status('running')
    trigger.unlink()
    try:
        with d.maintenance_lock():
            update(args)
        web_status('done')
    except BaseException:
        web_status('failed')
        raise

def web_finished():
    status = Path('/run/sub2-fingerprint-update/status.json')
    if status.exists() and json.loads(status.read_text('utf-8')).get('state') == 'running':
        web_status('failed')

def main():
    p = argparse.ArgumentParser()
    p.add_argument('--migrate', action='store_true')
    p.add_argument('--archive')
    p.add_argument('--sha256')
    modes = p.add_mutually_exclusive_group()
    modes.add_argument('--check', action='store_true')
    modes.add_argument('--recover', action='store_true')
    modes.add_argument('--web', action='store_true')
    modes.add_argument('--web-finished', action='store_true')
    args = p.parse_args()
    d.require(os.geteuid() == 0, '更新需要 sudo 权限')
    d.require(args.migrate or Path(__file__).resolve().parent == d.ROOT, '请调用安装目录里的 update.sh')
    d.require(not args.migrate or not (args.archive or args.sha256), '迁移直接核对当前新版解压包')
    d.require(not (args.web or args.web_finished) or not (args.migrate or args.archive or args.sha256), '页面更新不接受自定义参数')
    if args.web_finished:
        web_finished()
        return
    if args.web:
        web_update(args)
        return
    with d.maintenance_lock():
        update(args)

if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, KeyError, tarfile.TarError, subprocess.SubprocessError) as exc:
        print('更新已停止：', exc, file=sys.stderr)
        sys.exit(1)
