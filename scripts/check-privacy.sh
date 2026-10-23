#!/bin/bash
# Scan tracked and untracked, non-ignored project files. Binary screenshots need
# a separate visual review; this check must never print matched secrets.
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - <<'PY'
import pathlib
import re
import subprocess
import sys

root = pathlib.Path('.')
if (root / '.git').exists():
    paths = subprocess.check_output([
        'git', 'ls-files', '--cached', '--others', '--exclude-standard', '-z'
    ]).decode().split('\0')
else:
    excluded = {'.git', '.local', '.tools', '.build', 'dist', 'Launchbit', '__pycache__'}
    paths = [str(p) for p in root.rglob('*') if p.is_file() and not excluded.intersection(p.parts)]

patterns = {
    'home-directory path': r'/Users/[A-Za-z0-9_.-]+|/home/[a-z][a-z0-9_-]+/',
    'personal identifier': r'\bimac\b|maccluster-imac|iervn6341|arsf32303|hoz30',
    'private network address': r'\b(?:10\.[0-9]{1,3}|192\.168|172\.(?:1[6-9]|2[0-9]|3[01]))\.[0-9]{1,3}\.[0-9]{1,3}\b',
    'email address': r'[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}',
    'credential material': r'BEGIN [A-Z ]*(?:PRIVATE KEY|CERTIFICATE)|client-(?:certificate|key)-data|certificate-authority-data|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{20,}|xox[bp]-[A-Za-z0-9-]+',
    'bootstrap token': r'\b[a-z0-9]{6}\.[a-z0-9]{16}\b',
}
# These are actual software defaults, not deployment addresses. Exempt only
# these complete CIDRs in the implementation and its explanatory document.
default_cidrs = {'cmd/maccluster-agent/egress.go', 'docs/egress-design.md'}
# Deliberately invalid/demo tokens used by parser tests and examples.
demo_tokens = {'abcdef.0123456789abcdef'}
failures = []
count = 0
for name in sorted(set(paths)):
    if not name or name == 'scripts/check-privacy.sh':
        continue
    path = root / name
    if not path.is_file() or path.is_symlink():
        continue
    raw = path.read_bytes()
    if b'\0' in raw:
        continue
    try:
        text = raw.decode('utf-8')
    except UnicodeDecodeError:
        continue
    count += 1
    for lineno, line in enumerate(text.splitlines(), 1):
        for label, pattern in patterns.items():
            candidate = line
            if label == 'private network address' and name in default_cidrs:
                candidate = re.sub(r'(?<![\d.])(?:10\.96\.0\.0/12|10\.244\.0\.0/16)(?!\d)', '<default-cidr>', candidate)
            if label == 'bootstrap token':
                for token in demo_tokens:
                    candidate = candidate.replace(token, '<demo-token>')
            if re.search(pattern, candidate):
                failures.append(f'{name}:{lineno}: {label} (value redacted)')
if failures:
    print('\n'.join(failures))
    print('Privacy check failed. Replace identifying values with documented placeholders.', file=sys.stderr)
    sys.exit(1)
print(f'Privacy check passed ({count} text files, including untracked files).')
print('Binary images are not scanned; review screenshots separately before publishing.')
PY
