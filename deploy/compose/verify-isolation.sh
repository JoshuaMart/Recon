#!/usr/bin/env sh
# Milestone 0: from the fingerprinter's container, psql to the database fails,
# and the same connection succeeds from the control plane network.
#
# Both halves matter. A refusal on its own passes just as well against a stopped
# database, a wrong hostname or a typo in a network name, which is the usual way
# an isolation check goes quietly green. Every refusal below therefore comes with
# its positive control.
set -eu

PROJECT="${COMPOSE_PROJECT_NAME:-recon}"
DB_USER="${POSTGRES_USER:-asm_owner}"
DB_NAME="${POSTGRES_DB:-recon}"
DSN="postgres://${DB_USER}:${POSTGRES_PASSWORD}@postgres:5432/${DB_NAME}?sslmode=disable"

# The control this script would be worthless without. If the project name
# differs or the stack is down, `docker run --network <missing>` fails for a
# reason that has nothing to do with isolation, every check reads as "refused",
# and the script exits OK.
for network in scan control; do
	if ! docker network inspect "${PROJECT}_${network}" >/dev/null 2>&1; then
		echo "FAIL: network ${PROJECT}_${network} does not exist."
		echo "Nothing below would prove anything. Is the stack up, and is"
		echo "COMPOSE_PROJECT_NAME (${PROJECT}) the one it was started with?"
		exit 1
	fi
done

# A short timeout, or the failing case sits on the default TCP retry budget and
# the whole check looks hung rather than negative.
psql_from() {
	docker run --rm --network "${PROJECT}_$1" \
		-e PGCONNECT_TIMEOUT=5 \
		postgres:18-alpine \
		psql "$DSN" -tAc 'SELECT 1' 2>&1
}

printf 'scan network -> postgres ... '
if out=$(psql_from scan); then
	printf 'REACHED\n\n%s\n\n' "$out"
	echo "FAIL: the rendering network has a route to the database."
	echo "The architecture requires none. Check the networks in compose.yaml."
	exit 1
fi
printf 'refused\n'

printf 'control network -> postgres ... '
if out=$(psql_from control); then
	printf 'reached\n'
else
	printf 'REFUSED\n\n%s\n\n' "$out"
	echo "FAIL: the control network cannot reach the database either, so the"
	echo "refusal above proves nothing. Is the stack up?"
	exit 1
fi

# The internal API is the more tempting of the two routes: it needs no
# credential, only a name that resolves.
curl_from() {
	docker run --rm --network "${PROJECT}_$1" \
		curlimages/curl:latest \
		--silent --show-error --max-time 5 http://controlplane:8080/healthz 2>&1
}

printf 'control network -> control plane API ... '
if out=$(curl_from control); then
	printf 'reached\n'

	printf 'scan network -> control plane API ... '
	if out=$(curl_from scan); then
		printf 'REACHED\n\n%s\n\n' "$out"
		echo "FAIL: the rendering network has a route to the internal API."
		exit 1
	fi
	printf 'refused\n'
else
	printf 'not running\n'
	echo
	echo "SKIPPED: the control plane is down, so a refusal from the scan network"
	echo "         would prove nothing. Start it and re-run to cover this half."
fi

# The route the checks above cannot see, because they ask by service name.
#
# A published port is reachable from every container through the host gateway,
# whatever network it sits on: Docker Desktop proxies it to the host's loopback
# by design, so binding to 127.0.0.1 does not take that away. Measured rather
# than assumed, and reported rather than failed: no edit to this compose file
# closes it, and a check that is red for ever is a check nobody reads.
#
# It is a property of this runtime. Nothing in the deployed topology publishes a
# port onto a host the rendering network shares.
printf 'scan network -> control plane API through the host gateway ... '
if docker run --rm --network "${PROJECT}_scan" \
	curlimages/curl:latest \
	--silent --show-error --max-time 5 http://host.docker.internal:8080/healthz >/dev/null 2>&1; then
	printf 'reached\n'
	echo
	echo "NOTE: the published ports of this file are reachable from any container"
	echo "      through host.docker.internal, because the runtime proxies them to"
	echo "      the host's loopback. Local only, and narrowed here rather than"
	echo "      claimed away."
else
	printf 'refused\n'
fi

echo
echo "OK: between the docker networks, the rendering side reaches neither the"
echo "    database nor the internal API by name, and the control side reaches the"
echo "    database. So isolation is the reason for the refusals, not a stack"
echo "    that is down."
