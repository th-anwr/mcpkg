#!/usr/bin/env bash
# One-shot status on the core box:  sudo /opt/mcpkg/scripts/status.sh
# Discovery needs no command: the scheduler crawls every source as soon as the box is up
# (registry hourly, npm/PyPI/GitHub daily, crates weekly) and queues new tools for checks.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
C="docker compose -f deploy/docker-compose.yml --env-file .env"
echo "== services"
$C ps --format 'table {{.Service}}\t{{.State}}\t{{.Status}}' 2>/dev/null || $C ps
echo
echo "== recent discovery (scheduler)"
$C logs --no-log-prefix --since 24h scheduler 2>/dev/null | grep -E "discovery|failed|error" | tail -n 12
echo
echo "== recent problems (workers)"
$C logs --no-log-prefix --since 1h worker-light worker-heavy 2>/dev/null | grep -E "level=(WARN|ERROR)" | tail -n 8
echo
echo "== catalog and queue"
./scripts/mcpkg.sh stats
