-- The model. This is the most irreversible migration of the project, which is
-- why the phase that writes it does nothing else first.
--
-- Two conventions hold throughout. Enumerations are text with a named CHECK
-- rather than an enum type: these lists still move, and altering an enum is a
-- migration where editing a constraint is a line. And org_id is on every
-- business table from the first day, because the columns are what is urgent,
-- not the policy that will read them.

-- +goose Up

CREATE TABLE org (
    id         uuid PRIMARY KEY,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Identity through a join table, never a tenant column on the user: somebody
-- has to be able to belong to several organizations without a migration of the
-- authentication layer.
CREATE TABLE app_user (
    id         uuid PRIMARY KEY,
    email      text UNIQUE NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE membership (
    user_id uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
    org_id  uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    role    text NOT NULL DEFAULT 'owner',
    PRIMARY KEY (user_id, org_id)
);

-- A token belongs to the organization, not to a person: one modelled as a
-- person's becomes unmanageable when they leave, and unusable for machine
-- access. Only the hash is stored, so the value is printed once and never
-- recoverable.
CREATE TABLE api_token (
    id         uuid PRIMARY KEY,
    org_id     uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    created_by uuid REFERENCES app_user(id),
    name       text NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    scopes     text[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    revoked_at timestamptz
);
CREATE INDEX api_token_org_idx ON api_token (org_id);

CREATE TABLE program (
    id                 uuid PRIMARY KEY,
    org_id             uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    name               text NOT NULL,
    platform           text,
    -- Descriptive, never a join key between organizations: two of them
    -- tracking the same public target hold two independent inventories.
    platform_ref       text,
    authorized_from    timestamptz NOT NULL,
    authorized_to      timestamptz,
    authorization_ref  text,
    rate_limit_rps     int NOT NULL DEFAULT 10 CHECK (rate_limit_rps > 0),
    discovery_interval interval NOT NULL DEFAULT '7 days',
    last_discovery_at  timestamptz,
    state              text NOT NULL DEFAULT 'active',
    -- Even with one user, two concurrent writes lose each other silently, and
    -- a lost scope is a scan outside the perimeter.
    version            int NOT NULL DEFAULT 1,
    -- Nullable, and the ambiguity is the point of having two: an actor can be
    -- a person or the system, and the two must stay distinguishable.
    created_by         uuid REFERENCES app_user(id),
    updated_by         uuid REFERENCES app_user(id),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT program_state_known CHECK (state IN ('active', 'suspended', 'archived')),
    CONSTRAINT program_window_ordered CHECK (authorized_to IS NULL OR authorized_to > authorized_from)
);
CREATE INDEX program_org_idx ON program (org_id, state);

-- A rule has a period of validity, not an existence. Removing one is setting
-- its valid_to, never a DELETE: an asset classified out of scope by a rule
-- that has since been closed stays reclassifiable, and one can still explain
-- why it was classified that way.
CREATE TABLE scope_rule (
    id         uuid PRIMARY KEY,
    org_id     uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    program_id uuid NOT NULL REFERENCES program(id) ON DELETE CASCADE,
    kind       text NOT NULL,
    matcher    text NOT NULL,
    pattern    text NOT NULL,
    valid_from timestamptz NOT NULL DEFAULT now(),
    valid_to   timestamptz,
    note       text,
    version    int NOT NULL DEFAULT 1,
    created_by uuid REFERENCES app_user(id),
    updated_by uuid REFERENCES app_user(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT scope_rule_kind_known CHECK (kind IN ('include', 'exclude')),
    CONSTRAINT scope_rule_matcher_known CHECK (matcher IN ('apex', 'fqdn', 'cidr', 'regex', 'url_prefix')),
    CONSTRAINT scope_rule_window_ordered CHECK (valid_to IS NULL OR valid_to >= valid_from)
);
CREATE INDEX scope_rule_program_idx ON scope_rule (program_id, valid_to);

CREATE TABLE asset (
    id               uuid PRIMARY KEY,
    org_id           uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    program_id       uuid NOT NULL REFERENCES program(id) ON DELETE CASCADE,
    kind             text NOT NULL,
    -- The canonical form. Deduplication depends entirely on it, which is why
    -- one function in the application owns it and SQL never parses it.
    key              text NOT NULL,
    -- Derivable from the key, therefore written at creation rather than
    -- waited for from an observation. Deriving it in SQL later would be a
    -- second implementation of key parsing, and split_part is wrong on the
    -- first IPv6 key.
    host             text,
    parent_asset_id  uuid REFERENCES asset(id),
    discovery_source text NOT NULL,
    discovery_path   jsonb,
    scope_status     text NOT NULL,
    first_seen       timestamptz NOT NULL,
    last_seen        timestamptz NOT NULL,
    CONSTRAINT asset_kind_known CHECK (kind IN ('fqdn', 'ip', 'service', 'url')),
    CONSTRAINT asset_scope_known CHECK (scope_status IN ('in_scope', 'out_of_scope', 'unknown')),
    CONSTRAINT asset_seen_ordered CHECK (last_seen >= first_seen),
    -- An asset is never its own parent. The case is not theoretical: a
    -- response seen on a service descends from the port that produced it, and
    -- two events that used to be two kinds became one asset.
    CONSTRAINT asset_not_its_own_parent CHECK (parent_asset_id IS DISTINCT FROM id),
    UNIQUE (program_id, kind, key)
);
CREATE INDEX asset_org_program_idx ON asset (org_id, program_id);
CREATE INDEX asset_parent_idx ON asset (parent_asset_id) WHERE parent_asset_id IS NOT NULL;
CREATE INDEX asset_discovery_path_idx ON asset USING gin (discovery_path);

-- The immutable journal. Deduplicated on write, so two consecutive rows of one
-- (asset, layer) are two distinct states by construction, which is what makes
-- the diff trivial and the timeline a list of changes rather than of probes.
CREATE TABLE observation (
    id                    bigserial,
    org_id                uuid NOT NULL,
    asset_id              uuid NOT NULL REFERENCES asset(id) ON DELETE CASCADE,
    -- When this state began, and the last time it was seen unchanged.
    observed_at           timestamptz NOT NULL,
    last_confirmed_at     timestamptz NOT NULL,
    run_id                uuid,
    source                text NOT NULL,
    layer                 text NOT NULL,
    outcome               text NOT NULL,
    -- Two columns because one row outlives several versions: deduplication
    -- deliberately does not compare the producer version, so the first pass
    -- keeps its own and the last confirmation moves the other.
    producer_version      text,
    last_producer_version text,
    data                  jsonb NOT NULL,
    -- PostgreSQL requires the partition key in the primary key, hence the pair
    -- rather than the bigserial alone.
    PRIMARY KEY (id, observed_at),
    CONSTRAINT observation_layer_known CHECK (layer IN ('dns', 'tcp', 'http', 'fingerprint')),
    CONSTRAINT observation_outcome_known CHECK (outcome IN ('ok', 'fail', 'error')),
    -- Taken as a GREATEST rather than assigned: two probes landing at once
    -- could otherwise walk the confirmation window backwards.
    CONSTRAINT observation_window_ordered CHECK (last_confirmed_at >= observed_at)
) PARTITION BY RANGE (observed_at);

-- The chain lookup deduplication performs on every write.
CREATE INDEX observation_chain_idx ON observation (asset_id, layer, observed_at DESC);
CREATE INDEX observation_run_idx ON observation (run_id) WHERE run_id IS NOT NULL;

-- No DEFAULT partition. A row whose month is missing must fail loudly: that is
-- the only signal the creation mechanism has broken, and a default partition
-- would absorb it silently while having to be scanned on every later attach.
SELECT ensure_monthly_partitions('observation', 2);

-- Per layer verdict and counters. Death is a property of a layer rather than
-- of an asset: a name that no longer resolves and a host whose ports all time
-- out are not the same finding, and one of the two proves nothing.
CREATE TABLE asset_layer (
    asset_id                 uuid NOT NULL REFERENCES asset(id) ON DELETE CASCADE,
    org_id                   uuid NOT NULL,
    layer                    text NOT NULL,
    state                    text NOT NULL DEFAULT 'unmeasured',
    informative_failures     int NOT NULL DEFAULT 0,
    non_informative_failures int NOT NULL DEFAULT 0,
    -- Carries the 24 hour floor. Without it the threshold is not checkable at
    -- all: three failures can land inside ninety minutes.
    first_failure_at         timestamptz,
    last_ok_at               timestamptz,
    last_checked_at          timestamptz,
    PRIMARY KEY (asset_id, layer),
    CONSTRAINT asset_layer_layer_known CHECK (layer IN ('dns', 'tcp', 'http', 'fingerprint')),
    CONSTRAINT asset_layer_state_known CHECK (state IN ('unmeasured', 'healthy', 'failing', 'dead'))
);

-- The materialized view, and the only table the search API reads.
CREATE TABLE asset_current (
    asset_id     uuid PRIMARY KEY REFERENCES asset(id) ON DELETE CASCADE,
    org_id       uuid NOT NULL,
    program_id   uuid NOT NULL,
    kind         text NOT NULL,
    key          text NOT NULL,
    scope_status text NOT NULL,

    dns_state  text,
    tcp_state  text,
    http_state text,
    lifecycle  text NOT NULL DEFAULT 'candidate',

    -- Scheduling. Three due dates rather than a queue table: the frozen target
    -- list of a run is the lease, so there is nothing here to expire.
    next_resolve_at      timestamptz,
    next_full_at         timestamptz,
    next_fingerprint_at  timestamptz,
    -- Sorted before the due date, so a change detected after a mass baseline
    -- does not queue behind every one of them.
    fingerprint_priority smallint NOT NULL DEFAULT 100,
    backoff_tier         int NOT NULL DEFAULT 0,
    total_attempts       int NOT NULL DEFAULT 0,

    -- Which observer gets a result on this target, which is a relation between
    -- an observer and a target rather than a property of either.
    http_reachable        boolean,
    fingerprint_reachable boolean,
    -- Signed: consecutive successes above zero, failures below, so the
    -- threshold reads the same in both directions with one column each.
    http_streak        int NOT NULL DEFAULT 0,
    fingerprint_streak int NOT NULL DEFAULT 0,

    -- Promoted columns: what filtering is done on.
    host         text,
    port         int,
    scheme       text,
    ip           inet,
    status_code  int,
    status_chain int[],
    final_url    text,
    title        text,
    server       text,
    asn          int,
    asn_org      text,
    country      char(2),
    region       text,
    city         text,
    is_cdn       boolean,
    cdn_provider text,
    waf_detected boolean,
    waf_vendor   text,
    technologies text[],

    -- Volatility as seven daily buckets plus one of margin. No total is
    -- stored, so nothing has to be decremented when a change expires and no
    -- sweep of the inventory is needed for a display value.
    change_buckets int[] NOT NULL DEFAULT '{0,0,0,0,0,0,0,0}',
    buckets_day    date,

    attributes jsonb NOT NULL DEFAULT '{}',

    first_seen          timestamptz NOT NULL,
    last_seen           timestamptz NOT NULL,
    last_checked_at     timestamptz,
    last_fingerprint_at timestamptz,
    last_ok_at          timestamptz,
    -- Null on an asset that has never changed, which the console reads as
    -- "never". A date borrowed from its discovery would make every filter on
    -- recency return everything just found.
    last_changed_at     timestamptz,

    CONSTRAINT asset_current_lifecycle_known CHECK (
        lifecycle IN ('candidate', 'active', 'flapping', 'inactive', 'unobservable', 'archived')),
    CONSTRAINT asset_current_scope_known CHECK (
        scope_status IN ('in_scope', 'out_of_scope', 'unknown')),
    CONSTRAINT asset_current_buckets_sized CHECK (array_length(change_buckets, 1) = 8)
);

CREATE INDEX asset_current_tenant_idx ON asset_current (org_id, program_id, lifecycle);
CREATE INDEX asset_current_host_idx ON asset_current (org_id, host);
CREATE INDEX asset_current_port_idx ON asset_current (org_id, port);
CREATE INDEX asset_current_status_idx ON asset_current (org_id, status_code);
CREATE INDEX asset_current_scheme_idx ON asset_current (org_id, scheme);
CREATE INDEX asset_current_asn_idx ON asset_current (org_id, asn);
CREATE INDEX asset_current_country_idx ON asset_current (org_id, country);
CREATE INDEX asset_current_cdn_idx ON asset_current (org_id, cdn_provider);
CREATE INDEX asset_current_tech_idx ON asset_current USING gin (technologies);
CREATE INDEX asset_current_attributes_idx ON asset_current USING gin (attributes);
-- The list paginates on this, and a cursor needs a total order.
CREATE INDEX asset_current_recent_idx ON asset_current (org_id, last_seen DESC, asset_id);

-- The query of an ASM inventory is a suffix, "everything under this domain",
-- which no ordinary index can serve. Reversing turns it into a prefix, and
-- reverse() is immutable, so it can be indexed.
CREATE INDEX asset_current_key_reversed_idx ON asset_current (reverse(key) text_pattern_ops);

-- Partial, so a row that has left the scheduler costs nothing to skip.
CREATE INDEX asset_current_due_resolve_idx ON asset_current (next_resolve_at, asset_id)
    WHERE next_resolve_at IS NOT NULL;
CREATE INDEX asset_current_due_full_idx ON asset_current (next_full_at, asset_id)
    WHERE next_full_at IS NOT NULL;
CREATE INDEX asset_current_due_fingerprint_idx
    ON asset_current (fingerprint_priority, next_fingerprint_at, asset_id)
    WHERE next_fingerprint_at IS NOT NULL;

-- One execution of a scanner over one perimeter, producing one report.
CREATE TABLE run (
    id           uuid PRIMARY KEY,
    org_id       uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    program_id   uuid NOT NULL REFERENCES program(id) ON DELETE CASCADE,
    kind         text NOT NULL,
    scope        text NOT NULL,
    state        text NOT NULL DEFAULT 'pending',
    deadline     timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- Written by the first report. It is the only thing that separates a run
    -- something has opened from one whose provisioning failed, and those two
    -- call for opposite actions.
    started_at   timestamptz,
    finished_at  timestamptz,
    target_count int,
    summary      jsonb,
    error        text,
    CONSTRAINT run_kind_known CHECK (kind IN ('discovery', 'verification')),
    CONSTRAINT run_scope_known CHECK (scope IN ('enum', 'resolve', 'ports', 'full')),
    CONSTRAINT run_state_known CHECK (state IN ('pending', 'running', 'completed', 'failed', 'expired'))
);
CREATE INDEX run_program_idx ON run (program_id, created_at DESC);

-- One discovery run per program at a time. It prevents a provisioning storm,
-- since last_discovery_at is written when the run is created rather than when
-- it completes, and it bounds concurrency where the budget cannot.
CREATE UNIQUE INDEX run_one_live_discovery_idx ON run (program_id)
    WHERE kind = 'discovery' AND state IN ('pending', 'running');

-- The frozen target list of a verification run, and the whole of the
-- reservation mechanism: selection excludes assets already listed in a run
-- that has not finished, so the list expires when the run does.
CREATE TABLE run_target (
    run_id   uuid NOT NULL REFERENCES run(id) ON DELETE CASCADE,
    asset_id uuid NOT NULL REFERENCES asset(id) ON DELETE CASCADE,
    org_id   uuid NOT NULL,
    key      text NOT NULL,
    PRIMARY KEY (run_id, asset_id)
);
CREATE INDEX run_target_asset_idx ON run_target (asset_id);

-- +goose Down

DROP TABLE IF EXISTS run_target;
DROP TABLE IF EXISTS run;
DROP TABLE IF EXISTS asset_current;
DROP TABLE IF EXISTS asset_layer;
DROP TABLE IF EXISTS observation;
DROP TABLE IF EXISTS asset;
DROP TABLE IF EXISTS scope_rule;
DROP TABLE IF EXISTS program;
DROP TABLE IF EXISTS api_token;
DROP TABLE IF EXISTS membership;
DROP TABLE IF EXISTS app_user;
DROP TABLE IF EXISTS org;
