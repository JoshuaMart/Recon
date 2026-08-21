-- Runs: selecting what is due, freezing it, and expiring what never delivered.
--
-- There is no queue table and no lease column. Three due dates decide
-- eligibility, and the frozen target list of a run is the whole of the
-- reservation: selection skips what a live run already holds, and the
-- reservation expires when the run does.

-- SelectDueHosts is the scheduler's pass.
--
-- The tiebreaker on the identity is not decoration. Assets are written in bulk,
-- so thousands of rows routinely carry the same due date to the microsecond:
-- one report writes them in one transaction. With a LIMIT over a set of ties
-- and nothing to break them, which rows come back is the planner's choice, and
-- it can differ between two ticks for reasons nothing here controls.
--
-- Only fqdn assets are selected. An address is never a target of its own: it
-- enters this inventory as the answer to a name, so the name is where the
-- schedule belongs, and a service is observed through its host's run.
--
-- name: SelectDueHosts :many
SELECT c.asset_id, c.key, c.lifecycle, c.backoff_tier
  FROM asset_current c
 WHERE c.org_id = @org_id::uuid
   AND c.program_id = @program_id::uuid
   AND c.kind = 'fqdn'
   AND c.scope_status = 'in_scope'
   AND c.lifecycle <> 'archived'
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

-- AddRunTargets freezes the list in one round trip.
--
-- The copy path rather than a statement per target: a batch is hundreds of
-- rows, and a run that costs five hundred round trips to define is one nobody
-- will let the scheduler start often.
--
-- name: AddRunTargets :copyfrom
INSERT INTO run_target (run_id, asset_id, org_id, key) VALUES ($1, $2, $3, $4);

-- LiveRunForProgram is what a second request runs into.
--
-- started_at comes back with it because it is the only thing that separates a
-- run something has actually opened from one whose provisioning failed, and
-- those two call for opposite actions.
--
-- name: LiveRunForProgram :one
SELECT id, kind, scope, state, deadline, created_at, started_at, target_count
  FROM run
 WHERE program_id = @program_id::uuid
   AND kind = @kind::text
   AND state IN ('pending', 'running')
 ORDER BY created_at DESC
 LIMIT 1;

-- ExpireRuns is the deadline sweeper.
--
-- It does not have to repair anything. Due dates are moved only when a report
-- is ingested, so an abandoned run leaves the inventory exactly as it found it;
-- expiring it frees its targets and makes the failure visible.
--
-- name: ExpireRuns :many
WITH expired AS (
    UPDATE run SET
        state       = 'expired',
        finished_at = @at::timestamptz,
        error       = COALESCE(error, 'the deadline passed and no report was delivered')
     WHERE state IN ('pending', 'running')
       AND deadline <= @at::timestamptz
    RETURNING id, org_id, program_id, kind, scope, started_at, target_count
),
-- A discovery run that delivered nothing gives its cadence slot back.
--
-- last_discovery_at is written at creation so that the cadence cannot start a
-- second run while the first is in flight. Left there after a run died, it also
-- means the programme waits a whole discovery interval for a replacement: a run
-- that failed in thirty seconds would cost a week of coverage. Clearing it here
-- is what makes the next tick provision one, and it cannot double-start,
-- because there is no longer a run in flight for the condition to see.
replaced AS (
    UPDATE program p SET last_discovery_at = NULL
      FROM expired e
     WHERE p.id = e.program_id AND e.kind = 'discovery'
    RETURNING p.id
)
SELECT id, org_id, program_id, kind, scope, started_at, target_count FROM expired;

-- MarkRunRunning records that a scanner opened the run.
--
-- name: MarkRunRunning :exec
UPDATE run SET
    state      = 'running',
    started_at = COALESCE(started_at, @at::timestamptz)
 WHERE id = @run_id::uuid AND state = 'pending';

-- RunForTargets reads what the target list endpoint has to check.
--
-- name: RunForTargets :one
SELECT r.id, r.org_id, r.program_id, r.kind, r.scope, r.state, r.deadline
  FROM run r
 WHERE r.id = @run_id::uuid;

-- ProgramForScheduling reads what a run needs to be defined.
--
-- name: ProgramForScheduling :one
SELECT id, org_id, name, state, authorized_from, authorized_to, rate_limit_rps,
       discovery_interval, last_discovery_at, version
  FROM program
 WHERE id = @program_id::uuid;

-- TouchDiscovery records that a discovery run was provisioned.
--
-- Written at creation rather than at completion. A run that dies on the way
-- must not be restarted by the cadence: the deadline sweeper already handles
-- it, and confusing the two would start two.
--
-- name: TouchDiscovery :exec
UPDATE program SET last_discovery_at = @at::timestamptz WHERE id = @program_id::uuid;

-- ProgramsDueForDiscovery is the pass that provisions enumeration.
--
-- The authorization window is checked here rather than only the state. Without
-- those two lines an expired programme would be provisioned on every tick and
-- refused when the run opens: an execution billed to do nothing, every thirty
-- seconds.
--
-- name: ProgramsDueForDiscovery :many
SELECT p.id, p.org_id, p.name, p.rate_limit_rps
  FROM program p
 WHERE p.state = 'active'
   AND p.authorized_from <= @at::timestamptz
   AND (p.authorized_to IS NULL OR p.authorized_to > @at::timestamptz)
   AND (p.last_discovery_at IS NULL
        OR p.last_discovery_at + p.discovery_interval <= @at::timestamptz)
   -- The condition that does the work. It prevents a provisioning storm and
   -- bounds concurrency to one discovery run per programme.
   AND NOT EXISTS (
        SELECT 1 FROM run r
         WHERE r.program_id = p.id AND r.kind = 'discovery'
           AND r.state IN ('pending', 'running'))
 ORDER BY p.id;

-- ApexesForProgram is a discovery run's perimeter.
--
-- name: ApexesForProgram :many
SELECT DISTINCT pattern
  FROM scope_rule
 WHERE program_id = @program_id::uuid
   AND kind = 'include'
   AND matcher = 'apex'
   AND valid_from <= @at::timestamptz
   AND (valid_to IS NULL OR valid_to > @at::timestamptz)
 ORDER BY pattern;

-- ExclusionsForProgram is the second safety net in front of the network.
--
-- The patterns travel with the perimeter rather than being trusted to the
-- scope re-evaluation at ingestion, because a rule may have changed between a
-- run being defined and a run starting, and the re-evaluation happens after the
-- packet. One probe too many is not much, and it is not a property to accept
-- knowingly when the pattern fits in the invocation.
--
-- name: ExclusionsForProgram :many
SELECT DISTINCT pattern
  FROM scope_rule
 WHERE program_id = @program_id::uuid
   AND kind = 'exclude'
   AND matcher IN ('apex', 'fqdn')
   AND valid_from <= @at::timestamptz
   AND (valid_to IS NULL OR valid_to > @at::timestamptz)
 ORDER BY pattern;

-- RecordRunStart names the execution the platform created.
--
-- Written after the start rather than with the run, because the run is
-- committed first: a platform that refuses on a quota must leave a row the
-- deadline sweeper owns, not an absence.
--
-- name: RecordRunStart :exec
UPDATE run SET external_id = @external_id::text WHERE id = @run_id::uuid;
