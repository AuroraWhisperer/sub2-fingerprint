"""Build default Linux release archives with a clean, explicit file list."""
import argparse
import gzip
import hashlib
import io
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
DEPLOY_FILES = ('deploy.py','update.py','install.sh','uninstall.sh','update.sh','sub2-fingerprint.service',
                'DEPLOYMENT.md','Open-Detector.ps1','config.example.json')

def sha(data):
    return hashlib.sha256(data).hexdigest()

def make_archive(path, files):
    with path.open('wb') as raw, gzip.GzipFile(filename='', fileobj=raw, mode='wb', mtime=0) as gz:
        with tarfile.open(mode='w|', fileobj=gz, format=tarfile.USTAR_FORMAT) as archive:
            for name, data in sorted(files.items()):
                item = tarfile.TarInfo(name)
                item.mode = 0o755 if name.endswith('.sh') or name == 'sub2-fingerprint' else 0o644
                item.size = len(data)
                archive.addfile(item, io.BytesIO(data))

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--go', default='go'); parser.add_argument('--out', type=Path, default=ROOT/'dist')
    args = parser.parse_args(); out = args.out.resolve(); out.mkdir(parents=True,exist_ok=True)
    version = (ROOT/'VERSION').read_text().strip()
    if not re.fullmatch(r'v\d+\.\d+\.\d+',version): raise ValueError('invalid VERSION')
    if os.environ.get('GITHUB_REF_TYPE')=='tag' and os.environ['GITHUB_REF_NAME']!=version:
        raise ValueError('release tag must match VERSION')
    goroot = Path(subprocess.check_output([args.go,'env','GOROOT'],text=True).strip())
    files = {n:(ROOT/'deploy'/n).read_bytes().replace(b'\r\n',b'\n') for n in DEPLOY_FILES}
    files['VERSION'] = (version+'\n').encode()
    for name, source in [('ModelTrace.txt',ROOT/'web/ModelTrace-LICENSE.txt'),
                         ('ModelTrace-sources.txt',ROOT/'web/MODELTRACE-PROVENANCE.txt'),
                         ('Go.txt',goroot/'LICENSE'),('Go-PATENTS.txt',goroot/'PATENTS')]:
        files['LICENSES/'+name]=source.read_bytes().replace(b'\r\n',b'\n')
    checks=[]
    with tempfile.TemporaryDirectory() as scratch:
        binary = Path(scratch)/'sub2-fingerprint'
        for arch in ('amd64','arm64'):
            env=dict(os.environ,GOOS='linux',GOARCH=arch,CGO_ENABLED='0',GOTOOLCHAIN='local')
            subprocess.run([args.go,'build','-trimpath','-buildvcs=false','-ldflags=-s -w -buildid=','-o',str(binary),'.'],cwd=ROOT,env=env,check=True)
            content={**files,'sub2-fingerprint':binary.read_bytes()}
            content['SHA256SUMS']=''.join(sha(data)+'  '+name+'\n' for name,data in sorted(content.items())).encode()
            artifact=out/f'sub2-fingerprint-linux-{arch}.tar.gz'; make_archive(artifact,content)
            checks.append(sha(artifact.read_bytes())+'  '+artifact.name+'\n')
    (out/'SHA256SUMS.txt').write_bytes(''.join(checks).encode())
    print('Built',version,'in',out)

if __name__=='__main__': main()
