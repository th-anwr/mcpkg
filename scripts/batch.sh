#!/usr/bin/env bash
# Push tools into a stage in batches (e.g. re-run smoke tests for the 2,000 most-starred tools).
#
#   ./scripts/batch.sh health -all                      # everything, starting at the first stage
#   ./scripts/batch.sh smoke -min-stars 100 -limit 2000 # heavy re-check of popular tools
#   ./scripts/batch.sh enrich -never-checked             # after switching LLM model
set -euo pipefail
cd "$(dirname "$0")/.."
STAGE="${1:?usage: batch.sh <stage> [enqueue filters]}"
shift
docker compose -f deploy/docker-compose.yml --env-file .env run --rm --no-deps web enqueue -stage "$STAGE" "$@"
