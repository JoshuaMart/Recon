-- One live verification run per programme, enforced by the database.
--
-- The scheduler already refused a second one, by reading the live run and then
-- writing a new one. Under read committed neither transaction sees the other's
-- uncommitted rows, so a console request and a scheduled tick firing together
-- both pass the check and both freeze the same hosts. The frozen list is the
-- whole of the reservation, and a reservation decided by a read followed by a
-- write is not one.
--
-- Discovery has carried this index since the first migration. The absence of
-- the matching one for verification was the gap: two runs holding the same host
-- is double scan traffic against somebody's perimeter, which is the one cost
-- this system is not allowed to be careless with.

-- +goose Up

CREATE UNIQUE INDEX run_one_live_verification_idx ON run (program_id)
    WHERE kind = 'verification' AND state IN ('pending', 'running');

-- +goose Down

DROP INDEX IF EXISTS run_one_live_verification_idx;
