-- One live verification run per programme, enforced by the database. The scheduler already
-- refused a second by reading the live run then writing a new one, but under read committed
-- neither transaction sees the other's uncommitted rows, so a console request and a
-- scheduled tick firing together both pass and freeze the same hosts. A reservation decided
-- by a read then a write is not one.
--
-- Discovery has had this index since the first migration; verification's absence was the
-- gap. Two runs on one host is double scan traffic against someone's perimeter, the one cost
-- this system must not be careless with.

-- +goose Up

CREATE UNIQUE INDEX run_one_live_verification_idx ON run (program_id)
    WHERE kind = 'verification' AND state IN ('pending', 'running');

-- +goose Down

DROP INDEX IF EXISTS run_one_live_verification_idx;
