#!/usr/bin/env bash
# Prepares an Ubuntu 24.04 box and starts mcpkg. Normally run for you by push.sh;
# works on any Ubuntu VM (AWS, Hetzner, ...). Idempotent: safe to re-run.
#
#   sudo ROLE=core   bash /opt/mcpkg/deploy/aws/bootstrap.sh   # db + web + scheduler + light worker
#   sudo ROLE=worker bash /opt/mcpkg/deploy/aws/bootstrap.sh   # security + smoke worker only
#
# Optional: REPO_URL (git-clone/pull instead of push.sh), WITH_GVISOR=1, HEAVY_ON_CORE=0,
#           DOCKER_VERSION=28.5 (pinned Docker Engine; falls back to the latest if unavailable)
# Prebuilt images: set MCPKG_IMAGE (and for workers MCPKG_PROXY_IMAGE, SANDBOX_IMAGE) in .env to
# pull tagged images from GHCR (built by .github/workflows/release.yml) instead of building here.
set -euo pipefail

ROLE="${ROLE:-core}"
REPO_URL="${REPO_URL:-}"
APP_DIR="${APP_DIR:-/opt/mcpkg}"
WITH_GVISOR="${WITH_GVISOR:-1}"
HEAVY_ON_CORE="${HEAVY_ON_CORE:-0}"
DOCKER_VERSION="${DOCKER_VERSION:-28.5}"
case "$ROLE" in core|worker) ;; *) echo "unknown ROLE=$ROLE" >&2; exit 2 ;; esac

export DEBIAN_FRONTEND=noninteractive
step() { printf '\n\033[1;34m[bootstrap]\033[0m %s\n' "$*"; }

step "packages"
# First-boot user-data may still be running apt/docker installs: wait for it.
if command -v cloud-init >/dev/null; then cloud-init status --wait >/dev/null 2>&1 || true; fi
apt-get update -y -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl git gnupg >/dev/null

if ! command -v docker >/dev/null; then
  step "installing Docker"
  curl -fsSL https://get.docker.com -o /tmp/get-docker.sh
  sh /tmp/get-docker.sh --version "$DOCKER_VERSION" || {
    echo "!! Docker $DOCKER_VERSION not available; installing the latest release" >&2
    sh /tmp/get-docker.sh
  }
fi
systemctl enable --now docker >/dev/null

# gVisor adds a second kernel boundary around untrusted smoke tests (SANDBOX_RUNTIME=runsc).
if [ "$WITH_GVISOR" = "1" ] && { [ "$ROLE" = "worker" ] || [ "$HEAVY_ON_CORE" = "1" ]; } && ! command -v runsc >/dev/null; then
  step "installing gVisor"
  curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor --yes -o /usr/share/keyrings/gvisor-archive-keyring.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" \
    > /etc/apt/sources.list.d/gvisor.list
  apt-get update -y -qq && apt-get install -y -qq runsc >/dev/null
  runsc install
  systemctl restart docker
fi

# Swap: cheap insurance on 2 GB boxes during image builds and npm installs.
if [ ! -f /swapfile ]; then
  fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile >/dev/null && swapon /swapfile
  echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi

if [ -n "$REPO_URL" ]; then
  if [ -d "$APP_DIR/.git" ]; then git -C "$APP_DIR" pull --ff-only; else git clone --depth 1 "$REPO_URL" "$APP_DIR"; fi
fi
[ -f "$APP_DIR/go.mod" ] || { echo "!! no code in $APP_DIR: run deploy/aws/push.sh from CloudShell (or set REPO_URL)" >&2; exit 1; }
cd "$APP_DIR"
[ -f .env ] || { echo "!! $APP_DIR/.env is missing: push.sh copies it, or create it with scripts/make-env.sh" >&2; exit 1; }
chmod 600 .env

imds() { # imds <path>: EC2 instance metadata (IMDSv2); empty off AWS
  local token
  token=$(curl -s -m 2 -X PUT http://169.254.169.254/latest/api/token -H "X-aws-ec2-metadata-token-ttl-seconds: 60" || true)
  [ -n "$token" ] && curl -s -m 2 -H "X-aws-ec2-metadata-token: $token" "http://169.254.169.254/latest/meta-data/$1" || true
}

# Core: expose Postgres on the private IP (security group allows only mcpkg boxes) so workers can connect.
if [ "$ROLE" = "core" ] && ! grep -qE '^DB_BIND=' .env; then
  PRIV="$(imds local-ipv4)"
  if [ -n "$PRIV" ]; then
    echo "DB_BIND=$PRIV" >> .env
    step "Postgres reachable by workers at $PRIV:5432"
  fi
fi

COMPOSE="docker compose -f deploy/docker-compose.yml --env-file .env"
envget() { grep -E "^$1=" .env | tail -1 | cut -d= -f2- || true; }
HEAVY=0; { [ "$ROLE" = "worker" ] || [ "$HEAVY_ON_CORE" = "1" ]; } && HEAVY=1
if [ -n "$(envget MCPKG_IMAGE)" ]; then
  # Prebuilt, tagged images (no Go/apt build on a 2 GB box). Never use :latest in production.
  step "pulling images $(envget MCPKG_IMAGE)"
  if [ "$ROLE" = "core" ]; then $COMPOSE pull; fi
  if [ "$HEAVY" = "1" ]; then
    $COMPOSE --profile heavy pull worker-heavy egress-proxy
    RUNNER="$(envget SANDBOX_IMAGE)"
    if [[ "$RUNNER" == */* ]]; then docker pull -q "$RUNNER" >/dev/null; else docker build -q -f sandbox/runner.Dockerfile -t mcpkg-runner:latest sandbox >/dev/null; fi
  fi
else
  step "building images (first time: several minutes)"
  if [ "$ROLE" = "core" ]; then
    $COMPOSE build
  fi
  if [ "$HEAVY" = "1" ]; then
    $COMPOSE --profile heavy build worker-heavy egress-proxy
    docker build -q -f sandbox/runner.Dockerfile -t mcpkg-runner:latest sandbox >/dev/null
  fi
fi

step "starting $ROLE services"
if [ "$ROLE" = "core" ]; then
  if [ "$HEAVY_ON_CORE" = "1" ]; then $COMPOSE --profile heavy up -d; else $COMPOSE up -d; fi
else
  $COMPOSE --profile heavy up -d --no-deps worker-heavy egress-proxy
fi

# Spot workers: on the 2-minute interruption notice, stop the worker cleanly.
# (Unfinished jobs also return to the queue automatically when their lease expires.)
if [ "$ROLE" = "worker" ]; then
  cat > /usr/local/bin/mcpkg-spot-watch <<'EOF'
#!/usr/bin/env bash
while true; do
  TOKEN=$(curl -s -m 2 -X PUT http://169.254.169.254/latest/api/token -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
  if curl -sf -m 2 -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/spot/instance-action >/dev/null; then
    docker compose -f /opt/mcpkg/deploy/docker-compose.yml --env-file /opt/mcpkg/.env stop -t 90 worker-heavy
    exit 0
  fi
  sleep 5
done
EOF
  chmod +x /usr/local/bin/mcpkg-spot-watch
  cat > /etc/systemd/system/mcpkg-spot-watch.service <<'EOF'
[Unit]
Description=Stop the mcpkg worker cleanly on spot interruption
After=docker.service

[Service]
ExecStart=/usr/local/bin/mcpkg-spot-watch
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now mcpkg-spot-watch >/dev/null
fi

step "status"
$COMPOSE ps --format 'table {{.Service}}\t{{.State}}\t{{.Status}}' 2>/dev/null || $COMPOSE ps
echo
echo "mcpkg $ROLE is up."
