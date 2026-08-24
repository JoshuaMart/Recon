-- What the ceiling held back, and a bound on the feed's own record.
--
-- Both are things the phase noticed once it ran rather than while it was
-- designed, which is the honest reason for a second migration.

-- +goose Up

-- The count the ceiling refused, beside the count the logs delivered.
--
-- It was only ever a log line, and a log line is not readable: the assertion it
-- answers says the dropped count is readable, and "grep the control plane" is
-- not that. It belongs here rather than anywhere else because what the ceiling
-- held back is the same kind of fact as what the apex delivered, and somebody
-- reading one wants the other in the same row.
ALTER TABLE ct_apex ADD COLUMN dropped bigint NOT NULL DEFAULT 0;

ALTER TABLE ct_apex ADD CONSTRAINT ct_apex_dropped_is_a_count CHECK (dropped >= 0);

COMMENT ON COLUMN ct_apex.dropped IS
    'Candidates the per programme ceiling refused. A silent cap reads as a small answer rather '
    'than a truncated one, so this is stored beside san_count rather than logged.';

-- +goose Down

ALTER TABLE ct_apex DROP CONSTRAINT IF EXISTS ct_apex_dropped_is_a_count;
ALTER TABLE ct_apex DROP COLUMN IF EXISTS dropped;
