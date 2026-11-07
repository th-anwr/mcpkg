#!/usr/bin/env bash
# Run any mcpkg command on a deployed box, inside the app image:
#   sudo /opt/mcpkg/scripts/mcpkg.sh stats
#   sudo /opt/mcpkg/scripts/mcpkg.sh enqueue -stage smoke -min-stars 100
#   sudo /opt/mcpkg/scripts/mcpkg.sh add https://github.com/owner/repo
set -euo pipefail
cd "$(dirname "$0")/.."
exec docker compose -f deploy/docker-compose.yml --env-file .env run --rm --no-deps web "$@"
