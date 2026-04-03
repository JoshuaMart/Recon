-- name: ListHostnames :many
SELECT id, wildcard_id, fqdn, ip, cdn, status, type, ports, last_seen_at, created_at, updated_at
FROM hostnames
WHERE
    (sqlc.narg('wildcard_id')::UUID IS NULL OR wildcard_id = sqlc.narg('wildcard_id')) AND
    (sqlc.narg('status')::hostname_status IS NULL OR status = sqlc.narg('status')) AND
    (sqlc.narg('type')::hostname_type IS NULL OR type = sqlc.narg('type')) AND
    (sqlc.narg('port')::TEXT IS NULL OR (ports->'tcp') ? sqlc.narg('port'))
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountHostnames :one
SELECT COUNT(*) FROM hostnames
WHERE
    (sqlc.narg('wildcard_id')::UUID IS NULL OR wildcard_id = sqlc.narg('wildcard_id')) AND
    (sqlc.narg('status')::hostname_status IS NULL OR status = sqlc.narg('status')) AND
    (sqlc.narg('type')::hostname_type IS NULL OR type = sqlc.narg('type')) AND
    (sqlc.narg('port')::TEXT IS NULL OR (ports->'tcp') ? sqlc.narg('port'));

-- name: GetHostname :one
SELECT * FROM hostnames WHERE id = $1;

-- name: GetHostnameByFQDN :one
SELECT * FROM hostnames WHERE fqdn = $1;

-- name: UpsertHostname :one
INSERT INTO hostnames (wildcard_id, fqdn, ip, cdn, status, type, dns, ports, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (fqdn) DO UPDATE SET
    ip = EXCLUDED.ip,
    cdn = EXCLUDED.cdn,
    status = EXCLUDED.status,
    type = EXCLUDED.type,
    dns = EXCLUDED.dns,
    ports = EXCLUDED.ports,
    last_seen_at = EXCLUDED.last_seen_at,
    updated_at = now()
RETURNING *;

-- name: UpdateHostnameStatus :exec
UPDATE hostnames SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdateHostnameLastSeen :exec
UPDATE hostnames SET last_seen_at = now(), status = 'alive', updated_at = now() WHERE id = $1;

-- name: ListHostnamesByWildcard :many
SELECT * FROM hostnames WHERE wildcard_id = $1 ORDER BY created_at;
