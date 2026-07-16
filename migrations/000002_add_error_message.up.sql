-- Add error_message column for storing failure reasons.
ALTER TABLE jobs ADD COLUMN error_message TEXT;
