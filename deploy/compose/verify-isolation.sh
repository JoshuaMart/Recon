#!/bin/sh
# Checks the two network properties of the stack, each next to a probe that must
# succeed, so a missing tool cannot pass for isolation.
#
#   the fingerprinter reaches recon, and cannot reach postgres
#   postgres reaches recon, and cannot reach the internet
set -u
compose="docker compose --env-file .env -f deploy/compose/compose.yaml"
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
exit $fail
