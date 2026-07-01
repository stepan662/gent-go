-- name: InsertDefinition :exec
INSERT INTO process_definitions (name, version, definition, content_hash, created_at)
VALUES (sqlc.arg(name), sqlc.arg(version), sqlc.arg(definition), sqlc.arg(content_hash), sqlc.arg(created_at))
ON CONFLICT (name, version) DO UPDATE SET definition = EXCLUDED.definition;

-- name: GetDefinition :one
SELECT name, version, definition, content_hash, created_at
FROM process_definitions
WHERE name = sqlc.arg(name) AND version = sqlc.arg(version);

-- name: LatestVersion :one
SELECT MAX(version) FROM process_definitions WHERE name = sqlc.arg(name);

-- name: FindVersionByHash :one
SELECT MAX(version) FROM process_definitions
WHERE name = sqlc.arg(name) AND content_hash = sqlc.arg(content_hash);

-- ListDefinitions is hand-written in db_registry.go (dynamic ORDER BY + keyset
-- cursor; see paginate.go).

-- name: DeleteDependencies :exec
DELETE FROM process_dependencies
WHERE parent_name = sqlc.arg(parent_name) AND parent_version = sqlc.arg(parent_version);

-- name: InsertDependency :exec
INSERT INTO process_dependencies (parent_name, parent_version, task_id, child_key, child_name, child_version)
VALUES (sqlc.arg(parent_name), sqlc.arg(parent_version), sqlc.arg(task_id), sqlc.arg(child_key), sqlc.arg(child_name), sqlc.arg(child_version));

-- name: GetDependencyVersion :one
SELECT child_version FROM process_dependencies
WHERE parent_name = sqlc.arg(parent_name)
  AND parent_version = sqlc.arg(parent_version)
  AND task_id = sqlc.arg(task_id)
  AND child_key = sqlc.arg(child_key);

-- name: UpsertChannel :exec
INSERT INTO process_channels (name, channel, version, updated_at)
VALUES (sqlc.arg(name), sqlc.arg(channel), sqlc.arg(version), sqlc.arg(updated_at))
ON CONFLICT (name, channel) DO UPDATE SET version = EXCLUDED.version, updated_at = EXCLUDED.updated_at;

-- name: GetChannel :one
SELECT version FROM process_channels
WHERE name = sqlc.arg(name) AND channel = sqlc.arg(channel);

-- name: DeleteChannel :exec
DELETE FROM process_channels WHERE name = sqlc.arg(name) AND channel = sqlc.arg(channel);

-- ListChannels is hand-written in db_registry.go (dynamic ORDER BY + keyset
-- cursor; see paginate.go).

-- name: LoadDefinitionsOnChannel :many
SELECT pc.version, pd.definition
FROM process_channels pc
JOIN process_definitions pd ON pd.name = pc.name AND pd.version = pc.version
WHERE pc.channel = sqlc.arg(channel)
ORDER BY pc.name;

-- name: InsertInstance :exec
INSERT INTO process_instances
    (id, process_name, process_version, task,
     input_data, outputs_data, output_data, error_data, external_data, engine_state,
     parent_id, spawn_task_id,
     call_stack, retry_count, wake_at, status, wait_state, error, created_at, updated_at)
VALUES
    (sqlc.arg(id), sqlc.arg(process_name), sqlc.arg(process_version), sqlc.arg(task),
     sqlc.arg(input_data), sqlc.arg(outputs_data), sqlc.arg(output_data),
     sqlc.arg(error_data), sqlc.arg(external_data), sqlc.arg(engine_state),
     sqlc.arg(parent_id), sqlc.arg(spawn_task_id),
     sqlc.arg(call_stack), sqlc.arg(retry_count), sqlc.arg(wake_at),
     sqlc.arg(status), sqlc.arg(wait_state), sqlc.arg(error), sqlc.arg(created_at), sqlc.arg(updated_at));

-- name: UpdateInstance :exec
-- input_data is intentionally NOT written: the process input is immutable after
-- creation, so re-writing it every update would be pure churn.
UPDATE process_instances
SET task             = sqlc.arg(task),
    outputs_data     = sqlc.arg(outputs_data),
    output_data      = sqlc.arg(output_data),
    error_data       = sqlc.arg(error_data),
    external_data    = sqlc.arg(external_data),
    engine_state     = sqlc.arg(engine_state),
    retry_count      = sqlc.arg(retry_count),
    wake_at    = sqlc.arg(wake_at),
    status           = sqlc.arg(status),
    wait_state       = sqlc.arg(wait_state),
    error            = sqlc.arg(error),
    updated_at       = sqlc.arg(updated_at),
    worker_id        = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg(id);

-- name: UpdateInstanceProgress :exec
-- Mid-process write: neither input_data (immutable) nor output_data (only set on
-- completion, which goes through UpdateInstance with a status change) is touched.
UPDATE process_instances
SET task             = sqlc.arg(task),
    outputs_data     = sqlc.arg(outputs_data),
    error_data       = sqlc.arg(error_data),
    external_data    = sqlc.arg(external_data),
    engine_state     = sqlc.arg(engine_state),
    retry_count      = sqlc.arg(retry_count),
    wake_at    = sqlc.arg(wake_at),
    wait_state       = sqlc.arg(wait_state),
    updated_at       = sqlc.arg(updated_at),
    worker_id        = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg(id);

-- name: GetInstance :one
-- Column order matches the process_instances row struct (context columns then task,
-- appended by migrations 019 and 020) so sqlc returns dbgen.ProcessInstance directly.
SELECT id, process_name, process_version, parent_id,
       call_stack, retry_count, wake_at, status, error,
       created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id,
       input_data, outputs_data, output_data, error_data, external_data, engine_state, task
FROM process_instances
WHERE id = sqlc.arg(id);

-- ListInstances is hand-written in db_instances.go and ListExternalTasks in
-- db_external.go (dynamic ORDER BY + keyset cursor; see paginate.go). The
-- external-task queue is still served by the partial idx_external_queue index.

-- name: InsertSignal :exec
INSERT INTO process_signals (id, instance_id, task_id, result, created_at)
VALUES (sqlc.arg(id), sqlc.arg(instance_id), sqlc.arg(task_id), sqlc.arg(result), sqlc.arg(created_at));

-- name: PopOldestSignal :one
-- Deletes and returns the oldest buffered signal for (instance, task), giving FIFO
-- delivery. Run inside the arm transaction, which already holds the instance row lock.
DELETE FROM process_signals
WHERE id = (
    SELECT s.id FROM process_signals s
    WHERE s.instance_id = sqlc.arg(instance_id) AND s.task_id = sqlc.arg(task_id)
    ORDER BY s.created_at, s.id LIMIT 1
)
RETURNING result;

-- name: SetExternalResult :exec
-- Un-parks an external task by storing the submitted/buffered result in external_data
-- and clearing the wait. It does NOT touch worker_id/lease: callers run it under the
-- instance row lock and either the instance is parked (lease already NULL) or the
-- engine is mid-arm and must keep its lease until it finishes advancing.
UPDATE process_instances
SET external_data = sqlc.arg(external_data),
    wait_state   = '',
    wake_at      = NULL,
    updated_at   = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: CountBufferedSignals :one
SELECT COUNT(*) FROM process_signals
WHERE instance_id = sqlc.arg(instance_id) AND task_id = sqlc.arg(task_id);

-- name: RenewWorkerLeasesChunk :execrows
-- Renews up to chunk_size of this worker's leases, soonest-to-expire first, that
-- are not already stamped to new_expiry. Called in a loop (one small transaction
-- per chunk) so a row locked by an in-flight advance stalls only its chunk, never
-- every lease at once. The new_expiry predicate makes each row eligible once per
-- pass, so the loop terminates.
UPDATE process_instances
SET lease_expires_at = sqlc.arg(new_expiry)
WHERE id IN (
    SELECT pi.id FROM process_instances pi
    WHERE pi.worker_id = sqlc.arg(worker_id)
      AND pi.lease_expires_at < sqlc.arg(new_expiry)
    ORDER BY pi.lease_expires_at ASC
    LIMIT sqlc.arg(chunk_size)
);

-- name: CountActiveSiblings :one
SELECT COUNT(*) FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
  AND spawn_task_id = sqlc.arg(spawn_task_id)
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: GetWaitState :one
SELECT wait_state FROM process_instances WHERE id = sqlc.arg(id);

-- name: WakeParent :exec
UPDATE process_instances
SET wait_state = CASE WHEN status = 'running' THEN 'collecting' ELSE '' END,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: GetChildrenForTask :many
SELECT id, process_name, process_version, parent_id,
       call_stack, retry_count, wake_at, status, error,
       created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id,
       input_data, outputs_data, output_data, error_data, external_data, engine_state, task
FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
  AND spawn_task_id = sqlc.arg(spawn_task_id);

-- name: FindParentsOf :many
SELECT pd.parent_name, pc.version AS parent_version, defp.definition AS parent_definition,
       pd.child_name, pd.child_version AS baked_version
FROM process_dependencies pd
JOIN process_channels pc ON pc.name = pd.parent_name AND pc.channel = sqlc.arg(channel)
JOIN process_definitions defp ON defp.name = pd.parent_name AND defp.version = pc.version
WHERE pd.parent_version = pc.version
  AND pd.child_name IN (SELECT value FROM json_each(sqlc.arg(names)));

-- name: FailAncestors :exec
UPDATE process_instances
SET status = 'failing', error = sqlc.arg(error), updated_at = sqlc.arg(updated_at)
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)))
  AND status IN ('running', 'cancelling');

-- name: FindStaleRefs :many
SELECT pd.parent_name, pc.version AS parent_version,
       pd.task_id, pd.child_name,
       pd.child_version AS baked_version, pc2.version AS channel_version
FROM process_dependencies pd
JOIN process_channels pc  ON pc.name  = pd.parent_name AND pc.channel = sqlc.arg(channel)
JOIN process_channels pc2 ON pc2.name = pd.child_name  AND pc2.channel = sqlc.arg(channel)
WHERE pd.parent_version = pc.version
  AND pd.child_version < pc2.version;

-- name: InsertLog :exec
INSERT INTO process_logs
    (id, instance_id, level, event, task_id, message, code, data, meta, created_at)
VALUES
    (sqlc.arg(id), sqlc.arg(instance_id), sqlc.arg(level), sqlc.arg(event),
     sqlc.arg(task_id), sqlc.arg(message), sqlc.arg(code), sqlc.arg(data), sqlc.arg(meta), sqlc.arg(created_at));

-- ListLogs (per-instance) and ListTreeLogs (subtree) are hand-written in
-- db_logs.go: both take a dynamic ORDER BY + keyset cursor (see paginate.go), and
-- the subtree view additionally needs a WITH RECURSIVE walk over
-- process_instances.parent_id that sqlc's SQLite grammar can't parse. Both runtime
-- drivers support it.

-- name: DeleteLogsBefore :execrows
DELETE FROM process_logs WHERE created_at < sqlc.arg(before);

-- name: PinContextObject :exec
-- Writes (or re-pins) a context object. ON CONFLICT keeps the immutable content and
-- sets pinned = 1: re-referencing a previously-dereferenced object (a looping task
-- recomputing the same big output) makes it pinned again without touching any log
-- reference the row may also carry.
INSERT INTO process_objects (instance_id, hash, content, size, pinned, log_until, created_at)
VALUES (sqlc.arg(instance_id), sqlc.arg(hash), sqlc.arg(content), sqlc.arg(size), 1, NULL, sqlc.arg(created_at))
ON CONFLICT (instance_id, hash) DO UPDATE SET pinned = 1;

-- name: ReferenceLogObject :exec
-- Records that a log row references this (pre-redacted) content until log_until, so
-- it survives at least as long as the log. ON CONFLICT keeps the immutable content and
-- extends the horizon, leaving any context pin intact (a shared, secret-free row).
INSERT INTO process_objects (instance_id, hash, content, size, pinned, log_until, created_at)
VALUES (sqlc.arg(instance_id), sqlc.arg(hash), sqlc.arg(content), sqlc.arg(size), 0, sqlc.arg(log_until), sqlc.arg(created_at))
ON CONFLICT (instance_id, hash) DO UPDATE SET log_until = excluded.log_until;

-- name: GetObject :one
-- Trusted internal read for context resolution (the instance owns the object).
SELECT content FROM process_objects WHERE instance_id = sqlc.arg(instance_id) AND hash = sqlc.arg(hash);

-- name: GetLogObject :one
-- Serve-safe read for the log endpoint: only log-referenced rows are returned, whose
-- content is always pre-redacted or (when shared) byte-identical to it, hence secret-free.
SELECT content FROM process_objects
WHERE instance_id = sqlc.arg(instance_id) AND hash = sqlc.arg(hash) AND log_until IS NOT NULL;

-- name: DeleteDereferencedObject :exec
-- Context dereference: delete the row outright when no live log still needs it, so a
-- replaced value (and any secret in it) does not linger.
DELETE FROM process_objects
WHERE instance_id = sqlc.arg(instance_id) AND hash = sqlc.arg(hash)
  AND (log_until IS NULL OR log_until < sqlc.arg(now));

-- name: UnpinObject :exec
-- Context dereference for a row a log still needs: drop the context pin so the GC sweep
-- reclaims it once the log horizon passes. No-op if DeleteDereferencedObject removed it.
UPDATE process_objects SET pinned = 0
WHERE instance_id = sqlc.arg(instance_id) AND hash = sqlc.arg(hash);

-- name: DeleteExpiredObjects :execrows
-- GC sweep: reclaim rows no longer pinned by context and no longer needed by any log.
DELETE FROM process_objects
WHERE pinned = 0 AND (log_until IS NULL OR log_until < sqlc.arg(before));
