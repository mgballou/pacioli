#!/bin/sh
#
# Prints the binary serving on a real socket: the startup log, a chart and one
# entry put in over the API, four request and response exchanges, and a clean
# stop when it is signalled.
#
#   tools/serve-demo.sh [binary] [addr]

set -eu

bin=${1:-bin/pacioli}
addr=${2:-127.0.0.1:58080}
base="http://$addr"

deposit_key='deposit-2026-09-03-0001'

log=$(mktemp)
server=""
cleanup() {
	[ -n "$server" ] && kill "$server" 2>/dev/null || true
	rm -f "$log"
}
trap cleanup EXIT

exchange() {
	echo
	echo "\$ curl -sS -i '$base$1'"
	curl -sS -i "$base$1"
}

open_account() {
	echo
	echo "\$ curl -sS -i -X POST $base/v1/accounts \\"
	echo "    -H 'Content-Type: application/json' \\"
	echo "    -d '$1'"
	printf '%s' "$1" | curl -sS -i -X POST "$base/v1/accounts" \
		-H 'Content-Type: application/json' --data-binary @-
}

post() {
	echo
	echo "\$ curl -sS -i -X POST $base/v1/transactions \\"
	echo "    -H 'Idempotency-Key: $1' --data-binary @- <<'JSON'"
	printf '%s\n' "$2"
	echo "JSON"
	printf '%s' "$2" | curl -sS -i -X POST "$base/v1/transactions" \
		-H 'Content-Type: application/json' -H "Idempotency-Key: $1" --data-binary @-
}


echo "\$ $bin serve -addr $addr &"
"$bin" serve -addr "$addr" 2>"$log" &
server=$!

# Ready is two things: the socket answers and the schema line is in the log.
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


open_account '{"code": "assets.cash", "name": "Cash at bank", "kind": "asset", "currency": "GBP"}'
open_account '{"code": "liabilities.customer", "name": "Customer balances", "kind": "liability", "currency": "GBP"}'

post "$deposit_key" '{
  "currency": "GBP",
  "description": "Customer deposit",
  "postings": [
    {"account": "assets.cash", "amount_minor": 4500},
    {"account": "liabilities.customer", "amount_minor": -4500}
  ]
}'


exchange "/v1/accounts/assets.cash"
exchange "/v1/accounts/assets.csah"
exchange "/v1/accounts?kind=asset"
exchange "/v1/trial-balance"


echo
echo "\$ kill -INT \"\$server\"; wait \"\$server\"; echo \$?"
kill -INT "$server"
if wait "$server"; then status=0; else status=$?; fi
server=""
tail -n +3 "$log"
printf '%s\n' "$status"

exit "$status"
