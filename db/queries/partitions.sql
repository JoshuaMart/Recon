-- name: EnsureMonthPartition :exec
SELECT ensure_month_partition(sqlc.arg(parent)::text, (sqlc.arg(month)::timestamptz)::date);
