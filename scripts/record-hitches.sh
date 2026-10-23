#!/bin/zsh
# Records the running MacCluster with Instruments' "Animation Hitches" template and lists every dropped frame
# (when, how long, and whether the app or the render server was late). Needs Xcode's command line tools.
#
#   ./scripts/record-hitches.sh [seconds]      (default 8)
#
# While it records, do the slow action once (for example collapse and expand the sidebar). The summary is written to
# ./maccluster-hitches.txt; it has timings and process names only. The full .trace stays in a temporary folder.
set -euo pipefail
SECS="${1:-8}"
HERE="$(cd "$(dirname "$0")" && pwd)"
PID="$(pgrep -f "MacCluster.app/Contents/MacOS/MacCluster" | head -1 || true)"
[[ -n "$PID" ]] || { print -u2 "MacCluster is not running."; exit 1; }
DIR="$(mktemp -d)"
TRACE="$DIR/hitches.trace"
print "Recording MacCluster (pid $PID) for ${SECS}s. After the countdown, collapse and expand the sidebar once."
# xctrace is started in the background and stopped by this script (SIGINT makes it save the trace), so the
# recording ends even where --time-limit does not stop it.
xcrun xctrace record --template "Animation Hitches" --instrument "Time Profiler" --attach "$PID" --time-limit "${SECS}s" --output "$TRACE" --no-prompt >"$DIR/record.log" 2>&1 &
REC=$!
sleep 3; print "NOW"
sleep $(( SECS > 3 ? SECS - 3 : 1 ))
print "Stopping the recording..."
kill -INT "$REC" 2>/dev/null || true
for i in {1..60}; do kill -0 "$REC" 2>/dev/null || break; sleep 1; done
if kill -0 "$REC" 2>/dev/null; then print -u2 "xctrace did not finish saving after 60s; stopping it."; kill -KILL "$REC" 2>/dev/null || true; fi
[[ -d "$TRACE" ]] || { print -u2 "No trace was saved. xctrace said:"; cat "$DIR/record.log" >&2; exit 1; }
print "Reading the trace (this can take a minute)..."
OUT="$PWD/maccluster-hitches.txt"
{
  print "macOS $(sw_vers -productVersion) $(sw_vers -buildVersion)"
  python3 "$HERE/summarize-hitches.py" "$TRACE"
} > "$OUT"
print "Wrote $OUT (trace kept in $TRACE; re-summarize with: python3 $HERE/summarize-hitches.py $TRACE)"
print -- "--- worst hitches:"
sed -n '1,14p' "$OUT" | cut -c1-200
