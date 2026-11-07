#!/usr/bin/env bash
# Nightly Postgres backup. Keep these: the AWS free plan closes the account when
# credits run out or after 6 months, and backups make moving hosts a 10-minute job.
#
#   ./scripts/backup.sh                  # local file in ./backups
#   S3_BUCKET=my-bucket ./scripts/backup.sh
# cron:  15 3 * * *  cd /opt/mcpkg && ./scripts/backup.sh >> /var/log/mcpkg-backup.log 2>&1
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p backups
FILE="backups/mcpkg-$(date -u +%Y%m%dT%H%M%SZ).sql.gz"
docker compose -f deploy/docker-compose.yml --env-file .env exec -T db pg_dump -U mcpkg -d mcpkg --no-owner | gzip -9 > "$FILE"
echo "wrote $FILE ($(du -h "$FILE" | cut -f1))"
if [ -n "${S3_BUCKET:-}" ]; then
  aws s3 cp "$FILE" "s3://$S3_BUCKET/mcpkg/$(basename "$FILE")" --storage-class STANDARD_IA
fi
# keep the last 14 local backups
find backups -name 'mcpkg-*.sql.gz' -type f | sort | head -n -14 | xargs -r rm -f
# restore: gunzip -c FILE | docker compose -f deploy/docker-compose.yml --env-file .env exec -T db psql -U mcpkg -d mcpkg
