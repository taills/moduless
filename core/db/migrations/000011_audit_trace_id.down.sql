DROP INDEX IF EXISTS idx_audit_logs_trace;
ALTER TABLE audit_logs DROP COLUMN IF EXISTS trace_id;
