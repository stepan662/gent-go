-- Rename the buffered-signal column payload -> result. The value delivered to an external
-- task IS its result (validated against the task's result_schema), matching the API field
-- on /instances/{id}/signal and /external-tasks/resolve. RENAME COLUMN is supported by both
-- SQLite (3.25+) and PostgreSQL.
ALTER TABLE process_signals RENAME COLUMN payload TO result;
