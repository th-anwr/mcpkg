#!/usr/bin/env bash
# Shared helpers for the AWS scripts. Meant to run in AWS CloudShell (or any shell
# with the AWS CLI v2 configured). State lives in ~/.mcpkg so later commands know
# the core box's IPs; CloudShell keeps $HOME between sessions.
# shellcheck disable=SC2034  # variables are used by the scripts that source this file

REGION="${REGION:-us-east-1}"
KEY_NAME="${KEY_NAME:-mcpkg}"
KEY_FILE="${KEY_FILE:-$HOME/.ssh/$KEY_NAME.pem}"
SG_NAME="${SG_NAME:-mcpkg}"
STATE_DIR="${STATE_DIR:-$HOME/.mcpkg}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SSH_OPTS=(-i "$KEY_FILE" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 -o ServerAliveInterval=30 -o LogLevel=ERROR)

aws_() { aws --region "$REGION" "$@"; }

log() { printf '\033[1;34m==>\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31mxx\033[0m %s\n' "$*" >&2; exit 1; }

# ensure_key creates the EC2 key pair (and the local private key file) once.
ensure_key() {
  if aws_ ec2 describe-key-pairs --key-names "$KEY_NAME" >/dev/null 2>&1; then
    [ -f "$KEY_FILE" ] || die "key pair '$KEY_NAME' exists in AWS but $KEY_FILE is missing here. Use KEY_NAME=<other> or delete the old pair."
    return
  fi
  mkdir -p "$(dirname "$KEY_FILE")"
  log "creating key pair $KEY_NAME -> $KEY_FILE"
  aws_ ec2 create-key-pair --key-name "$KEY_NAME" --key-type ed25519 --query KeyMaterial --output text > "$KEY_FILE"
  chmod 600 "$KEY_FILE"
}

# ensure_sg prints the security group id, creating it on first use:
# HTTP(S) open to the world, Postgres only between mcpkg instances, SSH added per caller IP.
ensure_sg() {
  local id
  id=$(aws_ ec2 describe-security-groups --filters "Name=group-name,Values=$SG_NAME" --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null || true)
  if [ -z "$id" ] || [ "$id" = "None" ]; then
    id=$(aws_ ec2 create-security-group --group-name "$SG_NAME" --description "mcpkg" --query GroupId --output text)
    aws_ ec2 authorize-security-group-ingress --group-id "$id" --protocol tcp --port 80 --cidr 0.0.0.0/0 >/dev/null
    aws_ ec2 authorize-security-group-ingress --group-id "$id" --protocol tcp --port 443 --cidr 0.0.0.0/0 >/dev/null
    aws_ ec2 authorize-security-group-ingress --group-id "$id" --protocol tcp --port 5432 --source-group "$id" >/dev/null
    log "created security group $id"
  fi
  echo "$id"
}

# allow_ssh_from_me opens port 22 for this shell's current public IP (CloudShell's IP can change).
allow_ssh_from_me() {
  local sg="$1" ip
  ip=$(curl -fsS https://checkip.amazonaws.com | tr -d '[:space:]')
  [ -n "$ip" ] || die "could not detect your public IP"
  if aws_ ec2 authorize-security-group-ingress --group-id "$sg" --protocol tcp --port 22 --cidr "$ip/32" >/dev/null 2>&1; then
    log "SSH allowed from $ip"
  fi
}

save_state() { # save_state <file> KEY=VALUE...
  mkdir -p "$STATE_DIR"
  local f="$STATE_DIR/$1"; shift
  : > "$f"
  for kv in "$@"; do echo "$kv" >> "$f"; done
}

load_core() {
  [ -f "$STATE_DIR/core.env" ] || die "no core box recorded in $STATE_DIR/core.env (run launch.sh core first, or set CORE_PUBLIC_IP/CORE_PRIVATE_IP)"
  # shellcheck disable=SC1091
  . "$STATE_DIR/core.env"
}

wait_ssh() { # wait_ssh <host>
  local host="$1" i
  for i in $(seq 1 40); do
    if ssh "${SSH_OPTS[@]}" -o BatchMode=yes "ubuntu@$host" true 2>/dev/null; then
      return 0
    fi
    [ "$i" = 1 ] && log "waiting for SSH on $host (fresh instances take 1-2 minutes)"
    sleep 6
  done
  die "SSH to $host timed out. Check the security group allows your IP: ./deploy/aws/allow-my-ip.sh"
}

# env_value <file> <KEY> prints a value from a dotenv file.
env_value() { grep -E "^$2=" "$1" | tail -n1 | cut -d= -f2-; }

# env_set <file> <KEY> <VALUE> replaces or appends a dotenv line.
env_set() {
  local f="$1" k="$2" v="$3" tmp
  tmp="$(mktemp)"
  grep -vE "^$k=" "$f" > "$tmp" || true
  printf '%s=%s\n' "$k" "$v" >> "$tmp"
  cat "$tmp" > "$f"
  rm -f "$tmp"
}
