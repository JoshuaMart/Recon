CREATE TABLE fingerprint_queue (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    url         TEXT NOT NULL,
    hostname_id UUID NOT NULL REFERENCES hostnames(id) ON DELETE CASCADE,
    source      TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending',
    retry_count INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_fingerprint_queue_pending ON fingerprint_queue(status) WHERE status = 'pending';
