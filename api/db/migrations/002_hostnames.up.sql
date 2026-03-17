CREATE TYPE hostname_status AS ENUM ('alive', 'dead', 'unreachable');
CREATE TYPE hostname_type AS ENUM ('web', 'other', 'unknown');

CREATE TABLE hostnames (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    wildcard_id  UUID NOT NULL REFERENCES wildcards(id) ON DELETE CASCADE,
    fqdn         TEXT NOT NULL UNIQUE,
    ip           INET,
    cdn          TEXT,
    status       hostname_status NOT NULL DEFAULT 'unreachable',
    type         hostname_type NOT NULL DEFAULT 'unknown',
    dns          JSONB,
    ports        JSONB,
    last_seen_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_hostnames_wildcard_id ON hostnames(wildcard_id);
CREATE INDEX idx_hostnames_status ON hostnames(status);
CREATE INDEX idx_hostnames_type ON hostnames(type);
