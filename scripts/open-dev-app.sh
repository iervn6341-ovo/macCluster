#!/bin/zsh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
open "$ROOT/dist/MacCluster.app" --args --state-dir "$ROOT/.local/agent"
