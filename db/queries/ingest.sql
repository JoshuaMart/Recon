-- Ingestion writes. The budget is a few round trips per observation, so what travels
-- together travels in one statement, not a readable sequence of several.

-- UpsertAssetAndProjection writes the identity and its projection at once and returns
-- the state that preceded them.
--
-- The previous row is read on the statement's starting snapshot, so the inserting CTE
-- is invisible to the reading one. That makes the previous lifecycle and scope status
-- free, where the alternative is a second round trip on the hottest write path.
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
        -- Somebody typed this asset in: an act, not an observation, and the documented way an
        -- archived asset returns by hand. Without it the write hands back due dates that every
        -- selection filters out on lifecycle, claiming a schedule nothing will read.
        @revive::boolean         AS revive,
        -- Derived in the control plane from the address the run connected to. They travel with
        -- the upsert, as the enrichment is in hand when the row is written and a second round
        -- trip on the hottest write path is what the budget refuses.
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
        -- first_seen can only move backwards, which keeps a cursor on it safe: a row that moves
        -- becomes older than a cursor already passed, so it is neither re-emitted nor missed.
        first_seen   = LEAST(asset.first_seen, EXCLUDED.first_seen),
        last_seen    = GREATEST(asset.last_seen, EXCLUDED.last_seen),
        -- Re-evaluated on every ingestion, not only when a run starts: that is
        -- what lets a rule change reclassify history without rescanning.
        scope_status = EXCLUDED.scope_status,
        -- Derivable from the key, so a row written before it was filled gets it on its next
        -- observation, not through a catch-up migration reimplementing key parsing in SQL.
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
        -- Re-evaluated every pass, not frozen: a target can move between runs. COALESCE keeps
        -- what a pass without an address could not say, so enrichment does not erase itself.
        ip      = COALESCE(EXCLUDED.ip, asset_current.ip),
        asn     = COALESCE(EXCLUDED.asn, asset_current.asn),
        asn_org = COALESCE(EXCLUDED.asn_org, asset_current.asn_org),
        country = COALESCE(EXCLUDED.country, asset_current.country),
        region  = COALESCE(EXCLUDED.region, asset_current.region),
        city    = COALESCE(EXCLUDED.city, asset_current.city),
        -- Only in-scope assets are scheduled. An asset leaving the perimeter loses its due
        -- dates in the statement that reclassifies it, or it keeps being scanned outside the
        -- authorization.
        --
        -- And only assets something will select. An archived row has no due dates, so COALESCE
        -- would hand it the caller's, every selection filters archived out, and the queue
        -- would count it as due forever. A caller bringing an archived asset back says so with
        -- revive, the flag that moves the lifecycle two lines below, so the two agree by
        -- construction.
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
-- A left join, not scalar subqueries, so the generator sees these are absent on a
-- first appearance: a column it believes non-null cannot be scanned the first time.
--
-- The layer counters come back with the identity because transitions are decided in Go
-- in this transaction, and reading them separately would add a round trip for values
-- already in hand. One jsonb object, not four sets of columns: the set of layers is
-- open, and adding one would otherwise change this signature and every call site.
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

-- WriteObservation deduplicates against the head of the chain and applies everything the
-- observation decides. It inserts only when the observation differs from the last of the same
-- (asset, layer), so each row means "this state held from observed_at to last_confirmed_at":
-- volume drops by over an order of magnitude without losing information, and consecutive rows
-- are distinct states by construction.
--
-- The comparison covers outcome and data, never the producer version: a version bump changing
-- nothing in the result must not write a row, which is why it is stored twice.
--
-- Counters, lifecycle and promoted columns travel with it. They are decided in Go from the
-- counters the upsert returned, so this stores a verdict; a second statement would double the
-- hottest path's round trips and open a window where state contradicts the last observation.
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
        -- Promoted columns are written only by the layer that owns them. COALESCE looks
        -- equivalent but a page that loses its title would keep the previous one forever.
        @promote::boolean          AS promote,
        -- The scheme a render actually spoke, sent only by a render. It is the address half of
        -- the same fact as the status code: an imported service has a port and no scheme, so
        -- the console had only `host:port` and no address to open though a browser had already
        -- completed a request against it.
        sqlc.narg(scheme)::text       AS scheme,
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
        -- Follows the render, never the observation: a failure moving it would make a list say
        -- "rendered five minutes ago, no cookies" about an asset no browser got a page from.
        sqlc.narg(last_fingerprint_at)::timestamptz AS last_fingerprint_at,
        sqlc.narg(next_fingerprint_at)::timestamptz AS next_fingerprint_at,
        sqlc.narg(fingerprint_priority)::smallint   AS fingerprint_priority,
        -- The finding without its date, so that a pass which re-confirms it
        -- compares equal and keeps the original.
        sqlc.narg(takeover)::jsonb    AS takeover,
        @takeover_kind::text          AS takeover_kind,
        -- The pivots, lifted out of the payload by the caller. A facet aggregates over what the
        -- table holds, and a counter maintained on write cannot be maintained if the write never
        -- sees the value: a precondition of search, not a convenience.
        --
        -- One producer per value: the render owns the four below and the probe owns the
        -- certificate, and each layer's keys are removed and rewritten together so a value a
        -- layer stops reporting stops being counted.
        sqlc.narg(favicon_hash)::text     AS favicon_hash,
        sqlc.narg(script_hashes)::text[]  AS script_hashes,
        sqlc.narg(cookie_names)::text[]   AS cookie_names,
        sqlc.narg(external_hosts)::text[] AS external_hosts,
        -- Objects rather than names, because the render is the only producer
        -- that knows a version. The column keeps the names.
        sqlc.narg(tech_render)::jsonb     AS tech_render,
        sqlc.narg(cert_spki_hash)::text   AS cert_spki_hash,
        -- One code per hop, the render's and not the probe's: the scanner reports redirect URLs
        -- and the final code, never each hop's, so the probe does not hold what this column is
        -- for, and inventing intermediate codes is worse than an empty column.
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
        -- GREATEST, not assigned: two probes landing together could walk the confirmation window
        -- backwards and violate the constraint keeping it ordered.
        last_confirmed_at = GREATEST(o.last_confirmed_at, i.observed_at),
        last_producer_version = COALESCE(i.producer_version, o.last_producer_version)
      FROM head h, input i
     WHERE o.id = h.id AND o.observed_at = h.observed_at
       AND h.outcome = i.outcome
       -- jsonb equality already ignores object key order, not array element order, which is why
       -- normalization sorts the arrays whose order carries nothing.
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
    -- The attributes as they will be, computed once. The projection stores the object, the
    -- technology column derives from it and the pivot counters are the difference between it
    -- and what the row holds: three expressions for one thing is how a counter drifts from
    -- the table it is a function of.
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
                   -- Cleared only by the layer that could have produced it: DNS sees an orphan CNAME and
                   -- HTTP an unclaimed service, and neither may erase the other's finding.
                   WHEN c.attributes -> 'takeover_candidate' ->> 'kind' = i.takeover_kind
                       THEN c.attributes - 'takeover_candidate'
                   ELSE c.attributes
               END
               -- The layer's own keys go away and come back together. Merging alone would keep the
               -- favicon of a page that no longer has one (the keep-a-title COALESCE in another guise)
               -- and keep counting a pivot nothing carries.
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
    -- What this observation does to the counters: the difference between the projection as
    -- it will be and as it is, which this statement still holds as it has not written yet.
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
    -- A decrement cannot travel through the same statement: PostgreSQL evaluates CHECK constraints
    -- on the proposed row before resolving the conflict, so a proposed count of -1 fails
    -- "count >= 0" though the resulting row would be zero. The constraint is what catches a
    -- decrement running twice, so it must stay. An UPDATE is the honest shape anyway: a decrement
    -- targets a value the projection carried, and a miss means the counter drifted (the recount
    -- says so). The two never touch the same row: a value has one delta, split on its sign.
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

        -- Kept once established, like the ingest path's. A scheme belongs to the service, not
        -- one request: the first observer to establish it is right, and a null from another
        -- layer must not erase it.
        scheme       = COALESCE(c.scheme, i.scheme),

        status_code  = CASE WHEN i.promote THEN i.status_code  ELSE c.status_code  END,
        final_url    = CASE WHEN i.promote THEN i.final_url    ELSE c.final_url    END,
        -- Written by the render alone and rewritten whole each render, so a page that stops
        -- redirecting stops showing a chain. Untouched by other layers: one producer per value.
        status_chain = CASE WHEN i.layer = 'fingerprint' THEN i.status_chain
                            ELSE c.status_chain END,
        title        = CASE WHEN i.promote THEN i.title        ELSE c.title        END,
        server       = CASE WHEN i.promote THEN i.server       ELSE c.server       END,
        -- Derived from the two keys feeding it, not written by one layer, so a render's and a
        -- probe's detections are both filterable and neither erases the other.
        technologies = technology_names(a.after),
        waf_detected = CASE WHEN i.promote THEN i.waf_detected ELSE c.waf_detected END,
        waf_vendor   = CASE WHEN i.promote THEN i.waf_vendor   ELSE c.waf_vendor   END,
        -- Structural and seen identically by both observers, so re-evaluated on every pass that
        -- can see it, never frozen: a target can move behind an edge between runs.
        is_cdn       = COALESCE(i.is_cdn, c.is_cdn),
        -- Follows is_cdn, not coalescing alone: a target that left an edge would keep its old
        -- provider, and a name beside a false flag is worse than neither.
        cdn_provider = CASE WHEN i.is_cdn IS NULL THEN c.cdn_provider ELSE i.cdn_provider END,

        http_streak    = COALESCE(i.http_streak, c.http_streak),
        http_reachable = COALESCE(i.http_reachable, c.http_reachable),
        fingerprint_streak    = COALESCE(i.fingerprint_streak, c.fingerprint_streak),
        fingerprint_reachable = COALESCE(i.fingerprint_reachable, c.fingerprint_reachable),
        last_fingerprint_at   = COALESCE(i.last_fingerprint_at, c.last_fingerprint_at),

        -- A service earns its render once it has answered. The date only moves forward, so a
        -- baseline already due is not pushed back by the pass re-confirming the service.
        next_fingerprint_at = CASE
            WHEN i.next_fingerprint_at IS NULL OR c.scope_status <> 'in_scope' THEN c.next_fingerprint_at
            ELSE LEAST(COALESCE(c.next_fingerprint_at, i.next_fingerprint_at), i.next_fingerprint_at) END,
        -- Applied with the first schedule only. A baseline enters low, and a pass merely
        -- re-confirming the service must not push a render someone is waiting on back to it.
        fingerprint_priority = CASE
            WHEN i.fingerprint_priority IS NULL OR c.next_fingerprint_at IS NOT NULL
                THEN c.fingerprint_priority
            ELSE i.fingerprint_priority END,

        -- Computed in the CTE above, as counters and the technology column read the same value.
        -- The takeover timestamp is added there, not by the probe: a date inside the payload
        -- would differ every pass, so an hourly-probed dangling CNAME would write a row an hour
        -- and defeat deduplication on the assets worth following.
        attributes = a.after,

        last_seen       = GREATEST(c.last_seen, i.observed_at),
        last_checked_at = GREATEST(c.last_checked_at, i.observed_at),
        last_ok_at      = COALESCE(i.last_ok_at, c.last_ok_at),

        -- A change is an insertion that had something to differ from. Only an insertion: a
        -- confirmation of the same state is a probe, and a date moving each probe would make
        -- every recency filter return the whole inventory. And only where a head existed: a
        -- first observation is first contact, already carried as the asset's age, and counting it
        -- once per layer would score a fresh asset three or four so "volatility > 2" would
        -- return everything just found.
        last_changed_at = CASE
            WHEN EXISTS (SELECT 1 FROM inserted) AND EXISTS (SELECT 1 FROM head)
                THEN i.observed_at ELSE c.last_changed_at END,

        -- Seven daily buckets plus one of margin, so no total is stored and nothing is
        -- decremented as a change expires. Shift and increment are one call: separated, the
        -- increment lands in another day's bucket.
        change_buckets = CASE
            WHEN EXISTS (SELECT 1 FROM inserted) AND EXISTS (SELECT 1 FROM head)
                THEN record_change(c.change_buckets, c.buckets_day,
                                   (i.observed_at AT TIME ZONE 'UTC')::date)
            ELSE c.change_buckets END,
        -- Written from the same instant the bucket was chosen with, in UTC as observed_at is
        -- stored. The database's local calendar day would move every boundary by the offset,
        -- invisible to any test not crossing midnight.
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

-- DeadExternalHosts is the internal half, read from the referencing side. An external host is
-- out of scope by definition (someone else's, never probed), so its death cannot be observed;
-- what is detectable is the cross reference: a host a page loads a script from, present in the
-- same organization's inventory and declared dead by the lifecycle (the home-made CDN, static
-- bucket or build domain dying while production pages still point at it). Only inactive: an
-- archived asset never came alive, which is not the same finding.
--
-- @tenant: scoped
-- name: DeadExternalHosts :many
SELECT DISTINCT c.host
  FROM asset_current c
 WHERE c.org_id = @org_id::uuid
   AND c.host = ANY(@hosts::text[])
   -- The name, not its services: a service going quiet while the host resolves is a port
   -- closing, which the lifecycle speaks for, and would raise a critical alert whenever
   -- someone turned off a port.
   AND c.kind = 'fqdn'
   AND c.lifecycle = 'inactive';

-- ReferencesToHost is the same finding read from the other side. One direction covers
-- about half the cases, depending on the order of two unrelated events: a page can start
-- pointing at an already dead host (the statement above) or a host pages point at can die
-- (only this one).
--
-- Bounded: a widely referenced internal host is the case that matters and would produce
-- a thousand events.
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

-- ThirdPartyHosts is what the external half walks: the more dangerous case, a genuine third
-- party whose domain expired and anyone can re-register. Resolving names the organization does
-- not own is allowed, since a public resolver query touches no one's infrastructure. Hosts
-- already in the same inventory (the internal half's) and archived assets are skipped.
--
-- The cap bounds a page, never the tick: a fixed limit in a fixed order would walk the same
-- lowest identifiers each run and leave the tail permanently unswept.
--
-- The recorded verdict comes back with the row, as a failed lookup must leave the previous
-- answer alone: rewriting the list without it drops the entry, the next good tick reads it as
-- newly dead, and one bad resolver minute re-sends every critical alert.
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

-- MarkDeadExternalHosts writes the verdict onto the referencing asset, never a table of its
-- own: the third party has no asset row, identity or due date, it is a property of the pages
-- pointing at it, and creating one would schedule checks against a domain nothing authorizes
-- probing. The previous list comes back, making emission idempotent: the event is produced
-- when a host enters the list, and a tick finding the domain still gone says nothing.
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

-- StoreFavicon keeps one copy of an image per organization, one row per distinct favicon
-- however many assets share it. Not in the projection: a kilobyte or two per asset in the
-- hottest write path and every search response is gigabytes on a large inventory, and an image
-- is neither a filter nor a pivot but the depiction of one. DO NOTHING makes a known favicon
-- free in steady state. It is not a screenshot cache: screenshots never reach PostgreSQL.
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

-- ListScopeRules returns a program's perimeter as it stands at an instant. A parameter,
-- not now(): valid_to is written by the application and now() is the database's clock,
-- so a rule closed a moment ago would read as in force for the width of the gap.
--
-- @tenant: keyed
-- name: ListScopeRules :many
SELECT id, kind, matcher, pattern
  FROM scope_rule
 WHERE program_id = @program_id::uuid
   AND valid_from <= @at::timestamptz
   AND (valid_to IS NULL OR valid_to > @at::timestamptz)
 ORDER BY id;

-- ListProgramAssets walks a program for a reclassification. The address comes with it: a
-- CIDR rule matches a name only through what it resolved to, so without it the walk
-- re-evaluates a CIDR-excluded asset as having no address, it falls to the apex include,
-- returns to scope and gets its due dates back: a scan outside the authorization from the
-- pass meant to prevent one.
--
-- @tenant: keyed
-- name: ListProgramAssets :many
SELECT a.id, a.kind, a.key, a.host, a.scope_status, c.ip
  FROM asset a
  LEFT JOIN asset_current c ON c.asset_id = a.id
 WHERE a.program_id = @program_id::uuid
 ORDER BY a.id;

-- ApplyScopeStatus moves one asset and its schedule together. An asset that should be
-- out but keeps its dates is scanned outside the authorization, and one that should be in
-- without them costs coverage. Writing the rule and reclassifying in two transactions
-- leaves a window scanning what was just taken away.
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
    -- Archived rows are not scheduled here either (see the upsert): no due dates, so
    -- COALESCE would hand them these, every selection filters archived out and the queue
    -- counts a slot nothing drains. A rule bringing an abandoned asset into scope does not by
    -- itself decide to chase it.
    next_resolve_at = CASE WHEN @scope_status::text = 'in_scope' AND lifecycle <> 'archived'
                           THEN COALESCE(next_resolve_at, sqlc.narg(next_resolve_at)::timestamptz) END,
    next_full_at    = CASE WHEN @scope_status::text = 'in_scope' AND lifecycle <> 'archived'
                           THEN COALESCE(next_full_at, sqlc.narg(next_full_at)::timestamptz) END,
    next_fingerprint_at = CASE WHEN @scope_status::text = 'in_scope'
                               THEN next_fingerprint_at END
 WHERE asset_id = (SELECT id FROM updated);

-- ProgramForRun reads what an ingestion checks before writing anything. The
-- authorization window is checked, not only the state: a run that started before an
-- expiry must not write after it.
--
-- @tenant: keyed
-- name: ProgramForRun :one
SELECT p.id, p.org_id, p.state, p.authorized_from, p.authorized_to
  FROM program p
 WHERE p.id = @program_id::uuid;

-- ReplaceGenericPivots is the whole of the seed's write. Replacement, not merge: a merge
-- would let an entry added outside the repository survive every deployment, invisibly.
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

-- OrgForRun turns a run credential into the tenant it belongs to. One statement before
-- the ingestion transaction and outside it, on the system pool, once per report and never
-- per observation, so the write path's round trip budget does not move.
--
-- The run is read again inside the scoped transaction, under the policy, which makes this
-- lookup safe to be unfiltered: a credential whose organization does not match its run
-- finds nothing there.
--
-- @tenant: cross-org
-- @why: the same shape as PrincipalForToken. A credential names itself rather
--       than an organization, so the statement resolving it is the one that
--       discovers the tenant and cannot be filtered by one.
-- name: OrgForRun :one
SELECT org_id FROM run WHERE id = @run_id::uuid;

-- RunForIngest reads everything an ingestion checks before writing anything. The
-- authorization window comes back with the run, not a second query: a run that started
-- before an expiry must not write after it, and the check is worth nothing if it can be
-- forgotten.
-- @tenant: keyed
-- name: RunForIngest :one
SELECT r.id, r.org_id, r.program_id, r.kind, r.scope, r.state, r.deadline,
       p.state AS program_state, p.authorized_from, p.authorized_to,
       -- The first-run grace, read here and not in a query of its own: ingestion has a round
       -- trip budget and this statement already stands between credential and write, so the
       -- three facts the grace is decided from ride along.
       p.created_at AS program_created_at,
       EXISTS (SELECT 1 FROM run d
                WHERE d.program_id = p.id AND d.kind = 'discovery'
                  AND d.state = 'completed') AS completed_discovery,
       EXISTS (SELECT 1 FROM run d
                WHERE d.program_id = p.id AND d.kind = 'discovery') AS any_discovery,
       -- Bounded, because the grace only asks whether the inventory is under the threshold. A
       -- full count per ingestion is not the "rides along for nothing" claimed above.
       (SELECT count(*) FROM (
            SELECT 1 FROM asset a WHERE a.program_id = p.id LIMIT 500) bounded) AS assets
  FROM run r
  JOIN program p ON p.id = r.program_id
 WHERE r.id = @run_id::uuid
   -- Held until the ingestion commits. A retry of the same report waits here, then finds the
   -- run completed and is refused, instead of writing the document beside the first attempt
   -- and racing it to close the run. The sweeper and a cancel skip a row held this way.
   FOR UPDATE OF r;

-- @tenant: keyed
-- name: ListRunTargets :many
SELECT key FROM run_target WHERE run_id = @run_id::uuid;

-- CreateRun writes the row a definition is about to be launched against. apex is null on a
-- verification and required on a discovery, by a table constraint: a discovery enumerates
-- one root domain, the scanner's limit, so a programme with several apexes gets one run
-- each.
--
-- @tenant: scoped
-- name: CreateRun :exec
INSERT INTO run (id, org_id, program_id, kind, scope, state, deadline, target_count, apex)
VALUES (@id::uuid, @org_id::uuid, @program_id::uuid, @kind::text, @scope::text,
        'pending', @deadline::timestamptz, sqlc.narg(target_count)::int,
        sqlc.narg(apex)::text);

-- CloseRun records what a run did. started_at is written by the first report and never
-- moved: it alone separates a run something opened from one whose provisioning failed,
-- which call for opposite actions.
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

-- RescheduleAsset moves an asset's due dates after a report answered for it. The run's
-- scope decides which: an asset due for full needs no resolve run (full runs every rung
-- below), but not the reverse, as a resolve run learns nothing about a port.
--
-- A host the report never mentioned is not passed here: no observation, counter or new
-- due date, so the next tick selects it again. Silence is not a measurement, and treating
-- it as one is how a truncated run archives live assets.
--
-- @tenant: keyed
-- name: RescheduleAsset :exec
WITH target AS (
    SELECT asset_id, org_id, attributes FROM asset_current WHERE asset_id = @asset_id::uuid
),
given_back AS (
    -- An archived asset gives back all its pivots though no value changed, the path that gets
    -- forgotten: a pivot announced at 41 linking 12 sends someone looking for thirty hosts that do
    -- not exist. No payload comparison signals it; the lifecycle transition does, so it travels
    -- in the schedule's statement, not a sweep of its own. An UPDATE, not an upsert: giving back
    -- only happens to an existing counter, and the column's CHECK catches it running twice.
    UPDATE pivot_count pc SET count = pc.count - 1
      FROM target t, pivot_values(t.attributes) p
     WHERE @archive::boolean
       AND pc.org_id = t.org_id AND pc.pivot_type = p.pivot_type
       AND pc.pivot_value = p.pivot_value
    RETURNING pc.pivot_value
)
UPDATE asset_current SET
    -- A budget-exhausted asset ends archived, not inactive: it is not dead, it never existed,
    -- and a console reads the two oppositely.
    lifecycle = CASE WHEN @archive::boolean THEN 'archived' ELSE lifecycle END,
    -- The keys go with the counters, making pivot_count a function of asset_current and not a
    -- tally with its own opinion: a counter reflecting nothing cannot be repaired, and this
    -- one recomputes from a scan of the table.
    --
    -- technologies and external_hosts stay: they carry no counter (a filter and an
    -- aggregation), and the invariant speaks only of what is counted.
    attributes = CASE
        WHEN @archive::boolean THEN attributes - ARRAY['favicon_hash', 'cert_spki_hash',
                                                       'script_hashes', 'cookie_names']
        ELSE attributes END,
    -- An archived asset carries no due date, whether this report archived it or it was already
    -- out of the scheduler. The second half is not symmetry: an archived asset is reached only by
    -- an enumeration finding it again, and a failed rediscovery must not pull it into a queue it
    -- left, or it would leave with a fresh date on every report mentioning it. The lifecycle
    -- column already holds this report's verdict (the projection wrote it a statement ago), so a
    -- revived asset reads 'active' and takes its dates normally.
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
    -- Separates "given up after forty tries" from "never managed to test". The second usually
    -- points at a local problem (a resolver, a banned address) and must stay visible.
    total_attempts = total_attempts + 1
 WHERE asset_id = (SELECT asset_id FROM target);

-- ScheduleDeclaredURLs gives a declared path its render once its service has answered. A
-- URL has no liveness of its own: the service answers. So a hand-entered URL earns nothing
-- until its service is reached, then is rendered at the path as declared, not the service
-- root. A scanned path is a byproduct; a declared one is an act.
--
-- One statement for a whole report, not per service: it reads state the observations have
-- already written.
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

-- PrincipalForToken resolves a console credential. Only the hash is stored, so the value
-- is printed once and unrecoverable. The window and revocation are checked in the
-- statement, not after: a caller forgetting one would hold a credential nobody can revoke.
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
