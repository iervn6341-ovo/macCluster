#!/bin/zsh
# Regenerates app/Resources/AppIcon.icns from app/Resources/AppIcon.svg.
# Needs macOS with a GUI session (qlmanage, swift, iconutil). scripts/build.sh runs it automatically
# when the SVG changed; the generated .icns and .sha256 are committed so CI does not need it.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
qlmanage -t -s 1024 -o "$WORK" "$ROOT/app/Resources/AppIcon.svg" >/dev/null 2>&1
swift "$ROOT/scripts/make-icon.swift" "$WORK/AppIcon.svg.png" "$WORK/AppIcon.iconset"
iconutil -c icns "$WORK/AppIcon.iconset" -o "$ROOT/app/Resources/AppIcon.icns"
# build.sh compares this with the SVG to know when the icns is stale.
shasum -a 256 "$ROOT/app/Resources/AppIcon.svg" | cut -d" " -f1 > "$ROOT/app/Resources/AppIcon.sha256"
print "Wrote $ROOT/app/Resources/AppIcon.icns"
