#!/usr/bin/env bash
# Resilience tests for the flash-sale gate and the draft publish / retry flow, against a running stack (make up).
# Unlike smoke-test.sh this injects failures (stops services, edits Valkey and Postgres directly), so it is slower
# (~2-3 min) and must run on the host that runs docker compose.
#
# Usage: ./scripts/resilience-test.sh      requires curl + jq + docker compose
#
# 1. a checkout with several missing flash gates initialises all of them in one round
# 2. the standalone gate-sync job realigns a drifted gate
# 3. a published draft that is still pending (consumer lag) is never re-published
# 4. a released draft is re-published only after next_retry_at (retry backoff)
# 5. Submit's publish fails (Kafka down) -> the reclaim job publishes it once Kafka is back
# 6. a draft stuck in processing (worker died) is recovered after the lease and completed
set -uo pipefail
cd "$(dirname "$0")/.."

BASE=${BASE:-http://localhost:8080}
PASS=0
FAIL=0
JAR_DIR=$(mktemp -d)
SHOPPER_JAR=$JAR_DIR/shopper; ADMIN_JAR=$JAR_DIR/admin
JAR=$SHOPPER_JAR

cleanup() {
  # whatever happened, leave the stack running as `make up` started it
  docker compose start kafka gate-sync >/dev/null 2>&1
  rm -rf "$JAR_DIR"
}
trap cleanup EXIT

green() { printf '\033[32m%s\033[0m\n' "$*"; }
red()   { printf '\033[31m%s\033[0m\n' "$*"; }
title() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
info()  { printf '  ..    %s\n' "$*"; }
check() { # check "description" actual expected
  if [[ "$2" == "$3" ]]; then green "  PASS  $1 ($2)"; PASS=$((PASS+1));
  else red "  FAIL  $1: got [$2], want [$3]"; FAIL=$((FAIL+1)); fi
}

# call METHOD PATH [BODY] [IDEMPOTENCY_KEY] [TIMEOUT] -> sets $STATUS and $BODY
call() {
  local args=(-s -m "${5:-10}" -X "$1" "$BASE$2" -H 'Content-Type: application/json' -b "$JAR" -c "$JAR" -w $'\n%{http_code}')
  [[ -n "${3:-}" ]] && args+=(-d "$3")
  [[ -n "${4:-}" ]] && args+=(-H "Idempotency-Key: $4")
  local out; out=$(curl "${args[@]}")
  STATUS=$(tail -n1 <<<"$out")
  BODY=$(sed '$d' <<<"$out")
}

admin_put() { local me=$JAR; JAR=$ADMIN_JAR; call PUT "/api/admin/products/$1" "$2"; JAR=$me; }
set_product() { admin_put "$1" "{\"price\":$2,\"stock\":$3}"; }
key()         { echo "resil-$(date +%s)-$RANDOM$RANDOM"; }

db() { docker compose exec -T postgres psql -U checkout -d checkout -tAq -v ON_ERROR_STOP=1 -c "$1"; }
vk() { docker compose exec -T valkey valkey-cli "$@" | tr -d '\r'; }
available() { db "SELECT available_stock FROM products WHERE id = $1"; }
draft_field() { db "SELECT $2 FROM checkout_drafts WHERE id = '$1'"; }

# payload ITEMS_JSON EXPECTED_TOTAL
payload() {
  jq -nc --argjson items "$1" --argjson total "$2" '{
    items: $items, coupon_code: "", donation: 0, payment_method: "credit_card",
    shipping: {region:"TW_MAIN", county:"新北市", district:"新莊區", address:"中正路 1 號", zip:"242",
               recipient_name:"測試者", recipient_phone:"0912345678"},
    customer: {name:"測試者", email:"resilience@example.com", phone:"0912345678"},
    invoice: {type:"personal"}, expected_total: $total }'
}

# submit PAYLOAD [TIMEOUT] -> sets DRAFT_ID (empty if rejected synchronously)
submit() {
  call POST /api/checkouts "$1" "$(key)" "${2:-10}"
  DRAFT_ID=$(jq -r '.draft_id // empty' <<<"$BODY")
}

# wait_status ID STATUS TIMEOUT_SECONDS -> polls the draft row; prints the last status seen
wait_status() {
  local st=""
  for _ in $(seq 1 "$3"); do
    st=$(draft_field "$1" status)
    [[ "$st" == "$2" || "$st" == failed ]] && break
    sleep 1
  done
  echo "$st"
}

# clone_draft NEW_ID STATUS ATTEMPTS PUBLISHED_AT NEXT_RETRY_AT UPDATED_AT
# Inserts a draft in the given state, copying payload and owner from $TEMPLATE_DRAFT, and reserves its stock exactly
# like Submit does (available_stock - qty + held reservation), so a worker can turn it into a real order.
# The last three arguments are SQL expressions, e.g. "now()", "NULL", "now() - interval '2 minutes'".
clone_draft() {
  db "BEGIN;
      INSERT INTO checkout_drafts (id, customer_id, idempotency_key, payload, status, attempts, published_at, next_retry_at, updated_at)
      SELECT '$1', customer_id, '$1', payload, '$2', $3, $4, $5, $6 FROM checkout_drafts WHERE id = '$TEMPLATE_DRAFT';
      UPDATE products p SET available_stock = p.available_stock - (i->>'quantity')::int
        FROM checkout_drafts d, jsonb_array_elements(d.payload->'items') i
       WHERE d.id = '$1' AND p.id = (i->>'product_id')::bigint;
      INSERT INTO inventory_reservations (draft_id, product_id, quantity, status)
      SELECT '$1', (i->>'product_id')::bigint, (i->>'quantity')::int, 'held'
        FROM checkout_drafts d, jsonb_array_elements(d.payload->'items') i WHERE d.id = '$1';
      COMMIT;"
}

title "0. setup"
for _ in $(seq 1 60); do
  curl -s -m 2 -o /dev/null "$BASE/healthz" && break
  sleep 1
done
call GET /healthz
check "API is up" "$STATUS" 200
JAR=$ADMIN_JAR; call POST /api/auth/login '{"email":"admin@example.com","password":"admin1234"}'; JAR=$SHOPPER_JAR
check "admin login" "$STATUS" 200
call POST /api/auth/register "{\"email\":\"resilience-$(date +%s)-$RANDOM@example.com\",\"password\":\"password123\",\"name\":\"測試者\"}"
check "shopper registered" "$STATUS" 201
set_product 1 5500 10 >/dev/null; set_product 2 1980 100 >/dev/null
admin_put 2 '{"flash_sale":false}' >/dev/null
# a real, completed draft whose payload (product 2 x1) the DB-level scenarios clone
submit "$(payload '[{"product_id":2,"quantity":1,"unit_price":1980}]' 1980)"
TEMPLATE_DRAFT=$DRAFT_ID
check "template draft completed" "$(wait_status "$TEMPLATE_DRAFT" completed 20)" completed
check "a completed draft was marked published" "$(draft_field "$TEMPLATE_DRAFT" "published_at IS NOT NULL")" t

title "1. several missing flash gates are initialised in one round"
docker compose stop gate-sync >/dev/null 2>&1   # so the sync job cannot rebuild the gates first
admin_put 2 '{"flash_sale":true}' >/dev/null
check "product 2 is now a flash sale product" "$(jq -r .flash_sale <<<"$BODY")" true
vk DEL gate:1 gate:2 >/dev/null
check "both gates deleted" "$(vk EXISTS gate:1 gate:2)" 0
submit "$(payload '[{"product_id":1,"quantity":1,"unit_price":5500},{"product_id":2,"quantity":1,"unit_price":1980}]' 7480)"
check "checkout with two missing gates accepted" "$STATUS" 202
check "draft completed" "$(wait_status "$DRAFT_ID" completed 20)" completed
# Both gates must exist AND have been decremented by this checkout (gate == available_stock). With one gate per
# retry, the second gate would still be missing and the first would not have been decremented.
check "gate:1 created and decremented (= available_stock)" "$(vk GET gate:1)" "$(available 1)"
check "gate:2 created and decremented (= available_stock)" "$(vk GET gate:2)" "$(available 2)"

title "2. the gate-sync job realigns a drifted gate"
docker compose start gate-sync >/dev/null 2>&1
vk SET gate:1 999 >/dev/null
info "gate:1 set to 999, waiting for gate-sync (runs every 30s)"
want=$(available 1); got=""
for _ in $(seq 1 45); do
  got=$(vk GET gate:1)
  [[ "$got" == "$want" ]] && break
  sleep 1
done
check "gate:1 realigned with available_stock" "$got" "$want"
admin_put 2 '{"flash_sale":false}' >/dev/null
check "turning flash sale off deletes gate:2" "$(vk EXISTS gate:2)" 0

title "3. consumer lag: a published draft that is still pending is never re-published"
LAG_DRAFT=$(key)
# published, but no message is actually in Kafka: stands in for a message waiting behind a long consumer lag
clone_draft "$LAG_DRAFT" pending 0 "now()" "now()" "now()"
info "waiting 15s (3 reclaim runs)"
sleep 15
check "still pending (not re-published)" "$(draft_field "$LAG_DRAFT" status)" pending
check "never claimed by a worker" "$(draft_field "$LAG_DRAFT" attempts)" 0
# now deliver the 'queued' message: the draft was only waiting, so it completes normally
printf '{"draft_id":"%s"}\n' "$LAG_DRAFT" | docker compose exec -T kafka \
  /opt/kafka/bin/kafka-console-producer.sh --bootstrap-server localhost:9092 --topic checkout.requested >/dev/null 2>&1
check "completes once its message is consumed" "$(wait_status "$LAG_DRAFT" completed 20)" completed

title "4. retry backoff: a released draft is re-published only after next_retry_at"
RETRY_DRAFT=$(key)
# the state Release leaves after a transient error on attempt 1
clone_draft "$RETRY_DRAFT" pending 1 "NULL" "now() + interval '8 seconds'" "now()"
sleep 4
check "not re-published before next_retry_at" "$(draft_field "$RETRY_DRAFT" "published_at IS NULL")" t
check "still pending before next_retry_at" "$(draft_field "$RETRY_DRAFT" status)" pending
check "completed after the backoff" "$(wait_status "$RETRY_DRAFT" completed 30)" completed
check "order created after next_retry_at" \
  "$(db "SELECT o.created_at >= d.next_retry_at FROM orders o JOIN checkout_drafts d ON d.id = o.draft_id WHERE d.id = '$RETRY_DRAFT'")" t
check "claimed exactly once more (attempts 1 -> 2)" "$(draft_field "$RETRY_DRAFT" attempts)" 2

title "5. Submit's publish fails (Kafka down) -> published by the reclaim job when Kafka is back"
docker compose stop kafka >/dev/null 2>&1
info "kafka stopped, submitting (the publish has to time out first, this can take a while)"
submit "$(payload '[{"product_id":2,"quantity":1,"unit_price":1980}]' 1980)" 90
check "checkout still accepted while Kafka is down" "$STATUS" 202
PUB_DRAFT=$DRAFT_ID
if [[ -n "$PUB_DRAFT" ]]; then
  check "draft left unpublished" "$(draft_field "$PUB_DRAFT" "published_at IS NULL")" t
  info "starting kafka again"
  docker compose up -d --wait kafka >/dev/null 2>&1
  check "completed after Kafka is back" "$(wait_status "$PUB_DRAFT" completed 120)" completed
  check "marked published by the reclaim job" "$(draft_field "$PUB_DRAFT" "published_at IS NOT NULL")" t
else
  docker compose up -d --wait kafka >/dev/null 2>&1
fi

title "6. worker died: a draft stuck in processing is recovered after the lease"
STUCK_DRAFT=$(key)
# claimed once (attempts = 1) by a worker that died 2 minutes ago; the lease is 60s
clone_draft "$STUCK_DRAFT" processing 1 "now() - interval '2 minutes'" "now() - interval '2 minutes'" "now() - interval '2 minutes'"
check "recovered and completed" "$(wait_status "$STUCK_DRAFT" completed 30)" completed
check "claimed again by a live worker (attempts 1 -> 2)" "$(draft_field "$STUCK_DRAFT" attempts)" 2

set_product 1 5500 10 >/dev/null; set_product 2 1980 100 >/dev/null
printf '\n\033[1mResult: %d passed, %d failed\033[0m\n' "$PASS" "$FAIL"
[[ $FAIL -eq 0 ]]
