-- Rename the workflow "step" vocabulary to "task" across all tables and indexes.
-- ALTER TABLE ... RENAME COLUMN is supported by both SQLite (3.25+) and PostgreSQL,
-- and carries along primary-key and index references to the renamed columns.

ALTER TABLE process_instances    RENAME COLUMN step_queue    TO task_queue;
ALTER TABLE process_instances    RENAME COLUMN spawn_step_id TO spawn_task_id;
ALTER TABLE process_dependencies RENAME COLUMN step_id       TO task_id;
ALTER TABLE process_logs         RENAME COLUMN step_id       TO task_id;

-- Rename the partial index over (parent_id, spawn_task_id). SQLite has no
-- ALTER INDEX ... RENAME, so drop and recreate it (the column reference already
-- followed the column rename above).
DROP INDEX IF EXISTS idx_instances_parent_step;
CREATE INDEX IF NOT EXISTS idx_instances_parent_task
    ON process_instances (parent_id, spawn_task_id)
    WHERE parent_id != '';
