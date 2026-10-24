-- An event joins the open notification of its group, or opens one due at the
-- end of its window. Locking the row keeps two events from opening two.
-- name: OpenNotification :one
SELECT id FROM notification
WHERE group_key = $1 AND sent_at IS NULL AND due_at > sqlc.arg(now)::timestamptz
ORDER BY id DESC LIMIT 1 FOR UPDATE;

-- name: AppendNotification :exec
UPDATE notification
SET items = (items || sqlc.arg(items)::text[])[1:sqlc.arg(cap)::int],
    total = total + sqlc.arg(n)::int, title = sqlc.arg(title)
WHERE id = $1;

-- name: InsertNotification :exec
INSERT INTO notification (kind, group_key, title, items, total, link, created_at, due_at)
VALUES ($1, $2, $3, (sqlc.arg(items)::text[])[1:sqlc.arg(cap)::int], sqlc.arg(n)::int, sqlc.narg(link), sqlc.arg(now)::timestamptz, sqlc.arg(due)::timestamptz);

-- name: DueNotifications :many
SELECT * FROM notification WHERE sent_at IS NULL AND due_at <= sqlc.arg(now)::timestamptz
ORDER BY due_at, id LIMIT 20 FOR UPDATE SKIP LOCKED;

-- name: NotificationSent :exec
UPDATE notification SET sent_at = sqlc.arg(now)::timestamptz, outcome = $2, attempts = attempts + 1, last_error = sqlc.narg(error)
WHERE id = $1;

-- name: NotificationDelivered :exec
UPDATE notification SET delivered = array_append(delivered, sqlc.arg(channel)::text)
WHERE id = $1 AND NOT (sqlc.arg(channel)::text = ANY(delivered));

-- name: NotificationRetry :exec
UPDATE notification SET due_at = sqlc.arg(due)::timestamptz, attempts = attempts + 1, last_error = sqlc.narg(error)
WHERE id = $1;

-- name: RecentNotifications :many
SELECT * FROM notification ORDER BY created_at DESC, id DESC LIMIT 30;

-- name: AlertingSearches :many
SELECT * FROM saved_search WHERE alert ORDER BY id;

-- The matches now: new ones are returned, gone ones deleted.
-- name: AddMatches :many
INSERT INTO saved_search_match (saved_search_id, asset_id, since)
SELECT sqlc.arg(search_id), unnest(sqlc.arg(assets)::uuid[]), sqlc.arg(now)::timestamptz
ON CONFLICT DO NOTHING
RETURNING asset_id;

-- name: DropMatches :exec
DELETE FROM saved_search_match WHERE saved_search_id = sqlc.arg(search_id) AND NOT (asset_id = ANY(sqlc.arg(assets)::uuid[]));

-- name: SearchEvaluated :exec
UPDATE saved_search SET evaluated_at = sqlc.arg(now)::timestamptz WHERE id = $1;

-- name: MatchLabels :many
SELECT ac.asset_id, ac.kind, ac.key, ac.status_code, ac.title FROM asset_current ac WHERE ac.asset_id = ANY(sqlc.arg(assets)::uuid[])
ORDER BY ac.key;

-- name: GetHealthState :one
SELECT * FROM health_state WHERE component = $1;

-- name: PutHealthState :exec
INSERT INTO health_state (component, failing_since, announced, detail) VALUES ($1, sqlc.narg(failing_since), $2, sqlc.narg(detail))
ON CONFLICT (component) DO UPDATE SET failing_since = EXCLUDED.failing_since, announced = EXCLUDED.announced, detail = EXCLUDED.detail;
