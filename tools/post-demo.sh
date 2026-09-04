#!/bin/sh
#
# Prints a transaction taken over HTTP and one refused, with the account balances
# either side of them.
#
#   tools/post-demo.sh [binary] [addr]

set -eu

bin=${1:-bin/pacioli}
addr=${2:-127.0.0.1:58080}
base="http://$addr"

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

post() {
	echo
	echo "\$ curl -sS -i -X POST '$base/v1/transactions' --data-binary @- <<'JSON'"
	printf '%s\n' "$1"
	echo "JSON"
	printf '%s' "$1" | curl -sS -i -X POST "$base/v1/transactions" \
		-H 'Content-Type: application/json' --data-binary @-
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


# The chart goes in over psql: there is no endpoint that opens an account yet.
seed="INSERT INTO accounts (code, name, kind, currency) VALUES
  ('assets.cash',          'Cash at bank',      'asset',     'GBP'),
  ('liabilities.customer', 'Customer balances', 'liability', 'GBP');"

echo
echo "\$ docker compose -f compose.test.yaml exec -T postgres psql -U ledger -d ledger_test <<'SQL'"
printf '%s\n' "$seed"
echo "SQL"
printf '%s\n' "$seed" | docker compose -f compose.test.yaml exec -T postgres psql -U ledger -d ledger_test -v ON_ERROR_STOP=1

get "/v1/accounts"


post '{
  "currency": "GBP",
  "description": "Customer deposit",
  "postings": [
    {"account": "assets.cash", "amount_minor": 4500},
    {"account": "liabilities.customer", "amount_minor": -4500}
  ]
}'

post '{
  "currency": "GBP",
  "description": "Customer deposit, one leg mistyped",
  "postings": [
    {"account": "assets.cash", "amount_minor": 4500},
    {"account": "liabilities.customer", "amount_minor": -4000}
  ]
}'


get "/v1/accounts"


kill -INT "$server"
if wait "$server"; then status=0; else status=$?; fi
server=""

exit "$status"
