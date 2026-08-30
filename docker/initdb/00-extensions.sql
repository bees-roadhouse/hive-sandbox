-- Extensions live in their own schema so `public` stays empty.
--
-- That is what makes a per-test private schema genuinely the only thing on its
-- search path, and it is why internal/testdb asserts `public` is absent from
-- search_path at all. Installing pgvector into `public` would put a schema on
-- every search path that nothing else is supposed to reach.
--
-- This runs ONCE, on an empty data directory, and it is provisioning rather
-- than migration one: CREATE EXTENSION needs rights the migration role does
-- not have, so it cannot live in internal/store/migrations.
--
-- Mirrors what scripts/db-up.sh does for the development database. Change both
-- together, or a stack and a test run disagree about where `vector` lives.
CREATE SCHEMA IF NOT EXISTS extensions;
CREATE EXTENSION IF NOT EXISTS vector SCHEMA extensions;
