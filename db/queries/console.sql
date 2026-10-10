-- The console's own reads and writes: perimeters, rules and the queue. Everything it asks
-- about the inventory itself is the search layer's (a compiled tree, not a named query).
-- What is here has no filter tree behind it: a program is not a filter, nor is "why is
-- nothing moving".

-- ListPrograms is the switcher and the programs screen. Without the counters by default:
-- the switcher is on every page, and the per-program aggregation over asset_current is the
-- only scan in this file. They are asked for, not given.
--
-- @tenant: scoped
-- name: ListPrograms :many
SELECT p.id, p.name, p.platform, p.platform_ref, p.state,
       p.authorized_from, p.authorized_to, p.authorization_ref,
       p.rate_limit_rps, p.discovery_interval, p.last_discovery_at,
       p.version, p.created_at, p.updated_at,
       -- The rules in force, counted against a caller-supplied instant: valid_to is written by the
       -- application and now() is the database's clock, so a rule closed a moment ago would read
       -- as in force for the width of the gap.
       (SELECT count(*) FROM scope_rule r
         WHERE r.program_id = p.id
           AND r.valid_from <= @at::timestamptz
           AND (r.valid_to IS NULL OR r.valid_to > @at::timestamptz))::int AS rules_in_force
  FROM program p
 WHERE p.org_id = @org_id::uuid
 ORDER BY p.name;

-- CountProgramAssets is the aggregation the list runs only when asked. One statement over
-- the tenant, grouped here, not one per program: ten programs would be ten scans of the
-- projection for a number beside a name.
--
-- @tenant: scoped
-- name: CountProgramAssets :many
SELECT c.program_id,
       count(*)::int AS assets,
       count(*) FILTER (WHERE c.scope_status = 'in_scope')::int AS in_scope
  FROM asset_current c
 WHERE c.org_id = @org_id::uuid
   AND c.lifecycle <> 'archived'
 GROUP BY c.program_id;

-- GetProgram reads one, for the screen that edits it.
--
-- @tenant: scoped
-- name: GetProgram :one
SELECT p.id, p.name, p.platform, p.platform_ref, p.state,
       p.authorized_from, p.authorized_to, p.authorization_ref,
       p.rate_limit_rps, p.discovery_interval, p.last_discovery_at,
       p.version, p.created_at, p.updated_at
  FROM program p
 WHERE p.org_id = @org_id::uuid AND p.id = @program_id::uuid;

-- CreateProgram opens a perimeter. No version travels in: there is nothing to overwrite.
--
-- @tenant: scoped
-- name: CreateProgram :one
INSERT INTO program (id, org_id, name, platform, platform_ref,
                     authorized_from, authorized_to, authorization_ref,
                     rate_limit_rps, discovery_interval, state, created_by, updated_by)
VALUES (@program_id::uuid, @org_id::uuid, @name::text,
        sqlc.narg('platform')::text, sqlc.narg('platform_ref')::text,
        @authorized_from::timestamptz, sqlc.narg('authorized_to')::timestamptz,
        sqlc.narg('authorization_ref')::text,
        @rate_limit_rps::int, @discovery_interval::interval, @state::text,
        sqlc.narg('actor')::uuid, sqlc.narg('actor')::uuid)
RETURNING id, name, platform, platform_ref, state,
          authorized_from, authorized_to, authorization_ref,
          rate_limit_rps, discovery_interval, last_discovery_at,
          version, created_at, updated_at;

-- UpdateProgram edits one and refuses a stale version by returning no row. The refusal is
-- the absence of a row, not a check run first: a read then a write lets a concurrent writer
-- slip between, and two writes silently losing each other is a lost scope, hence a scan
-- outside the perimeter.
--
-- @tenant: scoped
-- name: UpdateProgram :one
UPDATE program
   SET name              = @name::text,
       platform          = sqlc.narg('platform')::text,
       platform_ref      = sqlc.narg('platform_ref')::text,
       authorized_from   = @authorized_from::timestamptz,
       authorized_to     = sqlc.narg('authorized_to')::timestamptz,
       authorization_ref = sqlc.narg('authorization_ref')::text,
       rate_limit_rps    = @rate_limit_rps::int,
       discovery_interval = @discovery_interval::interval,
       state             = @state::text,
       version           = version + 1,
       updated_by        = sqlc.narg('actor')::uuid,
       updated_at        = @at::timestamptz
 WHERE org_id = @org_id::uuid
   AND id = @program_id::uuid
   AND version = @version::int
RETURNING id, name, platform, platform_ref, state,
          authorized_from, authorized_to, authorization_ref,
          rate_limit_rps, discovery_interval, last_discovery_at,
          version, created_at, updated_at;

-- ListRules reads a program's rules, closed ones included: a rule has a validity period,
-- not an existence, and an asset classified out of scope by a since-closed rule stays
-- explainable.
--
-- @tenant: scoped
-- name: ListRules :many
SELECT r.id, r.kind, r.matcher, r.pattern, r.valid_from, r.valid_to, r.note,
       r.version, r.created_at,
       (r.valid_from <= @at::timestamptz
        AND (r.valid_to IS NULL OR r.valid_to > @at::timestamptz)) AS in_force
  FROM scope_rule r
 WHERE r.org_id = @org_id::uuid AND r.program_id = @program_id::uuid
 ORDER BY r.valid_to NULLS FIRST, r.created_at DESC;

-- CreateRule opens one.
--
-- @tenant: scoped
-- name: CreateRule :one
INSERT INTO scope_rule (id, org_id, program_id, kind, matcher, pattern,
                        valid_from, valid_to, note, created_by, updated_by)
VALUES (@rule_id::uuid, @org_id::uuid, @program_id::uuid,
        @kind::text, @matcher::text, @pattern::text,
        @valid_from::timestamptz, sqlc.narg('valid_to')::timestamptz,
        sqlc.narg('note')::text, sqlc.narg('actor')::uuid, sqlc.narg('actor')::uuid)
RETURNING id, kind, matcher, pattern, valid_from, valid_to, note, version, created_at;

-- UpdateRule edits or closes one, refusing a stale version the same way. Closing sets
-- valid_to, never a DELETE; no delete statement exists in this file at all, stronger than
-- a convention, as one that does not exist cannot be called by a client written in a hurry.
--
-- @tenant: scoped
-- name: UpdateRule :one
UPDATE scope_rule
   -- COALESCE on both: closing a rule is not restating it, and a caller forced to send them
   -- back would be a round trip from rewriting a rule it meant to close. The note matters
   -- most, being the explanation a closed rule exists to preserve.
   --
   -- So a note cannot be cleared through this statement, only replaced. Nothing asks to today.
   SET pattern    = COALESCE(sqlc.narg('pattern')::text, pattern),
       note       = COALESCE(sqlc.narg('note')::text, note),
       valid_to   = sqlc.narg('valid_to')::timestamptz,
       version    = version + 1,
       updated_by = sqlc.narg('actor')::uuid
 WHERE org_id = @org_id::uuid
   AND program_id = @program_id::uuid
   AND id = @rule_id::uuid
   AND version = @version::int
RETURNING id, kind, matcher, pattern, valid_from, valid_to, note, version, created_at;

-- QueueDepth answers "why is nothing moving". Three numbers per program and queue,
-- disjoint (a row held by a run is not also due). Rows with no due date are counted
-- nowhere, the only honest choice since null is how a row leaves the scheduler; filing
-- them under later would show a queue that never drains.
--
-- The queues are the due-date columns, not the observation layers: three schedules against
-- four layers, and layer names would promise a tcp queue that does not exist.
--
-- @tenant: scoped
-- name: QueueDepth :many
WITH held AS (
    SELECT DISTINCT t.asset_id
      FROM run_target t
      JOIN run r ON r.id = t.run_id
     WHERE r.org_id = @org_id::uuid
       AND r.state IN ('pending', 'running')
), slots AS (
    SELECT c.program_id, 'resolve' AS queue, c.next_resolve_at AS due_at, c.asset_id
      FROM asset_current c WHERE c.org_id = @org_id::uuid AND c.next_resolve_at IS NOT NULL
    UNION ALL
    SELECT c.program_id, 'full', c.next_full_at, c.asset_id
      FROM asset_current c WHERE c.org_id = @org_id::uuid AND c.next_full_at IS NOT NULL
    UNION ALL
    SELECT c.program_id, 'fingerprint', c.next_fingerprint_at, c.asset_id
      FROM asset_current c WHERE c.org_id = @org_id::uuid AND c.next_fingerprint_at IS NOT NULL
)
SELECT s.program_id, s.queue,
       count(*) FILTER (
           WHERE s.due_at <= @at::timestamptz AND h.asset_id IS NULL)::int AS due,
       count(*) FILTER (WHERE s.due_at > @at::timestamptz)::int AS later,
       count(*) FILTER (WHERE h.asset_id IS NOT NULL)::int AS in_run
  FROM slots s
  LEFT JOIN held h ON h.asset_id = s.asset_id
 GROUP BY 1, 2;

-- RecentRuns is the other half of the answer. Without it an empty queue and a full one look
-- alike: the screen shows a number and nobody knows whether anything runs.
--
-- @tenant: scoped
-- name: RecentRuns :many
SELECT r.id, r.program_id, r.kind, r.scope, r.state, r.deadline,
       r.created_at, r.started_at, r.finished_at, r.target_count, r.error,
       -- What the platform called the execution, the only way to find its logs. It survives the
       -- request that started the run: read from a form result alone, it would be lost on the
       -- first reload, when someone is looking for it.
       r.external_id,
       -- Two spellings and a zero. The summary gained json tags when the queue view began reading
       -- it back, so older runs carry the Go identifier; reading only the tagged key would show
       -- them at zero observations, indistinguishable via COALESCE from a run that delivered
       -- nothing. A run that has not delivered has no summary, where a null would scan as an
       -- error and not zero.
       COALESCE((r.summary ->> 'observations')::int,
                (r.summary ->> 'Observations')::int, 0)::int AS observations
  FROM run r
 WHERE r.org_id = @org_id::uuid
 ORDER BY r.created_at DESC
 LIMIT @batch::int;
