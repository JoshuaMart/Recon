-- A discovery run enumerates one root domain, because that is all the scanner takes.
-- FastRecon declares -d/--domain as a single string, beside --exclude (stringArray) and
-- --targets (strings): flags accepting several values say so. The scheduler joined a
-- programme's apexes with a comma into that one flag, which every single-apex programme
-- survived (a list of one has no separator); the first with two got "domain
-- \"ual.com,united.com\" is not a valid domain name" and never started.
--
-- So the unit was wrong, not the rendering: a run covers an apex, not a perimeter, and the
-- column below makes that sayable. Without it a programme with three apexes has three
-- identical pending rows and nobody can tell which enumeration failed.

-- +goose Up

ALTER TABLE run ADD COLUMN apex text;

-- Backfilled, not left null, and completely: a scope rule is never deleted, only closed by
-- valid_to, so every discovery run still has the rule that authorized it. The programme's
-- oldest apex is the one a single-apex programme had, which covers every run written before
-- this migration that matters.
UPDATE run r SET apex = (
    SELECT s.pattern
      FROM scope_rule s
     WHERE s.program_id = r.program_id
       AND s.kind = 'include'
       AND s.matcher = 'apex'
     ORDER BY s.valid_from, s.pattern
     LIMIT 1)
 WHERE r.kind = 'discovery' AND r.apex IS NULL;

-- Both directions; the second catches the mistake this migration makes impossible. A
-- discovery with no apex cannot say what it enumerated; a verification with one claims a
-- mandate it lacks, being driven by a frozen target list and never a domain.
ALTER TABLE run ADD CONSTRAINT run_apex_matches_kind CHECK (
    (kind = 'discovery' AND apex IS NOT NULL) OR
    (kind <> 'discovery' AND apex IS NULL));

COMMENT ON COLUMN run.apex IS
    'The single root domain a discovery run enumerates. Null on a verification, which is '
    'driven by a frozen target list. The scanner takes one domain per execution, so a '
    'programme with several apexes gets one run each rather than one run naming several.';

-- One live discovery per apex, not per programme. The replaced bound had the same purpose
-- (stop a provisioning storm, since last_discovery_at is written at creation) on the wrong
-- unit: a programme with three apexes could enumerate only one.
DROP INDEX run_one_live_discovery_idx;
CREATE UNIQUE INDEX run_one_live_discovery_idx ON run (program_id, apex)
    WHERE kind = 'discovery' AND state IN ('pending', 'running');

-- +goose Down

DROP INDEX run_one_live_discovery_idx;
CREATE UNIQUE INDEX run_one_live_discovery_idx ON run (program_id)
    WHERE kind = 'discovery' AND state IN ('pending', 'running');

ALTER TABLE run DROP CONSTRAINT IF EXISTS run_apex_matches_kind;
ALTER TABLE run DROP COLUMN IF EXISTS apex;
