#!/usr/bin/env bash
# Email alerts before your credits run out: 50%, 80% and 100% (forecasted) of a monthly budget.
#   EMAIL=you@example.com LIMIT=40 ./deploy/aws/budget.sh
set -euo pipefail
EMAIL="${EMAIL:?set EMAIL}"
LIMIT="${LIMIT:-40}"
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)

notif() { # $1 threshold, $2 ACTUAL|FORECASTED
  printf '{"Notification":{"NotificationType":"%s","ComparisonOperator":"GREATER_THAN","Threshold":%s,"ThresholdType":"PERCENTAGE"},"Subscribers":[{"SubscriptionType":"EMAIL","Address":"%s"}]}' "$2" "$1" "$EMAIL"
}

aws budgets create-budget --account-id "$ACCOUNT" \
  --budget "{\"BudgetName\":\"mcpkg-monthly\",\"BudgetLimit\":{\"Amount\":\"$LIMIT\",\"Unit\":\"USD\"},\"TimeUnit\":\"MONTHLY\",\"BudgetType\":\"COST\",\"CostTypes\":{\"IncludeCredit\":false}}" \
  --notifications-with-subscribers "[$(notif 50 ACTUAL),$(notif 80 ACTUAL),$(notif 100 FORECASTED)]"
echo "budget mcpkg-monthly (\$$LIMIT/month, credits excluded so alerts reflect real usage) created; alerts go to $EMAIL"
