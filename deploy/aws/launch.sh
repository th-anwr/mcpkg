#!/usr/bin/env bash
# Launch an mcpkg instance. Run in AWS CloudShell from the repo root.
#
#   ./deploy/aws/launch.sh core     # on-demand t4g.small + Elastic IP, always on
#   ./deploy/aws/launch.sh worker   # SPOT c7a.xlarge for security + smoke passes
#
# Optional env: REGION (us-east-1), KEY_NAME (mcpkg), CORE_TYPE, WORKER_TYPE,
# REPO_URL (only if you want instances to git-clone instead of using push.sh).
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
. "$(dirname "$0")/lib.sh"

ROLE="${1:?usage: launch.sh core|worker}"
CORE_TYPE="${CORE_TYPE:-t4g.small}"       # ARM, 2 GB: Postgres + web + scheduler + light worker
WORKER_TYPE="${WORKER_TYPE:-c7a.xlarge}"  # x86, 4 vCPU / 8 GB: sandboxed smoke tests
REPO_URL="${REPO_URL:-}"

case "$ROLE" in
  core)
    ARCH=arm64; TYPE=$CORE_TYPE; DISK=30; MARKET=()
    if [ -f "$STATE_DIR/core.env" ]; then
      load_core
      if aws_ ec2 describe-instances --instance-ids "$CORE_ID" --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null | grep -qE 'running|pending|stopped'; then
        die "a core box already exists ($CORE_ID, $CORE_PUBLIC_IP). Terminate it first: ./deploy/aws/teardown.sh all"
      fi
    fi
    ;;
  worker)
    ARCH=amd64; TYPE=$WORKER_TYPE; DISK=40
    if [ "${SPOT:-1}" = "1" ]; then
      MARKET=(--instance-market-options 'MarketType=spot,SpotOptions={SpotInstanceType=one-time,InstanceInterruptionBehavior=terminate}')
    else
      MARKET=() # SPOT=0: on-demand (about 2-3x the price; use if spot is unavailable on your account)
    fi
    ;;
  *) die "usage: launch.sh core|worker" ;;
esac

ensure_key
SG_ID=$(ensure_sg)
allow_ssh_from_me "$SG_ID"
AMI=$(aws_ ssm get-parameter --name "/aws/service/canonical/ubuntu/server/24.04/stable/current/${ARCH}/hvm/ebs-gp3/ami-id" --query Parameter.Value --output text)

# First boot: install Docker right away so push.sh has less to do. Secrets never go in user-data.
if [ -n "$REPO_URL" ]; then
  FETCH="git clone --depth 1 $REPO_URL /opt/mcpkg || true"
else
  FETCH="true"
fi
USER_DATA=$(cat <<EOF
#!/bin/bash
export DEBIAN_FRONTEND=noninteractive
apt-get update -y && apt-get install -y git curl ca-certificates
curl -fsSL https://get.docker.com -o /tmp/get-docker.sh && { sh /tmp/get-docker.sh --version "${DOCKER_VERSION:-28.5}" || sh /tmp/get-docker.sh; }
$FETCH
EOF
)

log "launching $ROLE ($TYPE, $ARCH) in $REGION"
ID=$(aws_ ec2 run-instances \
  --image-id "$AMI" --instance-type "$TYPE" --key-name "$KEY_NAME" \
  --security-group-ids "$SG_ID" "${MARKET[@]}" \
  --block-device-mappings "DeviceName=/dev/sda1,Ebs={VolumeSize=$DISK,VolumeType=gp3,DeleteOnTermination=true}" \
  --metadata-options "HttpTokens=required,HttpPutResponseHopLimit=1,HttpEndpoint=enabled" \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=mcpkg-$ROLE},{Key=app,Value=mcpkg}]" \
  --user-data "$USER_DATA" \
  --query 'Instances[0].InstanceId' --output text)
# HttpPutResponseHopLimit=1: containers cannot reach the instance metadata service.
aws_ ec2 wait instance-running --instance-ids "$ID"

PRIVATE_IP=$(aws_ ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)
if [ "$ROLE" = "core" ]; then
  # Elastic IP: the address survives stop/start, so DNS and SITE_URL stay valid. Best effort.
  ALLOC=""
  if ALLOC=$(aws_ ec2 allocate-address --domain vpc --tag-specifications "ResourceType=elastic-ip,Tags=[{Key=app,Value=mcpkg}]" --query AllocationId --output text 2>/dev/null) \
     && aws_ ec2 associate-address --instance-id "$ID" --allocation-id "$ALLOC" >/dev/null; then
    PUBLIC_IP=$(aws_ ec2 describe-addresses --allocation-ids "$ALLOC" --query 'Addresses[0].PublicIp' --output text)
  else
    log "could not attach an Elastic IP; using the instance's public IP (it changes if the box is stopped)"
    PUBLIC_IP=$(aws_ ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
  fi
  save_state core.env "CORE_ID=$ID" "CORE_PUBLIC_IP=$PUBLIC_IP" "CORE_PRIVATE_IP=$PRIVATE_IP" "CORE_EIP_ALLOC=$ALLOC"
else
  PUBLIC_IP=$(aws_ ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
  mkdir -p "$STATE_DIR"
  echo "$ID $PUBLIC_IP $PRIVATE_IP" >> "$STATE_DIR/workers.list"
fi

log "$ROLE is running: $ID  public=$PUBLIC_IP  private=$PRIVATE_IP"
echo
echo "Next:  ./deploy/aws/push.sh $ROLE $PUBLIC_IP"
