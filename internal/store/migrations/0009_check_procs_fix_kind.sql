-- 0009_check_procs_fix_kind.sql -- widen check_procs.kind to allow 'fix'
-- (#131 added the project's fix command to CHECK, but 0006's CHECK
-- constraint still refused anything but 'test' and 'lint', so every fix
-- run's RecordCheckStart insert failed and the command ran untracked).
--
-- SQLite cannot ALTER a CHECK constraint, so this rebuilds the table: rename
-- it aside, create the replacement with the wider list, copy every row
-- across, carry check_procs' AUTOINCREMENT high-water mark in
-- sqlite_sequence, and drop the renamed original.
--
-- Migration 0003 showed a table rebuild can fail under adlio/schema's
-- Migrator.Apply: it opens one transaction before any migration file's SQL
-- runs, and PRAGMA foreign_keys is documented as a no-op once a transaction
-- is already open, so a rebuild there raised a FOREIGN KEY constraint error
-- dropping a table another table still REFERENCES. That risk does not apply
-- here: no table in this schema REFERENCES check_procs, and no trigger or
-- view names it, so dropping check_procs_old has no child rows to violate
-- even with foreign_keys ON, and this rebuild is safe inside the migrator's
-- shared transaction.
ALTER TABLE check_procs RENAME TO check_procs_old;

CREATE TABLE check_procs (
    gen               INTEGER PRIMARY KEY AUTOINCREMENT,
    ticket_id         INTEGER NOT NULL UNIQUE REFERENCES tickets(id) ON DELETE CASCADE,
    kind              TEXT    NOT NULL CHECK (kind IN ('fix', 'lint', 'test')),
    pgid              INTEGER NOT NULL CHECK (pgid > 0),
    proc_start        TEXT    CHECK (proc_start IS NULL OR length(proc_start) > 0),
    started_at        TEXT    NOT NULL,
    budget_started_at TEXT    NOT NULL
);

INSERT INTO check_procs (gen, ticket_id, kind, pgid, proc_start, started_at, budget_started_at)
    SELECT gen, ticket_id, kind, pgid, proc_start, started_at, budget_started_at FROM check_procs_old;

DELETE FROM sqlite_sequence WHERE name = 'check_procs';
INSERT INTO sqlite_sequence (name, seq)
    SELECT 'check_procs', seq FROM sqlite_sequence WHERE name = 'check_procs_old';

DROP TABLE check_procs_old;
