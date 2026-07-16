-- Rollback migration 000002: remove error_message column.
ALTER TABLE jobs DROP COLUMN IF EXISTS error_message;
