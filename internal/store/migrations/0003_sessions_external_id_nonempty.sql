-- 0003_sessions_external_id_nonempty.sql — forbid sessions.external_id = ''
-- (F035). An empty external_id never means anything: it is not a valid
-- runtime session id and it is not "no session id yet" (that is NULL).
-- Every current writer already leaves it NULL or fills a real id, so this
-- closes off a case nothing produces today rather than migrating live data.
--
-- SQLite has no ALTER TABLE ... ADD CONSTRAINT, so a CHECK would need the
-- table rebuilt: a new sessions table, existing rows copied across, the old
-- table dropped, the replacement renamed into place. runs.session_id
-- REFERENCES sessions(id) (0001_init.sql), and this database runs with
-- PRAGMA foreign_keys=ON (store.go's Open), so that rebuild needs
-- foreign_keys off around it -- but adlio/schema's Migrator.Apply opens one
-- transaction before running any migration file's SQL, and PRAGMA
-- foreign_keys is documented to be a no-op once a transaction is already
-- open. A rebuild here fails with a FOREIGN KEY constraint error on any
-- database where runs already has a row (verified by hand against
-- modernc.org/sqlite, this repo's driver, reproducing that same
-- one-shared-transaction shape), so this migration uses a trigger instead:
-- it needs no rebuild and no foreign-key bracket, only the table's current
-- schema, the same choice 0002 made for messages.created_at.
--
-- Two triggers cover every write path onto external_id: BEFORE INSERT for a
-- new session row (Reserve, and the planning record builders), and BEFORE
-- UPDATE OF external_id for upsertSessionTx's fill-on-first-terminalizing-
-- commit. Either firing RAISE(ABORT) fails that statement, and the
-- transaction it's part of, before anything is written. NULL = '' is NULL,
-- not true, in SQLite, so a NULL external_id passes both triggers'
-- conditions unchanged, same as before this migration.
CREATE TRIGGER sessions_external_id_nonempty_insert BEFORE INSERT ON sessions
WHEN NEW.external_id = ''
BEGIN SELECT RAISE(ABORT, 'sessions.external_id must not be empty'); END;

CREATE TRIGGER sessions_external_id_nonempty_update BEFORE UPDATE OF external_id ON sessions
WHEN NEW.external_id = ''
BEGIN SELECT RAISE(ABORT, 'sessions.external_id must not be empty'); END;
