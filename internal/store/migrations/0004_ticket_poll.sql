-- 0004_ticket_poll.sql - the babysit poll's schedule on the ticket (PKG9 D8).
--
-- Three nullable columns. ALTER TABLE ADD COLUMN needs no table rebuild, so
-- it runs inside the one transaction adlio/schema opens around every
-- pending migration file together (the Package 7 lesson: a rebuild with
-- foreign_keys=OFF cannot run there). The fingerprint's lowercase-hex rule
-- is enforced by CommitHandlerResult's validation, the one writer.
ALTER TABLE tickets ADD COLUMN next_poll_at TEXT;
ALTER TABLE tickets ADD COLUMN poll_interval_s INTEGER
    CHECK (poll_interval_s IS NULL OR (poll_interval_s BETWEEN 30 AND 300));
ALTER TABLE tickets ADD COLUMN poll_fingerprint TEXT
    CHECK (poll_fingerprint IS NULL OR length(poll_fingerprint) = 64);

CREATE INDEX tickets_next_poll_idx ON tickets (next_poll_at);
