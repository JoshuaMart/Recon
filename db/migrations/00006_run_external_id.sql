-- What the platform called the execution.
--
-- A run row is Recon's record of asking; this is the platform's record of
-- running. They are two identifiers because they are created by two systems at
-- two moments, and the second one may never arrive: the run is written and
-- committed before anything is started, so that a platform refusing on a quota
-- leaves a row the deadline sweeper owns rather than nothing at all.
--
-- Without it, the logs of a run that went wrong are unfindable. That is the
-- whole of its purpose, and it is enough of one.

-- +goose Up

ALTER TABLE run ADD COLUMN external_id text;

-- +goose Down

ALTER TABLE run DROP COLUMN external_id;
