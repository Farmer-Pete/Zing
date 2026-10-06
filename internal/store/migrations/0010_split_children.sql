-- 0010_split_children.sql - an approved planner split: each child is a
-- ticket row keyed by its split key under its parent, and a child waits
-- on the tickets in ticket_dependencies until each is done.
CREATE TABLE ticket_dependencies (
    ticket_id            INTEGER NOT NULL REFERENCES tickets(id),
    depends_on_ticket_id INTEGER NOT NULL REFERENCES tickets(id),
    PRIMARY KEY (ticket_id, depends_on_ticket_id),
    CHECK (ticket_id != depends_on_ticket_id)
);
CREATE INDEX ticket_dependencies_on_idx ON ticket_dependencies (depends_on_ticket_id);

ALTER TABLE tickets ADD COLUMN split_key TEXT
    CHECK (split_key IS NULL OR split_key GLOB 'c[0-9]*');
CREATE UNIQUE INDEX tickets_split_key_uk ON tickets (parent_ticket_id, split_key)
    WHERE split_key IS NOT NULL;
