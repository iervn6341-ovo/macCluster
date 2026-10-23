#!/usr/bin/env python3
"""Launch a local, agent-free sidebar profiling app. Click its sidebar toggle normally.

Build once with --build, then relaunch for each control (quit the previous profiling app first).
Each click writes a one-second process CPU/main-display-link summary under --output.
The display link measures main-thread cadence, not GPU-presented frames.
"""
import argparse
import json
import os
from pathlib import Path
import plistlib
import re
import shutil
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--build', action='store_true')
    parser.add_argument('--layout', choices=['native', 'swiftui'], default='swiftui')
    parser.add_argument('--surface', choices=['glass', 'material'], default='glass')
    parser.add_argument('--detail', choices=['normal', 'fixed', 'empty'], default='normal')
    parser.add_argument('--page', choices=['empty', 'logs'], default='empty')
    parser.add_argument('--hide-toolbar', action='store_true')
    parser.add_argument('--output', type=Path, required=True, help='A new directory for this run')
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    scratch = root / '.local/sidebar-build'
    env = os.environ.copy()
    env['CLANG_MODULE_CACHE_PATH'] = str(scratch / 'clang')
    env['SWIFTPM_MODULECACHE_OVERRIDE'] = str(scratch / 'swift')
    build = ['xcrun', 'swift', 'build', '--package-path', str(root / 'app'), '--scratch-path', str(scratch),
             '-c', 'release', '--disable-sandbox', '--manifest-cache', 'local', '-debug-info-format', 'none']
    if args.build:
        sdk = subprocess.check_output(['xcrun', '--sdk', 'macosx', '--show-sdk-version'], text=True).strip()
        subprocess.run(build + ['-Xlinker', '-platform_version', '-Xlinker', 'macos', '-Xlinker', '14.0', '-Xlinker', sdk], env=env, check=True)
    binary_dir = Path(subprocess.check_output(build + ['--show-bin-path'], env=env, text=True).strip())
    app = root / '.local/MacCluster Sidebar Profile.app'
    contents = app / 'Contents'
    running = subprocess.run(['pgrep', '-f', '^' + re.escape(str(contents / 'MacOS/MacCluster')) + r'( |$)'],
                             capture_output=True, text=True)
    if running.returncode == 0:
        parser.error('Quit the previous MacCluster Sidebar Profile app before packaging another run.')
    if args.output.exists():
        parser.error('--output must be a new directory for each run')
    (contents / 'MacOS').mkdir(parents=True, exist_ok=True)
    (contents / 'Resources').mkdir(exist_ok=True)
    # No agent is bundled: accidental backend calls fail locally rather than reaching real state.
    executable = contents / 'MacOS/MacCluster'
    shutil.copy2(binary_dir / 'MacCluster', executable)
    for resource in (root / 'app/Resources').glob('KubernetesIcon*'):
        shutil.copy2(resource, contents / 'Resources' / resource.name)
    with (contents / 'Info.plist').open('wb') as file:
        plistlib.dump({'CFBundleExecutable': 'MacCluster', 'CFBundleIdentifier': 'com.maccluster.sidebar-profile',
                      'CFBundleName': 'MacCluster Sidebar Profile', 'CFBundlePackageType': 'APPL',
                      'CFBundleVersion': '1', 'NSHighResolutionCapable': True, 'LSMinimumSystemVersion': '14.0'}, file)
    subprocess.run(['codesign', '--force', '--deep', '--sign', '-', str(app)], check=True)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    env.update({'MACCLUSTER_SIDEBAR_PROFILE': str(output), 'MACCLUSTER_SIDEBAR_LAYOUT': args.layout,
                'MACCLUSTER_SIDEBAR_SURFACE': args.surface, 'MACCLUSTER_SIDEBAR_DETAIL': args.detail,
                'MACCLUSTER_SIDEBAR_PAGE': args.page,
                'MACCLUSTER_SIDEBAR_TOOLBAR': 'hidden' if args.hide_toolbar else 'normal'})
    with (output / 'launch.log').open('w') as log:
        process = subprocess.Popen([str(executable), '--state-dir', str(output / 'state')],
                                   env=env, stdout=log, stderr=log, start_new_session=True)
    (output / 'pid').write_text(str(process.pid))
    print(json.dumps({'app': str(app), 'pid': process.pid, 'output': str(output)}))


if __name__ == '__main__':
    main()
