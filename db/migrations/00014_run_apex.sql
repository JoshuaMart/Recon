-- A discovery run enumerates one root domain, because that is all the scanner
-- takes.
--
-- FastRecon declares -d/--domain as a single string, beside an --exclude that
-- says stringArray and a --targets that says strings: the flags that accept
-- more than one value say so. The scheduler was joining a programme's apexes
-- with a comma into that one flag, which every run of every single apex
-- programme survived because a list of one has no separator in it. The first
-- programme with two apexes got "domain \"ual.com,united.com\" is not a valid
-- domain name" and never started.
--
-- So the unit was wrong rather than the rendering. A run covers an apex, not a
-- perimeter, and the column below is what makes the difference sayable: without
-- it a programme with three apexes has three identical pending rows and nobody
-- can tell which enumeration failed.

-- +goose Up

ALTER TABLE run ADD COLUMN apex text;

-- Backfilled rather than left null, and it can be complete: a scope rule is
-- never deleted, only closed by valid_to, so every discovery run that ever
-- existed still has the rule that authorized it. The oldest apex of the
-- programme is the one a single apex programme had, which is every run written
-- before this migration that matters.
UPDATE run r SET apex = (
    SELECT s.pattern
      FROM scope_rule s
     WHERE s.program_id = r.program_id
       AND s.kind = 'include'
       AND s.matcher = 'apex'
     ORDER BY s.valid_from, s.pattern
     LIMIT 1)
 WHERE r.kind = 'discovery' AND r.apex IS NULL;

-- Both directions, and the second one is the one that catches the mistake this
-- migration exists to make impossible. A discovery with no apex is a run that
-- cannot say what it enumerated; a verification with one is a row claiming a
-- mandate it does not have, since a verification is driven by a frozen target
-- list and never by a domain.
ALTER TABLE run ADD CONSTRAINT run_apex_matches_kind CHECK (
    (kind = 'discovery' AND apex IS NOT NULL) OR
    (kind <> 'discovery' AND apex IS NULL));

COMMENT ON COLUMN run.apex IS
    'The single root domain a discovery run enumerates. Null on a verification, which is '
    'driven by a frozen target list. The scanner takes one domain per execution, so a '
    'programme with several apexes gets one run each rather than one run naming several.';

-- One live discovery per apex rather than per programme. The bound it replaces
-- had the same purpose, to stop a provisioning storm where last_discovery_at is
-- written at creation, and it was expressed on the wrong unit: it made a
-- programme with three apexes able to enumerate one of them.
DROP INDEX run_one_live_discovery_idx;
CREATE UNIQUE INDEX run_one_live_discovery_idx ON run (program_id, apex)
    WHERE kind = 'discovery' AND state IN ('pending', 'running');

-- +goose Down

DROP INDEX run_one_live_discovery_idx;
CREATE UNIQUE INDEX run_one_live_discovery_idx ON run (program_id)
    WHERE kind = 'discovery' AND state IN ('pending', 'running');

ALTER TABLE run DROP CONSTRAINT IF EXISTS run_apex_matches_kind;
ALTER TABLE run DROP COLUMN IF EXISTS apex;
