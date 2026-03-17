CREATE TABLE web_results (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    hostname_id     UUID NOT NULL REFERENCES hostnames(id) ON DELETE CASCADE,
    url             TEXT NOT NULL UNIQUE,
    chain           JSONB,
    technologies    JSONB,
    cookies         JSONB,
    metadata        JSONB,
    external_hosts  JSONB,
    scanned_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_web_results_hostname_id ON web_results(hostname_id);
