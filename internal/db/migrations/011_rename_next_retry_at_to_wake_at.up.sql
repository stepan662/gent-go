-- Rename next_retry_at -> wake_at: the column is the per-instance "earliest time
-- to claim this instance" timer, now used by retry-backoff, the delay action, and
-- any future timer — not just retries. retry_count keeps the retry name; only this
-- shared timer is generalized.
--
-- Plain RENAME COLUMN (SQLite >= 3.25, Postgres). Safe here because the only index
-- over the column (idx_instances_pending, status+next_retry_at) was already dropped
-- in migration 010; the live runnable index (idx_instances_runnable) does not
-- reference it.
ALTER TABLE process_instances RENAME COLUMN next_retry_at TO wake_at;
