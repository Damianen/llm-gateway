#!/usr/bin/env bash
# End-to-end smoke test: builds the gateway and a fake upstream, boots both,
# provisions a project + virtual key through the admin API, fires one request
# per inbound dialect (plus a streaming one), and prints the usage summary.
# No real network, no real provider keys.
set -euo pipefail

cd "$(dirname "$0")/.."

GW_PORT="${GW_PORT:-9100}"
FAKE_PORT="${FAKE_PORT:-9101}"
GW="http://127.0.0.1:$GW_PORT"
TMP="$(mktemp -d)"
cleanup() {
  # Word splitting is intentional: jobs -p yields one PID per line.
  # shellcheck disable=SC2046
  kill $(jobs -p) >/dev/null 2>&1 || true
  rm -rf "$TMP"
}
trap cleanup EXIT

say() { printf '\n==> %s\n' "$*"; }

say "building gateway and fakeupstream"
go build -o "$TMP/gateway" ./cmd/gateway
go build -o "$TMP/fakeupstream" ./cmd/fakeupstream

say "starting fake upstream on :$FAKE_PORT"
"$TMP/fakeupstream" -listen "127.0.0.1:$FAKE_PORT" &

cat >"$TMP/config.yaml" <<EOF
server:
  listen: "127.0.0.1:$GW_PORT"
database:
  path: $TMP/gateway.db
rate_limits:
  default_rpm: 100
  default_tpm: 100000
models:
  - name: sonnet
    provider: anthropic
    upstream_model: claude-sonnet-4-6
    base_url: http://127.0.0.1:$FAKE_PORT/anthropic
    api_key_env: SMOKE_ANTHROPIC_KEY
    pricing: { input_usd_per_mtok: 3.0, output_usd_per_mtok: 15.0 }
  - name: fast
    provider: openai
    upstream_model: gpt-fast
    base_url: http://127.0.0.1:$FAKE_PORT/openai/v1
    api_key_env: SMOKE_OPENAI_KEY
    pricing: { input_usd_per_mtok: 1.0, output_usd_per_mtok: 5.0 }
EOF

export SMOKE_ANTHROPIC_KEY="smoke-anthropic-key"
export SMOKE_OPENAI_KEY="smoke-openai-key"
export GATEWAY_ADMIN_TOKEN="smoke-admin-token"

say "starting gateway on :$GW_PORT"
"$TMP/gateway" -config "$TMP/config.yaml" &

for i in $(seq 1 50); do
  curl -fsS "$GW/healthz" >/dev/null 2>&1 && break
  [ "$i" -eq 50 ] && { echo "gateway did not become healthy"; exit 1; }
  sleep 0.1
done
say "gateway healthy"

say "creating project"
curl -fsS -X POST -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" \
  -d '{"name":"smoke"}' "$GW/admin/projects"
echo

say "creating virtual key"
KEY_RESP="$(curl -fsS -X POST -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" \
  -d '{"project":"smoke","name":"smoke-key"}' "$GW/admin/keys")"
KEY="$(printf '%s' "$KEY_RESP" | sed -n 's/.*"key":"\(sk-gw-[a-f0-9]*\)".*/\1/p')"
[ -n "$KEY" ] || { echo "failed to extract key from: $KEY_RESP"; exit 1; }
echo "issued key ${KEY:0:12}..."

say "OpenAI-dialect request (/v1/chat/completions -> openai upstream)"
OAI_RESP="$(curl -fsS -X POST -H "Authorization: Bearer $KEY" \
  -d '{"model":"fast","messages":[{"role":"user","content":"ping"}]}' \
  "$GW/v1/chat/completions")"
echo "$OAI_RESP"
printf '%s' "$OAI_RESP" | grep -q "Hello from fake openai." || { echo "unexpected openai-dialect response"; exit 1; }

say "Anthropic-dialect request (/v1/messages -> anthropic upstream)"
ANT_RESP="$(curl -fsS -X POST -H "x-api-key: $KEY" \
  -d '{"model":"sonnet","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}' \
  "$GW/v1/messages")"
echo "$ANT_RESP"
printf '%s' "$ANT_RESP" | grep -q "Hello from fake anthropic." || { echo "unexpected anthropic-dialect response"; exit 1; }

say "streaming request (OpenAI dialect)"
STREAM_RESP="$(curl -fsS -N -X POST -H "Authorization: Bearer $KEY" \
  -d '{"model":"fast","stream":true,"messages":[{"role":"user","content":"ping"}]}' \
  "$GW/v1/chat/completions")"
printf '%s' "$STREAM_RESP" | grep -q '\[DONE\]' || { echo "stream did not terminate with [DONE]"; exit 1; }
echo "stream OK ($(printf '%s' "$STREAM_RESP" | grep -c '^data:') data frames)"

say "usage summary"
USAGE="$(curl -fsS -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" \
  "$GW/admin/usage?project=smoke&group_by=model")"
echo "$USAGE"
printf '%s' "$USAGE" | grep -q '"requests":3' || { echo "usage totals mismatch (want 3 requests)"; exit 1; }

say "metrics spot check"
curl -fsS "$GW/metrics" | grep '^gateway_requests_total' || { echo "gateway metrics missing"; exit 1; }

say "SMOKE OK"
