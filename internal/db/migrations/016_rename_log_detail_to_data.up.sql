-- Rename process_logs.detail to data. The column now holds a single raw payload
-- string (a process/task input, output, or result snippet) rather than a JSON
-- object of mixed fields. The small structured facts that used to live here (retry
-- attempt/max, goto targets, child counts, …) now go in the human-readable message
-- column. ALTER TABLE ... RENAME COLUMN is supported by both SQLite (3.25+) and
-- PostgreSQL.
ALTER TABLE process_logs RENAME COLUMN detail TO data;
