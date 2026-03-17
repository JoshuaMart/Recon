-- name: ListWebResults :many
SELECT wr.id, wr.hostname_id, wr.url,
    wr.chain->0->>'status_code' AS status_code,
    wr.chain->0->>'title' AS title,
    wr.chain,
    wr.technologies,
    wr.external_hosts,
    wr.scanned_at
FROM web_results wr
JOIN hostnames h ON h.id = wr.hostname_id
WHERE
    (sqlc.narg('wildcard_id')::UUID IS NULL OR h.wildcard_id = sqlc.narg('wildcard_id')) AND
    (sqlc.narg('hostname_id')::UUID IS NULL OR wr.hostname_id = sqlc.narg('hostname_id'))
ORDER BY wr.scanned_at DESC NULLS LAST
LIMIT $1 OFFSET $2;

-- name: CountWebResults :one
SELECT COUNT(*) FROM web_results wr
JOIN hostnames h ON h.id = wr.hostname_id
WHERE
    (sqlc.narg('wildcard_id')::UUID IS NULL OR h.wildcard_id = sqlc.narg('wildcard_id')) AND
    (sqlc.narg('hostname_id')::UUID IS NULL OR wr.hostname_id = sqlc.narg('hostname_id'));

-- name: GetWebResult :one
SELECT * FROM web_results WHERE id = $1;

-- name: GetWebResultByURL :one
SELECT * FROM web_results WHERE url = $1;

-- name: UpsertWebResult :one
INSERT INTO web_results (hostname_id, url, chain, technologies, cookies, metadata, external_hosts, scanned_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (url) DO UPDATE SET
    chain = EXCLUDED.chain,
    technologies = EXCLUDED.technologies,
    cookies = EXCLUDED.cookies,
    metadata = EXCLUDED.metadata,
    external_hosts = EXCLUDED.external_hosts,
    scanned_at = EXCLUDED.scanned_at,
    updated_at = now()
RETURNING *;
