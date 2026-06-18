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

-- name: ListDefinitions :many
SELECT name, version, definition, content_hash, created_at
FROM process_definitions
ORDER BY name, version;

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

-- name: ListChannels :many
SELECT channel, version FROM process_channels
WHERE name = sqlc.arg(name)
ORDER BY channel;

-- name: LoadDefinitionsOnChannel :many
SELECT pc.version, pd.definition
FROM process_channels pc
JOIN process_definitions pd ON pd.name = pc.name AND pd.version = pc.version
WHERE pc.channel = sqlc.arg(channel)
ORDER BY pc.name;

-- name: InsertInstance :exec
INSERT INTO process_instances
    (id, process_name, process_version, task_queue, context_data, parent_id, spawn_task_id,
     call_stack, retry_count, wake_at, status, wait_state, error, created_at, updated_at)
VALUES
    (sqlc.arg(id), sqlc.arg(process_name), sqlc.arg(process_version),
     sqlc.arg(task_queue), sqlc.arg(context_data), sqlc.arg(parent_id), sqlc.arg(spawn_task_id),
     sqlc.arg(call_stack), sqlc.arg(retry_count), sqlc.arg(wake_at),
     sqlc.arg(status), sqlc.arg(wait_state), sqlc.arg(error), sqlc.arg(created_at), sqlc.arg(updated_at));

-- name: UpdateInstance :exec
UPDATE process_instances
SET task_queue       = sqlc.arg(task_queue),
    context_data     = sqlc.arg(context_data),
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
UPDATE process_instances
SET task_queue       = sqlc.arg(task_queue),
    context_data     = sqlc.arg(context_data),
    retry_count      = sqlc.arg(retry_count),
    wake_at    = sqlc.arg(wake_at),
    wait_state       = sqlc.arg(wait_state),
    updated_at       = sqlc.arg(updated_at),
    worker_id        = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg(id);

-- name: GetInstance :one
SELECT id, process_name, process_version, task_queue, context_data, parent_id,
       call_stack, retry_count, wake_at, status, error,
       created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id
FROM process_instances
WHERE id = sqlc.arg(id);

-- name: ListInstances :many
-- Empty status lists every instance; a non-empty status filters to it.
SELECT id, process_name, process_version, task_queue, context_data, parent_id,
       call_stack, retry_count, wake_at, status, error,
       created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id
FROM process_instances
WHERE (sqlc.arg(status) = '' OR status = sqlc.arg(status))
ORDER BY created_at DESC;

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
SELECT id, process_name, process_version, task_queue, context_data, parent_id,
       call_stack, retry_count, wake_at, status, error,
       created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id
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
    (id, instance_id, level, event, task_id, message, code, detail, created_at)
VALUES
    (sqlc.arg(id), sqlc.arg(instance_id), sqlc.arg(level), sqlc.arg(event),
     sqlc.arg(task_id), sqlc.arg(message), sqlc.arg(code), sqlc.arg(detail), sqlc.arg(created_at));

-- name: ListLogs :many
-- Empty level lists every level. since=0 lists from the start. The (after_ts,
-- after_id) pair is a keyset cursor: pass (0, '') for the first page. The tuple
-- comparison is spelled out (not row-value syntax) so it runs on SQLite too.
SELECT id, instance_id, level, event, task_id, message, code, detail, created_at
FROM process_logs
WHERE instance_id = sqlc.arg(instance_id)
  AND (sqlc.arg(level) = '' OR level = sqlc.arg(level))
  AND created_at >= sqlc.arg(since)
  AND (created_at > sqlc.arg(after_ts)
       OR (created_at = sqlc.arg(after_ts) AND id > sqlc.arg(after_id)))
ORDER BY created_at, id
LIMIT sqlc.arg(lim);

-- Subtree log view ("logs for X and all its descendants") is hand-written in
-- db_logs.go (ListTreeLogs): a WITH RECURSIVE walk over process_instances.parent_id
-- that sqlc's SQLite grammar can't parse. Both runtime drivers support it.

-- name: DeleteLogsBefore :execrows
DELETE FROM process_logs WHERE created_at < sqlc.arg(before);
