#!/usr/bin/env bash
# Run SQL against the catalog database:  sudo /opt/mcpkg/scripts/sql.sh "select count(*) from tools"
set -euo pipefail
cd "$(dirname "$0")/.."
exec docker compose -f deploy/docker-compose.yml --env-file .env exec -T db psql -U mcpkg -d mcpkg -c "$1"
