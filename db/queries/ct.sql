-- The apexes the matcher walks to: the wildcards of active programs whose
-- rule is in force.
-- name: CTApexes :many
SELECT w.id, w.apex, w.program_id FROM wildcard w
JOIN scope_rule r ON r.id = w.rule_id
JOIN program p ON p.id = w.program_id
WHERE r.valid_to IS NULL AND p.state = 'active';

-- The watched apexes follow the set: new ones start watched now, those that
-- left go with their counts.
-- name: WatchCTApexes :exec
INSERT INTO ct_apex (wildcard_id, watched_since)
SELECT unnest(sqlc.arg(wildcards)::uuid[]), sqlc.arg(now)::timestamptz
ON CONFLICT (wildcard_id) DO NOTHING;

-- name: UnwatchCTApexes :exec
DELETE FROM ct_apex WHERE NOT (wildcard_id = ANY(sqlc.arg(wildcards)::uuid[]));

-- name: CountCTApex :exec
UPDATE ct_apex SET
  names = names + sqlc.arg(names)::bigint, wildcards = wildcards + sqlc.arg(wildcard_certs)::bigint,
  dropped = dropped + sqlc.arg(dropped)::bigint,
  last_name_at = GREATEST(last_name_at, sqlc.narg(last_name_at)::timestamptz),
  last_wildcard_at = GREATEST(last_wildcard_at, sqlc.narg(last_wildcard_at)::timestamptz)
WHERE wildcard_id = $1;

-- A host a certificate named, in scope: probed now, in the CT lane.
-- name: EnterCTLane :execrows
UPDATE asset_current SET ct_lane = true, next_resolve_at = sqlc.arg(now)::timestamptz, backoff_tier = 0
WHERE asset_id = $1 AND kind = 'fqdn' AND scope_status = 'in_scope';

-- name: NextDueCTProgram :one
SELECT ac.program_id FROM asset_current ac JOIN program p ON p.id = ac.program_id
WHERE ac.ct_lane AND p.state = 'active' AND ac.kind = 'fqdn'
  AND (ac.next_full_at <= sqlc.arg(now)::timestamptz OR ac.next_resolve_at <= sqlc.arg(now)::timestamptz)
ORDER BY LEAST(COALESCE(ac.next_full_at, 'infinity'), COALESCE(ac.next_resolve_at, 'infinity')), ac.asset_id
LIMIT 1;

-- name: DueFullCT :many
SELECT key FROM asset_current
WHERE program_id = $1 AND ct_lane AND kind = 'fqdn' AND next_full_at <= sqlc.arg(now)::timestamptz
ORDER BY next_full_at, asset_id
LIMIT sqlc.arg(lim);

-- name: DueResolveCT :many
SELECT key FROM asset_current
WHERE program_id = $1 AND ct_lane AND kind = 'fqdn' AND next_resolve_at <= sqlc.arg(now)::timestamptz
  AND (next_full_at IS NULL OR next_full_at > sqlc.arg(now)::timestamptz)
ORDER BY next_resolve_at, asset_id
LIMIT sqlc.arg(lim);

-- name: EnqueueCandidate :one
INSERT INTO run (program_id, kind, stage, targets, trigger, queued_at)
VALUES ($1, 'candidate', $2, sqlc.arg(targets)::text[], 'ct', sqlc.arg(now)::timestamptz)
ON CONFLICT (kind) WHERE kind = 'candidate' AND state IN ('queued', 'running') DO NOTHING
RETURNING *;

-- The services a full of the lane's hosts found answering HTTP: what the
-- certificate notification lists. Answering, and in this run: a dead origin
-- or a state left from before a revival is not a name serving HTTP now.
-- name: CTLiveServices :many
SELECT s.key, s.status_code, s.title FROM asset_current s
JOIN asset a ON a.id = s.asset_id
JOIN asset_current h ON h.asset_id = a.parent_asset_id
WHERE s.program_id = $1 AND s.kind = 'service' AND s.http_state = 'responding'
  AND s.last_seen >= sqlc.arg(since)::timestamptz
  AND h.ct_lane AND h.key = ANY(sqlc.arg(hosts)::text[])
ORDER BY s.key;

-- After a candidate run: a host leaves the lane once measured by a full, or
-- archived.
-- name: LeaveCTLane :execrows
UPDATE asset_current SET ct_lane = false
WHERE program_id = $1 AND ct_lane AND key = ANY(sqlc.arg(hosts)::text[])
  AND (lifecycle = 'archived' OR (sqlc.arg(after_full)::bool AND lifecycle <> 'candidate'));

-- name: StartCTSession :one
INSERT INTO ct_session (connected_at) VALUES (sqlc.arg(now)::timestamptz) RETURNING id;

-- name: CountCTSession :exec
UPDATE ct_session SET certificates = certificates + sqlc.arg(certificates)::bigint, matched = matched + sqlc.arg(matched)::bigint,
  created = created + sqlc.arg(created)::bigint, revived = revived + sqlc.arg(revived)::bigint,
  overflow = overflow + sqlc.arg(overflow)::bigint, dropped = dropped + sqlc.arg(dropped)::bigint
WHERE id = $1;

-- name: EndCTSession :exec
UPDATE ct_session SET disconnected_at = sqlc.arg(now)::timestamptz, error = sqlc.narg(error) WHERE id = $1;

-- name: LastCTSession :one
SELECT * FROM ct_session ORDER BY id DESC LIMIT 1;

-- name: EndOpenCTSessions :exec
UPDATE ct_session SET disconnected_at = sqlc.arg(now)::timestamptz, error = 'the process stopped' WHERE disconnected_at IS NULL;
