-- name: ListSavedSearches :many
SELECT * FROM saved_search ORDER BY name;

-- name: CreateSavedSearch :one
INSERT INTO saved_search (name, query, view, alert, created_at, updated_at)
VALUES ($1, $2, $3, $4, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
RETURNING *;

-- name: GetSavedSearch :one
SELECT * FROM saved_search WHERE id = $1 FOR UPDATE;

-- name: UpdateSavedSearch :one
UPDATE saved_search SET name = $2, query = $3, view = $4, alert = $5, updated_at = sqlc.arg(now)::timestamptz
WHERE id = $1 RETURNING *;

-- name: DeleteSavedSearch :execrows
DELETE FROM saved_search WHERE id = $1;
