-- name: EnqueueRun :one
INSERT INTO run (program_id, kind, stage, wildcard_id, trigger, queued_at)
VALUES ($1, $2, $3, sqlc.narg(wildcard_id), $4, sqlc.arg(now)::timestamptz)
ON CONFLICT (wildcard_id) WHERE state IN ('queued', 'running') DO NOTHING
RETURNING *;

-- name: ClaimQueuedRun :one
UPDATE run SET state = 'running', started_at = sqlc.arg(now)::timestamptz, deadline = sqlc.arg(deadline)::timestamptz
WHERE id = (
  SELECT r.id FROM run r JOIN program p ON p.id = r.program_id
  WHERE r.state = 'queued' AND p.state = 'active'
  ORDER BY r.queued_at, r.id
  LIMIT 1 FOR UPDATE OF r SKIP LOCKED
)
RETURNING *;

-- name: CountRunning :one
SELECT count(*)::int FROM run WHERE state = 'running';

-- name: RequeueRun :exec
UPDATE run SET state = 'queued', started_at = NULL, deadline = NULL WHERE id = $1 AND state = 'running';

-- name: FinishRun :execrows
UPDATE run
SET state = sqlc.arg(state), finished_at = sqlc.arg(now)::timestamptz, error = sqlc.narg(error),
    report_meta = sqlc.narg(report_meta)::jsonb, ingest = sqlc.narg(ingest)::jsonb
WHERE id = sqlc.arg(id) AND state = 'running';

-- name: TimeOutRuns :many
UPDATE run SET state = 'timed_out', finished_at = sqlc.arg(now)::timestamptz, error = 'deadline passed'
WHERE state = 'running' AND deadline < sqlc.arg(now)::timestamptz
RETURNING id;

-- name: FailInterruptedRuns :execrows
UPDATE run SET state = 'failed', finished_at = sqlc.arg(now)::timestamptz, error = 'interrupted: the process stopped during the run'
WHERE state = 'running';

-- name: GetRun :one
SELECT * FROM run WHERE id = $1;

-- name: ListRuns :many
SELECT r.*, w.apex FROM run r LEFT JOIN wildcard w ON w.id = r.wildcard_id
WHERE (sqlc.narg(program_id)::uuid IS NULL OR r.program_id = sqlc.narg(program_id))
ORDER BY r.queued_at DESC, r.id DESC
LIMIT sqlc.arg(lim);

-- name: ListOpenRules :many
SELECT * FROM scope_rule WHERE program_id = $1 AND valid_to IS NULL;

-- name: GetWildcard :one
SELECT w.*, (r.valid_to IS NULL)::bool AS rule_in_force FROM wildcard w JOIN scope_rule r ON r.id = w.rule_id WHERE w.id = $1;

-- name: OldestWildcards :many
SELECT w.id, w.program_id FROM wildcard w
JOIN scope_rule r ON r.id = w.rule_id
JOIN program p ON p.id = w.program_id
WHERE r.valid_to IS NULL AND p.state = 'active'
  AND (sqlc.narg(program_id)::uuid IS NULL OR w.program_id = sqlc.narg(program_id))
  AND NOT EXISTS (SELECT 1 FROM run x WHERE x.wildcard_id = w.id AND x.state IN ('queued', 'running'))
ORDER BY w.last_recon_at NULLS FIRST, w.created_at, w.id
LIMIT sqlc.arg(lim);

-- name: NewWildcards :many
SELECT w.id FROM wildcard w WHERE w.program_id = $1 AND w.created_at = sqlc.arg(now)::timestamptz;

-- name: MarkWildcardRecon :exec
UPDATE wildcard SET last_recon_at = sqlc.arg(now)::timestamptz, last_recon_run = sqlc.arg(run_id),
  assets_found = (SELECT count(*) FROM asset WHERE asset.wildcard_id = wildcard.id)
WHERE wildcard.id = sqlc.arg(id);
