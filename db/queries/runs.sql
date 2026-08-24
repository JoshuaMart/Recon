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
-- @tenant: scoped
-- name: SelectDueHosts :many
SELECT c.asset_id, c.key, c.lifecycle, c.backoff_tier
  FROM asset_current c
 WHERE c.org_id = @org_id::uuid
   AND c.program_id = @program_id::uuid
   AND c.kind = 'fqdn'
   AND c.scope_status = 'in_scope'
   AND c.lifecycle <> 'archived'
   -- What belongs to the candidate lane, excluded here because the exclusion is
   -- mutual: without it the two passes fight over the same names, each freezing
   -- what the other was about to take, and which one wins is whichever tick
   -- fired first.
   --
   -- The lane is the pair and never the lifecycle alone, and that correction is
   -- worth the two lines it costs. CANDIDATE is not a Certificate Transparency
   -- state: a host somebody typed into the assets form is one too, until its
   -- first answer. Excluding on the lifecycle alone would divert it into a lane
   -- pinned to resolve, where a resolution would report that the name answers
   -- and nothing would ever sweep its ports, which is the opposite of what a
   -- hand entered host is promised and it is promised it because a person is
   -- waiting.
   --
   -- A null full date is what actually separates them. A candidate that has not
   -- earned the expensive rung is exactly the state the aggressive curve
   -- describes, and it is readable here without joining the asset's source.
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

-- AddRunTargets freezes the list in one round trip.
--
-- One statement rather than one per target: a batch is hundreds of rows, and a
-- run that costs five hundred round trips to define is one nobody will let the
-- scheduler start often.
--
-- Columns as parallel arrays rather than COPY, and not by preference.
-- PostgreSQL refuses COPY FROM outright on a table carrying a policy, with
-- "COPY FROM not supported with row-level security", so the copy path stopped
-- working the day isolation was enabled. This is the same single round trip,
-- and it makes the organization a scalar of the statement rather than a column
-- repeated per row, so a batch mixing two tenants is no longer expressible.
--
-- generate_subscripts rather than a multi-argument unnest, which sqlc's
-- analyser cannot type. The arrays are built together by the caller and are the
-- same length by construction; a short one would read as nulls rather than
-- fail, which is why they are built in one loop and never assembled.
--
-- @tenant: scoped
-- name: AddRunTargets :execrows
INSERT INTO run_target (run_id, asset_id, org_id, key)
SELECT @run_id::uuid, (@asset_ids::uuid[])[i], @org_id::uuid, (@keys::text[])[i]
  FROM generate_subscripts(@asset_ids::uuid[], 1) AS i;

-- LiveRunForProgram is what a second request runs into.
--
-- started_at comes back with it because it is the only thing that separates a
-- run something has actually opened from one whose provisioning failed, and
-- those two call for opposite actions.
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

-- ExpireRuns is the deadline sweeper.
--
-- It does not have to repair anything. Due dates are moved only when a report
-- is ingested, so an abandoned run leaves the inventory exactly as it found it;
-- expiring it frees its targets and makes the failure visible.
--
-- @tenant: cross-org
-- @why: the deadline sweeper serves every tenant in one tick. A run nothing
--       expires holds its targets forever, and the assets it froze are invisible to
--       every later pass.
-- name: ExpireRuns :many
UPDATE run SET
    state       = 'expired',
    finished_at = @at::timestamptz,
    error       = COALESCE(error, 'the deadline passed and no report was delivered')
 WHERE state IN ('pending', 'running')
   AND deadline <= @at::timestamptz
RETURNING id, org_id, program_id, kind, scope, started_at, target_count;

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

-- TouchDiscovery records that a discovery run was provisioned.
--
-- Written at creation rather than at completion. A run that dies on the way
-- must not be restarted by the cadence: the deadline sweeper already handles
-- it, and confusing the two would start two.
--
-- @tenant: keyed
-- name: TouchDiscovery :exec
UPDATE program SET last_discovery_at = @at::timestamptz WHERE id = @program_id::uuid;

-- ProgramsDueForDiscovery is the pass that provisions enumeration.
--
-- The authorization window is checked here rather than only the state. Without
-- those two lines an expired programme would be provisioned on every tick and
-- refused when the run opens: an execution billed to do nothing, every thirty
-- seconds.
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
        -- Or the last discovery ended without delivering, and the retry delay
        -- has passed. A run that failed in thirty seconds must not cost a week
        -- of coverage, and clearing last_discovery_at instead would be worse
        -- twice over: it destroys the record of when this programme was last
        -- enumerated, and it turns a permanently broken runner into a loop that
        -- provisions on every tick and bills every one of them.
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
   -- A programme with nothing to enumerate is not due. It would otherwise be
   -- selected on every tick and refused on every tick, which at a one minute
   -- cadence is a warning a minute forever rather than a signal.
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

-- ExclusionsForProgram is the second safety net in front of the network.
--
-- The patterns travel with the perimeter rather than being trusted to the
-- scope re-evaluation at ingestion, because a rule may have changed between a
-- run being defined and a run starting, and the re-evaluation happens after the
-- packet. One probe too many is not much, and it is not a property to accept
-- knowingly when the pattern fits in the invocation.
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

-- RecordRunStart names the execution the platform created.
--
-- Written after the start rather than with the run, because the run is
-- committed first: a platform that refuses on a quota must leave a row the
-- deadline sweeper owns, not an absence.
--
-- @tenant: keyed
-- name: RecordRunStart :exec
UPDATE run SET external_id = @external_id::text WHERE id = @run_id::uuid;

-- ProgramsDueForVerification is the pass on due dates.
--
-- The counterpart of the discovery cadence and the thing that was missing: due
-- dates were written, the queue view counted them as what the next tick can
-- dispatch, and no tick dispatched them. The engine has existed since
-- verification was built; only the trigger was absent, so a run went out when
-- somebody pressed a button and never otherwise.
--
-- The authorization window is checked here as well as at the run, and what it
-- buys is worth stating exactly rather than borrowing the discovery pass's
-- sentence. Nothing is billed either way: the run is refused before the platform
-- is called. What this prevents is the pass waking an expired programme, walking
-- its due assets, freezing nothing and logging a refusal, once a minute, forever
-- until somebody edits the programme. A warning a minute is not a signal.
--
-- full_due decides the rung without a second query. A full run executes every
-- rung below it and moves both dates, so taking full first cannot starve
-- resolve, and an asset due for full does not need a resolve run.
--
-- The absence of a live verification run is the condition that does the work,
-- exactly as it is for discovery. It is not the guarantee: that is the unique
-- index, because a check that reads and then writes is not a reservation under
-- read committed. This only keeps the pass from provisioning what the index
-- would refuse, once a minute, forever.
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
   -- The same predicate the selection uses, so a programme is never woken for
   -- work the selection would then refuse to find. A name is the unit: an
   -- address is never a target of its own, and a service is observed through
   -- its host's run.
   AND c.kind = 'fqdn'
   AND c.scope_status = 'in_scope'
   AND c.lifecycle <> 'archived'
   AND NOT (c.lifecycle = 'candidate' AND c.next_full_at IS NULL)
   AND (c.next_full_at <= @at::timestamptz OR c.next_resolve_at <= @at::timestamptz)
   AND NOT EXISTS (
        SELECT 1 FROM run r
         WHERE r.program_id = p.id
           -- Either kind. A live verification is the bound the unique index
           -- keeps; a live discovery is the perimeter being walked right now,
           -- and it freezes no targets because it is the one allowed to find
           -- things, so nothing else can see the hosts it is scanning. Waking a
           -- programme in that state would select assets whose report has not
           -- landed yet, and send a second scanner at hosts the first one is
           -- connected to.
           AND r.kind IN ('verification', 'discovery')
           AND r.state IN ('pending', 'running'))
 GROUP BY p.id, p.org_id, p.name
 ORDER BY p.id;

-- SelectDueCandidates is the candidate lane's half of the selection.
--
-- The same columns and the same lease as SelectDueHosts, with the lifecycle
-- reversed: a candidate is selected by this and by no other pass, or the two
-- fight over the same names and each freezes what the other was about to take.
--
-- The rung is not a parameter. A candidate run is pinned to resolve by a CHECK
-- on the table, because one round trip to a resolver pool and nothing sent to
-- the target is what lets this lane run beside a verification without adding up
-- to anything the rate budget has an opinion about.
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
   -- The pair, not the lifecycle. A hand entered host is a candidate too until
   -- it answers, and it carries a full date because somebody typed it in to
   -- find out what it exposes: taking it here would answer that a name
   -- resolves and never sweep a port.
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

-- ProgramsDueForCandidates wakes the lane that must not wait.
--
-- The difference from ProgramsDueForVerification is the whole reason the lane
-- exists, and it is one line: only a live *candidate* run holds this pass back.
--
-- A live verification does not, which is the point: a sweep holds its slot for
-- its whole deadline, and a candidate arriving a minute in would wait half an
-- hour for a check the aggressive curve wanted at sixty seconds.
--
-- Nor does a live discovery, and that is a decision rather than an oversight.
-- Discovery blocks verification because a full sweep would send a second
-- scanner at hosts the first one is connected to. A resolve sends nothing to
-- the target at all, so there is nothing to collide with, and blocking here
-- would spend the freshness advantage on a run that cannot interfere with it.
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
