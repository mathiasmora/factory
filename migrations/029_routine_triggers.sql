-- A Routine now carries its own activation state. Scheduling stays a property
-- of the schedule: a Routine can be active with label triggers and no cron, so
-- schedule_enabled cannot stand in for "this Routine may start Work".
ALTER TABLE routines
ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1));

-- Rows that already refuse new Work must not appear active in the operator UI.
UPDATE routines
SET enabled = 0
WHERE archived = 1 OR read_only = 1 OR migration_only = 1;

-- Label triggers move out of labels.toml so the UI can author them and so a
-- rename cannot orphan a trigger: the routine id is the key, not the name.
CREATE TABLE routine_triggers (
    routine_id TEXT NOT NULL REFERENCES routines(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    kind TEXT NOT NULL CHECK (kind IN ('github_issue', 'github_pull_request')),
    label TEXT NOT NULL CHECK (length(label) BETWEEN 1 AND 200),
    item_state TEXT NOT NULL CHECK (item_state IN ('open', 'closed', 'merged')),
    poll_interval_seconds INTEGER NOT NULL
        CHECK (poll_interval_seconds BETWEEN 10 AND 86400),
    -- Bounds a merged pull-request trigger. Without it the first cycle admits
    -- Work for every pull request ever merged with the label.
    merged_after INTEGER,
    next_poll_at INTEGER,
    PRIMARY KEY (routine_id, position),
    UNIQUE (routine_id, kind, label),
    CHECK (kind = 'github_pull_request' OR item_state IN ('open', 'closed')),
    CHECK (item_state != 'merged' OR merged_after IS NOT NULL)
);

CREATE INDEX routine_triggers_due
ON routine_triggers(next_poll_at, routine_id, position);

-- Records that an operator's labels.toml was imported, so a restart does not
-- recreate triggers the operator has since deleted in the UI.
CREATE TABLE routine_trigger_imports (
    path TEXT PRIMARY KEY,
    imported_at INTEGER NOT NULL,
    trigger_count INTEGER NOT NULL CHECK (trigger_count >= 0)
);
