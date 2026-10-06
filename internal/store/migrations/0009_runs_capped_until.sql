-- 0009_runs_capped_until.sql -- a run parked by a Claude session limit
-- keeps outcome='error' and interrupted=1 (the free resume of migration
-- 0005) and records the reset instant it waits for. NULL for every other
-- run. Fixed layout 2006-01-02T15:04:05Z, so TEXT comparison orders it.
ALTER TABLE runs ADD COLUMN capped_until TEXT CHECK (
    capped_until IS NULL OR (interrupted = 1 AND length(capped_until) = 20)
);
