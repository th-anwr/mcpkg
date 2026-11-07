#!/usr/bin/env bash
# Stop paying for boxes you don't need.
#
#   ./deploy/aws/teardown.sh workers   # terminate all spot workers (do this after the first pass)
#   ./deploy/aws/teardown.sh all       # also the core box + its Elastic IP (DELETES the database!)
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
. "$(dirname "$0")/lib.sh"

WHAT="${1:?usage: teardown.sh workers|all}"

ids_for() { # ids_for <Name tag>
  aws_ ec2 describe-instances \
    --filters "Name=tag:app,Values=mcpkg" "Name=tag:Name,Values=$1" "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'Reservations[].Instances[].InstanceId' --output text
}

WORKERS=$(ids_for mcpkg-worker)
if [ -n "$WORKERS" ]; then
  log "terminating workers: $WORKERS"
  # shellcheck disable=SC2086
  aws_ ec2 terminate-instances --instance-ids $WORKERS >/dev/null
else
  log "no running workers"
fi
rm -f "$STATE_DIR/workers.list"

if [ "$WHAT" = "all" ]; then
  echo "This terminates the core box and DELETES its database. Run ./scripts/backup.sh on the box first if you need the data."
  read -r -p "Type DELETE to continue: " answer
  [ "$answer" = "DELETE" ] || die "aborted"
  CORE=$(ids_for mcpkg-core)
  if [ -n "$CORE" ]; then
    # shellcheck disable=SC2086
    aws_ ec2 terminate-instances --instance-ids $CORE >/dev/null
    log "terminating core: $CORE"
  fi
  if [ -f "$STATE_DIR/core.env" ]; then
    load_core
    if [ -n "${CORE_EIP_ALLOC:-}" ]; then
      # shellcheck disable=SC2086
      [ -n "$CORE" ] && aws_ ec2 wait instance-terminated --instance-ids $CORE
      aws_ ec2 release-address --allocation-id "$CORE_EIP_ALLOC" && log "released Elastic IP $CORE_PUBLIC_IP"
    fi
    rm -f "$STATE_DIR/core.env"
  fi
fi
log "done"
