#!/bin/sh
#
# Prints the binary serving on a real socket: the startup log, four request and
# response exchanges, and a clean stop when it is signalled.
#
#   tools/serve-demo.sh [binary] [addr]

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

exchange() {
	echo
	echo "\$ curl -sS -i '$base$1'"
	curl -sS -i "$base$1"
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


# One psql transaction, because the balance trigger is checked at commit.
seed=$(cat <<'SQL'
BEGIN;
INSERT INTO accounts (code, name, kind, currency) VALUES
  ('assets.cash',          'Cash at bank',      'asset',     'GBP'),
  ('liabilities.customer', 'Customer balances', 'liability', 'GBP');
INSERT INTO transactions (id, currency, description)
  VALUES ('11111111-1111-1111-1111-111111111111', 'GBP', 'Customer deposit');
INSERT INTO postings (transaction_id, account_id, currency, amount_minor)
  SELECT '11111111-1111-1111-1111-111111111111', id, 'GBP',
         CASE code WHEN 'assets.cash' THEN 4500 ELSE -4500 END
    FROM accounts;
COMMIT;
SQL
)

echo
echo "\$ docker compose -f compose.test.yaml exec -T postgres psql -U ledger -d ledger_test <<'SQL'"
printf '%s\n' "$seed"
echo "SQL"
printf '%s\n' "$seed" | docker compose -f compose.test.yaml exec -T postgres psql -U ledger -d ledger_test -v ON_ERROR_STOP=1


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
