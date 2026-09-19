#!/bin/sh
#
# Prints seven steps over the HTTP API: an empty ledger, three accounts, an entry
# taken, an entry refused, a replay, the trial balance, and a clean stop.
#
#   tools/demo.sh [binary] [addr]

set -eu

bin=${1:-bin/pacioli}
addr=${2:-127.0.0.1:58080}
base="http://$addr"

# Readable keys, not uuids. Step 4 needs its own, or its refusal is a key conflict.
deposit_key='deposit-2026-09-03-0001'
mistyped_key='deposit-2026-09-03-0002'

log=$(mktemp)
server=""
cleanup() {
	[ -n "$server" ] && kill "$server" 2>/dev/null || true
	rm -f "$log"
}
trap cleanup EXIT

step() {
	echo
	echo "  == $1"
	echo
}

get() {
	echo "\$ curl -sS $base$1"
	curl -sS "$base$1"
}

open_account() {
	echo "\$ curl -sS -i -X POST $base/v1/accounts \\"
	echo "    -H 'Content-Type: application/json' \\"
	echo "    -d '$1'"
	printf '%s' "$1" | curl -sS -i -X POST "$base/v1/accounts" \
		-H 'Content-Type: application/json' --data-binary @-
}

post() {
	echo "\$ curl -sS -i -X POST $base/v1/transactions \\"
	echo "    -H 'Idempotency-Key: $1' --data-binary @- <<'JSON'"
	printf '%s\n' "$2"
	echo "JSON"
	printf '%s' "$2" | curl -sS -i -X POST "$base/v1/transactions" \
		-H 'Content-Type: application/json' -H "Idempotency-Key: $1" --data-binary @-
}


step '1/7  an empty ledger, and a server on it'

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

echo
get /v1/trial-balance


step '2/7  three accounts, opened over the API'

open_account '{"code": "assets.cash", "name": "Cash at bank", "kind": "asset", "currency": "GBP"}'
echo
open_account '{"code": "liabilities.customer", "name": "Customer balances", "kind": "liability", "currency": "GBP"}'
echo
open_account '{"code": "revenue.fees", "name": "Fee income", "kind": "revenue", "currency": "GBP"}'


step '3/7  a deposit of 45.00, less a 1.50 fee'

deposit='{
  "currency": "GBP",
  "description": "Customer deposit, less fee",
  "postings": [
    {"account": "assets.cash", "amount_minor": 4500},
    {"account": "liabilities.customer", "amount_minor": -4350},
    {"account": "revenue.fees", "amount_minor": -150}
  ]
}'

post "$deposit_key" "$deposit"


step '4/7  the same deposit with the fee mistyped, and the ledger refusing it'

post "$mistyped_key" '{
  "currency": "GBP",
  "description": "Customer deposit, fee mistyped",
  "postings": [
    {"account": "assets.cash", "amount_minor": 4500},
    {"account": "liabilities.customer", "amount_minor": -4350},
    {"account": "revenue.fees", "amount_minor": -1500}
  ]
}'


step '5/7  the deposit sent again, under the key it was sent with'

post "$deposit_key" "$deposit"


step '6/7  the trial balance'

get /v1/trial-balance


step '7/7  the server stops, and finishes what it accepted first'

echo "\$ kill -INT \"\$server\"; wait \"\$server\"; echo \$?"
kill -INT "$server"
if wait "$server"; then status=0; else status=$?; fi
server=""
tail -n +3 "$log"
printf '%s\n' "$status"

exit "$status"
