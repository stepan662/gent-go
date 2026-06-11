-- name: InsertDefinition :exec
INSERT INTO process_definitions (name, version, definition, content_hash, created_at)
VALUES (sqlc.arg(name), sqlc.arg(version), sqlc.arg(definition), sqlc.arg(content_hash), sqlc.arg(created_at))
ON CONFLICT (name, version) DO UPDATE SET definition = EXCLUDED.definition;

-- name: GetDefinition :one
SELECT name, version, definition, content_hash, created_at
FROM process_definitions
WHERE name = sqlc.arg(name) AND version = sqlc.arg(version);

-- name: GetDefinitionRaw :one
SELECT definition FROM process_definitions
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
INSERT INTO process_dependencies (parent_name, parent_version, step_id, child_key, child_name, child_version)
VALUES (sqlc.arg(parent_name), sqlc.arg(parent_version), sqlc.arg(step_id), sqlc.arg(child_key), sqlc.arg(child_name), sqlc.arg(child_version));

-- name: GetDependencies :many
SELECT parent_name, parent_version, step_id, child_key, child_name, child_version
FROM process_dependencies
WHERE parent_name = sqlc.arg(parent_name) AND parent_version = sqlc.arg(parent_version)
ORDER BY step_id, child_key;

-- name: GetDependencyVersion :one
SELECT child_version FROM process_dependencies
WHERE parent_name = sqlc.arg(parent_name)
  AND parent_version = sqlc.arg(parent_version)
  AND step_id = sqlc.arg(step_id)
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

-- name: ListChannelsForChannel :many
SELECT name, channel, version, updated_at FROM process_channels
WHERE channel = sqlc.arg(channel);

-- name: InsertInstance :exec
INSERT INTO process_instances
    (id, process_name, process_version, step_queue, context_data, parent_id,
     call_stack, retry_count, next_retry_at, status, wait_state, error, created_at, updated_at)
VALUES
    (sqlc.arg(id), sqlc.arg(process_name), sqlc.arg(process_version),
     sqlc.arg(step_queue), sqlc.arg(context_data), sqlc.arg(parent_id),
     sqlc.arg(call_stack), sqlc.arg(retry_count), sqlc.arg(next_retry_at),
     sqlc.arg(status), sqlc.arg(wait_state), sqlc.arg(error), sqlc.arg(created_at), sqlc.arg(updated_at));

-- name: UpdateInstance :exec
UPDATE process_instances
SET step_queue       = sqlc.arg(step_queue),
    context_data     = sqlc.arg(context_data),
    retry_count      = sqlc.arg(retry_count),
    next_retry_at    = sqlc.arg(next_retry_at),
    status           = sqlc.arg(status),
    wait_state       = sqlc.arg(wait_state),
    error            = sqlc.arg(error),
    updated_at       = sqlc.arg(updated_at),
    worker_id        = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg(id);

-- name: UpdateInstanceProgress :exec
UPDATE process_instances
SET step_queue       = sqlc.arg(step_queue),
    context_data     = sqlc.arg(context_data),
    retry_count      = sqlc.arg(retry_count),
    next_retry_at    = sqlc.arg(next_retry_at),
    updated_at       = sqlc.arg(updated_at),
    worker_id        = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg(id);

-- name: GetInstance :one
SELECT id, process_name, process_version, step_queue, context_data, parent_id,
       call_stack, retry_count, next_retry_at, status, error,
       created_at, updated_at, worker_id, lease_expires_at, wait_state
FROM process_instances
WHERE id = sqlc.arg(id);

-- name: ListInstances :many
SELECT id, process_name, process_version, step_queue, context_data, parent_id,
       call_stack, retry_count, next_retry_at, status, error,
       created_at, updated_at, worker_id, lease_expires_at, wait_state
FROM process_instances
ORDER BY created_at DESC;

-- name: ListInstancesByStatus :many
SELECT id, process_name, process_version, step_queue, context_data, parent_id,
       call_stack, retry_count, next_retry_at, status, error,
       created_at, updated_at, worker_id, lease_expires_at, wait_state
FROM process_instances
WHERE status = sqlc.arg(status)
ORDER BY created_at DESC;

-- name: RenewWorkerLeases :exec
UPDATE process_instances
SET lease_expires_at = sqlc.arg(lease_expires_at)
WHERE worker_id = sqlc.arg(worker_id);

-- name: CountActiveSiblings :one
SELECT COUNT(*) FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: SetParentCollecting :exec
UPDATE process_instances SET wait_state = 'collecting', updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: GetSiblings :many
SELECT id, process_name, process_version, step_queue, context_data, parent_id,
       call_stack, retry_count, next_retry_at, status, error,
       created_at, updated_at, worker_id, lease_expires_at, wait_state
FROM process_instances
WHERE parent_id = sqlc.arg(parent_id);

-- name: CountFailingChildren :one
SELECT COUNT(*) FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
  AND status = 'failed';

-- name: GetFirstFailingChild :one
SELECT id, error FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
  AND status = 'failed'
LIMIT 1;


-- name: FindStaleRefs :many
SELECT pd.parent_name, pc.version AS parent_version,
       pd.step_id, pd.child_name,
       pd.child_version AS baked_version, pc2.version AS channel_version
FROM process_dependencies pd
JOIN process_channels pc  ON pc.name  = pd.parent_name AND pc.channel = sqlc.arg(channel)
JOIN process_channels pc2 ON pc2.name = pd.child_name  AND pc2.channel = sqlc.arg(channel)
WHERE pd.parent_version = pc.version
  AND pd.child_version < pc2.version;
