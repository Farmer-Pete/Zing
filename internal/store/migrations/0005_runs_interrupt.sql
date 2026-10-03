-- 0005_runs_interrupt.sql -- the interrupted-run columns #45 needs (design
-- section 5.1, D6): a run a shutdown or a dead-serve reclaim cut off keeps
-- outcome='error' (SQLite cannot change the CHECK without a table rebuild,
-- which migration 0003 showed fails under the migrator's shared
-- transaction), with `interrupted` marking that it can resume for free
-- (D5), and the three identity columns a started agent process is recorded
-- under so a later serve can tell a live orphan from a dead one (section
-- 6.3, 7.1).
--
-- Rolling compatibility: an older binary ignores the four new columns. A
-- newer binary reads interrupted = 0 for any row an older binary wrote,
-- which is exactly the old meaning (never interrupted). No backfill, and no
-- lock concerns: this is a local SQLite database and ADD COLUMN is metadata
-- only.
ALTER TABLE runs ADD COLUMN interrupted INTEGER NOT NULL DEFAULT 0 CHECK (interrupted IN (0, 1));
ALTER TABLE runs ADD COLUMN pgid INTEGER CHECK (pgid IS NULL OR pgid > 0);
ALTER TABLE runs ADD COLUMN proc_start TEXT CHECK (proc_start IS NULL OR length(proc_start) > 0);
ALTER TABLE runs ADD COLUMN started_at TEXT;
