-- Optimize the process_instances index set for the claim query, validated by
-- EXPLAIN ANALYZE over 1M rows + a 200k-runnable-backlog drain benchmark.
--
-- The claim (status IN (runnable) AND wait_state <> 'waiting' ... ORDER BY
-- created_at) is best served by a PARTIAL index on created_at covering only the
-- runnable rows: the planner walks it in created_at order and stops at LIMIT
-- (O(LIMIT) instead of scanning + top-N sorting the whole runnable set), and being
-- small — rows enter on insert and leave the moment they reach a terminal state or
-- start waiting — it is far cheaper to maintain on this high-churn table than a
-- full-table index. Both engines' planners use it (the IN-list is satisfied by the
-- static predicate, so it no longer forces a bitmap scan + sort).
--
-- It replaces idx_process_instances_status_wait. The other two status-prefixed
-- indexes were redundant — the planner never picked them for the claim even under a
-- backlog — so they were pure per-insert overhead.
DROP INDEX IF EXISTS idx_instances_pending;            -- (status, next_retry_at), migration 001
DROP INDEX IF EXISTS idx_instances_status_created_at;  -- (status, created_at), migration 006
CREATE INDEX idx_instances_runnable ON process_instances (created_at)
    WHERE status IN ('running', 'failing', 'cancelling') AND wait_state <> 'waiting';
DROP INDEX IF EXISTS idx_process_instances_status_wait; -- (status, wait_state), migration 003
