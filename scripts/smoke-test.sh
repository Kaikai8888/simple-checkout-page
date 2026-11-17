#!/usr/bin/env bash
# End-to-end smoke test against a running stack (make up).
# Usage: ./scripts/smoke-test.sh      requires curl + jq
set -uo pipefail

BASE=${BASE:-http://localhost:8080}
GW=${GW:-http://localhost:9090}
PASS=0
FAIL=0

green() { printf '\033[32m%s\033[0m\n' "$*"; }
red()   { printf '\033[31m%s\033[0m\n' "$*"; }
title() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
check() { # check "description" actual expected
  if [[ "$2" == "$3" ]]; then green "  PASS  $1 ($2)"; PASS=$((PASS+1));
  else red "  FAIL  $1: got [$2], want [$3]"; FAIL=$((FAIL+1)); fi
}

# call METHOD PATH [BODY] [IDEMPOTENCY_KEY] -> sets $STATUS and $BODY
call() {
  local args=(-s -m 10 -X "$1" "$BASE$2" -H 'Content-Type: application/json' -w $'\n%{http_code}')
  [[ -n "${3:-}" ]] && args+=(-d "$3")
  [[ -n "${4:-}" ]] && args+=(-H "Idempotency-Key: $4")
  local out; out=$(curl "${args[@]}")
  STATUS=$(tail -n1 <<<"$out")
  BODY=$(sed '$d' <<<"$out")
}

set_product() { call PUT "/api/admin/products/$1" "{\"price\":$2,\"stock\":$3}"; }
stock_of()    { call GET "/api/products/$1"; jq -r .stock <<<"$BODY"; }
key()         { echo "smoke-$(date +%s)-$RANDOM$RANDOM"; }

# payload ITEMS_JSON EXPECTED_TOTAL [COUPON] [PAYMENT_METHOD] [PHONE]
payload() {
  jq -nc --argjson items "$1" --argjson total "$2" --arg coupon "${3:-}" --arg pm "${4:-credit_card}" --arg phone "${5:-0912345678}" '{
    items: $items, coupon_code: $coupon, donation: 0, payment_method: $pm,
    shipping: {region:"TW_MAIN", county:"新北市", district:"新莊區", address:"中正路 1 號", zip:"242",
               recipient_name:"測試者", recipient_phone:$phone},
    customer: {name:"測試者", email:"smoke@example.com", phone:$phone},
    invoice: {type:"personal"}, expected_total: $total }'
}

# submit PAYLOAD -> sets DRAFT_ID (empty if rejected synchronously)
submit() {
  call POST /api/checkouts "$1" "${2:-$(key)}"
  DRAFT_ID=$(jq -r '.draft_id // empty' <<<"$BODY")
}

# wait_draft ID -> sets DRAFT (final json)
wait_draft() {
  for _ in $(seq 1 40); do
    call GET "/api/checkouts/$1"
    local st; st=$(jq -r .status <<<"$BODY")
    if [[ "$st" == completed || "$st" == failed ]]; then DRAFT=$BODY; return; fi
    sleep 0.5
  done
  DRAFT=$BODY
}

# pay ORDER_ID CARD [action] -> pays through the mock gateway like a browser would
pay() {
  call POST "/api/orders/$1/payments"
  local url; url=$(jq -r .payment_url <<<"$BODY")
  local sid=${url##*/}
  curl -s -m 10 -o /dev/null -X POST "$GW/pay/$sid" --data-urlencode "card_number=$2" --data-urlencode "action=${3:-pay}"
  sleep 0.5
  call GET "/api/orders/$1"
}

title "0. health"
call GET /healthz
check "API is up" "$STATUS" 200
set_product 1 5500 10 >/dev/null; set_product 101 300 50 >/dev/null

title "1. happy path: main product + add-on, credit card success"
submit "$(payload '[{"product_id":1,"quantity":1,"unit_price":5500},{"product_id":101,"quantity":1,"unit_price":300}]' 5800)"
check "submit accepted" "$STATUS" 202
wait_draft "$DRAFT_ID"
check "draft completed" "$(jq -r .status <<<"$DRAFT")" completed
ORDER=$(jq -r .order_id <<<"$DRAFT")
check "stock deducted (10 -> 9)" "$(stock_of 1)" 9
pay "$ORDER" "4242 4242 4242 4242"
check "order paid" "$(jq -r .status <<<"$BODY")" paid
check "order total" "$(jq -r .total <<<"$BODY")" 5800

title "2. card declined, then retry succeeds"
submit "$(payload '[{"product_id":1,"quantity":1,"unit_price":5500}]' 5500)"
wait_draft "$DRAFT_ID"; ORDER=$(jq -r .order_id <<<"$DRAFT")
pay "$ORDER" "4000 0000 0000 0002"
check "order still pending_payment" "$(jq -r .status <<<"$BODY")" pending_payment
check "latest payment failed" "$(jq -r .latest_payment.status <<<"$BODY")" failed
pay "$ORDER" "4242 4242 4242 4242"
check "retry -> paid" "$(jq -r .status <<<"$BODY")" paid

title "3. price changed after the shopper loaded the page"
set_product 1 5500 10 >/dev/null
set_product 1 5000 10 >/dev/null
submit "$(payload '[{"product_id":1,"quantity":1,"unit_price":5500}]' 5500)"
wait_draft "$DRAFT_ID"
check "draft failed" "$(jq -r .status <<<"$DRAFT")" failed
check "error code" "$(jq -r .error.code <<<"$DRAFT")" ITEMS_CHANGED
check "item reason" "$(jq -r '.error.details.items[0].reason' <<<"$DRAFT")" PRICE_CHANGED
check "current price returned" "$(jq -r '.error.details.items[0].current_price' <<<"$DRAFT")" 5000
check "reservation released (stock back to 10)" "$(stock_of 1)" 10
set_product 1 5500 10 >/dev/null

title "4. insufficient stock is rejected synchronously"
set_product 1 5500 1 >/dev/null
submit "$(payload '[{"product_id":1,"quantity":2,"unit_price":5500}]' 11000)"
check "HTTP 409" "$STATUS" 409
check "reason" "$(jq -r '.error.details.items[0].reason' <<<"$BODY")" INSUFFICIENT_STOCK

title "5. two shoppers race for the last unit"
set_product 1 5500 1 >/dev/null
P=$(payload '[{"product_id":1,"quantity":1,"unit_price":5500}]' 5500)
for i in 1 2; do
  curl -s -m 10 -o /dev/null -w '%{http_code}\n' -X POST "$BASE/api/checkouts" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: $(key)-$i" -d "$P" &
done > "${TMPDIR:-/tmp}/race.txt"
wait
check "exactly one accepted" "$(grep -c 202 "${TMPDIR:-/tmp}/race.txt")" 1
check "exactly one sold out" "$(grep -c 409 "${TMPDIR:-/tmp}/race.txt")" 1
sleep 2
check "available stock is 0" "$(stock_of 1)" 0

title "6. coupons"
set_product 1 5500 10 >/dev/null
submit "$(payload '[{"product_id":1,"quantity":1,"unit_price":5500}]' 5000 RETURN500)"
wait_draft "$DRAFT_ID"
check "RETURN500 order created" "$(jq -r .status <<<"$DRAFT")" completed
call GET "/api/orders/$(jq -r .order_id <<<"$DRAFT")"
check "discount 500" "$(jq -r .discount <<<"$BODY")" 500
submit "$(payload '[{"product_id":1,"quantity":1,"unit_price":5500}]' 5400 ONCE100)"; wait_draft "$DRAFT_ID"
submit "$(payload '[{"product_id":1,"quantity":1,"unit_price":5500}]' 5400 ONCE100)"; wait_draft "$DRAFT_ID"
check "ONCE100 cannot be used twice" "$(jq -r .error.code <<<"$DRAFT")" COUPON_INVALID

title "7. idempotency: same key twice -> same draft"
K=$(key)
P=$(payload '[{"product_id":1,"quantity":1,"unit_price":5500}]' 5500)
submit "$P" "$K"; FIRST=$DRAFT_ID
submit "$P" "$K"
check "same draft id" "$DRAFT_ID" "$FIRST"

title "8. amount the shopper confirmed differs from the server's"
submit "$(payload '[{"product_id":1,"quantity":1,"unit_price":5500}]' 1234)"
wait_draft "$DRAFT_ID"
check "AMOUNT_MISMATCH" "$(jq -r .error.code <<<"$DRAFT")" AMOUNT_MISMATCH

title "9. invalid input"
submit "$(payload '[{"product_id":1,"quantity":1,"unit_price":5500}]' 5500 '' credit_card 123)"
check "HTTP 400 for bad phone" "$STATUS" 400

title "10. forged payment callback"
call POST /api/payments/callback '{"merchant_trade_no":"x","status":"SUCCESS","amount":1}'
check "HTTP 401" "$STATUS" 401

title "11. ATM virtual account"
submit "$(payload '[{"product_id":1,"quantity":1,"unit_price":5500}]' 5500 '' atm)"
wait_draft "$DRAFT_ID"; ORDER=$(jq -r .order_id <<<"$DRAFT")
pay "$ORDER" ""
check "ATM order paid" "$(jq -r .status <<<"$BODY")" paid

title "12. multiple products in one order"
set_product 1 5500 10 >/dev/null; set_product 2 1980 20 >/dev/null
submit "$(payload '[{"product_id":1,"quantity":2,"unit_price":5500},{"product_id":2,"quantity":3,"unit_price":1980},{"product_id":101,"quantity":1,"unit_price":300}]' 17240)"
wait_draft "$DRAFT_ID"
check "multi-item order created" "$(jq -r .status <<<"$DRAFT")" completed
ORDER=$(jq -r .order_id <<<"$DRAFT")
check "product 1 stock 10 -> 8" "$(stock_of 1)" 8
check "product 2 stock 20 -> 17" "$(stock_of 2)" 17
pay "$ORDER" "4242 4242 4242 4242"
check "order paid" "$(jq -r .status <<<"$BODY")" paid
check "3 order lines" "$(jq -r '.lines | length' <<<"$BODY")" 3
check "total" "$(jq -r .total <<<"$BODY")" 17240

title "13. one item short -> nothing is reserved (all-or-nothing)"
set_product 1 5500 1 >/dev/null
submit "$(payload '[{"product_id":2,"quantity":1,"unit_price":1980},{"product_id":1,"quantity":2,"unit_price":5500}]' 12980)"
check "HTTP 409" "$STATUS" 409
check "short product is #1" "$(jq -r '.error.details.items[0].product_id' <<<"$BODY")" 1
check "product 2 not reserved (still 17)" "$(stock_of 2)" 17

set_product 1 5500 10 >/dev/null; set_product 2 1980 100 >/dev/null
printf '\n\033[1mResult: %d passed, %d failed\033[0m\n' "$PASS" "$FAIL"
[[ $FAIL -eq 0 ]]
