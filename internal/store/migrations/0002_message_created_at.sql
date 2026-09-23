-- 0002_message_created_at.sql — add messages.created_at (design section 6.16).
--
-- SQLite forbids a non-constant DEFAULT on ADD COLUMN, so the column starts
-- nullable and is populated two ways: a one-time backfill for existing rows,
-- and an AFTER INSERT trigger for every future insert. The trigger fires for
-- every insert path (Package 1's InsertMessage, Package 3's commit inserts,
-- and this package's draft and batch inserts), so no insert-site code
-- changes anywhere. Ordering never depends on created_at; the monotonic id
-- remains the sort key, and the timestamp is display-only.
ALTER TABLE messages ADD COLUMN created_at TEXT;

UPDATE messages SET created_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE created_at IS NULL;

CREATE TRIGGER messages_set_created_at AFTER INSERT ON messages WHEN NEW.created_at IS NULL
BEGIN
    UPDATE messages SET created_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = NEW.id;
END;
