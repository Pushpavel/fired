#!/usr/bin/env bash
# End-to-end smoke test against a running fired daemon (real Firecracker).
#
#   FIRED_URL=http://10.0.0.5:8443 FIRED_API_TOKEN=... scripts/smoke-test.sh
#
# Builds a template from an image (skip with TEMPLATE=<existing name>),
# then exercises create / exec / files / ports / suspend / resume / archive
# / delete and prints timings. Requires curl and jq.
set -euo pipefail

URL="${FIRED_URL:-http://127.0.0.1:8443}"
TOKEN="${FIRED_API_TOKEN:?set FIRED_API_TOKEN}"
IMAGE="${IMAGE:-node:22-bookworm-slim}"
TEMPLATE="${TEMPLATE:-}"
KEEP="${KEEP:-0}"

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*"; exit 1; }
now()  { date +%s.%N; }
since(){ awk -v a="$1" -v b="$(now)" 'BEGIN{printf "%.0f ms", (b-a)*1000}'; }
api()  { # method path [json]
  local m="$1" p="$2"; shift 2
  curl -fsS -X "$m" -H "X-Fired-Token: $TOKEN" -H 'Content-Type: application/json' "$URL$p" ${1:+-d "$1"}
}
exec_() { # id cmd → json
  api POST "/v1/sandboxes/$1/exec" "$(jq -nc --arg c "$2" '{cmd:$c, timeout_s:120}')"
}
state() { api GET "/v1/sandboxes/$1" | jq -r .state; }

say "host"
api GET /v1/host | jq .

if [ -z "$TEMPLATE" ]; then
  TEMPLATE="smoke"
  say "build template $TEMPLATE from $IMAGE"
  t0=$(now)
  TID=$(api POST /v1/templates "$(jq -nc --arg n "$TEMPLATE" --arg i "$IMAGE" \
        '{name:$n, image:$i, memory_mb:1024, vcpus:2, run:["node -e \"console.log(1)\""]}')" | jq -r .id)
  while :; do
    st=$(api GET "/v1/templates/$TID" | jq -r .status)
    [ "$st" = ready ] && break
    if [ "$st" = failed ]; then
      api GET "/v1/templates/$TID/logs" || true
      fail "template build failed: $(api GET "/v1/templates/$TID" | jq -r .error)"
    fi
    sleep 2
  done
  echo "template $TID ready in $(since "$t0")"
fi

say "create sandbox"
t0=$(now)
SB=$(api POST /v1/sandboxes "$(jq -nc --arg t "$TEMPLATE" '{template:$t, labels:{smoke:"1"}, egress:{allow:["registry.npmjs.org"]}}')" | jq -r .id)
echo "created $SB in $(since "$t0")"
cleanup() { [ "$KEEP" = 1 ] || api DELETE "/v1/sandboxes/$SB" >/dev/null 2>&1 || true; }
trap cleanup EXIT

say "exec"
t0=$(now)
out=$(exec_ "$SB" 'node -v && uname -r && id -u && df -h / | tail -1')
echo "$out" | jq -r .stdout
echo "exec round trip $(since "$t0")"
[ "$(echo "$out" | jq -r .exit_code)" = 0 ] || fail "exec failed: $out"

say "writable root (overlay)"
exec_ "$SB" 'touch /usr/local/smoke && echo ok' | jq -r '.stdout, .stderr'

say "files"
curl -fsS -X PUT -H "X-Fired-Token: $TOKEN" --data-binary 'hello from the host' "$URL/v1/sandboxes/$SB/files?path=hello.txt" | jq -c .
[ "$(curl -fsS -H "X-Fired-Token: $TOKEN" "$URL/v1/sandboxes/$SB/files?path=hello.txt")" = "hello from the host" ] || fail "file read"

say "egress (allowed + denied)"
exec_ "$SB" 'curl -s -o /dev/null -w "%{http_code}\n" https://registry.npmjs.org/ || node -e "fetch(\"https://registry.npmjs.org/\").then(r=>console.log(r.status))"' | jq -r '.stdout, .stderr'
exec_ "$SB" 'node -e "fetch(\"https://example.com/\").then(r=>console.log(r.status)).catch(e=>console.log(\"denied:\", e.cause?.code||e.message))"' | jq -r '.stdout, .stderr'

say "port preview"
curl -fsS -X PUT -H "X-Fired-Token: $TOKEN" --data-binary \
  'require("http").createServer((q, s) => s.end("preview-ok")).listen(3000)' \
  "$URL/v1/sandboxes/$SB/files?path=server.js" >/dev/null
api POST "/v1/sandboxes/$SB/exec" '{"cmd":"node server.js","background":true}' | jq -c .
api POST "/v1/sandboxes/$SB/ports" '{"port":3000}' >/dev/null
for i in $(seq 1 50); do
  body=$(curl -s -H "X-Fired-Token: $TOKEN" "$URL/v1/sandboxes/$SB/ports/3000/proxy/") && [ "$body" = preview-ok ] && break
  sleep 0.1
done
[ "$body" = preview-ok ] || fail "preview: $body"
echo "preview ok"

say "suspend → resume (hot)"
t0=$(now); api POST "/v1/sandboxes/$SB/suspend" >/dev/null; echo "suspend $(since "$t0")"
[ "$(state "$SB")" = suspended ] || fail "not suspended"
t0=$(now)
body=$(curl -fsS -H "X-Fired-Token: $TOKEN" "$URL/v1/sandboxes/$SB/ports/3000/proxy/")
echo "wake-on-request + response $(since "$t0") → $body"
[ "$body" = preview-ok ] || fail "server process did not survive the snapshot"

say "archive → resume (cold)"
t0=$(now); api POST "/v1/sandboxes/$SB/archive" >/dev/null; echo "archive $(since "$t0")"
[ "$(state "$SB")" = archived ] || fail "not archived"
t0=$(now); api POST "/v1/sandboxes/$SB/resume" >/dev/null; echo "resume from cold $(since "$t0")"
body=$(curl -fsS -H "X-Fired-Token: $TOKEN" "$URL/v1/sandboxes/$SB/ports/3000/proxy/")
[ "$body" = preview-ok ] || fail "after cold resume: $body"
[ "$(curl -fsS -H "X-Fired-Token: $TOKEN" "$URL/v1/sandboxes/$SB/files?path=hello.txt")" = "hello from the host" ] || fail "file lost"

say "all good"
