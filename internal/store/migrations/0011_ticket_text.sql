-- 0011_ticket_text.sql - the tracker text a planning refresh last read:
-- the issue body (NULL for rows inserted before this migration) and the
-- owner's comments rendered by tracker.RenderComments.
ALTER TABLE tickets ADD COLUMN tracker_body TEXT;
ALTER TABLE tickets ADD COLUMN owner_comments TEXT NOT NULL DEFAULT '';
