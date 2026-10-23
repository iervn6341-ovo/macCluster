#!/bin/zsh
# Samples the running MacCluster for a few seconds so a stall (for example the sidebar hitching when it is shown
# again) can be located. While it counts down, do the slow action once. The output has function names and thread
# stacks only; no service names or addresses.
#
#   ./scripts/profile-hitch.sh [seconds]      (default 6)
set -euo pipefail
SECONDS_TO_SAMPLE="${1:-6}"
PID="$(pgrep -f "MacCluster.app/Contents/MacOS/MacCluster" | head -1 || true)"
[[ -n "$PID" ]] || { print -u2 "MacCluster is not running."; exit 1; }
OUT="$PWD/maccluster-hitch-sample.txt"
print "Sampling MacCluster (pid $PID) for ${SECONDS_TO_SAMPLE}s. Starting in 3 seconds; then click the sidebar toggle ONCE (collapse, wait, expand)."
for i in 3 2 1; do print "$i..."; sleep 1; done
print "NOW"
sample "$PID" "$SECONDS_TO_SAMPLE" 1 -file "$OUT" >/dev/null
print "Wrote $OUT"
print -- "--- main thread, heaviest first:"
awk '/Thread_.*main thread|com.apple.main-thread/{f=1} f&&NR>0{print} /^$/{if(f)exit}' "$OUT" | head -40
