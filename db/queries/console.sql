-- The console's own reads and writes: perimeters, rules, and the queue.
--
-- Everything the console asks about the inventory itself is the search layer's,
-- which compiles a tree rather than answering a named query. What is left here
-- is the surface that has no filter tree behind it: a program is not a filter,
-- and neither is "why is nothing moving".

-- ListPrograms is the switcher and the programs screen.
--
-- Without the counters, which is the default shape on purpose: the switcher
-- sits on every page, and an aggregation over asset_current per program is the
-- only thing in this file that costs a scan. They are asked for, not given.
--
-- @tenant: scoped
-- name: ListPrograms :many
SELECT p.id, p.name, p.platform, p.platform_ref, p.state,
       p.authorized_from, p.authorized_to, p.authorization_ref,
       p.rate_limit_rps, p.discovery_interval, p.last_discovery_at,
       p.version, p.created_at, p.updated_at,
       -- The rules in force, counted against an instant the caller supplies.
       -- valid_to is written by the application and now() is the database's
       -- clock, so comparing the two would make the answer depend on two clocks
       -- agreeing, and a rule closed a moment ago would read as still in force
       -- for the width of the gap.
       (SELECT count(*) FROM scope_rule r
         WHERE r.program_id = p.id
           AND r.valid_from <= @at::timestamptz
           AND (r.valid_to IS NULL OR r.valid_to > @at::timestamptz))::int AS rules_in_force
  FROM program p
 WHERE p.org_id = @org_id::uuid
 ORDER BY p.name;

-- CountProgramAssets is the aggregation the list only runs when asked.
--
-- One statement over the whole tenant rather than one per program, grouped
-- here. Ten programs would otherwise be ten scans of the projection for a
-- number that sits beside a name.
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

-- CreateProgram opens a perimeter.
--
-- No version travels in, because there is nothing to avoid overwriting.
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

-- UpdateProgram edits one, and refuses a stale version by returning no row.
--
-- The refusal is the absence of a row rather than a check the caller ran first.
-- A read then a write is two statements a concurrent writer can slip between,
-- and the whole point of the column is that two writes losing each other
-- silently is a lost scope, therefore a scan outside the perimeter.
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

-- ListRules reads a program's rules, closed ones included.
--
-- Closed ones included, because a rule has a period of validity rather than an
-- existence: an asset classified out of scope by a rule since closed stays
-- explainable, and a screen that hid them would take the answer away.
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

-- UpdateRule edits or closes one, and refuses a stale version the same way.
--
-- Closing is setting valid_to, never a DELETE. There is no delete statement in
-- this file at all, which is stronger than a convention: a statement that does
-- not exist cannot be called by a client written in a hurry.
--
-- @tenant: scoped
-- name: UpdateRule :one
UPDATE scope_rule
   -- COALESCE, because closing a rule is not restating it. A caller that had to
   -- send the pattern back to set valid_to would be one round trip away from
   -- rewriting a rule it only meant to close.
   SET pattern    = COALESCE(sqlc.narg('pattern')::text, pattern),
       note       = sqlc.narg('note')::text,
       valid_to   = sqlc.narg('valid_to')::timestamptz,
       version    = version + 1,
       updated_by = sqlc.narg('actor')::uuid
 WHERE org_id = @org_id::uuid
   AND program_id = @program_id::uuid
   AND id = @rule_id::uuid
   AND version = @version::int
RETURNING id, kind, matcher, pattern, valid_from, valid_to, note, version, created_at;

-- QueueDepth answers "why is nothing moving".
--
-- Three numbers per program and per queue, and they are disjoint: a row held by
-- a run is not also due. Rows with no due date are counted nowhere, and that is
-- the only choice that does not lie, since a null is how a row leaves the
-- scheduler. Filing them under later would show a queue that never drains.
--
-- The queues are the due date columns rather than the observation layers. There
-- are three schedules and four layers, and naming them after the layers would
-- promise a tcp queue that does not exist.
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

-- RecentRuns is the other half of the answer.
--
-- Without them an empty queue and a full one look alike: in both cases the
-- screen shows a number and nobody knows whether anything is running.
--
-- @tenant: scoped
-- name: RecentRuns :many
SELECT r.id, r.program_id, r.kind, r.scope, r.state, r.deadline,
       r.created_at, r.started_at, r.finished_at, r.target_count, r.error,
       -- COALESCE because a run that has not delivered has no summary at all,
       -- and a null here would be scanned as an error rather than as zero.
       COALESCE((r.summary ->> 'observations')::int, 0)::int AS observations
  FROM run r
 WHERE r.org_id = @org_id::uuid
 ORDER BY r.created_at DESC
 LIMIT @batch::int;
