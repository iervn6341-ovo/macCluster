#!/usr/bin/env python3
"""Offline build preflight regressions; run on a Mac with compatible Xcode installed."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

SOURCE = Path(__file__).resolve().parent.parent

def run(root, args, env=None):
    settings = os.environ.copy()
    for key in ('MACCLUSTER_GO', 'GOOS', 'GOARCH', 'GOROOT', 'GOTOOLCHAIN'):
        settings.pop(key, None)
    settings['PATH'] = '/usr/bin:/bin:/usr/sbin:/sbin'
    settings.update(env or {})
    return subprocess.run(['/bin/zsh', str(root / 'scripts/build.sh'), *args],
                          env=settings, text=True, capture_output=True, timeout=60)

with tempfile.TemporaryDirectory(prefix='maccluster-build-test-') as directory:
    root = Path(directory)
    (root / 'scripts').mkdir()
    shutil.copy2(SOURCE / 'scripts/build.sh', root / 'scripts/build.sh')
    shutil.copy2(SOURCE / 'go.mod', root / 'go.mod')
    for args in (['--help'], ['-h']):
        result = run(root, args)
        assert result.returncode == 0 and 'Usage:' in result.stdout, result.stderr
    assert not (root / '.tools').exists(), 'Help must not prepare or download tools'
    assert run(root, ['--unknown']).returncode != 0
    result = run(root, [])
    assert result.returncode != 0 and 'app/Package.swift is missing' in result.stderr
    assert not (root / '.tools').exists(), 'Missing sources must fail before preparing tools'
    result = run(root, ['--check'], {'MACCLUSTER_GO': '/nonexistent/go'})
    assert result.returncode != 0 and 'MACCLUSTER_GO' in result.stderr, result.stderr
    result = run(root, ['--check', '--no-repair'])
    assert result.returncode != 0 and 'Go ' in result.stderr, result.stderr
    assert not (root / '.tools/go').exists()

    # Fake only HTTPS download responses; extraction/execution must never follow a bad hash.
    fake_bin = root / 'fake-bin'
    fake_bin.mkdir()
    fake_curl = fake_bin / 'curl'
    fake_curl.write_text('''#!/bin/zsh
set -eu
out=''
for ((i=1; i<=$#; i++)); do
  if [[ "${argv[$i]}" == -o ]]; then out="${argv[$((i+1))]}"; fi
done
if [[ "$out" == */releases.json ]]; then
  arch=arm64
  [[ "$(uname -m)" == x86_64 ]] && arch=amd64
  print -r -- '[{"version":"go1.99.0","stable":true,"files":[{"os":"darwin","arch":"'"$arch"'","kind":"archive","filename":"go1.99.0.darwin-'"$arch"'.tar.gz","sha256":"0000000000000000000000000000000000000000000000000000000000000000"}]}]' > "$out"
else
  print 'tampered download' > "$out"
fi
''')
    fake_curl.chmod(0o755)
    result = run(root, ['--check'], {'PATH': str(fake_bin) + ':/usr/bin:/bin:/usr/sbin:/sbin'})
    assert result.returncode != 0 and 'checksum mismatch' in result.stderr, result.stderr
    assert not (root / '.tools/go').exists(), 'Bad checksum must not install a toolchain'
    assert not list((root / '.tools').glob('go-download.*')), 'Temporary download must be cleaned'
    # Exercise the real cleanup/preflight path, stopping at a fake Swift compiler.
    # No network, real compilation, signing, or user cache deletion is involved.
    fake_go = fake_bin / 'go'
    fake_go.write_text('''#!/bin/zsh
set -eu
case "$1" in
  version) print 'go version go1.99.0 darwin/arm64' ;;
  env)
    if [[ "$2" == GOOS ]]; then print darwin
    elif [[ "$(uname -m)" == arm64 ]]; then print arm64
    else print amd64; fi ;;
  build)
    for ((i=1; i<=$#; i++)); do
      if [[ "${argv[$i]}" == -o ]]; then print 'fake agent' > "${argv[$((i+1))]}"; fi
    done ;;
  *) exit 91 ;;
esac
''')
    fake_go.chmod(0o755)
    fake_xcrun = fake_bin / 'xcrun'
    fake_xcrun.write_text('''#!/bin/zsh
set -eu
case "$1:$2" in
  --sdk:macosx) print 27.0 ;;
  --find:*) print "/usr/bin/$2" ;;
  swift:--version) print 'Apple Swift version 6.4' ;;
  swift:build)
    print -rl -- "$@" > "${0:h:h}/swift-args.txt"
    print -u2 'stopped at fake Swift compiler'
    exit 73 ;;
  *) exit 92 ;;
esac
''')
    fake_xcrun.chmod(0o755)
    for name in ['app/Package.swift', 'app/Sources/MacCluster/MacCluster.swift',
                 'app/Resources/AppIcon.svg', 'app/Resources/KubernetesIconWhite.png',
                 'app/Resources/KubernetesIconWhite.source.txt']:
        target = root / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text('fixture')
    retained = ['.tools/gomod/keep', '.tools/gocache/keep', '.tools/gopath/keep',
                '.tools/go/keep', '.local/user-data/keep']
    for name in retained:
        target = root / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text('retain this')
    ui_caches = ['app/.build', '.local/swift-build', '.local/sidebar-build']
    build_env = {'PATH': str(fake_bin) + ':/usr/bin:/bin:/usr/sbin:/sbin',
                 'MACCLUSTER_GO': str(fake_go)}
    for iteration in range(2):
        for cache in ui_caches:
            directory = root / cache
            directory.mkdir(parents=True, exist_ok=True)
            (directory / 'stale-ui').write_text('stale')
        # Same-root caches must also be removed (the old policy kept them).
        (root / 'app/.build/.maccluster-root').write_text(str(root) + '\n')
        result = run(root, ['--check', '--no-repair'], build_env)
        assert result.returncode == 0, result.stderr
        assert all((root / c / 'stale-ui').exists() for c in ui_caches), '--check deleted UI caches'
        result = run(root, ['--no-repair'], build_env)
        assert result.returncode == 73 and 'fake Swift compiler' in result.stderr, result.stderr
        assert all(not (root / c / 'stale-ui').exists() for c in ui_caches), 'UI cache retained'
        assert all((root / name).read_text() == 'retain this' for name in retained), 'Go/user data deleted'
        args = (root / 'swift-args.txt').read_text().splitlines()
        for flag, value in [('--scratch-path', str(root / 'app/.build')),
                            ('--cache-path', str(root / 'app/.build/swiftpm-cache')),
                            ('--manifest-cache', 'none')]:
            assert args[args.index(flag) + 1] == value, args
        assert '--disable-dependency-cache' in args, args
        assert not (root / 'dist/maccluster-tools').exists(), 'Internal tool exposed as standalone artifact'
        assert not list((root / 'dist').glob('.maccluster-tools.*')), 'Failed build left a temporary tool'
print('Build tests passed: preflight, checksum cleanup, UI cache purge on every build, Go cache retention, and --check preservation.')
