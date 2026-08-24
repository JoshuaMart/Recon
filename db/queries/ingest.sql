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
-- @tenant: keyed
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
        sqlc.narg(next_full_at)::timestamptz    AS next_full_at,
        -- Somebody typed this asset in, which is an act rather than an
        -- observation. It is the documented way an archived asset comes back
        -- by hand, and without it the write hands back due dates that every
        -- selection then filters out on the lifecycle, so the answer claims a
        -- schedule nothing will ever read.
        @revive::boolean         AS revive,
        -- Derived in the control plane from the address the run connected to.
        -- They travel with the upsert rather than in a statement of their own:
        -- the enrichment is in hand at the moment the row is written, and a
        -- second round trip on the hottest write path is the thing the budget
        -- exists to refuse.
        sqlc.narg(ip)::inet      AS ip,
        sqlc.narg(asn)::int      AS asn,
        sqlc.narg(asn_org)::text AS asn_org,
        sqlc.narg(country)::char(2) AS country,
        sqlc.narg(region)::text  AS region,
        sqlc.narg(city)::text    AS city
),
previous AS (
    SELECT a.id, a.scope_status, a.discovery_source, c.lifecycle, c.backoff_tier,
           c.http_streak, c.fingerprint_streak, c.http_reachable, c.fingerprint_reachable,
           c.first_seen
      FROM asset a
      LEFT JOIN asset_current c ON c.asset_id = a.id
      JOIN input i ON a.program_id = i.program_id AND a.kind = i.kind AND a.key = i.key
),
layers AS (
    SELECT jsonb_object_agg(l.layer, jsonb_build_object(
               'state', l.state,
               'informative', l.informative_failures,
               'non_informative', l.non_informative_failures,
               'first_failure_at', l.first_failure_at,
               'last_ok_at', l.last_ok_at,
               'last_checked_at', l.last_checked_at)) AS layers
      FROM asset_layer l
      JOIN previous p ON p.id = l.asset_id
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
        host, port, scheme, next_resolve_at, next_full_at,
        ip, asn, asn_org, country, region, city, first_seen, last_seen)
    SELECT
        w.id, i.org_id, i.program_id, i.kind, i.key, i.scope_status,
        i.host, i.port, i.scheme,
        CASE WHEN i.scope_status = 'in_scope' THEN i.next_resolve_at END,
        CASE WHEN i.scope_status = 'in_scope' THEN i.next_full_at END,
        i.ip, i.asn, i.asn_org, i.country, i.region, i.city,
        i.seen_at, i.seen_at
      FROM written w, input i
    ON CONFLICT (asset_id) DO UPDATE SET
        first_seen   = LEAST(asset_current.first_seen, EXCLUDED.first_seen),
        last_seen    = GREATEST(asset_current.last_seen, EXCLUDED.last_seen),
        scope_status = EXCLUDED.scope_status,
        host         = COALESCE(asset_current.host, EXCLUDED.host),
        port         = COALESCE(asset_current.port, EXCLUDED.port),
        scheme       = COALESCE(asset_current.scheme, EXCLUDED.scheme),
        -- Re-evaluated on every pass rather than frozen: a target can move
        -- between two runs. COALESCE keeps what a pass without an address
        -- could not say, so an enrichment does not erase itself.
        ip      = COALESCE(EXCLUDED.ip, asset_current.ip),
        asn     = COALESCE(EXCLUDED.asn, asset_current.asn),
        asn_org = COALESCE(EXCLUDED.asn_org, asset_current.asn_org),
        country = COALESCE(EXCLUDED.country, asset_current.country),
        region  = COALESCE(EXCLUDED.region, asset_current.region),
        city    = COALESCE(EXCLUDED.city, asset_current.city),
        -- Only in-scope assets are scheduled. An asset leaving the perimeter
        -- loses its due dates in the same statement that reclassifies it,
        -- otherwise it keeps being scanned outside the authorization.
        --
        -- And only assets something will actually select. An archived row has
        -- no due dates, so COALESCE would hand it the caller's, and every
        -- selection filters archived out again: the date would be claimed by
        -- nothing and drained by nothing, while the queue counted it as due
        -- forever. A caller that means to bring an archived asset back says so
        -- with revive, which is the same flag that moves the lifecycle two
        -- lines below. The two now agree by construction rather than by the
        -- caller remembering to set both.
        next_resolve_at = CASE WHEN EXCLUDED.scope_status = 'in_scope'
                                AND (asset_current.lifecycle <> 'archived'
                                     OR (SELECT i.revive FROM input i))
                               THEN COALESCE(asset_current.next_resolve_at, EXCLUDED.next_resolve_at) END,
        next_full_at    = CASE WHEN EXCLUDED.scope_status = 'in_scope'
                                AND (asset_current.lifecycle <> 'archived'
                                     OR (SELECT i.revive FROM input i))
                               THEN COALESCE(asset_current.next_full_at, EXCLUDED.next_full_at) END,
        next_fingerprint_at = CASE WHEN EXCLUDED.scope_status = 'in_scope'
                                   THEN asset_current.next_fingerprint_at END,
        lifecycle = CASE
            WHEN (SELECT i.revive FROM input i) AND asset_current.lifecycle = 'archived'
                THEN 'candidate'
            ELSE asset_current.lifecycle END
    RETURNING asset_id
)
-- A left join rather than scalar subqueries, so that the generator can see
-- these are absent on a first appearance. A column it believes non-null is one
-- the caller cannot scan the first time an asset is written.
--
-- The layer counters come back with the identity because the transitions are
-- decided in Go, in this transaction, and reading them separately would put a
-- round trip on the hottest write path for values this statement already has
-- its hands on. They travel as one jsonb object rather than as four sets of
-- columns: the set of layers is open, and adding one would otherwise be a
-- signature change here and at every call site.
SELECT
    w.id      AS asset_id,
    w.created AS created,
    p.scope_status     AS previous_scope_status,
    p.lifecycle        AS previous_lifecycle,
    p.discovery_source AS previous_discovery_source,
    p.backoff_tier     AS previous_backoff_tier,
    p.http_streak      AS previous_http_streak,
    p.fingerprint_streak AS previous_fingerprint_streak,
    p.http_reachable   AS previous_http_reachable,
    p.fingerprint_reachable AS previous_fingerprint_reachable,
    p.first_seen       AS previous_first_seen,
    l.layers           AS previous_layers
  FROM written w
  LEFT JOIN previous p ON TRUE
  LEFT JOIN layers l ON TRUE
 WHERE EXISTS (SELECT 1 FROM projected);

-- WriteObservation deduplicates against the head of the chain, and applies
-- everything the observation decides.
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
-- The layer counters, the lifecycle and the promoted columns travel with it.
-- They are decided in Go, from the counters the upsert already returned, so
-- this statement stores a verdict rather than computing one. Splitting them
-- into a second statement would double the round trips of the hottest write
-- path of the system and open a window where an asset's state contradicts its
-- last observation.
--
-- @tenant: keyed
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
        @data::jsonb               AS data,
        -- The verdict, decided before this statement runs.
        @layer_state::text         AS layer_state,
        @informative::int          AS informative,
        @non_informative::int      AS non_informative,
        sqlc.narg(first_failure_at)::timestamptz AS first_failure_at,
        sqlc.narg(last_ok_at)::timestamptz       AS last_ok_at,
        @lifecycle::text           AS lifecycle,
        -- Promoted columns are written only by the layer that owns them. A
        -- COALESCE would look equivalent and is not: a page that loses its
        -- title would keep the previous one forever.
        @promote::boolean          AS promote,
        sqlc.narg(status_code)::int   AS status_code,
        sqlc.narg(final_url)::text    AS final_url,
        sqlc.narg(title)::text        AS title,
        sqlc.narg(server)::text       AS server,
        sqlc.narg(technologies)::text[] AS technologies,
        sqlc.narg(is_cdn)::boolean    AS is_cdn,
        sqlc.narg(cdn_provider)::text AS cdn_provider,
        sqlc.narg(waf_detected)::boolean AS waf_detected,
        sqlc.narg(waf_vendor)::text   AS waf_vendor,
        -- Signed: consecutive successes above zero, failures below. Null on a
        -- layer that says nothing about an observer's reach.
        sqlc.narg(http_streak)::int   AS http_streak,
        sqlc.narg(http_reachable)::boolean AS http_reachable,
        sqlc.narg(fingerprint_streak)::int AS fingerprint_streak,
        sqlc.narg(fingerprint_reachable)::boolean AS fingerprint_reachable,
        -- Follows the render and never the observation. A failure moving it
        -- would make a list say "rendered five minutes ago, no cookies" about
        -- an asset no browser ever obtained a page from.
        sqlc.narg(last_fingerprint_at)::timestamptz AS last_fingerprint_at,
        sqlc.narg(next_fingerprint_at)::timestamptz AS next_fingerprint_at,
        sqlc.narg(fingerprint_priority)::smallint   AS fingerprint_priority,
        -- The finding without its date, so that a pass which re-confirms it
        -- compares equal and keeps the original.
        sqlc.narg(takeover)::jsonb    AS takeover,
        @takeover_kind::text          AS takeover_kind,
        -- The pivots, lifted out of the payload by the caller. A facet
        -- aggregates over what the table holds and a counter maintained on
        -- write cannot be maintained if the write never sees the value, so
        -- this is the precondition for the whole search chapter rather than a
        -- convenience.
        --
        -- One producer per value: the render owns the four below and the probe
        -- owns the certificate, and each layer's keys are removed and rewritten
        -- together so that a value a layer stops reporting stops being counted.
        sqlc.narg(favicon_hash)::text     AS favicon_hash,
        sqlc.narg(script_hashes)::text[]  AS script_hashes,
        sqlc.narg(cookie_names)::text[]   AS cookie_names,
        sqlc.narg(external_hosts)::text[] AS external_hosts,
        -- Objects rather than names, because the render is the only producer
        -- that knows a version. The column keeps the names.
        sqlc.narg(tech_render)::jsonb     AS tech_render,
        sqlc.narg(cert_spki_hash)::text   AS cert_spki_hash,
        -- One code per hop, and the render's rather than the probe's: the
        -- scanner reports the redirect URLs and the final code, never the code
        -- of each hop, so the probe does not hold what this column is for.
        -- Writing it from the probe would mean inventing the intermediate
        -- codes, which is worse than an empty column.
        sqlc.narg(status_chain)::int[]    AS status_chain
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
),
layered AS (
    INSERT INTO asset_layer (
        asset_id, org_id, layer, state, informative_failures,
        non_informative_failures, first_failure_at, last_ok_at, last_checked_at)
    SELECT
        i.asset_id, i.org_id, i.layer, i.layer_state, i.informative,
        i.non_informative, i.first_failure_at, i.last_ok_at, i.observed_at
      FROM input i
    ON CONFLICT (asset_id, layer) DO UPDATE SET
        state                    = EXCLUDED.state,
        informative_failures     = EXCLUDED.informative_failures,
        non_informative_failures = EXCLUDED.non_informative_failures,
        first_failure_at         = EXCLUDED.first_failure_at,
        -- Never walked backwards: a layer that succeeded once has a date, and
        -- an observation that says nothing must not erase it.
        last_ok_at               = COALESCE(EXCLUDED.last_ok_at, asset_layer.last_ok_at),
        last_checked_at          = GREATEST(asset_layer.last_checked_at, EXCLUDED.last_checked_at)
    RETURNING asset_id
),
attrs AS (
    -- The attributes as they will be, computed once.
    --
    -- Three things need them and they must agree: the projection stores the
    -- object, the technology column is derived from it, and the pivot counters
    -- are the difference between it and what the row holds now. Three
    -- expressions saying the same thing is how a counter drifts from the table
    -- it is supposed to be a function of.
    SELECT c.asset_id, c.attributes AS before, merged.attributes AS after
      FROM asset_current c, input i,
           LATERAL (SELECT (
               CASE
                   WHEN i.takeover IS NOT NULL THEN
                       c.attributes || jsonb_build_object('takeover_candidate',
                           i.takeover || jsonb_build_object('detected_at', COALESCE(
                               CASE WHEN (c.attributes -> 'takeover_candidate') - 'detected_at' = i.takeover
                                    THEN c.attributes -> 'takeover_candidate' ->> 'detected_at' END,
                               to_char(i.observed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))))
                   -- Cleared only by the layer that could have produced it. DNS
                   -- sees an orphan CNAME and HTTP sees an unclaimed service,
                   -- and neither has any business erasing the other's finding.
                   WHEN c.attributes -> 'takeover_candidate' ->> 'kind' = i.takeover_kind
                       THEN c.attributes - 'takeover_candidate'
                   ELSE c.attributes
               END
               -- The layer's own keys go away and come back together. Merging
               -- alone would keep the favicon of a page that no longer has one,
               -- which is the COALESCE that keeps a title forever wearing
               -- another name, and it would keep counting a pivot nothing
               -- carries.
               - CASE
                   WHEN i.layer = 'fingerprint' THEN
                       ARRAY['favicon_hash', 'script_hashes', 'cookie_names',
                             'external_hosts', 'tech_render']
                   WHEN i.promote THEN ARRAY['cert_spki_hash', 'tech_http']
                   ELSE ARRAY[]::text[]
                 END
           ) || CASE
               WHEN i.layer = 'fingerprint' THEN jsonb_strip_nulls(jsonb_build_object(
                   'favicon_hash', i.favicon_hash,
                   'script_hashes',  CASE WHEN array_length(i.script_hashes, 1) > 0
                                          THEN to_jsonb(i.script_hashes) END,
                   'cookie_names',   CASE WHEN array_length(i.cookie_names, 1) > 0
                                          THEN to_jsonb(i.cookie_names) END,
                   'external_hosts', CASE WHEN array_length(i.external_hosts, 1) > 0
                                          THEN to_jsonb(i.external_hosts) END,
                   'tech_render',    CASE WHEN jsonb_typeof(i.tech_render) = 'array'
                                           AND jsonb_array_length(i.tech_render) > 0
                                          THEN i.tech_render END))
               WHEN i.promote THEN jsonb_strip_nulls(jsonb_build_object(
                   'cert_spki_hash', i.cert_spki_hash,
                   'tech_http',      CASE WHEN array_length(i.technologies, 1) > 0
                                          THEN to_jsonb(i.technologies) END))
               ELSE '{}'::jsonb
           END AS attributes) AS merged
     WHERE c.asset_id = i.asset_id
),
movement AS (
    -- What this observation does to the counters: the difference between the
    -- projection as it will be and as it is, which this statement still holds
    -- because it has not written yet.
    SELECT i.org_id, moved.pivot_type, moved.pivot_value, moved.delta
      FROM input i, attrs a,
           LATERAL (
               SELECT pivot_type, pivot_value, sum(delta)::int AS delta
                 FROM (SELECT gained.pivot_type, gained.pivot_value, 1 AS delta
                         FROM pivot_values(a.after) AS gained(pivot_type, pivot_value)
                       UNION ALL
                       SELECT lost.pivot_type, lost.pivot_value, -1 AS delta
                         FROM pivot_values(a.before) AS lost(pivot_type, pivot_value)) AS movements
                GROUP BY pivot_type, pivot_value
               HAVING sum(delta) <> 0
           ) AS moved
),
raised AS (
    -- Incrementing is the easy half, and the only one that may create a row.
    INSERT INTO pivot_count AS pc (org_id, pivot_type, pivot_value, count)
    SELECT m.org_id, m.pivot_type, m.pivot_value, m.delta
      FROM movement m WHERE m.delta > 0
    ON CONFLICT (org_id, pivot_type, pivot_value)
    DO UPDATE SET count = pc.count + EXCLUDED.count
    RETURNING pc.pivot_value
),
lowered AS (
    -- A decrement cannot travel through the same statement, and the reason is
    -- exact rather than stylistic: PostgreSQL evaluates a table's CHECK
    -- constraints on the proposed row *before* it resolves the conflict, so a
    -- proposed count of -1 fails "count >= 0" even though the row it would
    -- become is an update to zero. That is one error message away from
    -- somebody removing the constraint, and the constraint is what catches a
    -- decrement running twice.
    --
    -- An UPDATE is also the honest shape here: a decrement targets a value the
    -- projection already carried, so there is nothing to create. If it matches
    -- no row the counter had already drifted, and the recount is what says so.
    --
    -- The two never touch the same row: a value has one delta, and the deltas
    -- are split on its sign.
    UPDATE pivot_count pc SET count = pc.count + m.delta
      FROM movement m
     WHERE m.delta < 0
       AND pc.org_id = m.org_id AND pc.pivot_type = m.pivot_type
       AND pc.pivot_value = m.pivot_value
    RETURNING pc.pivot_value
),
projected AS (
    UPDATE asset_current c SET
        lifecycle  = i.lifecycle,
        dns_state  = CASE WHEN i.layer = 'dns'  THEN i.layer_state ELSE c.dns_state  END,
        tcp_state  = CASE WHEN i.layer = 'tcp'  THEN i.layer_state ELSE c.tcp_state  END,
        http_state = CASE WHEN i.layer = 'http' THEN i.layer_state ELSE c.http_state END,

        status_code  = CASE WHEN i.promote THEN i.status_code  ELSE c.status_code  END,
        final_url    = CASE WHEN i.promote THEN i.final_url    ELSE c.final_url    END,
        -- Written by the render alone, and rewritten whole on every render so
        -- that a page which stops redirecting stops showing a chain. Untouched
        -- by every other layer, which is what keeps one producer per value.
        status_chain = CASE WHEN i.layer = 'fingerprint' THEN i.status_chain
                            ELSE c.status_chain END,
        title        = CASE WHEN i.promote THEN i.title        ELSE c.title        END,
        server       = CASE WHEN i.promote THEN i.server       ELSE c.server       END,
        -- Derived from the two keys that feed it rather than written by one
        -- layer, so a render's detection and a probe's are both filterable and
        -- neither erases the other.
        technologies = technology_names(a.after),
        waf_detected = CASE WHEN i.promote THEN i.waf_detected ELSE c.waf_detected END,
        waf_vendor   = CASE WHEN i.promote THEN i.waf_vendor   ELSE c.waf_vendor   END,
        -- Structural and observed identically by both observers, so it is
        -- re-evaluated on every pass that can see it and never frozen: a
        -- target can move behind an edge between two runs.
        is_cdn       = COALESCE(i.is_cdn, c.is_cdn),
        -- Follows is_cdn rather than coalescing on its own. A target that
        -- moved off an edge would otherwise keep the provider it used to sit
        -- behind, and a console showing a name beside a false flag is worse
        -- than one showing neither.
        cdn_provider = CASE WHEN i.is_cdn IS NULL THEN c.cdn_provider ELSE i.cdn_provider END,

        http_streak    = COALESCE(i.http_streak, c.http_streak),
        http_reachable = COALESCE(i.http_reachable, c.http_reachable),
        fingerprint_streak    = COALESCE(i.fingerprint_streak, c.fingerprint_streak),
        fingerprint_reachable = COALESCE(i.fingerprint_reachable, c.fingerprint_reachable),
        last_fingerprint_at   = COALESCE(i.last_fingerprint_at, c.last_fingerprint_at),

        -- A service earns its render once it has answered. The date is only
        -- ever brought forward, so a baseline already due is not pushed back
        -- by the pass that re-confirms the service.
        next_fingerprint_at = CASE
            WHEN i.next_fingerprint_at IS NULL OR c.scope_status <> 'in_scope' THEN c.next_fingerprint_at
            ELSE LEAST(COALESCE(c.next_fingerprint_at, i.next_fingerprint_at), i.next_fingerprint_at) END,
        -- Applied with the first schedule and never again. A baseline enters
        -- low, and a pass that merely re-confirms the service must not push a
        -- render somebody is waiting on back down to it.
        fingerprint_priority = CASE
            WHEN i.fingerprint_priority IS NULL OR c.next_fingerprint_at IS NOT NULL
                THEN c.fingerprint_priority
            ELSE i.fingerprint_priority END,

        -- Computed in the CTE above, because the counters and the technology
        -- column are read from the same value. The takeover timestamp is added
        -- there rather than by the probe: a date inside the payload would
        -- differ on every pass, so a dangling CNAME probed hourly would write a
        -- row an hour and defeat deduplication on exactly the assets worth
        -- following.
        attributes = a.after,

        last_seen       = GREATEST(c.last_seen, i.observed_at),
        last_checked_at = GREATEST(c.last_checked_at, i.observed_at),
        last_ok_at      = COALESCE(i.last_ok_at, c.last_ok_at),

        -- A change is an insertion that had something to differ from.
        --
        -- Only an insertion, because a confirmation of the same state is a
        -- probe and a date that moved on every probe would make every filter on
        -- recency return the whole inventory. And only where a head existed,
        -- because a first observation is a first contact: the asset already
        -- carries its arrival as its age, and counting it here would count it
        -- once per layer, so a freshly discovered asset would score three or
        -- four and "volatility > 2" would return everything just found, which
        -- is the opposite of the question.
        last_changed_at = CASE
            WHEN EXISTS (SELECT 1 FROM inserted) AND EXISTS (SELECT 1 FROM head)
                THEN i.observed_at ELSE c.last_changed_at END,

        -- Seven daily buckets plus one of margin, so no total is stored and
        -- nothing has to be decremented as a change expires. The shift and the
        -- increment are one call: separated, the increment lands in a bucket
        -- belonging to another day.
        change_buckets = CASE
            WHEN EXISTS (SELECT 1 FROM inserted) AND EXISTS (SELECT 1 FROM head)
                THEN record_change(c.change_buckets, c.buckets_day,
                                   (i.observed_at AT TIME ZONE 'UTC')::date)
            ELSE c.change_buckets END,
        -- Written from the same instant the bucket was chosen with, and in UTC,
        -- because observed_at is stored in UTC. Reading the database's local
        -- calendar day here would move every boundary by the offset and be
        -- invisible to any test that does not cross midnight.
        buckets_day = CASE
            WHEN EXISTS (SELECT 1 FROM inserted) AND EXISTS (SELECT 1 FROM head)
                THEN (i.observed_at AT TIME ZONE 'UTC')::date
            ELSE c.buckets_day END
      FROM input i, attrs a
     WHERE c.asset_id = i.asset_id AND a.asset_id = c.asset_id
    RETURNING c.asset_id
)
SELECT
    EXISTS (SELECT 1 FROM confirmed) AS deduplicated,
    EXISTS (SELECT 1 FROM projected) AS projected,
    (SELECT h.data FROM head h WHERE NOT EXISTS (SELECT 1 FROM confirmed)) AS previous_data,
    (SELECT h.last_producer_version FROM head h WHERE NOT EXISTS (SELECT 1 FROM confirmed)) AS previous_producer_version;

-- DeadExternalHosts is the internal half, read from the referencing side.
--
-- An external host is out of scope by definition: it belongs to somebody else,
-- it gets no due date and nothing ever probes it, so its death cannot be
-- observed for lack of ever having looked. What is detectable is this cross
-- reference: a host a page loads a script from, present in the same
-- organization's inventory, and declared dead by the lifecycle. That is the
-- home made CDN, the static bucket, the build domain, dying while production
-- pages keep pointing at it.
--
-- Only inactive. An archived asset is one that never came alive rather than one
-- that died, and a script pointing at a name nothing ever answered on is not
-- the same finding.
--
-- @tenant: scoped
-- name: DeadExternalHosts :many
SELECT DISTINCT c.host
  FROM asset_current c
 WHERE c.org_id = @org_id::uuid
   AND c.host = ANY(@hosts::text[])
   -- The name, not one of its services. A service going quiet while the host
   -- still resolves is a port closing, which the lifecycle already speaks for,
   -- and reading it as a dead external host would raise a critical alert every
   -- time somebody turned off a port.
   AND c.kind = 'fqdn'
   AND c.lifecycle = 'inactive';

-- ReferencesToHost is the same finding read from the other side.
--
-- Covering one direction covers about half the cases, and which half depends on
-- the order two unrelated events happened in, which is not a property anybody
-- can predict or explain. The page can start pointing at a host that is already
-- dead, which the statement above sees, or a host that pages already point at
-- can die, which only this one sees.
--
-- Bounded, because a widely referenced internal host is exactly the case that
-- matters and exactly the one that would produce a thousand events.
--
-- @tenant: scoped
-- name: ReferencesToHost :many
SELECT c.asset_id, c.program_id, c.key
  FROM asset_current c
 WHERE c.org_id = @org_id::uuid
   AND c.attributes @> jsonb_build_object('external_hosts', jsonb_build_array(@host::text))
   AND c.lifecycle <> 'archived'
 ORDER BY c.asset_id
 LIMIT @cap::int;

-- ThirdPartyHosts is what the external half walks.
--
-- The other case, and the more dangerous of the two: a genuine third party
-- whose domain expired and anyone can re-register. Covering it means resolving
-- names the organization does not own, which is allowed because what the
-- guardrails protect is interaction with somebody else's infrastructure, and a
-- query to a public resolver reaches neither.
--
-- Hosts already in the same organization's inventory are skipped: those are the
-- internal half's, and they have a real lifecycle behind them. Archived assets
-- are skipped for the reason pivots are: an archived asset is not a lead.
--
-- The cap bounds one page and never the tick. A fixed limit with a fixed order
-- would walk the same lowest identifiers on every run, so a deployment with
-- more references than the cap would leave its tail permanently unswept: an
-- expired domain referenced only by a high identifier would never be resolved
-- and never told, and nothing anywhere would say so.
--
-- The verdict already recorded comes back with the row, because a lookup that
-- failed must leave the previous answer alone. Rewriting the list without it
-- would drop the entry and the next successful tick would read it as newly
-- dead, so one bad minute from a resolver re-sends every critical alert it
-- covers.
--
-- @tenant: cross-org
-- @why: the sweep serves every tenant in one tick, and the set of third party
--       hosts to resolve is deduplicated across all of them: the same public CDN
--       appears in every inventory and is worth one lookup, not one per tenant.
-- name: ThirdPartyHosts :many
SELECT c.asset_id, c.org_id, c.program_id, c.key, e.host,
       COALESCE(c.attributes -> 'dead_external_hosts', '[]'::jsonb) AS recorded
  FROM asset_current c,
       LATERAL jsonb_array_elements_text(c.attributes -> 'external_hosts') AS e(host)
 WHERE jsonb_typeof(c.attributes -> 'external_hosts') = 'array'
   AND c.lifecycle <> 'archived'
   AND c.asset_id > @after::uuid
   AND NOT EXISTS (
        SELECT 1 FROM asset_current own
         WHERE own.org_id = c.org_id AND own.host = e.host)
 ORDER BY c.asset_id, e.host
 LIMIT @cap::int;

-- MarkDeadExternalHosts writes the verdict onto the referencing asset.
--
-- Onto the asset and never into a table of its own, which is what keeps the
-- rule true: the third party has no asset row, no identity and no due date, it
-- is a property of the pages that point at it. Creating one would mean
-- scheduling checks against a domain nothing authorizes probing, which is doing
-- by the side door what is forbidden head on.
--
-- The previous list comes back, which is what makes the emission idempotent
-- without a second mechanism: the event is produced when a host *enters* it, and
-- the tick that finds the same domain still gone writes the same value and says
-- nothing.
--
-- @tenant: keyed
-- name: MarkDeadExternalHosts :one
WITH previous AS (
    SELECT asset_id, COALESCE(attributes -> 'dead_external_hosts', '[]'::jsonb) AS before
      FROM asset_current WHERE asset_id = @asset_id::uuid
),
written AS (
    UPDATE asset_current c SET attributes = CASE
        WHEN array_length(@dead::text[], 1) IS NULL THEN c.attributes - 'dead_external_hosts'
        ELSE c.attributes || jsonb_build_object('dead_external_hosts', to_jsonb(@dead::text[]))
        END
      FROM previous p
     WHERE c.asset_id = p.asset_id
    RETURNING c.asset_id
)
SELECT p.before FROM previous p WHERE EXISTS (SELECT 1 FROM written);

-- StoreFavicon keeps one copy of an image per organization.
--
-- One row per distinct favicon whatever the number of assets sharing it, which
-- is the property that matters since a shared favicon is the interesting case.
-- Not in the projection: one or two kilobytes per asset in the hottest write
-- path and in every search response is gigabytes on a large inventory, and an
-- image is neither a filter nor a pivot. It is the depiction of one.
--
-- DO NOTHING makes a known favicon a no-op, so writing costs nothing in steady
-- state. It is not a screenshot cache: a screenshot never reaches PostgreSQL
-- and normalization drops it, and the difference is not one of degree.
--
-- @tenant: scoped
-- name: StoreFavicon :exec
INSERT INTO favicon_image (org_id, hash, media_type, bytes)
VALUES (@org_id::uuid, @hash::text, @media_type::text, @bytes::bytea)
ON CONFLICT (org_id, hash) DO NOTHING;

-- CountObservations is the deduplication rate's denominator on a replayed set.
--
-- @tenant: scoped
-- name: CountObservations :one
SELECT count(*) FROM observation WHERE org_id = @org_id::uuid;

-- ListScopeRules returns a program's perimeter as it stands at an instant.
--
-- The instant is a parameter rather than now(): valid_to is written by the
-- application and now() is the database's clock, so comparing the two would
-- make the answer depend on two clocks agreeing, and a rule closed a moment
-- ago would read as still in force for the width of the gap.
--
-- @tenant: keyed
-- name: ListScopeRules :many
SELECT id, kind, matcher, pattern
  FROM scope_rule
 WHERE program_id = @program_id::uuid
   AND valid_from <= @at::timestamptz
   AND (valid_to IS NULL OR valid_to > @at::timestamptz)
 ORDER BY id;

-- ListProgramAssets walks a program for a reclassification.
--
-- The address comes with it. A CIDR rule can only match a name through what it
-- resolved to, so a walk without one re-evaluates every asset a CIDR exclusion
-- decided as though it had no address: it falls through to the apex include,
-- becomes in scope again, and gets its due dates back. That is a scan outside
-- the authorization, produced by the pass that exists to prevent one.
--
-- @tenant: keyed
-- name: ListProgramAssets :many
SELECT a.id, a.kind, a.key, a.host, a.scope_status, c.ip
  FROM asset a
  LEFT JOIN asset_current c ON c.asset_id = a.id
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
-- @tenant: keyed
-- name: ApplyScopeStatus :exec
WITH updated AS (
    UPDATE asset SET scope_status = @scope_status::text
     WHERE id = @asset_id::uuid
    RETURNING id
)
UPDATE asset_current SET
    scope_status = @scope_status::text,
    -- Archived rows are not scheduled here either, for the reason the upsert
    -- gives: they have no due dates, so COALESCE would hand them these, every
    -- selection filters archived out again, and the queue counts a slot nothing
    -- will ever drain. A rule that brings an abandoned asset back into scope
    -- does not by itself decide to chase it again.
    next_resolve_at = CASE WHEN @scope_status::text = 'in_scope' AND lifecycle <> 'archived'
                           THEN COALESCE(next_resolve_at, sqlc.narg(next_resolve_at)::timestamptz) END,
    next_full_at    = CASE WHEN @scope_status::text = 'in_scope' AND lifecycle <> 'archived'
                           THEN COALESCE(next_full_at, sqlc.narg(next_full_at)::timestamptz) END,
    next_fingerprint_at = CASE WHEN @scope_status::text = 'in_scope'
                               THEN next_fingerprint_at END
 WHERE asset_id = (SELECT id FROM updated);

-- ProgramForRun reads what an ingestion has to check before writing anything.
--
-- The authorization window is checked here rather than only the state: a run
-- that started before an expiry must not write after it.
--
-- @tenant: keyed
-- name: ProgramForRun :one
SELECT p.id, p.org_id, p.state, p.authorized_from, p.authorized_to
  FROM program p
 WHERE p.id = @program_id::uuid;

-- ReplaceGenericPivots is the whole of the seed's write.
--
-- Replacement rather than merge: a merge would let an entry added outside the
-- repository survive every deployment, and the divergence would be invisible.
--
-- @tenant: none
-- name: ClearGenericPivots :exec
DELETE FROM generic_pivot_value;

-- @tenant: none
-- name: InsertGenericPivot :exec
INSERT INTO generic_pivot_value (pivot_type, pattern, note)
VALUES (@pivot_type::text, @pattern::text, sqlc.narg(note)::text)
ON CONFLICT (pivot_type, pattern) DO UPDATE SET note = EXCLUDED.note;

-- EnsurePartitions creates this month's partition and the next ones.
--
-- @tenant: none
-- name: EnsurePartitions :one
SELECT ensure_monthly_partitions(to_regclass(@target::text), @months_ahead::int);

-- RunForIngest reads everything an ingestion checks before writing anything.
--
-- The authorization window comes back with the run rather than in a second
-- query: a run that started before an expiry must not write after it, and the
-- check is worth nothing if it is one somebody can forget to make.
--
-- OrgForRun turns a run credential into the tenant it belongs to.
--
-- One statement, before the ingestion transaction and outside it, on the system
-- pool. Once per report and never once per observation, so the round trip
-- budget the write path is measured against does not move.
--
-- The run is then read again inside the scoped transaction, under the policy,
-- which is what makes this lookup safe to be unfiltered: a credential whose
-- organization does not match its run finds nothing there.
--
-- @tenant: cross-org
-- @why: the same shape as PrincipalForToken. A credential names itself rather
--       than an organization, so the statement resolving it is the one that
--       discovers the tenant and cannot be filtered by one.
-- name: OrgForRun :one
SELECT org_id FROM run WHERE id = @run_id::uuid;

-- @tenant: keyed
-- name: RunForIngest :one
SELECT r.id, r.org_id, r.program_id, r.kind, r.scope, r.state, r.deadline,
       p.state AS program_state, p.authorized_from, p.authorized_to,
       -- The first run grace, read here rather than in a query of its own.
       -- Ingestion has a round trip budget and this statement already stands
       -- between the credential and the write, so the three facts the grace is
       -- decided from ride along for nothing.
       p.created_at AS program_created_at,
       EXISTS (SELECT 1 FROM run d
                WHERE d.program_id = p.id AND d.kind = 'discovery'
                  AND d.state = 'completed') AS completed_discovery,
       EXISTS (SELECT 1 FROM run d
                WHERE d.program_id = p.id AND d.kind = 'discovery') AS any_discovery,
       -- Bounded, because the grace only ever asks whether the inventory is
       -- under the threshold. A full count over a programme's assets on every
       -- ingestion is not the "rides along for nothing" this comment claims.
       (SELECT count(*) FROM (
            SELECT 1 FROM asset a WHERE a.program_id = p.id LIMIT 500) bounded) AS assets
  FROM run r
  JOIN program p ON p.id = r.program_id
 WHERE r.id = @run_id::uuid;

-- @tenant: keyed
-- name: ListRunTargets :many
SELECT key FROM run_target WHERE run_id = @run_id::uuid;

-- @tenant: scoped
-- name: CreateRun :exec
INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline, target_count)
VALUES (@id::uuid, @org_id::uuid, @program_id::uuid, @kind::text, @scope::text,
        'pending', @deadline::timestamptz, sqlc.narg(target_count)::int);

-- CloseRun records what a run did.
--
-- started_at is written by the first report and never moved afterwards: it is
-- the only thing that separates a run something actually opened from one whose
-- provisioning failed, and those two call for opposite actions.
--
-- @tenant: keyed
-- name: CloseRun :exec
UPDATE run SET
    state       = @state::text,
    started_at  = COALESCE(started_at, @at::timestamptz),
    finished_at = @at::timestamptz,
    summary     = @summary::jsonb,
    error       = sqlc.narg(error)::text
 WHERE id = @run_id::uuid;

-- RescheduleAsset moves an asset's due dates after a report answered for it.
--
-- Which dates move is decided by the run's scope. An asset due for full does
-- not need a resolve run, because full runs every rung below it, and the
-- reverse is not true: a resolve run learns nothing about a port.
--
-- A host the report never mentioned is not passed here at all. It gets no
-- observation, no counter and no new due date, so the next tick selects it
-- again. Silence is not a measurement, and turning it into one is how a
-- truncated run archives live assets.
--
-- @tenant: keyed
-- name: RescheduleAsset :exec
WITH target AS (
    SELECT asset_id, org_id, attributes FROM asset_current WHERE asset_id = @asset_id::uuid
),
given_back AS (
    -- An archived asset gives back all its pivots although no value changed,
    -- and this is the path that gets forgotten. A counter only ever drifts
    -- through the decrement, and it drifts upward: a pivot announced at 41 that
    -- links 12 is worse than an absent one, because it sends somebody looking
    -- for thirty hosts that do not exist.
    --
    -- Nothing in a payload comparison signals this. It is the lifecycle
    -- transition that says so, which is why it travels in the same statement as
    -- the schedule rather than in a sweep of its own.
    --
    -- An UPDATE rather than an upsert: giving a value back is only ever done to
    -- a counter that already exists, and the CHECK on the column is what
    -- catches it running twice.
    UPDATE pivot_count pc SET count = pc.count - 1
      FROM target t, pivot_values(t.attributes) p
     WHERE @archive::boolean
       AND pc.org_id = t.org_id AND pc.pivot_type = p.pivot_type
       AND pc.pivot_value = p.pivot_value
    RETURNING pc.pivot_value
)
UPDATE asset_current SET
    -- An asset whose budget ran out ends archived rather than inactive. It is
    -- not dead: it never existed, and the two readings call for opposite
    -- things in a console.
    lifecycle = CASE WHEN @archive::boolean THEN 'archived' ELSE lifecycle END,
    -- The keys go with the counters, which is what makes pivot_count a function
    -- of asset_current rather than a tally with its own opinion. A counter that
    -- reflects nothing cannot be repaired, for lack of anywhere to read the
    -- truth; this one recomputes from a scan of the table.
    --
    -- technologies and external_hosts stay where they are. They carry no
    -- counter: they are a filter and an aggregation, and the invariant speaks
    -- only about what is counted.
    attributes = CASE
        WHEN @archive::boolean THEN attributes - ARRAY['favicon_hash', 'cert_spki_hash',
                                                       'script_hashes', 'cookie_names']
        ELSE attributes END,
    -- An asset that is archived carries no due date, whether this report is what
    -- archived it or it was already out of the scheduler.
    --
    -- The second half is not symmetry. An archived asset is only ever reached by
    -- an enumeration finding it again, and a rediscovery that failed must not
    -- pull it back into a queue it left: without this it would leave with a
    -- fresh date on every report that mentions it and never be selected again
    -- by anything that could clear one. The lifecycle column already holds this
    -- report's verdict here, because the projection wrote it a statement ago, so
    -- an asset revived by a success reads 'active' and takes its dates normally.
    next_resolve_at = CASE
        WHEN @archive::boolean OR lifecycle = 'archived' OR scope_status <> 'in_scope' THEN NULL
        WHEN @move_resolve::boolean THEN sqlc.narg(next_resolve_at)::timestamptz
        ELSE next_resolve_at END,
    next_full_at = CASE
        WHEN @archive::boolean OR lifecycle = 'archived' OR scope_status <> 'in_scope' THEN NULL
        WHEN @move_full::boolean THEN sqlc.narg(next_full_at)::timestamptz
        ELSE next_full_at END,
    next_fingerprint_at = CASE WHEN @archive::boolean THEN NULL ELSE next_fingerprint_at END,
    backoff_tier = @backoff_tier::int,
    -- Separates "given up after forty tries" from "never managed to test". The
    -- second usually points at a local problem, a resolver or a banned
    -- address, rather than at the target, and it has to stay visible.
    total_attempts = total_attempts + 1
 WHERE asset_id = (SELECT asset_id FROM target);

-- ScheduleDeclaredURLs gives a declared path its render once its service has
-- answered.
--
-- A URL has no liveness of its own: what answers is the service. So a hand
-- entered URL earns nothing until the service it belongs to has been reached,
-- and then it is rendered at the path as declared rather than at the service
-- root. A scanned path is a byproduct; a declared one is an act.
--
-- One statement for a whole report rather than one per service: it reads a
-- state the observations have already written.
--
-- @tenant: keyed
-- name: ScheduleDeclaredURLs :exec
UPDATE asset_current u SET
    next_fingerprint_at  = @at::timestamptz,
    fingerprint_priority = @priority::smallint
  FROM asset a
  JOIN asset_current s ON s.asset_id = a.parent_asset_id
 WHERE u.asset_id = a.id
   AND a.program_id = @program_id::uuid
   AND a.kind = 'url'
   AND u.scope_status = 'in_scope'
   AND u.next_fingerprint_at IS NULL
   AND s.http_state = 'healthy';

-- PrincipalForToken resolves a console credential.
--
-- Only the hash is stored, so the value is printed once and never recoverable.
-- The window and the revocation are checked in the statement rather than after
-- it: a caller that forgot one of them would hold a credential nobody can take
-- away.
--
-- @tenant: cross-org
-- @why: a token names itself and never an organization, so this is the
--       statement that discovers the tenant and cannot be filtered by one. One row,
--       keyed by a hash the caller has to hold already.
-- name: PrincipalForToken :one
SELECT t.id, t.org_id, t.created_by, t.scopes
  FROM api_token t
 WHERE t.token_hash = @token_hash::bytea
   AND t.revoked_at IS NULL
   AND (t.expires_at IS NULL OR t.expires_at > @at::timestamptz);
