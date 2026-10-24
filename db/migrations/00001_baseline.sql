-- The baseline. Business tables start in phase 1; this one only records when
-- the database was first created, so a restored dump can be told from a fresh one.

-- +goose Up
CREATE TABLE instance (
  id         boolean PRIMARY KEY DEFAULT true CHECK (id),
  created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO instance DEFAULT VALUES;

-- +goose Down
DROP TABLE instance;
