-- Promoted by Changegate from session s_b3f8ab7108a0 (dev, agent claude-code, user srawlin).
-- Down was generated from the recorded undo handles of the live session.
DROP TABLE "public"."regions";
DROP INDEX IF EXISTS "public"."accounts_region_idx";
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS "accounts_region_not_null";
ALTER TABLE "public"."accounts" DROP COLUMN IF EXISTS "region";
