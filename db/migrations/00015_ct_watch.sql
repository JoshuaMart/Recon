-- Whether the CT feed watches a wildcard's apex: off keeps it in scope and in
-- recon without the names its certificates bring.

-- +goose Up
ALTER TABLE wildcard ADD COLUMN ct_watch boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE wildcard DROP COLUMN ct_watch;
