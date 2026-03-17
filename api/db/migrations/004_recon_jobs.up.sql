CREATE TYPE recon_mode AS ENUM ('normal', 'intensive');
CREATE TYPE job_status AS ENUM ('pending', 'running', 'completed', 'failed');

CREATE TABLE recon_jobs (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    wildcard_id      UUID NOT NULL REFERENCES wildcards(id) ON DELETE CASCADE,
    mode             recon_mode NOT NULL DEFAULT 'normal',
    status           job_status NOT NULL DEFAULT 'pending',
    scaleway_job_id  TEXT,
    started_at       TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_recon_jobs_wildcard_status ON recon_jobs(wildcard_id, status);
