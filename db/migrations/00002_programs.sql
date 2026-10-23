-- Programs, their scope rules, the wildcards those rules name, the scope values
-- Recon cannot track, and the state of the ScopesExtractor sync.

-- +goose Up
CREATE TABLE program (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  origin      text NOT NULL CHECK (origin IN ('scopesextractor', 'manual')),
  -- For a synced program, its identity on ScopesExtractor. A manual program may
  -- carry a platform as a label and never a slug.
  platform    text,
  slug        text,
  name        text NOT NULL,
  bounty      boolean,
  state       text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'paused', 'archived')),
  -- Who archived it: only a program the sync archived is reactivated by the sync.
  archived_by text CHECK (archived_by IN ('sync', 'user')),
  -- NULL inherits the global default.
  auto_scan   text CHECK (auto_scan IN ('off', 'on_new')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CHECK ((origin = 'scopesextractor') = (slug IS NOT NULL)),
  CHECK (origin = 'manual' OR platform IS NOT NULL),
  CHECK ((state = 'archived') = (archived_by IS NOT NULL))
);
CREATE UNIQUE INDEX program_synced_identity ON program (platform, slug) WHERE origin = 'scopesextractor';

CREATE TABLE scope_rule (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  program_id uuid NOT NULL REFERENCES program (id),
  kind       text NOT NULL CHECK (kind IN ('include', 'exclude')),
  matcher    text NOT NULL CHECK (matcher IN ('apex', 'fqdn', 'url_prefix', 'cidr')),
  -- Canonical form: a name for apex and fqdn, a URL without query for
  -- url_prefix, a network for cidr.
  pattern    text NOT NULL,
  origin     text NOT NULL CHECK (origin IN ('sync', 'local')),
  note       text,
  valid_from timestamptz NOT NULL,
  valid_to   timestamptz,
  CHECK (valid_to IS NULL OR valid_to >= valid_from)
);
-- One open rule per meaning and origin. Closed rules are history and may repeat.
CREATE UNIQUE INDEX scope_rule_open ON scope_rule (program_id, kind, matcher, pattern, origin) WHERE valid_to IS NULL;
CREATE INDEX scope_rule_program ON scope_rule (program_id);

-- One per include apex rule. It outlives its rule: closing the rule keeps the
-- recon history, and the wildcard reads as inactive.
CREATE TABLE wildcard (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  program_id     uuid NOT NULL REFERENCES program (id),
  rule_id        uuid NOT NULL UNIQUE REFERENCES scope_rule (id),
  apex           text NOT NULL,
  last_recon_at  timestamptz,
  last_recon_run uuid,
  assets_found   int NOT NULL DEFAULT 0,
  created_at     timestamptz NOT NULL
);
CREATE INDEX wildcard_recon_age ON wildcard (last_recon_at NULLS FIRST);

-- Scope values ScopesExtractor lists and Recon cannot turn into a rule: a
-- mobile app, an executable, a value that does not parse.
CREATE TABLE untracked_scope (
  program_id    uuid NOT NULL REFERENCES program (id),
  value         text NOT NULL,
  type          text NOT NULL,
  reason        text NOT NULL,
  first_seen_at timestamptz NOT NULL,
  last_seen_at  timestamptz NOT NULL,
  gone_at       timestamptz,
  PRIMARY KEY (program_id, value, type)
);

-- The last sync pass, one row.
CREATE TABLE scope_sync (
  id              boolean PRIMARY KEY DEFAULT true CHECK (id),
  last_started_at timestamptz,
  last_success_at timestamptz,
  last_error      text,
  last_error_at   timestamptz,
  last_summary    jsonb
);
INSERT INTO scope_sync DEFAULT VALUES;

-- +goose Down
DROP TABLE scope_sync;
DROP TABLE untracked_scope;
DROP TABLE wildcard;
DROP TABLE scope_rule;
DROP TABLE program;
