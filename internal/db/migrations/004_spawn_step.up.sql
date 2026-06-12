ALTER TABLE process_instances ADD COLUMN spawn_step_id TEXT NOT NULL DEFAULT '';
DROP INDEX IF EXISTS idx_instances_parent;
CREATE INDEX IF NOT EXISTS idx_instances_parent_step
    ON process_instances (parent_id, spawn_step_id)
    WHERE parent_id != '';
