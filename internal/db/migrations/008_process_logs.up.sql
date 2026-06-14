-- Per-instance execution audit trail. The engine appends one row per lifecycle
-- event (step started/succeeded/failed, retry scheduled, routing, status change,
-- child spawn/collect) as it advances an instance. Append-only and best-effort:
-- a dropped row on crash is an observability gap, never a state corruption, so
-- writes never join the instance's state transaction.
CREATE TABLE process_logs (
    id          TEXT   NOT NULL PRIMARY KEY,   -- uuid
    instance_id TEXT   NOT NULL,
    level       TEXT   NOT NULL,               -- debug|info|warn|error
    event       TEXT   NOT NULL,               -- step_started, step_succeeded, retry_scheduled, ...
    step_id     TEXT   NOT NULL DEFAULT '',
    message     TEXT   NOT NULL DEFAULT '',
    code        TEXT   NOT NULL DEFAULT '',    -- transport error code where relevant
    detail      TEXT   NOT NULL DEFAULT '{}',  -- JSON: attempt/max, goto, request/response snippets
    created_at  BIGINT NOT NULL
);

-- Reads are "logs for instance X, oldest first"; (created_at, id) also serves
-- cursor pagination. Within one instance advance() is single-threaded under the
-- lease, so events are naturally ordered and ms granularity + id tie-break suffices.
-- Subtree ("logs for X and all its descendants") walks process_instances.parent_id
-- with a recursive CTE and joins this same index. See ListSubtreeLogs.
CREATE INDEX idx_process_logs_instance ON process_logs (instance_id, created_at, id);
-- Retention pruner scans by age.
CREATE INDEX idx_process_logs_created  ON process_logs (created_at);
