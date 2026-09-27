-- 0003_sessions_external_id_nonempty.sql — forbid sessions.external_id = ''
-- (F035). An empty external_id never means anything: it is not a valid
-- runtime session id and it is not "no session id yet" (that is NULL). Every
-- current writer already leaves it NULL or fills a real id, so this closes
-- off a case nothing produces today rather than migrating live data.
--
-- SQLite has no ALTER TABLE ... ADD CONSTRAINT, so the table is rebuilt: a
-- new sessions table carries the same columns and constraints as
-- 0001_init.sql plus the new CHECK, existing rows are copied across, the old
-- table is dropped, and the replacement is renamed into its place. The one
-- index 0001_init.sql defines on sessions is recreated afterward.
--
-- runs.session_id REFERENCES sessions(id) (0001_init.sql), and this
-- database runs with PRAGMA foreign_keys=ON (store.go's Open). The
-- PRAGMA foreign_keys=OFF / ON pair below is the standard SQLite
-- table-rebuild procedure (sqlite.org/lang_altertable.html section 7, the
-- "12 steps"), included for correctness and because it is a no-op in this
-- runner rather than a functioning safeguard: adlio/schema's Migrator.Apply
-- opens one transaction before running any migration file's SQL, and
-- PRAGMA foreign_keys is documented to be a no-op once a transaction is
-- already open, so it cannot actually be turned off here. Verified by hand
-- (a standalone probe against modernc.org/sqlite, this repo's driver, using
-- the same DSN pragmas and the same "one shared transaction" shape
-- Migrator.Apply uses): with an existing sessions row and a runs row
-- referencing it, DROP TABLE sessions fails with "FOREIGN KEY constraint
-- failed" whether or not this PRAGMA precedes it, and PRAGMA
-- defer_foreign_keys=ON (the one FK pragma that SQLite does allow mid
-- transaction) does not save it either: DROP TABLE immediately counts every
-- existing referencing row as a deferred violation, and nothing later in
-- the rebuild decrements that counter back to zero, so the commit still
-- fails even though the final data has no real violation. On a database
-- that already has rows in runs, this migration will fail with a FOREIGN
-- KEY constraint error; on a fresh database (every existing test, and any
-- install before its first commit writes a run) sessions and runs are both
-- still empty when this migration applies, so the rebuild has nothing to
-- violate and succeeds. Making this safe against a populated database needs
-- a runner change (running this file's rebuild outside the migrator's
-- shared transaction, with foreign_keys off before it starts) that is out
-- of scope here.
PRAGMA foreign_keys=OFF;

CREATE TABLE sessions_new (
    id          INTEGER PRIMARY KEY,
    ticket_id   INTEGER NOT NULL REFERENCES tickets(id),
    job         TEXT NOT NULL,
    runtime     TEXT NOT NULL CHECK (runtime IN ('claude','codex','fake')),
    external_id TEXT CHECK (external_id IS NULL OR external_id <> ''),
    resumes     INTEGER NOT NULL DEFAULT 0
);

INSERT INTO sessions_new (id, ticket_id, job, runtime, external_id, resumes)
    SELECT id, ticket_id, job, runtime, external_id, resumes FROM sessions;

DROP TABLE sessions;

ALTER TABLE sessions_new RENAME TO sessions;

CREATE INDEX sessions_ticket_idx ON sessions (ticket_id);

PRAGMA foreign_key_check;

PRAGMA foreign_keys=ON;
