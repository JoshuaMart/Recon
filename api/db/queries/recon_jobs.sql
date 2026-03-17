-- name: ListJobs :many
SELECT * FROM recon_jobs
WHERE
    (sqlc.narg('wildcard_id')::UUID IS NULL OR wildcard_id = sqlc.narg('wildcard_id')) AND
    (sqlc.narg('status')::job_status IS NULL OR status = sqlc.narg('status'))
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountJobs :one
SELECT COUNT(*) FROM recon_jobs
WHERE
    (sqlc.narg('wildcard_id')::UUID IS NULL OR wildcard_id = sqlc.narg('wildcard_id')) AND
    (sqlc.narg('status')::job_status IS NULL OR status = sqlc.narg('status'));

-- name: GetJob :one
SELECT * FROM recon_jobs WHERE id = $1;

-- name: InsertJob :one
INSERT INTO recon_jobs (wildcard_id, mode, status)
VALUES ($1, $2, 'pending')
RETURNING *;

-- name: UpdateJobStatus :exec
UPDATE recon_jobs SET status = $2::job_status,
    started_at = CASE WHEN $2 = 'running' THEN now() ELSE started_at END,
    completed_at = CASE WHEN $2 IN ('completed', 'failed') THEN now() ELSE completed_at END
WHERE id = $1;

-- name: UpdateJobScalewayID :exec
UPDATE recon_jobs SET scaleway_job_id = $2 WHERE id = $1;

-- name: CountActiveJobs :one
SELECT COUNT(*) FROM recon_jobs WHERE status IN ('pending', 'running');

-- name: HasActiveJobForWildcard :one
SELECT EXISTS(
    SELECT 1 FROM recon_jobs WHERE wildcard_id = $1 AND status IN ('pending', 'running')
) AS has_active;
