ALTER TABLE process_instances ADD COLUMN wait_state TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_process_instances_status_wait
    ON process_instances (status, wait_state);
