CREATE TABLE wildcards (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    value               TEXT NOT NULL UNIQUE,
    active              BOOLEAN NOT NULL DEFAULT true,
    last_recon_at       TIMESTAMPTZ,
    last_revalidated_at TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
