-- Coverage per apex: what the CT feed brought under each wildcard being
-- watched, in place of the wildcard certificate counter on wildcard.

-- +goose Up
CREATE TABLE ct_apex (
  wildcard_id      uuid PRIMARY KEY REFERENCES wildcard (id),
  -- When the apex entered the set: the row goes when it leaves, so a period
  -- without watching never reads as coverage.
  watched_since    timestamptz NOT NULL,
  names            bigint NOT NULL DEFAULT 0 CHECK (names >= 0),
  wildcards        bigint NOT NULL DEFAULT 0 CHECK (wildcards >= 0),
  dropped          bigint NOT NULL DEFAULT 0 CHECK (dropped >= 0),
  last_name_at     timestamptz,
  last_wildcard_at timestamptz
);

INSERT INTO ct_apex (wildcard_id, watched_since, wildcards)
SELECT id, now(), ct_wildcards FROM wildcard WHERE ct_wildcards > 0;

ALTER TABLE wildcard DROP COLUMN ct_wildcards;

-- +goose Down
ALTER TABLE wildcard ADD COLUMN ct_wildcards int NOT NULL DEFAULT 0;
UPDATE wildcard w SET ct_wildcards = a.wildcards FROM ct_apex a WHERE a.wildcard_id = w.id;
DROP TABLE ct_apex;
