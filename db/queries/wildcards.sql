-- name: CreateWildcard :exec
INSERT INTO wildcard (program_id, rule_id, apex, created_at)
VALUES ($1, $2, $3, sqlc.arg(now)::timestamptz)
ON CONFLICT (rule_id) DO NOTHING;

-- name: ListWildcards :many
SELECT w.*, p.name AS program_name, p.platform, p.state AS program_state,
  (r.valid_to IS NULL AND p.state = 'active')::bool AS active,
  COALESCE(p.auto_scan, sqlc.arg(default_auto_scan)::text)::text AS auto_scan,
  ca.watched_since AS ct_watched_since, ca.names AS ct_names, ca.wildcards AS ct_wildcards,
  ca.dropped AS ct_dropped, ca.last_name_at AS ct_last_name_at, ca.last_wildcard_at AS ct_last_wildcard_at
FROM wildcard w
JOIN program p ON p.id = w.program_id
JOIN scope_rule r ON r.id = w.rule_id
LEFT JOIN ct_apex ca ON ca.wildcard_id = w.id
WHERE (sqlc.narg(program_id)::uuid IS NULL OR w.program_id = sqlc.narg(program_id))
  AND (sqlc.arg(include_inactive)::bool OR (r.valid_to IS NULL AND p.state = 'active'))
ORDER BY w.last_recon_at NULLS FIRST, w.created_at, w.id;
