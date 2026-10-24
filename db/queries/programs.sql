-- name: ListPrograms :many
SELECT sqlc.embed(p),
  (SELECT count(*) FROM scope_rule r WHERE r.program_id = p.id AND r.valid_to IS NULL)::int AS open_rules,
  (SELECT count(*) FROM wildcard w JOIN scope_rule r ON r.id = w.rule_id
     WHERE w.program_id = p.id AND r.valid_to IS NULL)::int AS wildcards
FROM program p
ORDER BY lower(p.name), p.id;

-- name: GetProgram :one
SELECT * FROM program WHERE id = $1;

-- name: CreateManualProgram :one
INSERT INTO program (origin, platform, name, auto_scan, created_at, updated_at)
VALUES ('manual', sqlc.narg(platform), $1, sqlc.narg(auto_scan), sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
RETURNING *;

-- name: UpdateProgram :one
UPDATE program
SET name = $2, state = $3, archived_by = sqlc.narg(archived_by), auto_scan = sqlc.narg(auto_scan), updated_at = sqlc.arg(now)::timestamptz
WHERE id = $1
RETURNING *;

-- name: ListSyncedPrograms :many
SELECT * FROM program WHERE origin = 'scopesextractor' FOR UPDATE;

-- name: UpsertSyncedProgram :one
INSERT INTO program (origin, platform, slug, name, bounty, created_at, updated_at)
VALUES ('scopesextractor', $1, $2, $3, $4, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (platform, slug) WHERE origin = 'scopesextractor'
DO UPDATE SET name = EXCLUDED.name, bounty = EXCLUDED.bounty,
  updated_at = CASE WHEN program.name IS DISTINCT FROM EXCLUDED.name OR program.bounty IS DISTINCT FROM EXCLUDED.bounty
                    THEN EXCLUDED.updated_at ELSE program.updated_at END
RETURNING *;

-- name: SetProgramState :exec
UPDATE program SET state = $2, archived_by = sqlc.narg(archived_by), updated_at = sqlc.arg(now)::timestamptz WHERE id = $1;
