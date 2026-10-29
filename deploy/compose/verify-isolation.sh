#!/bin/sh
# Checks the network properties of the stack, each next to a probe that must
# succeed, so a missing tool cannot pass for isolation.
#
#   the fingerprinter reaches recon, and cannot reach postgres
#   postgres reaches recon, and cannot reach the internet
#   chrome reaches the internet and the fingerprinter, and neither recon nor postgres
#
# The production stack: RECON_COMPOSE=deploy/compose/compose.prod.yaml
set -u
compose="docker compose --env-file .env -f ${RECON_COMPOSE:-deploy/compose/compose.yaml}"
fail=0

probe() { # service, description, expected (yes|no), command
	if $compose exec -T "$1" sh -c "$4" >/dev/null 2>&1; then got=yes; else got=no; fi
	if [ "$got" = "$3" ]; then
		echo "ok    $1: $2"
	else
		echo "FAIL  $1: $2 (reachable: $got, want $3)"
		fail=1
	fi
}

probe fingerprinter "reaches recon" yes "nc -z -w 3 recon 8080"
probe fingerprinter "cannot reach postgres" no "nc -z -w 3 postgres 5432"
probe postgres "reaches recon" yes "wget -q -T 3 -O /dev/null http://recon:8080/api/health"
probe postgres "cannot reach the internet" no "wget -q -T 3 -O /dev/null https://example.com"
# Chrome runs code a target wrote: it has bash and no network tool.
probe chrome-1 "reaches the internet" yes "timeout 5 bash -c '</dev/tcp/example.com/443'"
probe chrome-1 "reaches the fingerprinter" yes "timeout 3 bash -c '</dev/tcp/fingerprinter/3001'"
probe chrome-1 "cannot reach recon" no "timeout 3 bash -c '</dev/tcp/recon/8080'"
probe chrome-1 "cannot reach postgres" no "timeout 3 bash -c '</dev/tcp/postgres/5432'"
exit $fail
