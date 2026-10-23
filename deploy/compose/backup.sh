#!/bin/sh
# Database backups, run by the backup service of the compose stack.
#
#   backup.sh loop          a dump every day at BACKUP_HOUR (UTC), pruned after BACKUP_RETENTION_DAYS
#   backup.sh once          one dump now, prints its path
#   backup.sh verify FILE   restore FILE into a scratch database, check it, drop it
#   backup.sh restore FILE  restore FILE over the live database
#
# Connection settings come from the standard PG* variables.
set -eu

dir=${BACKUP_DIR:-/backups}
retention=${BACKUP_RETENTION_DAYS:-14}
hour=$(printf '%02d' "${BACKUP_HOUR:-3}")
scratch=recon_restore_check

once() {
	mkdir -p "$dir"
	file="$dir/recon-$(date -u +%Y%m%dT%H%M%SZ).dump"
	# Written under a temporary name, so a dump cut short is never taken for a good one.
	pg_dump --format=custom --file="$file.part" "$PGDATABASE"
	mv "$file.part" "$file"
	find "$dir" -name 'recon-*.dump' -mtime +"$retention" -delete
	echo "$file"
}

verify() {
	dropdb --if-exists "$scratch"
	createdb "$scratch"
	trap 'dropdb --if-exists "$scratch"' EXIT
	pg_restore --no-owner --exit-on-error --dbname="$scratch" "$1"
	version=$(psql -XAtq --dbname="$scratch" -c 'SELECT max(version_id) FROM goose_db_version')
	created=$(psql -XAtq --dbname="$scratch" -c 'SELECT created_at FROM instance')
	if [ -z "$version" ] || [ -z "$created" ]; then
		echo "restore check failed: schema version '$version', instance '$created'" >&2
		exit 1
	fi
	echo "restore ok: schema version $version, instance created $created"
}

restore() {
	pg_restore --clean --if-exists --no-owner --exit-on-error --dbname="$PGDATABASE" "$1"
	echo "restored $1 into $PGDATABASE"
}

loop() {
	last=""
	while :; do
		today=$(date -u +%F)
		if [ "$(date -u +%H)" = "$hour" ] && [ "$today" != "$last" ]; then
			if once; then last=$today; fi
		fi
		sleep 60
	done
}

case "${1:-}" in
loop) loop ;;
once) once ;;
verify) verify "${2:?verify needs a file}" ;;
restore) restore "${2:?restore needs a file}" ;;
*)
	echo "usage: backup.sh loop|once|verify FILE|restore FILE" >&2
	exit 2
	;;
esac
