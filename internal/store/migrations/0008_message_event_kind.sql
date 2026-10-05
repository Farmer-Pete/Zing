-- 0008_message_event_kind.sql -- typed events (#34 first step): an
-- update row may carry an event_kind naming a schema under
-- internal/store/schemas/events, with its payload in the existing
-- messages.payload column. Free-text markers keep event_kind NULL.
-- Older binaries ignore the column.
ALTER TABLE messages ADD COLUMN event_kind TEXT CHECK (
    event_kind IS NULL
    OR (length(event_kind) > 0 AND type = 'update' AND payload IS NOT NULL)
);
