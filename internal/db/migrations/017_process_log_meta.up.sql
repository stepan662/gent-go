-- Add process_logs.meta: small, complete, structured JSON metadata about an event
-- (e.g. {"url":"…"} on action_started, {"status":200} on action_succeeded). Unlike
-- data — the raw, possibly-truncated payload body — meta is always valid JSON, so
-- consumers can parse it reliably. Empty string = no metadata. ADD COLUMN with a
-- constant default works on both SQLite and PostgreSQL.
ALTER TABLE process_logs ADD COLUMN meta TEXT NOT NULL DEFAULT '';
