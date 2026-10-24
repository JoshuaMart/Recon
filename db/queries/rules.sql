-- name: ListRules :many
SELECT * FROM scope_rule WHERE program_id = $1 ORDER BY valid_from, id;

-- name: ListOpenRulesByOrigin :many
SELECT * FROM scope_rule WHERE program_id = $1 AND origin = $2 AND valid_to IS NULL;

-- name: GetRule :one
SELECT * FROM scope_rule WHERE program_id = $1 AND id = $2;

-- OpenRule returns no row when the same rule is already open.
-- name: OpenRule :one
INSERT INTO scope_rule (program_id, kind, matcher, pattern, origin, note, valid_from)
VALUES ($1, $2, $3, $4, $5, sqlc.narg(note), sqlc.arg(now)::timestamptz)
ON CONFLICT (program_id, kind, matcher, pattern, origin) WHERE valid_to IS NULL DO NOTHING
RETURNING *;

-- name: CloseRule :execrows
UPDATE scope_rule SET valid_to = sqlc.arg(now)::timestamptz WHERE id = $1 AND valid_to IS NULL;
