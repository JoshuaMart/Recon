-- name: EnqueueFingerprint :one
INSERT INTO fingerprint_queue (url, hostname_id, source)
VALUES ($1, $2, $3)
RETURNING *;

-- name: DequeueFingerprint :one
UPDATE fingerprint_queue
SET status = 'processing', updated_at = now()
WHERE id = (
    SELECT id FROM fingerprint_queue
    WHERE status = 'pending'
    ORDER BY created_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: MarkFingerprintDone :exec
UPDATE fingerprint_queue SET status = 'done', updated_at = now() WHERE id = $1;

-- name: MarkFingerprintFailed :exec
UPDATE fingerprint_queue SET status = 'failed', retry_count = retry_count + 1, updated_at = now() WHERE id = $1;

-- name: CountFingerprintsByStatus :many
SELECT status, COUNT(*) AS count FROM fingerprint_queue GROUP BY status;
