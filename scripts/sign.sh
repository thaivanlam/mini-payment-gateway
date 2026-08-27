#!/usr/bin/env bash
#
# Sign and send a request to the gateway.
#
#   ./scripts/sign.sh GET  /api/v1/merchants/me
#   ./scripts/sign.sh GET  /api/v1/ledger/balance?currency=VND
#   ./scripts/sign.sh POST /api/v1/payments '{"reference":"ORDER-1", ...}'
#
# Requires API_KEY and API_SECRET in the environment (printed by `make seed`).
#
# The signed string is the four components joined by newlines:
#
#   <timestamp>\n<method>\n<path>\n<body>
#
# The delimiters matter: plain concatenation would let (path "/a", body "b") and
# (path "/ab", body "") produce the same signature. This is the same recipe as
# service.ComputeRequestSignature -- one definition, two implementations that
# have to agree.

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
METHOD="${1:-GET}"
PATH_WITH_QUERY="${2:-/api/v1/merchants/me}"
BODY="${3:-}"

if [[ -z "${API_KEY:-}" || -z "${API_SECRET:-}" ]]; then
  echo "error: export API_KEY and API_SECRET first (see the output of 'make seed')" >&2
  exit 1
fi

# Only the path is signed, never the query string.
SIGNED_PATH="${PATH_WITH_QUERY%%\?*}"
TIMESTAMP="$(date +%s)"

SIGNATURE="$(printf '%s\n%s\n%s\n%s' "$TIMESTAMP" "$METHOD" "$SIGNED_PATH" "$BODY" \
  | openssl dgst -sha256 -hmac "$API_SECRET" \
  | awk '{print $NF}')"

ARGS=(
  -sS -X "$METHOD" "${BASE_URL}${PATH_WITH_QUERY}"
  -H "X-Api-Key: ${API_KEY}"
  -H "X-Timestamp: ${TIMESTAMP}"
  -H "X-Signature: ${SIGNATURE}"
)

if [[ -n "$BODY" ]]; then
  ARGS+=(-H "Content-Type: application/json")
  ARGS+=(-H "Idempotency-Key: ${IDEMPOTENCY_KEY:-$(uuidgen 2>/dev/null || echo "idem-${TIMESTAMP}-$$")}")
  ARGS+=(--data-raw "$BODY")
fi

curl "${ARGS[@]}" | { command -v jq >/dev/null && jq . || cat; }
