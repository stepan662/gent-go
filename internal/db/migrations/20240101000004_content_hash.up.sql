ALTER TABLE process_definitions ADD COLUMN content_hash TEXT NOT NULL DEFAULT '';
CREATE INDEX process_definitions_hash ON process_definitions (name, content_hash);
