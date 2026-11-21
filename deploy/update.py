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
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)

def running():
    state = d.system_property('ActiveState')
    d.require(state in ('active', 'inactive', 'failed'), '服务正在切换状态，请稍后再试')
    return state == 'active'

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
                if pid > 0 and os.path.samefile(f'/proc/{pid}/exe', d.ROOT / 'bin' / d.NAME):
                    return
        except (ValueError, OSError):
            pass
        time.sleep(0.5)
    raise ValueError('检测器健康检查失败')

class NoRedirect(request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None

def snapshot(folder, meta, files, active):
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
        journal = {'dev': meta['dev'], 'ino': meta['ino'], 'active': active,
                   'previous': sorted(previous), 'new': sorted(set(files.values()) - previous)}
        d.jwrite(stage / 'journal.json', journal, 0o600)
        os.rename(stage, folder)

def recover(folder):
    d.require(folder.resolve() == folder and d.root_owned(folder, 0o077) and
              not d.mounts_inside(folder), '恢复目录权限或挂载状态无效')
    j = json.loads((folder / 'journal.json').read_text())
    d.require((j['dev'], j['ino']) == (d.ROOT.stat().st_dev, d.ROOT.stat().st_ino), '恢复记录属于另一安装目录')
    for name in j['previous'] + j['new']:
        safe = name in d.ENTRY.values() or name == d.MARKER.name or name.startswith('LICENSES/')
        dest = d.ROOT / name
        d.require(safe and dest == dest.resolve() and d.ROOT in dest.parents, '恢复清单含无效程序路径')
    d.run('systemctl', 'stop', d.UNIT.name)
    for name in j['previous']:
        source = folder / 'old' / name
        d.require(d.regular(source) and (folder / 'old') in source.parents, '恢复文件路径无效')
        atomic_install(source, d.ROOT / name, source.stat().st_mode & 0o777)
    for name in j['new']:
        dest = d.ROOT / name
        d.require(dest == dest.resolve() and d.ROOT in dest.parents and name not in ('config/config.json', d.MARKER.name), '恢复目标无效')
        if dest.exists():
            dest.unlink()
            parent = dest.parent
            while parent != d.ROOT and not any(parent.iterdir()):
                parent.rmdir()
                parent = parent.parent
    atomic_install(folder / 'old.service', d.UNIT, 0o644)
    d.run('systemctl', 'daemon-reload')
    if j['active']:
        d.run('systemctl', 'start', d.UNIT.name)
        health()
    shutil.rmtree(folder)

def apply(bundle, files, meta, folder):
    active = running()
    snapshot(folder, meta, files, active)
    try:
        if active:
            d.run('systemctl', 'stop', d.UNIT.name)
        for src, dest in files.items():
            atomic_install(bundle / src, d.ROOT / dest, 0o755 if src in (d.NAME, 'uninstall.sh', 'update.sh') else 0o644)
        unit = d.ROOT / files[d.UNIT.name]
        d.run('systemd-analyze', '--man=no', 'verify', str(unit))
        atomic_install(unit, d.UNIT, 0o644)
        # Keep ownership of already installed files, including prior license material.
        updated = dict(meta, unit_sha256=d.digest(d.UNIT), files=sorted(set(meta['files']) | set(files.values())))
        temp_marker = folder / 'new-manifest.json'
        d.jwrite(temp_marker, updated)
        atomic_install(temp_marker, d.MARKER, 0o644)
        d.run('systemctl', 'daemon-reload')
        if active:
            d.run('systemctl', 'start', d.UNIT.name)
            health()
    except BaseException:
        print('新版本未通过。正在恢复原版本。', file=sys.stderr)
        recover(folder)
        raise
    shutil.rmtree(folder)
    print('更新完成。' + ('运行中，健康检查通过。' if active else '服务保持停止，启动后可核验健康。'))

def update(args):
    folder = d.ROOT / '.rollback'
    if args.recover:
        recover(folder)
        print('已恢复更新前的程序和运行状态。')
        return
    d.require(not os.path.lexists(folder), '存在未完成的更新。请先运行 update.sh --recover')
    meta = d.verify_manifest()
    current = file_version(d.ROOT / 'VERSION')
    newest = None
    if not args.archive:
        newest, url, sha, size = release_info()
        print('已安装', '.'.join(map(str, current)), 'GitHub 最新', '.'.join(map(str, newest)))
        if newest <= current or args.check:
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
        if candidate == current or args.check:
            print('本地发布包已验证。')
            return
        apply(bundle, files, meta, folder)

def main():
    p = argparse.ArgumentParser()
    p.add_argument('--archive')
    p.add_argument('--sha256')
    modes = p.add_mutually_exclusive_group()
    modes.add_argument('--check', action='store_true')
    modes.add_argument('--recover', action='store_true')
    args = p.parse_args()
    d.require(os.geteuid() == 0, '更新需要 sudo 权限')
    d.require(Path(__file__).resolve().parent == d.ROOT, '请调用安装目录里的 update.sh')
    with d.maintenance_lock():
        update(args)

if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, KeyError, d.subprocess.SubprocessError, tarfile.TarError) as exc:
        print('更新已停止：', exc, file=sys.stderr)
        sys.exit(1)
