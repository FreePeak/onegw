#!/bin/bash
# Smoke test: gateway + mock provider, exercising all surfaces end-to-end.
# Verifies: passthrough, cross-format translation, usage accounting, admin.
set -euo pipefail
cd "$(dirname "$0")/.."

GW_PORT=9920
GW="http://127.0.0.1:$GW_PORT"

# Build binaries.
go build -o /tmp/onegw-smoke ./cmd/onegw
go build -o /tmp/mockupstream ./cmd/mockupstream

# Mock provider.
/tmp/mockupstream -listen 127.0.0.1:9921 &
MOCK_PID=$!
trap 'kill $MOCK_PID $GW_PID 2>/dev/null || true' EXIT

# Gateway config.
mkdir -p /tmp/onegw-smoke-data
cat > /tmp/onegw-smoke.toml <<EOF
[server]
listen = "127.0.0.1:$GW_PORT"
data_dir = "/tmp/onegw-smoke-data"
admin_password = ""

[saver]
enabled = true

[usage]
flush_interval = "1s"

[[providers]]
name = "mock"
kind = "openai"
base_url = "http://127.0.0.1:9921"
api_key = "sk-mock"
models = ["mock-model"]

[[providers]]
name = "mocka"
kind = "anthropic"
base_url = "http://127.0.0.1:9921"
api_key = "sk-mock"
models = ["mock-claude"]
EOF

/tmp/onegw-smoke -config /tmp/onegw-smoke.toml &
GW_PID=$!

for i in $(seq 1 50); do
  if curl -sf "$GW/admin/health" >/dev/null 2>&1; then break; fi
  sleep 0.1
done

fail() { echo "FAIL: $1" >&2; exit 1; }

echo "== 1. health =="
curl -sf "$GW/admin/health" | grep -q '"ok"' || fail health

echo "== 2. OpenAI non-streaming passthrough =="
OUT=$(curl -sf "$GW/v1/chat/completions" -d '{"model":"mock/mock-model","mock_tokens":50,"messages":[{"role":"user","content":"hi"}]}')
echo "$OUT" | grep -q '"completion_tokens":50' || fail "openai non-stream usage: $OUT"

echo "== 3. OpenAI streaming passthrough =="
OUT=$(curl -sfN "$GW/v1/chat/completions" -d '{"model":"mock/mock-model","mock_tokens":30,"stream":true,"messages":[{"role":"user","content":"hi"}]}')
echo "$OUT" | grep -q '\[DONE\]' || fail "openai stream [DONE]"
echo "$OUT" | grep -q '"completion_tokens":30' || fail "openai stream usage"

echo "== 4. OpenAI client → Anthropic upstream (cross-format translation) =="
OUT=$(curl -sfN "$GW/v1/chat/completions" -d '{"model":"mocka/mock-claude","mock_tokens":25,"stream":true,"messages":[{"role":"user","content":"hi"}]}')
echo "$OUT" | grep -q '\[DONE\]' || fail "translated stream [DONE]"
echo "$OUT" | grep -q '"finish_reason":"stop"' || fail "translated finish_reason"
echo "$OUT" | grep -q '"completion_tokens":200' || fail "translated usage: $OUT"

echo "== 5. Anthropic client → OpenAI upstream (cross-format translation) =="
OUT=$(curl -sfN "$GW/v1/messages" -H 'anthropic-version: 2023-06-01' -d '{"model":"mock/mock-model","max_tokens":100,"mock_tokens":25,"stream":true,"messages":[{"role":"user","content":"hi"}]}')
echo "$OUT" | grep -q 'event: message_stop' || fail "anthropic stream shape: $OUT"
echo "$OUT" | grep -q '"input_tokens":200' || fail "anthropic translated usage: $OUT"

echo "== 6. models listing =="
OUT=$(curl -sf "$GW/v1/models")
echo "$OUT" | grep -q 'mock/mock-model' || fail models

echo "== 7. usage accounting (admin) =="
sleep 2   # allow a flush
sleep 1
OUT=$(curl -sf "$GW/admin/usage?password=admin&source=store&days=1")
echo "$OUT" | grep -q '"provider":"mock"' || fail "admin usage missing mock: $OUT"
REQS=$(echo "$OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(sum(r["requests"] for r in d["rows"]))')
[ "$REQS" -ge 4 ] || fail "expected >=4 requests in usage, got $REQS"

echo "== 8. unknown model 404 =="
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$GW/v1/chat/completions" -d '{"model":"nobody/none","messages":[]}')
[ "$CODE" = "404" ] || fail "unknown model: $CODE"

echo
echo "ALL SMOKE TESTS PASSED"
