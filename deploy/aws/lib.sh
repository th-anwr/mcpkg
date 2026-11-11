#!/usr/bin/env bash
# Shared helpers for the AWS scripts. Meant to run in AWS CloudShell (or any shell
# with the AWS CLI v2 configured). State lives in ~/.mcpkg so later commands know
# the core box's IPs; CloudShell keeps $HOME between sessions.
# shellcheck disable=SC2034  # variables are used by the scripts that source this file

# Git Bash on Windows rewrites arguments that look like paths ("/aws/service/...",
# "DeviceName=/dev/sda1") before they reach the native aws.exe. Linux ignores this.
export MSYS_NO_PATHCONV=1

REGION="${REGION:-us-east-1}"
KEY_NAME="${KEY_NAME:-mcpkg}"
KEY_FILE="${KEY_FILE:-$HOME/.ssh/$KEY_NAME.pem}"
SG_NAME="${SG_NAME:-mcpkg}"
STATE_DIR="${STATE_DIR:-$HOME/.mcpkg}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# UpdateHostKeys=no: parallel pushes (two workers at once) otherwise race to
# rewrite ~/.ssh/known_hosts. New hosts are still appended (accept-new).
SSH_OPTS=(-i "$KEY_FILE" -o StrictHostKeyChecking=accept-new -o UpdateHostKeys=no -o ConnectTimeout=10 -o ServerAliveInterval=30 -o LogLevel=ERROR)

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
  # tr: the Windows AWS CLI prints CRLF line endings, which ssh rejects as "invalid format".
  aws_ ec2 create-key-pair --key-name "$KEY_NAME" --key-type ed25519 --query KeyMaterial --output text | tr -d '\r' > "$KEY_FILE"
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

# require_green_ci is the deploy gate (docs/BENCH_GATE.md). push.sh deploys a
# commit only when GitHub shows its CI jobs passed (GATE_CHECKS, default
# go,docker,bench-gate). It reads the public check-runs API, so CI never has
# to commit anything back to main and branch protection keeps working.
#   GATE_SHA=v0.1.0 ./deploy/aws/push.sh core        # tag, branch or full sha
#   GATE_OVERRIDE="why" ...                           # break-glass, printed loudly
#   GATE_TOKEN=<read-only PAT> ...                    # only for a private repo
require_green_ci() {
  local ref="${GATE_SHA:-}" repo="${GATE_REPO:-}" checks="${GATE_CHECKS:-go,docker,bench-gate}" sha json
  if [ -n "${GATE_OVERRIDE:-}" ]; then
    printf '\033[1;31m!!\033[0m DEPLOY GATE OVERRIDDEN: %s\n' "$GATE_OVERRIDE" >&2
    return 0
  fi
  [ -n "$ref" ] || die "set GATE_SHA=<tag or commit>: push.sh only deploys commits whose CI passed (docs/DEPLOY.md)"
  sha="$ref"
  if command -v git >/dev/null 2>&1 && git -C "$REPO_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
    # push.sh uploads the working tree, so it must be exactly the commit CI tested.
    sha="$(git -C "$REPO_ROOT" rev-parse --verify -q "$ref^{commit}")" \
      || die "GATE_SHA=$ref is not a commit in this checkout (git fetch --tags origin)"
    [ "$(git -C "$REPO_ROOT" rev-parse HEAD)" = "$sha" ] \
      || die "this checkout is not at $ref: run 'git checkout $ref' first"
    [ -z "$(git -C "$REPO_ROOT" status --porcelain)" ] \
      || die "uncommitted changes in the checkout: commit and push them so CI tests what you deploy"
    if [ -z "$repo" ]; then
      repo="$(git -C "$REPO_ROOT" config --get remote.origin.url 2>/dev/null \
        | sed -E 's#^(https://github\.com/|git@github\.com:)##; s#\.git$##')"
    fi
  fi
  [[ "$repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || repo="th-anwr/mcpkg" # not a GitHub remote
  local hdr=(-H "Accept: application/vnd.github+json")
  [ -n "${GATE_TOKEN:-}" ] && hdr+=(-H "Authorization: Bearer $GATE_TOKEN")
  json="$(curl -fsS -m 20 "${hdr[@]}" "https://api.github.com/repos/$repo/commits/$sha/check-runs?per_page=100")" \
    || die "could not read CI results for $ref from GitHub ($repo). Private repo? set GATE_TOKEN"
  local py=python3
  "$py" -c '' 2>/dev/null || py=python # on Windows, python3 is often the Microsoft Store alias
  printf '%s' "$json" | "$py" -c '
import json, sys
ref, need = sys.argv[1], [c for c in sys.argv[2].split(",") if c]
latest = {}
for r in json.load(sys.stdin).get("check_runs", []):
    n = r.get("name")
    if r.get("conclusion") == "skipped":
        continue  # the weekly scheduled run skips bench-gate; that must not hide the push result
    if n in need and (n not in latest or r.get("id", 0) > latest[n].get("id", 0)):
        latest[n] = r  # newest run of each job (re-runs get higher ids)
bad = []
for n in need:
    r = latest.get(n)
    if r is None:
        bad.append(n + ": never ran")
    elif r.get("status") != "completed":
        bad.append(n + ": still " + str(r.get("status")))
    elif r.get("conclusion") != "success":
        bad.append(n + ": " + str(r.get("conclusion")))
if bad:
    sys.exit("deploy gate RED for %s: %s" % (ref, "; ".join(bad)))
print("deploy gate green for %s: %s" % (ref, ", ".join(need)))
' "$ref" "$checks" || die "deploy blocked: fix CI for $ref first (or GATE_OVERRIDE=<reason>)"
}

# require_lf_scripts stops a deploy whose scripts have Windows (CRLF) line
# endings. push.sh uploads the working tree as is, and bash on the box fails on
# "\r". A clone made with core.autocrlf=true looks clean to git but has them;
# .gitattributes prevents that for new clones, this catches older ones.
require_lf_scripts() {
  command -v git >/dev/null 2>&1 && git -C "$REPO_ROOT" rev-parse --git-dir >/dev/null 2>&1 || return 0
  local bad
  bad="$(git -C "$REPO_ROOT" ls-files --eol -- '*.sh' '*.conf' '*.yml' 'deploy/Caddyfile' | awk '$2 ~ /^w\/(crlf|mixed)$/ {print $NF}')"
  [ -z "$bad" ] || die "Windows line endings in: $(echo "$bad" | tr '\n' ' ')- re-clone with: git clone -c core.autocrlf=false <repo>"
}
