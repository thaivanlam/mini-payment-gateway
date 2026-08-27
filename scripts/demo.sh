#!/usr/bin/env bash
#
# End-to-end demo: authorize, capture, refund, then read the ledger.
# Requires API_KEY and API_SECRET (see 'make seed').

set -euo pipefail
cd "$(dirname "$0")/.."

REFERENCE="ORDER-DEMO-$(date +%s)"

echo "==> authorize ${REFERENCE}"
PAYMENT=$(./scripts/sign.sh POST /api/v1/payments "$(cat <<JSON
{
  "reference": "${REFERENCE}",
  "amount": 150000,
  "currency": "VND",
  "card": {"number": "4242424242424242", "exp_month": 12, "exp_year": 2030, "cvv": "123"},
  "capture": false,
  "metadata": {"customer_id": "cus_123"}
}
JSON
)" 2>/dev/null)
echo "$PAYMENT"

ID=$(echo "$PAYMENT" | grep -o '"id"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | cut -d'"' -f4)

echo "==> capture ${ID}"
./scripts/sign.sh POST "/api/v1/payments/${ID}/capture" '{"amount":150000}' 2>/dev/null

echo "==> refund 50000"
./scripts/sign.sh POST "/api/v1/payments/${ID}/refund" '{"amount":50000}' 2>/dev/null

echo "==> ledger entries"
./scripts/sign.sh GET "/api/v1/ledger/entries" 2>/dev/null

echo "==> balance"
./scripts/sign.sh GET "/api/v1/ledger/balance?currency=VND" 2>/dev/null
