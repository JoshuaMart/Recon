-- name: ListSavedSearches :many
SELECT * FROM saved_search ORDER BY name;

-- name: CreateSavedSearch :one
INSERT INTO saved_search (name, query, view, alert, created_at, updated_at)
VALUES ($1, $2, $3, $4, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
RETURNING *;

-- name: GetSavedSearch :one
SELECT * FROM saved_search WHERE id = $1 FOR UPDATE;

-- Switching the alert on, or changing what an alerting search asks, starts it
-- over: the next evaluation records the matches without notifying.
-- name: UpdateSavedSearch :one
UPDATE saved_search SET name = $2, query = $3, view = $4, alert = $5, updated_at = sqlc.arg(now)::timestamptz,
  evaluated_at = CASE WHEN $5 AND (NOT alert OR query <> $3 OR view <> $4) THEN NULL ELSE evaluated_at END
WHERE id = $1 RETURNING *;

-- name: ClearMatches :exec
DELETE FROM saved_search_match WHERE saved_search_id = $1;

-- name: DeleteSavedSearch :execrows
DELETE FROM saved_search WHERE id = $1;
