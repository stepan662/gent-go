-- Replace the per-instance task_queue JSON blob with a single `task` column holding
-- the current task id. The remaining queue is redundant: a process definition is
-- immutable and version-pinned, and the queue is always a contiguous suffix of its
-- task list, so it is reconstructed on read as def.Tasks from `task` onward (see
-- toInstance / taskQueueFrom in db_instances.go).
--
-- Empty string means the instance has no current task (completed or drained).
-- Prototype: in-flight task_queue state is dropped with no backfill — existing rows
-- get task='' and complete on their next claim rather than being migrated.
ALTER TABLE process_instances DROP COLUMN task_queue;
ALTER TABLE process_instances ADD COLUMN task TEXT NOT NULL DEFAULT '';
