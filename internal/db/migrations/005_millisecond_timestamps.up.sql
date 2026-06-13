-- Timestamp columns switch from unix seconds to unix milliseconds.
-- Columns stay BIGINT; existing values are scaled in place.
UPDATE process_definitions SET created_at = created_at * 1000;
UPDATE process_instances
SET created_at       = created_at * 1000,
    updated_at       = updated_at * 1000,
    next_retry_at    = next_retry_at * 1000,
    lease_expires_at = lease_expires_at * 1000;
UPDATE process_channels SET updated_at = updated_at * 1000;
