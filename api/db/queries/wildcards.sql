-- name: ListWildcards :many
SELECT w.*,
    COUNT(h.id) AS hostnames_total,
    COUNT(h.id) FILTER (WHERE h.status = 'alive') AS hostnames_alive,
    COUNT(h.id) FILTER (WHERE h.status = 'dead') AS hostnames_dead,
    COUNT(h.id) FILTER (WHERE h.status = 'unreachable') AS hostnames_unreachable,
    COUNT(h.id) FILTER (WHERE h.type = 'web') AS web_services
FROM wildcards w
LEFT JOIN hostnames h ON h.wildcard_id = w.id
GROUP BY w.id
ORDER BY w.created_at DESC;

-- name: GetWildcard :one
SELECT * FROM wildcards WHERE id = $1;

-- name: GetActiveWildcards :many
SELECT * FROM wildcards WHERE active = true ORDER BY created_at;

-- name: InsertWildcard :one
INSERT INTO wildcards (value) VALUES ($1) RETURNING *;

-- name: DeleteWildcard :exec
DELETE FROM wildcards WHERE id = $1;

-- name: UpdateLastReconAt :exec
UPDATE wildcards SET last_recon_at = now() WHERE id = $1;

-- name: UpdateLastRevalidatedAt :exec
UPDATE wildcards SET last_revalidated_at = now() WHERE id = $1;
