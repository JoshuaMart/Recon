-- Ingestion writes. The budget is a few round trips per observation, so what
-- travels together travels in one statement rather than in a readable sequence
-- of several.

-- UpsertAssetAndProjection writes the identity and its projection at once, and
-- returns the state that preceded them.
--
-- The previous row is read on the snapshot the statement started from, so the
-- CTE that inserts is invisible to the one that reads. That is what makes the
-- previous lifecycle and the previous scope status free: the alternative is a
-- second round trip on the hottest write path of the system.
--
-- name: UpsertAssetAndProjection :one
WITH input AS (
    SELECT
        @asset_id::uuid           AS id,
        @org_id::uuid             AS org_id,
        @program_id::uuid         AS program_id,
        @kind::text               AS kind,
        @key::text                AS key,
        sqlc.narg(host)::text     AS host,
        sqlc.narg(port)::int      AS port,
        sqlc.narg(scheme)::text   AS scheme,
        @discovery_source::text   AS discovery_source,
        sqlc.narg(discovery_path)::jsonb  AS discovery_path,
        sqlc.narg(parent_asset_id)::uuid  AS parent_asset_id,
        @scope_status::text       AS scope_status,
        @seen_at::timestamptz     AS seen_at,
        sqlc.narg(next_resolve_at)::timestamptz AS next_resolve_at,
        sqlc.narg(next_full_at)::timestamptz    AS next_full_at
),
previous AS (
    SELECT a.id, a.scope_status, a.discovery_source, c.lifecycle
      FROM asset a
      LEFT JOIN asset_current c ON c.asset_id = a.id
      JOIN input i ON a.program_id = i.program_id AND a.kind = i.kind AND a.key = i.key
),
written AS (
    INSERT INTO asset (
        id, org_id, program_id, kind, key, host, parent_asset_id,
        discovery_source, discovery_path, scope_status, first_seen, last_seen)
    SELECT
        COALESCE((SELECT id FROM previous), i.id),
        i.org_id, i.program_id, i.kind, i.key, i.host, i.parent_asset_id,
        i.discovery_source, i.discovery_path, i.scope_status, i.seen_at, i.seen_at
      FROM input i
    ON CONFLICT (program_id, kind, key) DO UPDATE SET
        -- first_seen can only move backwards, which is what makes a cursor on
        -- it safe: a row that moves becomes older than a cursor already
        -- passed, so it is neither re-emitted nor missed.
        first_seen   = LEAST(asset.first_seen, EXCLUDED.first_seen),
        last_seen    = GREATEST(asset.last_seen, EXCLUDED.last_seen),
        -- Re-evaluated on every ingestion, not only when a run starts: that is
        -- what lets a rule change reclassify history without rescanning.
        scope_status = EXCLUDED.scope_status,
        -- Derivable from the key, so a row written before it was filled gets
        -- it on its next observation rather than through a catch-up migration
        -- that would reimplement key parsing in SQL.
        host         = COALESCE(asset.host, EXCLUDED.host),
        -- Never touched on conflict: its value is the one from the first
        -- appearance, which is exactly the question lineage asks.
        discovery_source = asset.discovery_source
    RETURNING id, (xmax = 0) AS created
),
projected AS (
    INSERT INTO asset_current (
        asset_id, org_id, program_id, kind, key, scope_status,
        host, port, scheme, next_resolve_at, next_full_at, first_seen, last_seen)
    SELECT
        w.id, i.org_id, i.program_id, i.kind, i.key, i.scope_status,
        i.host, i.port, i.scheme,
        CASE WHEN i.scope_status = 'in_scope' THEN i.next_resolve_at END,
        CASE WHEN i.scope_status = 'in_scope' THEN i.next_full_at END,
        i.seen_at, i.seen_at
      FROM written w, input i
    ON CONFLICT (asset_id) DO UPDATE SET
        first_seen   = LEAST(asset_current.first_seen, EXCLUDED.first_seen),
        last_seen    = GREATEST(asset_current.last_seen, EXCLUDED.last_seen),
        scope_status = EXCLUDED.scope_status,
        host         = COALESCE(asset_current.host, EXCLUDED.host),
        port         = COALESCE(asset_current.port, EXCLUDED.port),
        scheme       = COALESCE(asset_current.scheme, EXCLUDED.scheme),
        -- Only in-scope assets are scheduled. An asset leaving the perimeter
        -- loses its due dates in the same statement that reclassifies it,
        -- otherwise it keeps being scanned outside the authorization.
        next_resolve_at = CASE WHEN EXCLUDED.scope_status = 'in_scope'
                               THEN COALESCE(asset_current.next_resolve_at, EXCLUDED.next_resolve_at) END,
        next_full_at    = CASE WHEN EXCLUDED.scope_status = 'in_scope'
                               THEN COALESCE(asset_current.next_full_at, EXCLUDED.next_full_at) END,
        next_fingerprint_at = CASE WHEN EXCLUDED.scope_status = 'in_scope'
                                   THEN asset_current.next_fingerprint_at END
    RETURNING asset_id
)
-- A left join rather than three scalar subqueries, so that the generator can
-- see these are absent on a first appearance. A column it believes non-null is
-- one the caller cannot scan the first time an asset is written.
SELECT
    w.id      AS asset_id,
    w.created AS created,
    p.scope_status     AS previous_scope_status,
    p.lifecycle        AS previous_lifecycle,
    p.discovery_source AS previous_discovery_source
  FROM written w
  LEFT JOIN previous p ON TRUE
 WHERE EXISTS (SELECT 1 FROM projected);

-- WriteObservation deduplicates against the head of the chain.
--
-- An observation is inserted only when it differs from the last one of the
-- same (asset, layer). Each row then means "this state held from observed_at
-- to last_confirmed_at", so volume drops by more than an order of magnitude
-- without losing information, and two consecutive rows are two distinct states
-- by construction.
--
-- The comparison covers outcome and data and never the producer version. A
-- version bump that changes nothing in the result must not write a row, which
-- is the whole reason that version is stored twice.
--
-- The previous payload comes back only when there was an insertion. Without
-- that condition, every observation would drag its own payload across the wire
-- while most of them deduplicate and have no diff to compute.
--
-- name: WriteObservation :one
WITH input AS (
    SELECT
        @org_id::uuid              AS org_id,
        @asset_id::uuid            AS asset_id,
        @observed_at::timestamptz  AS observed_at,
        sqlc.narg(run_id)::uuid    AS run_id,
        @source::text              AS source,
        @layer::text               AS layer,
        @outcome::text             AS outcome,
        sqlc.narg(producer_version)::text AS producer_version,
        @data::jsonb               AS data
),
head AS (
    SELECT o.id, o.observed_at, o.outcome, o.data, o.last_producer_version
      FROM observation o, input i
     WHERE o.asset_id = i.asset_id AND o.layer = i.layer
     ORDER BY o.observed_at DESC
     LIMIT 1
),
confirmed AS (
    UPDATE observation o SET
        -- Taken as a GREATEST rather than assigned: two probes landing at once
        -- could otherwise walk the confirmation window backwards and violate
        -- the constraint that keeps it ordered.
        last_confirmed_at = GREATEST(o.last_confirmed_at, i.observed_at),
        last_producer_version = COALESCE(i.producer_version, o.last_producer_version)
      FROM head h, input i
     WHERE o.id = h.id AND o.observed_at = h.observed_at
       AND h.outcome = i.outcome
       -- jsonb equality, which already ignores object key order. It does not
       -- ignore array element order, which is why normalization sorts the
       -- arrays whose order carries nothing.
       AND h.data = i.data
    RETURNING o.id
),
inserted AS (
    INSERT INTO observation (
        org_id, asset_id, observed_at, last_confirmed_at, run_id, source, layer,
        outcome, producer_version, last_producer_version, data)
    SELECT
        i.org_id, i.asset_id, i.observed_at, i.observed_at, i.run_id, i.source, i.layer,
        i.outcome, i.producer_version, i.producer_version, i.data
      FROM input i
     WHERE NOT EXISTS (SELECT 1 FROM confirmed)
    RETURNING id
)
SELECT
    EXISTS (SELECT 1 FROM confirmed) AS deduplicated,
    (SELECT h.data FROM head h WHERE NOT EXISTS (SELECT 1 FROM confirmed)) AS previous_data,
    (SELECT h.last_producer_version FROM head h WHERE NOT EXISTS (SELECT 1 FROM confirmed)) AS previous_producer_version;

-- CountObservations is the deduplication rate's denominator on a replayed set.
--
-- name: CountObservations :one
SELECT count(*) FROM observation WHERE org_id = @org_id::uuid;

-- ListScopeRules returns a program's perimeter as it stands at an instant.
--
-- The instant is a parameter rather than now(): valid_to is written by the
-- application and now() is the database's clock, so comparing the two would
-- make the answer depend on two clocks agreeing, and a rule closed a moment
-- ago would read as still in force for the width of the gap.
--
-- name: ListScopeRules :many
SELECT id, kind, matcher, pattern
  FROM scope_rule
 WHERE program_id = @program_id::uuid
   AND valid_from <= @at::timestamptz
   AND (valid_to IS NULL OR valid_to > @at::timestamptz)
 ORDER BY id;

-- ListProgramAssets walks a program for a reclassification.
--
-- name: ListProgramAssets :many
SELECT a.id, a.kind, a.key, a.host, a.scope_status
  FROM asset a
 WHERE a.program_id = @program_id::uuid
 ORDER BY a.id;

-- ApplyScopeStatus moves one asset and its schedule together.
--
-- The two have to move in the same statement. An asset that should be out but
-- keeps its due dates goes on being scanned outside the authorization, and one
-- that should be in but has none costs coverage. Writing the rule and then
-- reclassifying in two transactions leaves a window where the system scans
-- what was just taken away from it.
--
-- name: ApplyScopeStatus :exec
WITH updated AS (
    UPDATE asset SET scope_status = @scope_status::text
     WHERE id = @asset_id::uuid
    RETURNING id
)
UPDATE asset_current SET
    scope_status = @scope_status::text,
    next_resolve_at = CASE WHEN @scope_status::text = 'in_scope'
                           THEN COALESCE(next_resolve_at, sqlc.narg(next_resolve_at)::timestamptz) END,
    next_full_at    = CASE WHEN @scope_status::text = 'in_scope'
                           THEN COALESCE(next_full_at, sqlc.narg(next_full_at)::timestamptz) END,
    next_fingerprint_at = CASE WHEN @scope_status::text = 'in_scope'
                               THEN next_fingerprint_at END
 WHERE asset_id = (SELECT id FROM updated);

-- ProgramForRun reads what an ingestion has to check before writing anything.
--
-- The authorization window is checked here rather than only the state: a run
-- that started before an expiry must not write after it.
--
-- name: ProgramForRun :one
SELECT p.id, p.org_id, p.state, p.authorized_from, p.authorized_to
  FROM program p
 WHERE p.id = @program_id::uuid;
