#!/usr/bin/env bash
# Copy this repo + .env to an instance and (re)start it. No GitHub repo needed.
# Run in AWS CloudShell from the repo root. Safe to re-run after code changes.
#
#   ./deploy/aws/push.sh core [ip]      # ip defaults to the recorded core box
#   ./deploy/aws/push.sh worker <ip>    # worker gets DATABASE_URL pointed at the core's private IP
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
. "$(dirname "$0")/lib.sh"

ROLE="${1:?usage: push.sh core|worker [ip]}"
HOST="${2:-}"
cd "$REPO_ROOT"
[ -f .env ] || die ".env not found in $REPO_ROOT (run ./scripts/make-env.sh first)"

# Deploy gate (docs/BENCH_GATE.md): only commits whose CI passed on GitHub.
require_green_ci
require_lf_scripts
IMG="$(env_value .env MCPKG_IMAGE || true)"
if [ -n "$IMG" ] && [ -n "${GATE_SHA:-}" ] && [ "${IMG##*:}" != "$GATE_SHA" ] && [ "${IMG##*:}" != "sha-${GATE_SHA:0:12}" ]; then
  log "warning: .env runs image tag ${IMG##*:} but GATE_SHA is $GATE_SHA (set MCPKG_IMAGE, MCPKG_PROXY_IMAGE, SANDBOX_IMAGE to the same tag)"
fi

if [ "$ROLE" = "core" ] && [ -z "$HOST" ]; then
  load_core
  HOST="$CORE_PUBLIC_IP"
fi
[ -n "$HOST" ] || die "usage: push.sh $ROLE <public-ip>"

SG_ID=$(ensure_sg)
allow_ssh_from_me "$SG_ID"
wait_ssh "$HOST"

# Per-role .env: copy, then adjust.
ENV_TMP="$(mktemp)"
trap 'rm -f "$ENV_TMP"' EXIT
cp .env "$ENV_TMP"
case "$ROLE" in
  core)
    if [ "$(env_value "$ENV_TMP" SITE_URL)" = "auto" ]; then
      env_set "$ENV_TMP" SITE_URL "http://$HOST"
      log "SITE_URL set to http://$HOST (change it in .env when your domain is ready)"
    fi
    ;;
  worker)
    load_core
    DBURL="$(env_value "$ENV_TMP" DATABASE_URL)"
    env_set "$ENV_TMP" DATABASE_URL "${DBURL/@db:5432/@$CORE_PRIVATE_IP:5432}"
    env_set "$ENV_TMP" WORKER_ID "worker-${HOST//./-}"
    ;;
  *) die "usage: push.sh core|worker [ip]" ;;
esac

log "uploading code to $HOST:/opt/mcpkg"
tar --exclude=./.env --exclude=./.git --exclude=./bin --exclude=./backups -czf - . \
  | ssh "${SSH_OPTS[@]}" "ubuntu@$HOST" 'sudo mkdir -p /opt/mcpkg && sudo tar -xzf - -C /opt/mcpkg'
scp "${SSH_OPTS[@]}" -q "$ENV_TMP" "ubuntu@$HOST:/tmp/mcpkg.env"
ssh "${SSH_OPTS[@]}" "ubuntu@$HOST" 'sudo install -m 600 /tmp/mcpkg.env /opt/mcpkg/.env && rm -f /tmp/mcpkg.env'

log "bootstrapping $ROLE on $HOST (first run builds images: ~5-10 min)"
ssh "${SSH_OPTS[@]}" "ubuntu@$HOST" "sudo ROLE=$ROLE bash /opt/mcpkg/deploy/aws/bootstrap.sh"

if [ "$ROLE" = "core" ]; then
  echo
  echo "Site:   $(env_value "$ENV_TMP" SITE_URL)"
  echo "Health: curl -fsS http://$HOST/healthz"
  echo "Status: ./deploy/aws/ssh.sh core sudo /opt/mcpkg/scripts/status.sh   (discovery starts by itself)"
fi
