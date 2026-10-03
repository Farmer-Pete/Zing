-- 0006_check_procs.sql -- the running CHECK command's process identity (#55,
-- #45 parity): one row per ticket while a test or lint command runs, so a
-- later serve can tell a live orphaned command from a dead one. Older
-- binaries ignore the table.
CREATE TABLE check_procs (
    ticket_id         INTEGER PRIMARY KEY REFERENCES tickets(id) ON DELETE CASCADE,
    kind              TEXT    NOT NULL CHECK (kind IN ('test', 'lint')),
    pgid              INTEGER NOT NULL CHECK (pgid > 0),
    proc_start        TEXT    CHECK (proc_start IS NULL OR length(proc_start) > 0),
    started_at        TEXT    NOT NULL,
    budget_started_at TEXT    NOT NULL
);
