-- name: UpsertUntracked :exec
INSERT INTO untracked_scope (program_id, value, type, reason, first_seen_at, last_seen_at)
VALUES ($1, $2, $3, $4, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (program_id, value, type)
DO UPDATE SET reason = EXCLUDED.reason, last_seen_at = EXCLUDED.last_seen_at, gone_at = NULL;

-- name: MarkUntrackedGone :exec
UPDATE untracked_scope SET gone_at = sqlc.arg(now)::timestamptz
WHERE program_id = $1 AND last_seen_at < sqlc.arg(now)::timestamptz AND gone_at IS NULL;

-- name: ListUntracked :many
SELECT * FROM untracked_scope WHERE program_id = $1 AND gone_at IS NULL ORDER BY type, value;
