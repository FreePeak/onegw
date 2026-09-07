#!/bin/bash
# Memory benchmark: sustained streaming load against onegw with a mock
# upstream, measuring gateway RSS. Target: <= 100 MB resident.
#
# Load profile: N concurrent clients, each issuing streaming chat completions
# with multi-MB request bodies (large "sessions") for a fixed duration.
set -euo pipefail
cd "$(dirname "$0")/.."

GW_PORT=9940
DURATION=${DURATION:-20}
CONCURRENCY=${CONCURRENCY:-50}
BODY_TOKENS=${BODY_TOKENS:-200000}   # ~800 KB request bodies

go build -o /tmp/onegw-bench ./cmd/onegw
go build -o /tmp/mockupstream ./cmd/mockupstream

/tmp/mockupstream -listen 127.0.0.1:9941 >/dev/null 2>&1 &
MOCK_PID=$!
trap 'kill $MOCK_PID $GW_PID 2>/dev/null || true' EXIT

mkdir -p /tmp/onegw-bench-data
cat > /tmp/onegw-bench.toml <<EOF
[server]
listen = "127.0.0.1:$GW_PORT"
data_dir = "/tmp/onegw-bench-data"
admin_password = "bench"

[saver]
enabled = true

[usage]
flush_interval = "2s"

[[providers]]
name = "mock"
kind = "openai"
base_url = "http://127.0.0.1:9941"
api_key = "sk-mock"
models = ["m"]
EOF

# GODEBUG=madvdontneed forces RSS to reflect freed pages (macOS MADV_FREE
# otherwise keeps freed pages resident and overstates RSS).
GODEBUG=madvdontneed=1 GOMEMLIMIT=90MiB /tmp/onegw-bench -config /tmp/onegw-bench.toml >/dev/null 2>&1 &
GW_PID=$!
GW_PID=$(pgrep -f "onegw-bench -config" | head -1)

for i in $(seq 1 50); do
  curl -sf -H "X-Admin-Password: bench" "http://127.0.0.1:$GW_PORT/admin/health" >/dev/null 2>&1 && break
  sleep 0.1
done

echo "Load: $CONCURRENCY clients × $DURATION s, $BODY_TOKENS-token bodies, streaming"
START=$(date +%s)
END=$((START + DURATION))
REQUESTS=0

PAYLOAD_FILE=$(mktemp)
python3 - > "$PAYLOAD_FILE" <<PYEOF
import json
words = "alpha bravo charlie delta echo foxtrot golf hotel".split()
blob = " ".join(words[i % 8] for i in range($BODY_TOKENS))
print(json.dumps({"model": "mock/m", "mock_tokens": 100, "stream": True,
                  "messages": [{"role": "user", "content": blob}]}))
PYEOF
rm -f "$PAYLOAD_FILE.bak"

make_request() {
  while [ $(date +%s) -lt $END ]; do
    curl -sfN --max-time 10 "http://127.0.0.1:$GW_PORT/v1/chat/completions" -d @"$PAYLOAD_FILE" > /dev/null || return 0
    REQUESTS=$((REQUESTS + 1))
  done
}

LOAD_PIDS=""
for i in $(seq 1 $CONCURRENCY); do
  make_request &
  LOAD_PIDS="$LOAD_PIDS $!"
done

# Sample RSS every second.
MAX_RSS=0
while [ $(date +%s) -lt $END ]; do
  if [ -n "$GW_PID" ]; then
    RSS=$(ps -o rss= -p "$GW_PID" 2>/dev/null | tr -d ' ' || echo 0)
    [ -n "$RSS" ] && [ "$RSS" -gt "$MAX_RSS" ] && MAX_RSS=$RSS
  fi
  sleep 1
done

wait $LOAD_PIDS 2>/dev/null || true

# Cool-down sample after load drains.
sleep 2
RSS_END=$(ps -o rss= -p "$GW_PID" 2>/dev/null | tr -d ' ' || echo 0)

GATEWAY_TOKS=$(curl -sf -H "X-Admin-Password: bench" "http://127.0.0.1:$GW_PORT/admin/usage?source=store&days=1" 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); print(sum(r["input"]+r["output"] for r in d.get("rows",[])))' 2>/dev/null || echo 0)
echo "tokens relayed (gateway-counted): ${GATEWAY_TOKS:-0}"
echo "max RSS during load: $((MAX_RSS / 1024)) MiB"
echo "RSS 2s after load:   $((RSS_END / 1024)) MiB"

rm -f "$PAYLOAD_FILE"
if [ "$MAX_RSS" -le 102400 ]; then
  echo "PASS: RSS <= 100 MB under load"
else
  echo "FAIL: RSS exceeded 100 MB"
  exit 1
fi
