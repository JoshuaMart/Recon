-- Pivots: the values linking assets nothing else connects. Counting on the fly is one COUNT
-- per displayed value, unworkable with dozens per page, so the counter is maintained on
-- write in the transaction rewriting the projection, which still holds the old value at the
-- moment of the diff.

-- +goose Up

CREATE TABLE pivot_count (
    org_id      uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    pivot_type  text NOT NULL,
    pivot_value text NOT NULL,
    count       int NOT NULL,
    PRIMARY KEY (org_id, pivot_type, pivot_value),
    CONSTRAINT pivot_count_type_known CHECK (
        pivot_type IN ('favicon', 'cert_spki', 'script', 'cookie_name')),
    -- The counter drifts upward, never downward: a pivot announced at 41 linking 12 sends someone
    -- looking for thirty hosts that do not exist. A negative count means the decrement ran
    -- twice, and must fail here, not be discovered on a badge.
    CONSTRAINT pivot_count_not_negative CHECK (count >= 0)
);

-- One copy per distinct favicon however many assets share it, a shared favicon being the
-- interesting case. Not in the projection: a kilobyte or two per asset in the hottest write
-- path and every search response is gigabytes on a large inventory, and an image is the
-- depiction of a pivot, not one.
CREATE TABLE favicon_image (
    org_id     uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    hash       text NOT NULL,
    media_type text NOT NULL,
    bytes      bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, hash),
    -- The target chooses the value and could serve five megabytes as a favicon. Past the bound
    -- the image is not stored: hash and counter keep working, only the thumbnail is missing.
    CONSTRAINT favicon_image_bounded CHECK (octet_length(bytes) <= 65536)
);

-- Frequency on the internet, not inside the organization: PHPSESSID is noise because it is
-- universal. On a small perimeter variance dominates, and a local threshold cannot tell a
-- framework cookie from a rare application one.
CREATE TABLE generic_pivot_value (
    pivot_type text NOT NULL,
    pattern    text NOT NULL,
    note       text,
    PRIMARY KEY (pivot_type, pattern),
    CONSTRAINT generic_pivot_type_known CHECK (
        pivot_type IN ('favicon', 'cert_spki', 'script', 'cookie_name'))
);

-- The table reflects a repository file and is never an autonomous source. Only the seed
-- writes, so a hand-added entry cannot survive a deployment and hide the divergence.
REVOKE INSERT, UPDATE, DELETE ON generic_pivot_value FROM asm_app, asm_sys;

-- +goose Down

DROP TABLE IF EXISTS generic_pivot_value;
DROP TABLE IF EXISTS favicon_image;
DROP TABLE IF EXISTS pivot_count;
