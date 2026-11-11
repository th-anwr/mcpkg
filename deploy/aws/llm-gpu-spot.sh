#!/usr/bin/env bash
# OPTIONAL: self-host an open-weight model with vLLM on a g4dn.xlarge SPOT instance
# (1x NVIDIA T4, 16 GB) for the one-time 50k-description run, then terminate it.
#
# New AWS accounts usually have a GPU quota of 0. Request "All G and VT Spot Instance
# Requests" = 4 vCPUs in Service Quotas first (can take a day). If you can't get it,
# use a hosted open-model API instead (see docs/LLM.md): similar cost, zero ops.
#
#   KEY_NAME=my-key ./deploy/aws/llm-gpu-spot.sh
# Then set on the workers: LLM_BASE_URL=http://<private-ip>:8000/v1  LLM_MODEL=$MODEL
set -euo pipefail
export MSYS_NO_PATHCONV=1 # Git Bash on Windows: keep "/aws/service/..." from becoming a Windows path
REGION="${REGION:-us-east-1}"
KEY_NAME="${KEY_NAME:?set KEY_NAME}"
MODEL="${MODEL:-Qwen/Qwen2.5-7B-Instruct-AWQ}"   # T4-friendly (AWQ int4, fp16 compute); try Qwen3 8B AWQ too
VLLM_IMAGE="${VLLM_IMAGE:-vllm/vllm-openai:v0.6.6}"
aws="aws --region $REGION"

SG_ID=$($aws ec2 describe-security-groups --filters Name=group-name,Values=mcpkg --query 'SecurityGroups[0].GroupId' --output text)
# vLLM port reachable only from mcpkg instances.
$aws ec2 authorize-security-group-ingress --group-id "$SG_ID" --protocol tcp --port 8000 --source-group "$SG_ID" 2>/dev/null || true

# AWS Deep Learning Base AMI (NVIDIA driver + Docker + nvidia-container-toolkit preinstalled).
AMI=$($aws ssm get-parameter --name /aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-ubuntu-22.04/latest/ami-id --query Parameter.Value --output text)

USER_DATA=$(cat <<EOF
#!/bin/bash
docker run -d --restart unless-stopped --gpus all -p 8000:8000 --ipc=host \
  -v /opt/hf:/root/.cache/huggingface $VLLM_IMAGE \
  --model $MODEL --dtype half --max-model-len 8192 --gpu-memory-utilization 0.92 --max-num-seqs 64
EOF
)

ID=$($aws ec2 run-instances --image-id "$AMI" --instance-type g4dn.xlarge --key-name "$KEY_NAME" \
  --security-group-ids "$SG_ID" \
  --instance-market-options 'MarketType=spot,SpotOptions={SpotInstanceType=one-time,InstanceInterruptionBehavior=terminate}' \
  --metadata-options "HttpTokens=required,HttpPutResponseHopLimit=1" \
  --block-device-mappings "DeviceName=/dev/sda1,Ebs={VolumeSize=80,VolumeType=gp3,DeleteOnTermination=true}" \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=mcpkg-llm},{Key=app,Value=mcpkg}]" \
  --user-data "$USER_DATA" --query 'Instances[0].InstanceId' --output text)
$aws ec2 wait instance-running --instance-ids "$ID"
IP=$($aws ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)
echo "LLM instance $ID starting (model download takes a few minutes)."
echo "Set on workers: LLM_BASE_URL=http://$IP:8000/v1 LLM_MODEL=$MODEL LLM_API_KEY="
echo "Check quality first:  mcpkg eval-llm -n 100"
echo "Terminate when done:  aws ec2 terminate-instances --instance-ids $ID --region $REGION"
