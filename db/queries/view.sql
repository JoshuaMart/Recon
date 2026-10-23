-- name: GetAsset :one
SELECT * FROM asset WHERE id = $1;

-- name: GetTriage :one
SELECT * FROM triage WHERE asset_id = $1;

-- name: PutTriage :one
INSERT INTO triage (asset_id, state, tags, note, seen_at)
VALUES ($1, $2, sqlc.arg(tags)::text[], sqlc.narg(note), sqlc.narg(seen_at))
ON CONFLICT (asset_id) DO UPDATE
SET state = EXCLUDED.state, tags = EXCLUDED.tags, note = EXCLUDED.note, seen_at = EXCLUDED.seen_at
RETURNING *;

-- How many other assets share a value, and across how many programs.
-- name: PivotFavicon :one
SELECT count(*) FILTER (WHERE asset_id <> sqlc.arg(self))::int AS others, count(DISTINCT program_id)::int AS programs
FROM asset_current WHERE favicon_hash = sqlc.arg(value)::text;

-- name: PivotScript :one
SELECT count(*) FILTER (WHERE asset_id <> sqlc.arg(self))::int AS others, count(DISTINCT program_id)::int AS programs
FROM asset_current WHERE sqlc.arg(value)::text = ANY(script_hashes);

-- name: PivotCookie :one
SELECT count(*) FILTER (WHERE asset_id <> sqlc.arg(self))::int AS others, count(DISTINCT program_id)::int AS programs
FROM asset_current WHERE sqlc.arg(value)::text = ANY(cookie_names);

-- name: ListChildren :many
SELECT ac.asset_id, ac.kind, ac.key, ac.lifecycle, ac.status_code, ac.title
FROM asset a JOIN asset_current ac ON ac.asset_id = a.id
WHERE a.parent_asset_id = $1 ORDER BY ac.key;
