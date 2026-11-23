#!/usr/bin/python3
"""Install locally on a systemd host. Run from a verified, trusted release archive."""
import argparse
import contextlib
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import socket
import struct
import subprocess
import sys
from urllib.parse import urlsplit

NAME = 'sub2-fingerprint'
ROOT = Path('/opt') / NAME
UNIT = Path('/etc/systemd/system') / (NAME + '.service')
SOCKET = UNIT.with_suffix('.socket')
STATE = Path('/var/lib') / NAME
RUNS = ROOT / 'data/runs'  # Legacy v0.1.0, cleaned only after a successful migration.
MARKER = ROOT / 'install-manifest.json'
BUNDLE = Path(__file__).resolve().parent
ENTRY = {NAME: 'bin/' + NAME, 'VERSION': 'VERSION', 'DEPLOYMENT.md': 'docs/DEPLOYMENT.md',
         'uninstall.sh': 'uninstall.sh', 'deploy.py': 'deploy.py', 'update.sh': 'update.sh',
         'update.py': 'update.py', 'units/' + UNIT.name: 'docs/' + UNIT.name,
         'units/' + SOCKET.name: 'docs/' + SOCKET.name,
         'units/' + NAME + '-update.path': 'docs/' + NAME + '-update.path',
         'units/' + NAME + '-update.service': 'docs/' + NAME + '-update.service'}
PORT = 17861
RUN_FILE = re.compile(r'[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.(json|partial)')

def update_units():
    return tuple(UNIT.with_name(NAME + '-update.' + suffix) for suffix in ('path', 'service'))

def require(ok, msg):
    if not ok:
        raise ValueError(msg)

def run(*args, check=True):
    return subprocess.run(args, check=check, text=True, capture_output=True,
                          env={'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'LC_ALL': 'C'}, timeout=60)

def digest(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for block in iter(lambda: f.read(256 * 1024), b''):
            h.update(block)
    return h.hexdigest()

def regular(path):
    return path.is_file() and path.resolve() == path

def root_owned(path, disallowed_mode=0o022):
    stat = path.stat()
    return stat.st_uid == 0 and not (stat.st_mode & disallowed_mode)

def file_id(path):
    st = path.stat()
    return [st.st_dev, st.st_ino]

def safe_base(value):
    require(value and not any(c.isspace() or ord(c) < 32 for c in value), 'Sub2 地址无效')
    p = urlsplit(value)
    require(p.scheme in ('http', 'https') and p.hostname and p.path in ('', '/') and
            not p.query and not p.fragment and '@' not in p.netloc and '\\' not in value and
            p.port != 0, '请提供 Sub2API 的站点根地址，不带凭据、路径或查询参数')
    require(p.scheme == 'https' or p.hostname in ('127.0.0.1', '::1', 'localhost'), 'HTTP 地址仅支持回环主机。外部站点请用 HTTPS')
    require(not (p.hostname in ('127.0.0.1', '::1', 'localhost') and p.port == PORT), 'Sub2API 地址不能是检测器自身')
    return value.rstrip('/')

def memory_mib():
    entries = {}
    for line in Path('/proc/meminfo').read_text().splitlines():
        fields = line.split()
        if fields[0] in ('MemAvailable:', 'MemTotal:'):
            entries[fields[0]] = int(fields[1]) // 1024
    return entries['MemTotal:'], entries['MemAvailable:']

def mounts_inside(path, mountinfo=None):
    if mountinfo is None:
        mountinfo = Path('/proc/self/mountinfo').read_text()
    for line in mountinfo.splitlines():
        target = re.sub(r'\\([0-7]{3})', lambda m: chr(int(m[1], 8)), line.split()[4])
        if target == str(path) or target.startswith(str(path) + '/'):
            return True
    return False

def identity():
    import pwd, grp
    try:
        user = pwd.getpwnam(NAME)
    except KeyError:
        user = None
    try:
        group = grp.getgrnam(NAME)
    except KeyError:
        group = None
    return user, group

def bundle_files(root):
    sums = root / 'SHA256SUMS'
    require(regular(sums), '缺少 SHA256SUMS')
    hashes = {}
    for line in sums.read_text('utf-8').splitlines():
        m = re.fullmatch(r'([0-9a-fA-F]{64}) [ *](.+)', line)
        require(m, 'SHA256SUMS 格式无效')
        name = m[2][2:] if m[2].startswith('./') else m[2]
        require(not name.startswith('/') and not any(p in ('', '.', '..') for p in name.split('/')) and
                '\\' not in name and name not in hashes, 'SHA256SUMS 包含无效路径')
        hashes[name] = m[1].lower()
    files = dict(ENTRY)
    licenses = [p.relative_to(root).as_posix() for p in (root / 'LICENSES').rglob('*') if p.is_file()]
    require(licenses, '缺少 LICENSES/ 许可材料')
    files.update((n, n) for n in licenses)
    for n in set(files) | {'install.sh'} | set(hashes):
        p = root / n
        require(n in hashes and regular(p) and digest(p) == hashes[n], '包文件哈希不匹配：' + n)
    return files

def check_arch(binary):
    with binary.open('rb') as f:
        hdr = f.read(20)
    require(len(hdr) == 20 and hdr[:6] == b'\x7fELF\x02\x01' and
            struct.unpack('<H', hdr[18:20])[0] == {'aarch64': 183, 'x86_64': 62}.get(platform.machine()), '安装包与 Linux CPU 架构不匹配')

def system_property(prop, unit=None):
    r = run('systemctl', 'show', (unit or UNIT).name, '-p', prop, '--value', check=False)
    require(r.returncode in (0, 4), '无法读取 systemd 状态')
    return r.stdout.strip()

def vacant(path):
    require(path.parent.is_dir() and path.parent.resolve() == path.parent and root_owned(path.parent) and
            not os.path.lexists(path), '保留现有文件或目录：' + str(path))

def unused_unit(path):
    vacant(path)
    require(system_property('LoadState', path) == 'not-found' and not system_property('DropInPaths', path),
            '已存在同名 systemd 单元或附加配置：' + path.name)

def check_port():
    with socket.socket() as probe:
        probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        probe.bind(('127.0.0.1', PORT))
        probe.listen(1)

def create_state(uid, gid):
    vacant(STATE)
    STATE.mkdir(0o700)
    try:
        os.chown(STATE, uid, gid)
        st = STATE.stat()
        return {'path': str(STATE), 'dev': st.st_dev, 'ino': st.st_ino}
    except BaseException:
        STATE.rmdir()
        raise

def state_identity(mark, uid, gid):
    require(mark['path'] == str(STATE) and STATE.is_dir() and STATE.resolve() == STATE and not mounts_inside(STATE),
            '登录状态目录身份不符')
    st = STATE.stat()
    require((st.st_dev, st.st_ino) == (mark['dev'], mark['ino']) and state_permissions(st, uid, gid),
            '登录状态目录的归属、权限或身份不符')

def state_permissions(st, uid, gid):
    return (st.st_uid, st.st_gid, st.st_mode & 0o777) == (uid, gid, 0o700)

def check_state_contents():
    for p in STATE.iterdir():
        require(regular(p) and p.stat().st_nlink == 1 and re.fullmatch(r'[0-9a-f]{64}\.(?:session|pending)', p.name),
                '登录目录包含未识别文件，请先检查')

def remove_state(mark, uid, gid):
    state_identity(mark, uid, gid)
    check_state_contents()
    shutil.rmtree(STATE)

def preflight():
    require(platform.system() == 'Linux' and Path('/run/systemd/system').is_dir(), '需要正在运行的 Linux systemd')
    require({'cpu', 'memory', 'pids'} <= set(Path('/sys/fs/cgroup/cgroup.controllers').read_text().split()), '需要 cgroup v2 的 cpu、memory 和 pids 控制器')
    vacant(ROOT)
    vacant(STATE)
    unused_unit(UNIT)
    unused_unit(SOCKET)
    for unit in update_units():
        unused_unit(unit)
    require(identity() == (None, None), '已有同名用户或组')
    check_port()
    total, avail = memory_mib()
    print(f'内存 {total} MiB，可用 {avail} MiB。服务由回环 socket 按需唤起。')
    if avail < 128:
        print('当前余量小于试运行参考值 128 MiB。请先观察正常业务高峰的可用内存。')

def sync_dir(path):
    if os.name == 'posix':
        fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)

def write_new(path, data, mode, gid=None):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w', encoding='utf-8', newline='\n') as f:
        f.write(data)
        f.flush()
        os.fsync(f.fileno())
    if gid is not None:
        os.chown(path, 0, gid)
    os.chmod(path, mode)
    sync_dir(path.parent)

def jwrite(path, obj, mode=0o644, gid=None):
    write_new(path, json.dumps(obj, ensure_ascii=False, indent=2) + '\n', mode, gid)

def drop_user():
    if identity()[0] is not None:
        run('userdel', NAME)

def drop_group():
    if identity()[1] is not None:
        run('groupdel', NAME)

def remove_new_tree():
    require(ROOT.resolve() == ROOT and not mounts_inside(ROOT), '安装目录出现符号链接或挂载，已保留')
    shutil.rmtree(ROOT)

def safe_probe_key(value):
    require(value and len(value) <= 32 << 10 and not any(c.isspace() or ord(c) < 32 for c in value),
            'Probe API Key 无效')
    return value

def safe_group_name(value):
    require(value and len(value) <= 128 and value.strip() == value, 'Probe 分组名称无效')
    return value

def perform_install(base, probe_key, probe_group_name, files):
    shell = shutil.which('nologin', path='/usr/sbin:/sbin:/usr/bin:/bin')
    require(shell, '找不到 nologin shell')
    with contextlib.ExitStack() as undo:
        run('groupadd', '--system', NAME)
        undo.callback(drop_group)
        run('useradd', '--system', '--gid', NAME, '--no-create-home', '--no-user-group', '--no-log-init', '--home-dir', '/nonexistent', '--shell', shell, NAME)
        undo.callback(drop_user)
        user, group = identity()
        ROOT.mkdir(0o755)
        undo.callback(remove_new_tree)
        for src, dest in files.items():
            path = ROOT / dest
            path.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(BUNDLE / src, path)
            os.chmod(path, 0o755 if src in (NAME, 'uninstall.sh', 'update.sh') else 0o644)
        (ROOT / 'config').mkdir(0o755)
        cf = ROOT / 'config/config.json'
        jwrite(cf, {'sub2_url': base, 'parallel': 1, 'listen': f'127.0.0.1:{PORT}',
                    'data_dir': str(RUNS), 'probe_api_key': probe_key,
                    'probe_group_name': probe_group_name}, 0o640, group.gr_gid)
        state = create_state(user.pw_uid, group.gr_gid)
        undo.callback(remove_state, state, user.pw_uid, group.gr_gid)
        service = ROOT / files['units/' + UNIT.name]
        listener = ROOT / files['units/' + SOCKET.name]
        update_sources = [(ROOT / files['units/' + unit.name], unit) for unit in update_units()]
        jwrite(MARKER, {'format': 2, 'root': str(ROOT), 'dev': ROOT.stat().st_dev, 'ino': ROOT.stat().st_ino,
                        'uid': user.pw_uid, 'gid': group.gr_gid, 'service': str(UNIT), 'state': state,
                        'unit_sha256': digest(service), 'socket_sha256': digest(listener),
                        'update_units': {target.name: digest(source) for source, target in update_sources},
                        'files': sorted(set(files.values()) | {'config/config.json', MARKER.name})})
        run('systemd-analyze', '--man=no', 'verify', str(service), str(listener), *(str(source) for source, _ in update_sources))
        for source, target in [(service, UNIT), (listener, SOCKET), *update_sources]:
            with source.open('rb') as inp, target.open('xb') as out:
                undo.callback(target.unlink)
                shutil.copyfileobj(inp, out)
            os.chmod(target, 0o644)
        run('systemctl', 'daemon-reload')
        undo.callback(run, 'systemctl', 'disable', '--now', SOCKET.name, check=False)
        run('systemctl', 'enable', '--now', SOCKET.name)
        undo.callback(run, 'systemctl', 'disable', '--now', update_units()[0].name, check=False)
        run('systemctl', 'enable', '--now', update_units()[0].name)
        undo.pop_all()
    print('安装完成，回环 socket 已监听。首次访问会启动检测器。')

@contextlib.contextmanager
def maintenance_lock():
    import fcntl
    require(ROOT.resolve() == ROOT, '安装目录身份不符')
    fd = os.open(ROOT, os.O_DIRECTORY | os.O_RDONLY | os.O_NOFOLLOW)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        os.close(fd)

def verify_manifest():
    require(regular(MARKER) and ROOT.resolve() == ROOT and not mounts_inside(ROOT), '未找到本工具安装清单，或目录含挂载点')
    require(root_owned(ROOT) and root_owned(MARKER), '安装目录或清单归属与权限不符')
    meta = json.loads(MARKER.read_text('utf-8'))
    require(meta['format'] in (1, 2) and meta['root'] == str(ROOT) and meta['service'] == str(UNIT) and
            (meta['dev'], meta['ino']) == (ROOT.stat().st_dev, ROOT.stat().st_ino), '清单与现存目录不匹配')
    units = [(UNIT, meta['unit_sha256'])]
    if meta['format'] == 2:
        units.append((SOCKET, meta['socket_sha256']))
        state_identity(meta['state'], meta['uid'], meta['gid'])
    update_hashes = meta.get('update_units', {})
    require(not update_hashes or set(update_hashes) == {u.name for u in update_units()}, '更新服务清单无效')
    units.extend((u, update_hashes[u.name]) for u in update_units() if u.name in update_hashes)
    for unit, checksum in units:
        require(regular(unit) and root_owned(unit) and digest(unit) == checksum and
                system_property('FragmentPath', unit) == str(unit) and not system_property('DropInPaths', unit),
                '单元文件或附加配置已变更：' + unit.name)
    user, group = identity()
    require(user is None or (user.pw_uid == meta['uid'] and user.pw_gid == meta['gid'] and user.pw_dir == '/nonexistent' and user.pw_shell.endswith('/nologin')), '系统用户与清单不符')
    require(group is None or (group.gr_gid == meta['gid'] and not group.gr_mem), '系统组仍有其他用途')
    if group is not None:
        import pwd
        require(all(u.pw_name == NAME for u in pwd.getpwall() if u.pw_gid == group.gr_gid), '系统组仍被其他用户使用')
    expected = set(meta['files'])
    allowed_dirs = {p.as_posix() for f in expected for p in Path(f).parents}
    if meta['format'] == 1:
        allowed_dirs.update(('data', 'data/runs'))
    for p in ROOT.rglob('*'):
        rel = p.relative_to(ROOT).as_posix()
        require(not p.is_symlink(), '目录下含符号链接，请先审阅')
        require(p.is_file() or p.is_dir(), '目录下含特殊文件，请先审阅')
        if meta['format'] == 1 and rel.startswith('data/runs/'):
            require(p.parent == RUNS and p.is_file() and RUN_FILE.fullmatch(p.name), '结果目录中有本工具未识别的文件：' + rel)
        else:
            require(rel in (allowed_dirs if p.is_dir() else expected), '目录中有清单之外的文件：' + rel)
    return meta

def purge_legacy_runs():
    if RUNS.exists():
        require(RUNS.resolve() == RUNS and not mounts_inside(RUNS), '旧结果目录含链接或挂载点')
        for p in RUNS.iterdir():
            require(regular(p) and RUN_FILE.fullmatch(p.name), '旧结果目录中有未知文件，请先检查')
        for p in RUNS.iterdir():
            p.unlink()
        RUNS.rmdir()
    if RUNS.parent.is_dir():
        RUNS.parent.rmdir()

def uninstall(opts):
    meta = verify_manifest()
    require(meta['format'] == 2, '旧安装请使用原版本自带的卸载器')
    check_state_contents()
    if opts.check:
        print('已核对目录、账号及 systemd 单元。')
        return
    if meta.get('update_units'):
        watcher, worker = update_units()
        run('systemctl', 'disable', '--now', watcher.name)
        run('systemctl', 'stop', worker.name)
        run('systemctl', 'clean', '--what=runtime', worker.name)
    run('systemctl', 'disable', '--now', SOCKET.name)
    run('systemctl', 'stop', UNIT.name)
    verify_manifest()
    remove_state(meta['state'], meta['uid'], meta['gid'])
    SOCKET.unlink()
    UNIT.unlink()
    for unit in update_units():
        if unit.name in meta.get('update_units', {}):
            unit.unlink()
    run('systemctl', 'daemon-reload')
    drop_user()
    drop_group()
    remove_new_tree()
    print('已移除检测器、回环监听和本机登录状态。')

def main():
    os.umask(0o022)
    parser = argparse.ArgumentParser()
    modes = parser.add_subparsers(dest='op', required=True)
    ins = modes.add_parser('install')
    ins.add_argument('--sub2-url', required=True)
    key_input = ins.add_mutually_exclusive_group(required=True)
    key_input.add_argument('--probe-api-key')
    key_input.add_argument('--probe-api-key-stdin', action='store_true')
    ins.add_argument('--probe-group-name', default='Probe')
    ins.add_argument('--check', action='store_true')
    un = modes.add_parser('uninstall')
    un.add_argument('--check', action='store_true')
    args = parser.parse_args()
    if args.op == 'install':
        base = safe_base(args.sub2_url)
        probe_key = safe_probe_key(sys.stdin.readline((32 << 10) + 2).rstrip('\r\n')
                                   if args.probe_api_key_stdin else args.probe_api_key)
        probe_group_name = safe_group_name(args.probe_group_name)
        files = bundle_files(BUNDLE)
        check_arch(BUNDLE / NAME)
        preflight()
        if args.check:
            print('安装预检通过。')
            return
        require(os.geteuid() == 0, '安装需要 sudo 权限')
        perform_install(base, probe_key, probe_group_name, files)
    else:
        require(os.geteuid() == 0, '核对归属和清理需要 sudo 权限')
        with maintenance_lock():
            uninstall(args)

if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as err:
        print('部署工具已停止：', err, file=sys.stderr)
        sys.exit(1)
