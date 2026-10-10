-- What the platform called the execution. A run row is Recon's record of asking; this is the
-- platform's record of running. Two identifiers, from two systems at two moments, and the
-- second may never arrive: the run is committed before anything starts, so a platform
-- refusing on a quota leaves a row the deadline sweeper owns, not nothing.
--
-- Without it, the logs of a failed run are unfindable, which is its whole purpose.

-- +goose Up

ALTER TABLE run ADD COLUMN external_id text;

-- +goose Down

ALTER TABLE run DROP COLUMN external_id;
