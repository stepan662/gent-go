-- Replace child_idx (integer position) with child_key (string key) to support
-- named children in child/child_parallel call types.
CREATE TABLE IF NOT EXISTS process_dependencies_new (
    parent_name    TEXT    NOT NULL,
    parent_version INTEGER NOT NULL,
    step_id        TEXT    NOT NULL,
    child_key      TEXT    NOT NULL DEFAULT '',
    child_name     TEXT    NOT NULL,
    child_version  INTEGER NOT NULL,
    PRIMARY KEY (parent_name, parent_version, step_id, child_key),
    FOREIGN KEY (parent_name, parent_version) REFERENCES process_definitions(name, version),
    FOREIGN KEY (child_name, child_version)   REFERENCES process_definitions(name, version)
);
INSERT INTO process_dependencies_new
    SELECT parent_name, parent_version, step_id, CAST(child_idx AS TEXT), child_name, child_version
    FROM process_dependencies;
DROP TABLE process_dependencies;
ALTER TABLE process_dependencies_new RENAME TO process_dependencies;
CREATE INDEX IF NOT EXISTS process_dependencies_child
    ON process_dependencies (child_name, child_version);
