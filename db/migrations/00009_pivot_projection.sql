-- What the projection has to be able to say about itself. Three functions, each existing
-- because the alternative is an expression copied into several callers and fixed in one.

-- +goose Up

-- The counted keys of a projection, as rows.
--
-- This is the invariant: pivot_count is a function of asset_current's counted keys and
-- nothing else. Written once, the ingestion statement diffs it to move the counters and a
-- recount reads it to rebuild them; two expressions would let a counter drift from the
-- only place the truth lives.
--
-- technologies and external_hosts are absent on purpose: they carry no counter (a filter
-- and an aggregation), and the invariant speaks only of what is counted.
--
-- +goose StatementBegin
CREATE FUNCTION pivot_values(attributes jsonb)
RETURNS TABLE (pivot_type text, pivot_value text)
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    -- DISTINCT: a page loading one bundle twice is one asset carrying that hash. The counter
    -- answers "how many assets share this value", and a repetition would inflate it
    -- unquestioned.
    SELECT DISTINCT * FROM (
        SELECT 'favicon'::text, attributes ->> 'favicon_hash'
         WHERE attributes ->> 'favicon_hash' IS NOT NULL
        UNION ALL
        SELECT 'cert_spki'::text, attributes ->> 'cert_spki_hash'
         WHERE attributes ->> 'cert_spki_hash' IS NOT NULL
        UNION ALL
        SELECT 'script'::text, value
          FROM jsonb_array_elements_text(
                   CASE WHEN jsonb_typeof(attributes -> 'script_hashes') = 'array'
                        THEN attributes -> 'script_hashes' ELSE '[]'::jsonb END) AS value
        UNION ALL
        SELECT 'cookie_name'::text, value
          FROM jsonb_array_elements_text(
                   CASE WHEN jsonb_typeof(attributes -> 'cookie_names') = 'array'
                        THEN attributes -> 'cookie_names' ELSE '[]'::jsonb END) AS value
    ) AS counted(pivot_type, pivot_value)
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION pivot_values(jsonb) IS
    'The counted keys of a projection. pivot_count is a function of this and of nothing else.';

-- The daily buckets, shifted to a given day. The shift is lazy, which handles a dormant
-- asset with no code of its own: one unmoved for three weeks shows a gap wider than the
-- array and is zeroed, the right answer with no special case. No total is stored, so
-- nothing is decremented as a change expires and no sweep is needed for a display value.
--
-- +goose StatementBegin
CREATE FUNCTION shift_buckets(buckets int[], day date, today date)
RETURNS int[]
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT CASE
        -- Never shifted, or shifted past the end of the array. Both are an
        -- array with nothing left in it.
        WHEN day IS NULL OR buckets IS NULL OR today - day >= 8
            THEN '{0,0,0,0,0,0,0,0}'::int[]
        -- A date ahead of today is a clock that went backwards. Keeping the array loses nothing;
        -- shifting by a negative number would pad the wrong end.
        WHEN today <= day THEN buckets
        ELSE array_fill(0, ARRAY[today - day]) || buckets[1:8 - (today - day)]
    END
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION shift_buckets(int[], date, date) IS
    'The daily change buckets brought forward to a day. Lazy: an asset that has not moved is not rewritten.';

-- The technology column, derived from the two keys feeding it. Two producers is where "one
-- producer per value" bends, where it must: the probe contributes coverage (every service on
-- every pass; a render comes on five triggers weeks apart), the render depth (it shows what a
-- raw fetch cannot). They write different keys, so neither erases the other, and the column
-- is their union. It lives here, not in the ingestion statement, so a rebuild produces the
-- same array.
--
-- +goose StatementBegin
CREATE FUNCTION technology_names(attributes jsonb)
RETURNS text[]
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT COALESCE(array_agg(DISTINCT name ORDER BY name), '{}')
      FROM (
        SELECT jsonb_array_elements_text(
                   CASE WHEN jsonb_typeof(attributes -> 'tech_http') = 'array'
                        THEN attributes -> 'tech_http' ELSE '[]'::jsonb END) AS name
        UNION ALL
        -- The render reports objects, being the only one that knows a version. The column holds
        -- names and the object stays as evidence: it is queried by exact element, so "nginx 1.24"
        -- would not answer a filter on "nginx".
        SELECT jsonb_array_elements(
                   CASE WHEN jsonb_typeof(attributes -> 'tech_render') = 'array'
                        THEN attributes -> 'tech_render' ELSE '[]'::jsonb END) ->> 'name'
      ) AS found(name)
     WHERE name IS NOT NULL AND name <> ''
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION technology_names(jsonb) IS
    'The technology column, as the union of what the probe and the render each reported.';

-- Volatility is the sum of the last seven days, relative to the day the array was last
-- shifted. That frame is the trap: an asset unchanged for five days has not been shifted, so
-- summing its first seven buckets naively counts twelve-day-old changes as yesterday's.
-- STABLE, not IMMUTABLE, as it reads the calendar; a function, as an expression copied into
-- the filter, facet and row would be right in one of three. The day is UTC, never the
-- server's local one, or every boundary shifts by the offset, invisibly to tests not crossing
-- midnight.
--
-- +goose StatementBegin
CREATE FUNCTION volatility(buckets int[], day date)
RETURNS int
LANGUAGE sql
STABLE
PARALLEL SAFE
AS $$
    -- The slice needs the parentheses: a subscript straight onto a function call is a syntax
    -- error whose message names the bracket, not the cause.
    SELECT COALESCE(
        (SELECT sum(value)::int
           FROM unnest((shift_buckets(buckets, day, (now() AT TIME ZONE 'UTC')::date))[1:7]) AS value),
        0)
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION volatility(int[], date) IS
    'Changes over the last seven days. The eighth bucket is margin and is deliberately not summed.';

-- The column, carried into the key it is now derived from. `technologies` was written by the
-- http layer alone before this migration, so attributing all of it to the probe is exact;
-- without it a render landing on an earlier row computes the union from its own key alone and
-- the probe's findings vanish until the next full pass, the facet going quiet with nothing
-- failing. A function, not a bare UPDATE, so a test can call it on a row it wrote itself.
--
-- +goose StatementBegin
CREATE FUNCTION backfill_promoted_technologies()
RETURNS bigint
LANGUAGE plpgsql
AS $$
DECLARE
    touched bigint;
BEGIN
    UPDATE asset_current
       SET attributes = attributes || jsonb_build_object('tech_http', to_jsonb(technologies))
     WHERE COALESCE(array_length(technologies, 1), 0) > 0
       AND NOT (attributes ? 'tech_http');
    GET DIAGNOSTICS touched = ROW_COUNT;
    RETURN touched;
END
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION backfill_promoted_technologies() IS
    'Carries a pre-existing technologies column into the attributes key it is now derived from.';

-- +goose StatementBegin
DO $$
DECLARE
    touched bigint;
BEGIN
    SELECT backfill_promoted_technologies() INTO touched;
    RAISE NOTICE 'technologies carried into attributes on % rows', touched;
END
$$;
-- +goose StatementEnd

-- One change, recorded in today's bucket. Shift and increment are one operation, as
-- separating them lets the increment land in another day's bucket: the array is brought
-- forward, then the first bucket takes the change, and the caller writes buckets_day from
-- the same instant.
--
-- +goose StatementBegin
CREATE FUNCTION record_change(buckets int[], day date, today date)
RETURNS int[]
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT ARRAY[shifted[1] + 1] || shifted[2:8]
      FROM shift_buckets(buckets, day, today) AS shifted
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION record_change(int[], date, date) IS
    'The buckets after one change on a given day. Shift and increment together, never separately.';

-- Whether a value is too generic to be worth a badge. A display rule, never a collection one:
-- every name is indexed, so a misclassified entry loses no data and explicit search still
-- works. The export never calls it, as a file does not say what it omits. The list is static
-- and versioned in the repository because the relevant frequency is the internet's, not one
-- organization's (PHPSESSID is noise because it is universal).
--
-- +goose StatementBegin
CREATE FUNCTION generic_pivot(kind text, value text)
RETURNS boolean
LANGUAGE sql
STABLE
PARALLEL SAFE
AS $$
    SELECT EXISTS (
        SELECT 1
          FROM generic_pivot_value g
         WHERE g.pivot_type = kind
           -- The file writes globs ("_ga_*", "ASPSESSIONID*") and LIKE is not a glob. Convert by
           -- escaping what LIKE treats as special before turning the star into a percent: "_ga" left
           -- alone matches any three characters ending in "ga", silencing a real application cookie
           -- invisibly.
           AND value LIKE replace(
                   replace(replace(replace(g.pattern, '\', '\\'), '%', '\%'), '_', '\_'),
                   '*', '%')
    )
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION generic_pivot(text, text) IS
    'Whether a pivot value is too generic to badge. Display only: every value stays indexed and searchable.';

-- +goose Down

DROP FUNCTION IF EXISTS backfill_promoted_technologies();
DROP FUNCTION IF EXISTS generic_pivot(text, text);
DROP FUNCTION IF EXISTS record_change(int[], date, date);
DROP FUNCTION IF EXISTS technology_names(jsonb);
DROP FUNCTION IF EXISTS volatility(int[], date);
DROP FUNCTION IF EXISTS shift_buckets(int[], date, date);
DROP FUNCTION IF EXISTS pivot_values(jsonb);
