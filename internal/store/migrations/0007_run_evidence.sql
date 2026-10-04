-- 0007_run_evidence.sql -- what a run left behind (#43 split): the
-- agent's final message (capped at 64 KiB by the runtime), the path of the
-- stderr file runjob.go writes under DATA_DIR/runs, and the Claude CLI's
-- transcript path. runJobWith writes all three once, right after the
-- runtime returns. Every column is nullable: a run from before this
-- migration, or one cut off before its runtime returned, keeps NULL.
-- length() counts UTF-8 characters, not bytes, and would under-count
-- multi-byte text against the byte caps runtime.capFinalMessage and
-- runtime.capDetail enforce; length(CAST(x AS BLOB)) counts bytes and
-- also can't be fooled by an embedded NUL the way length() can.
ALTER TABLE runs ADD COLUMN final_message TEXT CHECK (final_message IS NULL OR (length(CAST(final_message AS BLOB)) > 0 AND length(CAST(final_message AS BLOB)) <= 65536));
ALTER TABLE runs ADD COLUMN stderr_path TEXT CHECK (stderr_path IS NULL OR length(CAST(stderr_path AS BLOB)) > 0);
ALTER TABLE runs ADD COLUMN transcript_path TEXT CHECK (transcript_path IS NULL OR length(CAST(transcript_path AS BLOB)) > 0);
