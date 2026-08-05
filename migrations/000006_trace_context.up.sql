-- Add trace_context JSONB column to jobs table for OpenTelemetry context propagation across workers
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS trace_context JSONB;
