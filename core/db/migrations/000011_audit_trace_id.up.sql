-- The trace id that produced this record.
--
-- Without it an audit entry cannot be joined to anything: not to the request
-- that caused it, not to the plugin log lines from the same request, not to a
-- slow query recorded elsewhere under the same id. Core has been assigning a
-- trace to every request and publishing it as X-Request-Id all along; the
-- audit trail was the one place it did not land.
--
-- Nullable with an empty default, so records written before this migration
-- stay valid rather than becoming rows that claim a trace they never had.
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS trace_id VARCHAR(64) NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_audit_logs_trace ON audit_logs (trace_id)
    WHERE trace_id <> '';
