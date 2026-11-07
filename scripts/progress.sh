#!/usr/bin/env bash
# Live numbers every 60 seconds (Ctrl-C to stop watching; nothing stops on the server).
#   sudo /opt/mcpkg/scripts/progress.sh
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
while true; do
  [ -t 1 ] && clear
  date -u '+%Y-%m-%d %H:%M UTC'
  ./scripts/mcpkg.sh stats
  sleep 60
done
