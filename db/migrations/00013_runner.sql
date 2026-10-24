-- Where a run executes, and the platform's name for its execution.

-- +goose Up
-- serve: fastrecon serve, synchronous. scaleway: a Serverless Job that posts
-- its report back.
ALTER TABLE run ADD COLUMN runner text NOT NULL DEFAULT 'serve' CHECK (runner IN ('serve', 'scaleway'));
-- The job run's id: the only handle on its logs, and what its state is read by.
ALTER TABLE run ADD COLUMN external_id text;
CREATE INDEX run_running_by_runner ON run (runner) WHERE state = 'running';

-- +goose Down
DROP INDEX run_running_by_runner;
ALTER TABLE run DROP COLUMN external_id;
ALTER TABLE run DROP COLUMN runner;
