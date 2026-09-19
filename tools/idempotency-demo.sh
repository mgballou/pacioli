#!/bin/sh
#
# Prints a transaction refused for want of an idempotency key, then the same key
# sent twice, the accounts after it, and that key sent with a different body.
#
#   tools/idempotency-demo.sh [binary] [addr]

set -eu

bin=${1:-bin/pacioli}
addr=${2:-127.0.0.1:58080}
base="http://$addr"

# Long enough that two clients do not pick it by accident, and readable.
key='deposit-2026-09-03-0001'

log=$(mktemp)
server=""
cleanup() {
	[ -n "$server" ] && kill "$server" 2>/dev/null || true
	rm -f "$log"
}
trap cleanup EXIT

get() {
	echo
	echo "\$ curl -sS '$base$1'"
	curl -sS "$base$1"
}

# post takes the whole -H argument so the request without a key can be shown as one.
post() {
	header=$1
	body=$2
	echo
	if [ -n "$header" ]; then
		echo "\$ curl -sS -i -X POST '$base/v1/transactions' -H '$header' --data-binary @- <<'JSON'"
	else
		echo "\$ curl -sS -i -X POST '$base/v1/transactions' --data-binary @- <<'JSON'"
	fi
	printf '%s\n' "$body"
	echo "JSON"
	if [ -n "$header" ]; then
		printf '%s' "$body" | curl -sS -i -X POST "$base/v1/transactions" \
			-H 'Content-Type: application/json' -H "$header" --data-binary @-
	else
		printf '%s' "$body" | curl -sS -i -X POST "$base/v1/transactions" \
			-H 'Content-Type: application/json' --data-binary @-
	fi
}


echo "\$ $bin serve -addr $addr &"
"$bin" serve -addr "$addr" 2>"$log" &
server=$!

waited=0
while [ "$(wc -l <"$log" | tr -d ' ')" -lt 2 ] || ! curl -sS -o /dev/null "$base/v1/trial-balance" 2>/dev/null; do
	waited=$((waited + 1))
	if [ "$waited" -gt 200 ]; then
		echo "the server never came up:" >&2
		cat "$log" >&2
		exit 1
	fi
	sleep 0.1
done
cat "$log"


seed="INSERT INTO accounts (code, name, kind, currency) VALUES
  ('assets.cash',          'Cash at bank',      'asset',     'GBP'),
  ('liabilities.customer', 'Customer balances', 'liability', 'GBP');"

echo
echo "\$ docker compose -f compose.test.yaml exec -T postgres psql -U ledger -d ledger_test <<'SQL'"
printf '%s\n' "$seed"
echo "SQL"
printf '%s\n' "$seed" | docker compose -f compose.test.yaml exec -T postgres psql -U ledger -d ledger_test -v ON_ERROR_STOP=1


deposit='{
  "currency": "GBP",
  "description": "Customer deposit",
  "postings": [
    {"account": "assets.cash", "amount_minor": 4500},
    {"account": "liabilities.customer", "amount_minor": -4500}
  ]
}'

post "" "$deposit"


post "Idempotency-Key: $key" "$deposit"
post "Idempotency-Key: $key" "$deposit"

get "/v1/accounts"


post "Idempotency-Key: $key" '{
  "currency": "GBP",
  "description": "Customer deposit, corrected",
  "postings": [
    {"account": "assets.cash", "amount_minor": 9000},
    {"account": "liabilities.customer", "amount_minor": -9000}
  ]
}'


kill -INT "$server"
if wait "$server"; then status=0; else status=$?; fi
server=""

exit "$status"
