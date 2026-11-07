#!/usr/bin/env bash
# Local development: Postgres in Docker, the app on your machine, smoke tests in
# "process" mode (NOT isolated: only for trying things out on servers you trust).
#
#   ./scripts/dev.sh            # start db, migrate, small discovery, web on :8080
#   ./scripts/dev.sh worker     # in a second terminal: run the pipeline
set -euo pipefail
cd "$(dirname "$0")/.."

export DATABASE_URL="${DATABASE_URL:-postgres://postgres:postgres@127.0.0.1:5433/mcpkg?sslmode=disable}"
export SANDBOX="${SANDBOX:-process}"
export SITE_URL="${SITE_URL:-http://localhost:8080}"
export LOG_LEVEL="${LOG_LEVEL:-info}"

if [ "${1:-}" = "worker" ]; then
  exec go run ./cmd/mcpkg worker
fi

if ! docker ps --format '{{.Names}}' | grep -q '^mcpkg-dev-db$'; then
  docker run -d --name mcpkg-dev-db -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=mcpkg -p 5433:5432 postgres:16-alpine >/dev/null
  echo "waiting for postgres…"
  until docker exec mcpkg-dev-db pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
fi

go run ./cmd/mcpkg migrate
# A small, fast first crawl. Drop -limit for everything.
go run ./cmd/mcpkg discover -source registry -limit 30
go run ./cmd/mcpkg discover -source npm -limit 30
echo "open $SITE_URL  (run './scripts/dev.sh worker' in another terminal to fill in checks)"
exec go run ./cmd/mcpkg serve
