#!/usr/bin/env bash
# SSH into a box (or run one command there).
#
#   ./deploy/aws/ssh.sh core
#   ./deploy/aws/ssh.sh core sudo /opt/mcpkg/scripts/mcpkg.sh stats
#   ./deploy/aws/ssh.sh 3.91.12.34 sudo docker ps
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
. "$(dirname "$0")/lib.sh"

TARGET="${1:?usage: ssh.sh core|<ip> [command...]}"
shift
if [ "$TARGET" = "core" ]; then
  load_core
  TARGET="$CORE_PUBLIC_IP"
fi
exec ssh "${SSH_OPTS[@]}" "ubuntu@$TARGET" "$@"
