#!/bin/sh
#
# Prints six steps over the HTTP API: two accounts opened, the balances, an
# entry taken, an entry refused, that entry sent again under its own key, and
# the balances after.
#
#   tools/post-demo.sh [binary] [addr]

set -eu

bin=${1:-bin/pacioli}
addr=${2:-127.0.0.1:58080}
base="http://$addr"

# Readable keys, not uuids. The refusal needs its own, or what it is refused for
# is the key already being held against a different body.
deposit_key='deposit-2026-09-07-0001'
mistyped_key='deposit-2026-09-07-0002'

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


step '1/6  an empty ledger, and a server on it'

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


step '2/6  two accounts, opened over the API'

open_account '{"code": "assets.cash", "name": "Cash at bank", "kind": "asset", "currency": "GBP"}'
echo
open_account '{"code": "liabilities.customer", "name": "Customer balances", "kind": "liability", "currency": "GBP"}'


step '3/6  the balances before: nothing posted to either'

get /v1/accounts


step '4/6  a deposit of 45.00, taken'

deposit='{
  "currency": "GBP",
  "description": "Customer deposit",
  "postings": [
    {"account": "assets.cash", "amount_minor": 4500},
    {"account": "liabilities.customer", "amount_minor": -4500}
  ]
}'

post "$deposit_key" "$deposit"


step '5/6  the same deposit with one leg mistyped, and the ledger refusing it'

post "$mistyped_key" '{
  "currency": "GBP",
  "description": "Customer deposit, one leg mistyped",
  "postings": [
    {"account": "assets.cash", "amount_minor": 4500},
    {"account": "liabilities.customer", "amount_minor": -4000}
  ]
}'


step '6/6  the deposit sent again under its key, and the balances after'

echo '  The reply carries Idempotent-Replayed and the id the first post was given.'
echo
post "$deposit_key" "$deposit"

echo
get /v1/accounts


kill -INT "$server"
if wait "$server"; then status=0; else status=$?; fi
server=""

exit "$status"
