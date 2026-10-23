#!/bin/zsh
# Writes the window/view structure of a running MacCluster to a text file, so a layout problem on another
# Mac (for example a different macOS release) can be diagnosed without screenshots or screen-recording access.
# It contains class names, frames, OS version and CPU type only; no service names, no addresses.
#
#   ./scripts/dump-ui.sh [/path/to/MacCluster.app]
set -euo pipefail
APP="${1:-$(cd "$(dirname "$0")/.." && pwd)/dist/MacCluster.app}"
[[ -d "$APP" ]] || { print -u2 "No app at $APP"; exit 1; }
OUT="$(mktemp -d)"
pkill -f "MacCluster.app/Contents/MacOS/MacCluster" 2>/dev/null || true
sleep 1
open -n "$APP" --env MACCLUSTER_DEV_SNAPSHOT_DIR="$OUT"
sleep 5
PID="$(pgrep -f "MacCluster.app/Contents/MacOS/MacCluster" | head -1)"
print "MacCluster is running (pid $PID). Open the screen that looks wrong (for example the settings pop-up"
print "and its second tab), then come back here and press Return."
read -r
kill -USR2 "$PID"
sleep 2
cp "$OUT/layers.txt" "$PWD/maccluster-ui-structure.txt"
print "Wrote $PWD/maccluster-ui-structure.txt"
print "--- first lines:"; head -3 "$PWD/maccluster-ui-structure.txt"
print "--- checks:"; grep "^CHECK" "$PWD/maccluster-ui-structure.txt" || print "(no tab view on screen)"
