-- Promoted by Changegate from session s_b3f8ab7108a0 (dev, agent claude-code, user srawlin).
-- Down was generated from the recorded undo handles of the live session.
ALTER TABLE accounts ADD COLUMN region text NOT NULL DEFAULT 'us';
CREATE INDEX accounts_region_idx ON accounts(region);
CREATE TABLE regions (code text PRIMARY KEY, name text NOT NULL);
