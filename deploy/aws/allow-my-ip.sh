#!/usr/bin/env bash
# CloudShell's public IP can change between sessions. Run this if SSH times out.
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
. "$(dirname "$0")/lib.sh"
allow_ssh_from_me "$(ensure_sg)"
