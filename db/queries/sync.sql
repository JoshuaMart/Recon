-- name: GetSyncState :one
SELECT * FROM scope_sync;

-- name: SyncStarted :exec
UPDATE scope_sync SET last_started_at = sqlc.arg(now)::timestamptz;

-- name: SyncSucceeded :exec
UPDATE scope_sync SET last_success_at = sqlc.arg(now)::timestamptz, last_summary = sqlc.arg(summary)::jsonb;

-- name: SyncFailed :exec
UPDATE scope_sync SET last_error = sqlc.arg(error)::text, last_error_at = sqlc.arg(now)::timestamptz;

-- Serializes sync passes across the timer and a manual trigger.
-- name: TryLockSync :one
SELECT pg_try_advisory_xact_lock(hashtext('recon.scope_sync'));
