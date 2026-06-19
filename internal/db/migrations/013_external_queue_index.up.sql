-- Partial index serving the external-task queue (ListExternalTasks): it covers only
-- instances currently parked on an external task (wait_state='external'), ordered the
-- way the queue lists them. Parked-external rows are a small slice of the table, so the
-- index stays cheap to maintain (a row enters on arm and leaves the moment the task is
-- resolved/timed out) and keeps the queue off a full scan. Both engines use it.
CREATE INDEX idx_external_queue ON process_instances (process_name, process_version, updated_at)
    WHERE wait_state = 'external';
