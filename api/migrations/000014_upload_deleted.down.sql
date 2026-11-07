-- PostgreSQL cannot drop a single enum value. The extra value is harmless; this migration is a deliberate no-op
-- so that rolling back to 000013 leaves the schema compatible with the older code.
SELECT 1;
