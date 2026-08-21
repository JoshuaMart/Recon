-- What the projection has to be able to say about itself.
--
-- Three functions, and each exists because the alternative is an expression
-- copied into several callers. Copied, it gets fixed in one of them.

-- +goose Up

-- The counted keys of a projection, as rows.
--
-- This *is* the invariant: pivot_count is a function of the counted keys of
-- asset_current and of nothing else. Written once, it is what the ingestion
-- statement diffs to move the counters, and what a recount reads to rebuild
-- them from scratch the day anybody doubts them. Two expressions saying the
-- same thing would let a counter drift from the only place the truth lives.
--
-- technologies and external_hosts are absent on purpose. They carry no counter:
-- they are a filter and an aggregation, and the invariant speaks only about
-- what is counted.
--
-- +goose StatementBegin
CREATE FUNCTION pivot_values(attributes jsonb)
RETURNS TABLE (pivot_type text, pivot_value text)
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    -- DISTINCT because a page loading the same bundle twice is one asset
    -- carrying that hash, not two. The counter answers "how many assets share
    -- this value", so counting a repetition would inflate it by exactly the
    -- kind of amount nobody would question.
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

-- The daily buckets, shifted to a given day.
--
-- The shift is lazy, which is what handles a dormant asset with no code of its
-- own: an asset that has not moved in three weeks shows a gap wider than the
-- array and is simply zeroed, which is the right answer rather than a special
-- case. No total is stored, so nothing has to be decremented as a change
-- expires and no sweep of the inventory is needed for a display value.
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
        -- A date ahead of today is a clock that went backwards. Keeping the
        -- array is the only answer that loses nothing: shifting by a negative
        -- number would pad the wrong end.
        WHEN today <= day THEN buckets
        ELSE array_fill(0, ARRAY[today - day]) || buckets[1:8 - (today - day)]
    END
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION shift_buckets(int[], date, date) IS
    'The daily change buckets brought forward to a day. Lazy: an asset that has not moved is not rewritten.';

-- The technology column, derived from the two keys that feed it.
--
-- Two producers is the one place the "one producer per value" rule bends, and
-- it bends where it has to: coverage says the probe must contribute, because it
-- sees every service on every pass while a render happens on five triggers that
-- can be three weeks apart; depth says the render must, because a rendered page
-- shows what a raw fetch cannot. They write different keys, so neither erases
-- the other, and the column is their union.
--
-- The union is here rather than in the ingestion statement because a rebuild
-- has to produce exactly the same array, and two expressions computing a column
-- is how a column stops meaning one thing.
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
        -- The render reports objects, because it is the only one that knows a
        -- version. The column holds names and the object stays as the evidence:
        -- the column is queried by exact element, so "nginx 1.24" would not
        -- answer a filter on "nginx".
        SELECT jsonb_array_elements(
                   CASE WHEN jsonb_typeof(attributes -> 'tech_render') = 'array'
                        THEN attributes -> 'tech_render' ELSE '[]'::jsonb END) ->> 'name'
      ) AS found(name)
     WHERE name IS NOT NULL AND name <> ''
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION technology_names(jsonb) IS
    'The technology column, as the union of what the probe and the render each reported.';

-- Volatility is the sum of the last seven days, read relative to the day the
-- array was last shifted.
--
-- That reference frame is the whole trap. The array of an asset unchanged for
-- five days has not been shifted, because nothing rewrote it, so summing its
-- first seven buckets naively counts changes twelve days old as if they were
-- yesterday's. STABLE rather than IMMUTABLE because it reads the calendar, and
-- a function rather than an expression because an expression copied into the
-- filter, the facet and the row would be right in one of the three.
--
-- The day is UTC and never the server's local one. observed_at is stored in
-- UTC and buckets_day is written from it, so comparing against a local calendar
-- day would move every bucket boundary by the offset and be invisible in any
-- test that does not cross midnight.
--
-- +goose StatementBegin
CREATE FUNCTION volatility(buckets int[], day date)
RETURNS int
LANGUAGE sql
STABLE
PARALLEL SAFE
AS $$
    -- The slice needs the parentheses: a subscript straight onto a function
    -- call is a syntax error rather than a slice, and the message names the
    -- bracket rather than the cause.
    SELECT COALESCE(
        (SELECT sum(value)::int
           FROM unnest((shift_buckets(buckets, day, (now() AT TIME ZONE 'UTC')::date))[1:7]) AS value),
        0)
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION volatility(int[], date) IS
    'Changes over the last seven days. The eighth bucket is margin and is deliberately not summed.';

-- One change, recorded in today's bucket.
--
-- The shift and the increment are one operation, and separating them is how the
-- increment lands in a bucket belonging to another day. The array is brought
-- forward first, then the first bucket takes the change, and buckets_day is
-- written from the same instant by the caller.
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

-- Whether a value is too generic to be worth a badge.
--
-- A display rule and never a collection one. Every name is indexed without
-- exception; this only removes the badge from a row, so a misclassified entry
-- loses no data and the explicit search still works. The export does not call
-- it at all, because a file does not say what it does not contain.
--
-- The list is a static one, versioned in the repository, because the relevant
-- frequency is on the internet rather than inside one organization: PHPSESSID
-- is noise because it is universal, not because it is locally frequent, and on
-- a small perimeter the variance dominates any local threshold.
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
           -- The file writes globs, "_ga_*" and "ASPSESSIONID*", and LIKE is
           -- not a glob. Converting one into the other means escaping what LIKE
           -- treats as special *before* turning the star into a percent:
           -- "_ga" left alone is a LIKE pattern matching any three characters
           -- ending in "ga", which would silence a real application cookie and
           -- do it invisibly.
           AND value LIKE replace(
                   replace(replace(replace(g.pattern, '\', '\\'), '%', '\%'), '_', '\_'),
                   '*', '%')
    )
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION generic_pivot(text, text) IS
    'Whether a pivot value is too generic to badge. Display only: every value stays indexed and searchable.';

-- +goose Down

DROP FUNCTION IF EXISTS generic_pivot(text, text);
DROP FUNCTION IF EXISTS record_change(int[], date, date);
DROP FUNCTION IF EXISTS technology_names(jsonb);
DROP FUNCTION IF EXISTS volatility(int[], date);
DROP FUNCTION IF EXISTS shift_buckets(int[], date, date);
DROP FUNCTION IF EXISTS pivot_values(jsonb);
