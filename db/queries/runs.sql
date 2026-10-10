-- Runs: selecting what is due, freezing it, and expiring what never delivered.
--
-- There is no queue table and no lease column. Three due dates decide eligibility, and
-- a run's frozen target list is its whole reservation: selection skips what a live run
-- holds, and the reservation expires with the run.

-- SelectDueHosts is the scheduler's pass.
--
-- The identity tiebreaker is not decoration: assets are written in bulk, so thousands of
-- rows share a due date to the microsecond, and a LIMIT over ties leaves the planner free
-- to return different rows on two ticks.
--
-- Only fqdn assets: an address is the answer to a name, so the schedule belongs on the
-- name, and a service is observed through its host's run.
--
-- @tenant: scoped
-- name: SelectDueHosts :many
SELECT c.asset_id, c.key, c.lifecycle, c.backoff_tier
  FROM asset_current c
 WHERE c.org_id = @org_id::uuid
   AND c.program_id = @program_id::uuid
   AND c.kind = 'fqdn'
   AND c.scope_status = 'in_scope'
   AND c.lifecycle <> 'archived'
   -- The candidate lane is excluded here, mutually: otherwise the two passes fight over the same
   -- names, each freezing what the other was about to take.
   --
   -- The lane is the pair, never the lifecycle alone. CANDIDATE is not a CT state: a host typed
   -- into the assets form is one until its first answer, and excluding on lifecycle alone would
   -- divert it into a lane pinned to resolve, where nothing ever sweeps its ports. A null full
   -- date separates them: a candidate that has not earned the expensive rung is the state the
   -- aggressive curve describes.
   AND NOT (c.lifecycle = 'candidate' AND c.next_full_at IS NULL)
   AND CASE WHEN @rung::text = 'full' THEN c.next_full_at ELSE c.next_resolve_at END
       <= @at::timestamptz
   -- The lease, and the whole of it. Two runs never hold the same host, and a
   -- run that dies frees its hosts by expiring rather than by restitution.
   AND NOT EXISTS (
        SELECT 1 FROM run_target t
          JOIN run r ON r.id = t.run_id
         WHERE t.asset_id = c.asset_id
           AND r.state IN ('pending', 'running'))
 ORDER BY CASE WHEN @rung::text = 'full' THEN c.next_full_at ELSE c.next_resolve_at END,
          c.asset_id
 LIMIT @batch::int;

-- AddRunTargets freezes the list in one round trip, not one per target: a run costing five
-- hundred round trips to define is one nobody lets the scheduler start often.
--
-- Parallel arrays, not COPY, by necessity: PostgreSQL refuses COPY FROM on a table with a
-- policy ("COPY FROM not supported with row-level security"). Same single round trip, with
-- the organization a scalar of the statement, so a batch mixing two tenants is inexpressible.
--
-- generate_subscripts, not a multi-argument unnest, which sqlc's analyser cannot type. The
-- arrays are the same length by construction (built in one loop, never assembled); a short
-- one would read as nulls rather than fail.
--
-- @tenant: scoped
-- name: AddRunTargets :execrows
INSERT INTO run_target (run_id, asset_id, org_id, key)
SELECT @run_id::uuid, (@asset_ids::uuid[])[i], @org_id::uuid, (@keys::text[])[i]
  FROM generate_subscripts(@asset_ids::uuid[], 1) AS i;

-- LiveRunForProgram is what a second request runs into. started_at comes back because
-- it alone separates a run something opened from one whose provisioning failed, which
-- call for opposite actions.
--
-- @tenant: keyed
-- name: LiveRunForProgram :one
SELECT id, kind, scope, state, deadline, created_at, started_at, target_count
  FROM run
 WHERE program_id = @program_id::uuid
   AND kind = @kind::text
   AND state IN ('pending', 'running')
 ORDER BY created_at DESC
 LIMIT 1;

-- LiveDiscoveryForApex is the per-apex half of the bound above. One apex in flight says
-- nothing about a programme's other enumerations. The unique index says the same and
-- would refuse the insert; this is asked first so a caller gets the run named back, and
-- the free apexes can still go out.
--
-- @tenant: keyed
-- name: LiveDiscoveryForApex :one
SELECT id, kind, scope, state, deadline, created_at, started_at, target_count
  FROM run
 WHERE program_id = @program_id::uuid
   AND kind = 'discovery'
   AND apex = @apex::text
   AND state IN ('pending', 'running')
 ORDER BY created_at DESC
 LIMIT 1;

-- ExpireRuns is the deadline sweeper. It repairs nothing: due dates only move on
-- ingestion, so an abandoned run leaves the inventory as it was, and expiring it frees
-- its targets and makes the failure visible.
--
-- @tenant: cross-org
-- @why: the deadline sweeper serves every tenant in one tick. A run nothing
--       expires holds its targets forever, and the assets it froze are invisible to
--       every later pass.
--
-- A run whose report is being written is skipped, not waited for. Ingestion holds the
-- row while it writes, possibly minutes: waiting would stop expiring everyone else's runs
-- and then close a run that had just delivered. The next tick finds it completed.
-- name: ExpireRuns :many
UPDATE run SET
    state       = 'expired',
    finished_at = @at::timestamptz,
    error       = COALESCE(error, 'the deadline passed and no report was delivered')
 WHERE id IN (
        SELECT r.id FROM run r
         WHERE r.state IN ('pending', 'running')
           AND r.deadline <= @at::timestamptz
           FOR UPDATE SKIP LOCKED)
RETURNING id, org_id, program_id, kind, scope, started_at, target_count;

-- OpenRunsOnPlatform is what the sweeper asks the platform about. Only runs it has an
-- execution name for: one never given a name was refused at the start and nothing can be
-- asked about it, so the deadline is its only way out.
--
-- @tenant: cross-org
-- @why: the sweeper serves every tenant in one tick, and the question it asks
--       is about an execution on the platform, which knows no tenant either.
-- name: OpenRunsOnPlatform :many
SELECT id, org_id, program_id, kind, scope, external_id, started_at
  FROM run
 WHERE state IN ('pending', 'running')
   AND external_id IS NOT NULL
 ORDER BY created_at;

-- FailRun closes an open run that delivered nothing, for the sweeper when the platform
-- says the execution failed and for a person cancelling from the console. Both skip a
-- run whose report is being written (see ExpireRuns) and read no row back then: the run
-- is about to be completed, which is not theirs to overrule.
--
-- @tenant: keyed
-- name: FailRun :one
UPDATE run SET
    state       = 'failed',
    finished_at = @at::timestamptz,
    error       = @error::text
 WHERE id = (
        SELECT r.id FROM run r
         WHERE r.id = @run_id::uuid
           AND r.state IN ('pending', 'running')
           FOR UPDATE SKIP LOCKED)
RETURNING id, program_id, kind, external_id;

-- StartRunOnPlatform records that the platform is executing a run. The first sign a
-- discovery run gets of being taken, having no target list to open. Skipped, not waited
-- for, under an ingestion: the scanner is still running while it delivers, so this is
-- exactly when a report holds the row.
--
-- @tenant: keyed
-- name: StartRunOnPlatform :exec
UPDATE run SET
    state      = 'running',
    started_at = COALESCE(started_at, @at::timestamptz)
 WHERE id = (
        SELECT r.id FROM run r
         WHERE r.id = @run_id::uuid
           AND r.state = 'pending'
           FOR UPDATE SKIP LOCKED);

-- MarkRunRunning records that a scanner opened the run.
--
-- @tenant: keyed
-- name: MarkRunRunning :exec
UPDATE run SET
    state      = 'running',
    started_at = COALESCE(started_at, @at::timestamptz)
 WHERE id = @run_id::uuid AND state = 'pending';

-- RunForTargets reads what the target list endpoint has to check.
--
-- @tenant: keyed
-- name: RunForTargets :one
SELECT r.id, r.org_id, r.program_id, r.kind, r.scope, r.state, r.deadline
  FROM run r
 WHERE r.id = @run_id::uuid;

-- ProgramForScheduling reads what a run needs to be defined.
--
-- @tenant: keyed
-- name: ProgramForScheduling :one
SELECT id, org_id, name, state, authorized_from, authorized_to, rate_limit_rps,
       discovery_interval, last_discovery_at, version
  FROM program
 WHERE id = @program_id::uuid;

-- TouchDiscovery records that a discovery run was provisioned, at creation not
-- completion: a run that dies must not be restarted by the cadence, as the deadline
-- sweeper handles it.
--
-- @tenant: keyed
-- name: TouchDiscovery :exec
UPDATE program SET last_discovery_at = @at::timestamptz WHERE id = @program_id::uuid;

-- ProgramsDueForDiscovery is the pass that provisions enumeration. The authorization
-- window is checked here, not only the state: otherwise an expired programme is
-- provisioned every tick and refused when the run opens, an execution billed for nothing
-- every thirty seconds.
--
-- @tenant: cross-org
-- @why: the discovery cadence provisions enumeration for every programme of
--       every tenant in one pass, and a per tenant sweep would need a list of tenants
--       to walk, which is the same crossing wearing a loop.
-- name: ProgramsDueForDiscovery :many
SELECT p.id, p.org_id, p.name, p.rate_limit_rps
  FROM program p
 WHERE p.state = 'active'
   AND p.authorized_from <= @at::timestamptz
   AND (p.authorized_to IS NULL OR p.authorized_to > @at::timestamptz)
   AND (p.last_discovery_at IS NULL
        OR p.last_discovery_at + p.discovery_interval <= @at::timestamptz
        -- Or the last discovery ended without delivering and the retry delay has passed. A run
        -- that failed in thirty seconds must not cost a week of coverage; clearing
        -- last_discovery_at instead would destroy the record of the last enumeration and turn
        -- a permanently broken runner into a loop provisioning and billing every tick.
        OR EXISTS (
             SELECT 1 FROM run r
              WHERE r.program_id = p.id
                AND r.kind = 'discovery'
                AND r.state IN ('expired', 'failed')
                AND r.finished_at + @retry::interval <= @at::timestamptz
                AND NOT EXISTS (
                      SELECT 1 FROM run later
                       WHERE later.program_id = p.id
                         AND later.kind = 'discovery'
                         AND later.created_at > r.created_at)))
   -- The condition that does the work. It prevents a provisioning storm and
   -- bounds concurrency to one discovery run per programme.
   AND NOT EXISTS (
        SELECT 1 FROM run r
         WHERE r.program_id = p.id AND r.kind = 'discovery'
           AND r.state IN ('pending', 'running'))
   -- A programme with nothing to enumerate is not due, or it is selected and refused every
   -- tick: at a one-minute cadence, a warning a minute forever.
   AND EXISTS (
        SELECT 1 FROM scope_rule s
         WHERE s.program_id = p.id
           AND s.kind = 'include' AND s.matcher = 'apex'
           AND s.valid_from <= @at::timestamptz
           AND (s.valid_to IS NULL OR s.valid_to > @at::timestamptz))
 ORDER BY p.id;

-- ApexesForProgram is a discovery run's perimeter.
--
-- @tenant: keyed
-- name: ApexesForProgram :many
SELECT DISTINCT pattern
  FROM scope_rule
 WHERE program_id = @program_id::uuid
   AND kind = 'include'
   AND matcher = 'apex'
   AND valid_from <= @at::timestamptz
   AND (valid_to IS NULL OR valid_to > @at::timestamptz)
 ORDER BY pattern;

-- ExclusionsForProgram is the second safety net in front of the network. The patterns
-- travel with the perimeter, not trusted to the scope re-evaluation at ingestion: a rule
-- may change between a run being defined and starting, and the re-evaluation happens
-- after the packet. One probe too many is small, but not a property to accept knowingly
-- when the pattern fits in the invocation.
--
-- @tenant: keyed
-- name: ExclusionsForProgram :many
SELECT DISTINCT matcher, pattern
  FROM scope_rule
 WHERE program_id = @program_id::uuid
   AND kind = 'exclude'
   AND valid_from <= @at::timestamptz
   AND (valid_to IS NULL OR valid_to > @at::timestamptz)
 ORDER BY matcher, pattern;

-- RecordRunStart names the execution the platform created. Written after the start, as
-- the run is committed first: a platform refusing on a quota must leave a row the
-- deadline sweeper owns, not an absence.
--
-- @tenant: keyed
-- name: RecordRunStart :exec
UPDATE run SET external_id = @external_id::text WHERE id = @run_id::uuid;

-- ProgramsDueForVerification is the pass on due dates, counterpart of the discovery cadence
-- and what was missing: due dates were written and counted in the queue view but no tick
-- dispatched them, so runs went out only on a button press.
--
-- The authorization window is checked here as well as at the run. Nothing is billed either
-- way (the run is refused before the platform is called); this stops the pass waking an
-- expired programme, walking its due assets and logging a refusal once a minute.
--
-- full_due decides the rung without a second query: a full run executes every rung below it
-- and moves both dates, so taking full first cannot starve resolve. The absence of a live
-- verification run does the work, as for discovery; the unique index is the guarantee
-- (read-then-write is not a reservation under read committed), this only avoids provisioning
-- what the index would refuse.
--
-- @tenant: cross-org
-- @why: the due date pass provisions verification for every programme of every
--       tenant in one pass, and a per tenant sweep would need a list of tenants to
--       walk, which is the same crossing wearing a loop.
-- name: ProgramsDueForVerification :many
SELECT p.id, p.org_id, p.name,
       bool_or(c.next_full_at <= @at::timestamptz) AS full_due
  FROM program p
  JOIN asset_current c ON c.program_id = p.id
 WHERE p.state = 'active'
   AND p.authorized_from <= @at::timestamptz
   AND (p.authorized_to IS NULL OR p.authorized_to > @at::timestamptz)
   -- The selection's predicate, so a programme is never woken for work the selection would
   -- then refuse to find. A name is the unit: an address is never a target of its own, and
   -- a service is observed through its host's run.
   AND c.kind = 'fqdn'
   AND c.scope_status = 'in_scope'
   AND c.lifecycle <> 'archived'
   AND NOT (c.lifecycle = 'candidate' AND c.next_full_at IS NULL)
   AND (c.next_full_at <= @at::timestamptz OR c.next_resolve_at <= @at::timestamptz)
   AND NOT EXISTS (
        SELECT 1 FROM run r
         WHERE r.program_id = p.id
           -- Either kind. A live verification is the bound the unique index keeps; a live
           -- discovery is the perimeter being walked now, and freezes no targets because it is the
           -- one allowed to find things, so nothing else sees its hosts. Waking a programme then
           -- would select assets whose report has not landed and send a second scanner at hosts the
           -- first is connected to.
           AND r.kind IN ('verification', 'discovery')
           AND r.state IN ('pending', 'running'))
 GROUP BY p.id, p.org_id, p.name
 ORDER BY p.id;

-- SelectDueCandidates is the candidate lane's half of the selection: SelectDueHosts'
-- columns and lease with the lifecycle reversed. A candidate is selected by this pass
-- only, or the two fight over the same names.
--
-- The rung is not a parameter. A CHECK pins a candidate run to resolve: one resolver
-- round trip with nothing sent to the target is what lets this lane run beside a
-- verification without adding up to anything the rate budget cares about.
--
-- @tenant: scoped
-- name: SelectDueCandidates :many
SELECT c.asset_id, c.key, c.lifecycle, c.backoff_tier
  FROM asset_current c
 WHERE c.org_id = @org_id::uuid
   AND c.program_id = @program_id::uuid
   AND c.kind = 'fqdn'
   AND c.scope_status = 'in_scope'
   AND c.lifecycle = 'candidate'
   -- The pair, not the lifecycle. A hand-entered host is a candidate until it answers and
   -- carries a full date because it was typed in to learn what it exposes: taking it here
   -- would say a name resolves and never sweep a port.
   AND c.next_full_at IS NULL
   AND c.next_resolve_at <= @at::timestamptz
   -- The lease is the same one, across lanes as well as within one: a host
   -- frozen by a verification run is not taken by a candidate run either.
   AND NOT EXISTS (
        SELECT 1 FROM run_target t
          JOIN run r ON r.id = t.run_id
         WHERE t.asset_id = c.asset_id
           AND r.state IN ('pending', 'running'))
 ORDER BY c.next_resolve_at, c.asset_id
 LIMIT @batch::int;

-- ProgramsDueForCandidates wakes the lane that must not wait. Unlike
-- ProgramsDueForVerification, only a live *candidate* run holds this pass back.
--
-- A live verification does not, which is the point: a sweep holds its slot for its whole
-- deadline, and a candidate arriving a minute in would wait half an hour for a check wanted
-- at sixty seconds. Nor does a live discovery, by decision: discovery blocks verification
-- because a full sweep would send a second scanner at hosts the first is connected to, while
-- a resolve sends nothing to the target.
--
-- @tenant: cross-org
-- @why: the candidate pass provisions for every programme of every tenant in
--       one pass, exactly as the due date pass does.
-- name: ProgramsDueForCandidates :many
SELECT p.id, p.org_id, p.name
  FROM program p
  JOIN asset_current c ON c.program_id = p.id
 WHERE p.state = 'active'
   AND p.authorized_from <= @at::timestamptz
   AND (p.authorized_to IS NULL OR p.authorized_to > @at::timestamptz)
   AND c.kind = 'fqdn'
   AND c.scope_status = 'in_scope'
   AND c.lifecycle = 'candidate'
   AND c.next_full_at IS NULL
   AND c.next_resolve_at <= @at::timestamptz
   AND NOT EXISTS (
        SELECT 1 FROM run r
         WHERE r.program_id = p.id
           AND r.kind = 'candidate'
           AND r.state IN ('pending', 'running'))
 GROUP BY p.id, p.org_id, p.name
 ORDER BY p.id;
