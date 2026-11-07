#!/usr/bin/env bash
# docker compose with the right file and .env:  sudo /opt/mcpkg/scripts/compose.sh logs --tail 50 web
set -euo pipefail
cd "$(dirname "$0")/.."
exec docker compose -f deploy/docker-compose.yml --env-file .env "$@"
