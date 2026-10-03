-- 0006_check_procs.sql -- the running CHECK command's process identity (#55,
-- #45 parity): one row per ticket while a test or lint command runs, so a
-- later serve can tell a live orphaned command from a dead one. gen is
-- each record's own generation: AUTOINCREMENT never reuses one, and INSERT
-- OR REPLACE on ticket_id gives a replacement a new gen, so a clear or a
-- reclaim that names a gen can never delete a later command's row, even
-- one with the same pgid and no start token. Older binaries ignore the
-- table.
CREATE TABLE check_procs (
    gen               INTEGER PRIMARY KEY AUTOINCREMENT,
    ticket_id         INTEGER NOT NULL UNIQUE REFERENCES tickets(id) ON DELETE CASCADE,
    kind              TEXT    NOT NULL CHECK (kind IN ('test', 'lint')),
    pgid              INTEGER NOT NULL CHECK (pgid > 0),
    proc_start        TEXT    CHECK (proc_start IS NULL OR length(proc_start) > 0),
    started_at        TEXT    NOT NULL,
    budget_started_at TEXT    NOT NULL
);
