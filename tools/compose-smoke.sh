#!/bin/sh
#
# Runs the README's two commands from nothing: `docker compose up` on a project
# with no containers and no volume, then the curl it shows, which has to answer
# an empty trial balance. Everything it started is removed however it ends.
#
# The curl address is read out of README.md, and the run fails if the README
# stops showing either command, so the two cannot drift apart.
#
#   tools/compose-smoke.sh

set -eu

# A project of its own, so a volume left by your own `docker compose up` is
# never the one this deletes.
compose="docker compose -p pacioli-smoke"

grep -qx '    docker compose up' README.md || {
	echo "README.md no longer shows '    docker compose up'; change tools/compose-smoke.sh with it" >&2
	exit 1
}
url=$(sed -n 's|^    curl -s \(localhost:[0-9]*/v1/trial-balance\)$|\1|p' README.md)
[ -n "$url" ] || {
	echo "README.md no longer shows '    curl -s localhost:<port>/v1/trial-balance'; change tools/compose-smoke.sh with it" >&2
	exit 1
}

cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		$compose logs --no-color >&2 || true
	fi
	$compose down --volumes --remove-orphans
	exit "$status"
}
trap cleanup EXIT

$compose down --volumes --remove-orphans

echo "\$ docker compose up --detach --wait"
$compose up --detach --wait --build

# The ledger has no healthcheck, so --wait returns once it is running, which can
# be a moment before it listens.
echo "\$ curl -s $url"
body=$(curl -sSf --retry 30 --retry-delay 1 --retry-all-errors "$url")
printf '%s\n' "$body"

if [ "$(printf '%s' "$body" | tr -d ' \n')" != '{"trial":[]}' ]; then
	echo 'a cold ledger should answer {"trial": []}' >&2
	exit 1
fi
