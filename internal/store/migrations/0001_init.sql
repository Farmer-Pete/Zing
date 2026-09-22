-- 0001_init.sql — Zing schema, version 1. Eight tables. Trust root.
CREATE TABLE projects (
    id             INTEGER PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    repo_url       TEXT NOT NULL,
    local_path     TEXT NOT NULL,
    tracker        TEXT NOT NULL CHECK (tracker IN ('github')),
    default_branch TEXT NOT NULL DEFAULT 'main'
);

CREATE TABLE tickets (
    id               INTEGER PRIMARY KEY,
    project_id       INTEGER NOT NULL REFERENCES projects(id),
    tracker_ref      TEXT NOT NULL,
    title            TEXT NOT NULL,
    body             TEXT NOT NULL DEFAULT '',
    kind             TEXT CHECK (kind IS NULL OR kind IN ('bug','feature')),
    state            TEXT NOT NULL CHECK (state IN
                       ('queued','planning','building','reviewing','judging','shipping','done','escalated','abandoned')),
    waiting_on       TEXT CHECK (waiting_on IS NULL OR waiting_on IN
                       ('questions','split','gate','perimeter','review','merge','children','error')),
    parent_ticket_id INTEGER REFERENCES tickets(id),
    branch           TEXT,
    pr_url           TEXT,
    claim_owner      TEXT,
    claim_expires_at TEXT,
    UNIQUE (project_id, tracker_ref)
);

CREATE TABLE sessions (
    id          INTEGER PRIMARY KEY,
    ticket_id   INTEGER NOT NULL REFERENCES tickets(id),
    job         TEXT NOT NULL,
    runtime     TEXT NOT NULL CHECK (runtime IN ('claude','codex','fake')),
    external_id TEXT,
    resumes     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE runs (
    id            INTEGER PRIMARY KEY,
    session_id    INTEGER NOT NULL REFERENCES sessions(id),
    turn          INTEGER NOT NULL,
    lens          TEXT CHECK (lens IS NULL OR lens IN
                    ('problem','simplification','correctness','security','fidelity','tests','quality','observability')),
    task_n        INTEGER,
    model         TEXT,
    outcome       TEXT CHECK (outcome IS NULL OR outcome IN
                    ('bug','feature','questions','ready','children','nothing_to_do','ok','question','error')),
    agent_seconds INTEGER,
    exit_code     INTEGER
);

CREATE TABLE messages (
    id        INTEGER PRIMARY KEY,
    ticket_id INTEGER NOT NULL REFERENCES tickets(id),
    run_id    INTEGER REFERENCES runs(id),
    parent_id INTEGER REFERENCES messages(id),
    type      TEXT NOT NULL CHECK (type IN
                ('question','answer','followup','resolved','escalation','state','update','reply','side')),
    author    TEXT NOT NULL CHECK (author IN ('zing','you','system','side')),
    -- state is coupled to type: question uses open/answered/resolved; answer and
    -- reply use draft/sent; every other type must be NULL (per section 9.1).
    -- The IS NOT NULL guards matter: SQLite passes a CHECK that evaluates to NULL,
    -- so without them a NULL state would slip past the first two branches.
    state     TEXT CHECK (
                  (type = 'question' AND state IS NOT NULL AND state IN ('open','answered','resolved'))
                  OR (type IN ('answer','reply') AND state IS NOT NULL AND state IN ('draft','sent'))
                  OR (type NOT IN ('question','answer','reply') AND state IS NULL)
                ),
    body      TEXT,
    payload   TEXT,
    batch_id  INTEGER,
    read_at   TEXT
);

CREATE TABLE artifacts (
    id        INTEGER PRIMARY KEY,
    ticket_id INTEGER NOT NULL REFERENCES tickets(id),
    run_id    INTEGER REFERENCES runs(id),
    type      TEXT NOT NULL CHECK (type IN
                ('claims','scenario','plan','planreview','children','file','fence','task','build_report','finding','verdict','respond')),
    version   INTEGER NOT NULL DEFAULT 1,
    payload   TEXT NOT NULL,
    sealed_at TEXT
);
-- Whole-document artifact types are unique per (ticket, type, version); per-row types are not.
CREATE UNIQUE INDEX artifacts_whole_doc_uk ON artifacts (ticket_id, type, version)
    WHERE type IN ('claims','plan','planreview','children');

CREATE TABLE push_subscriptions (
    id        INTEGER PRIMARY KEY,
    endpoint  TEXT NOT NULL UNIQUE,
    keys_json TEXT NOT NULL
);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT
);
INSERT INTO settings (key, value) VALUES
    ('draining','false'), ('stopped','false'), ('log_level','info'),
    ('last_good_binary',NULL), ('trust_root_hash',NULL);

CREATE INDEX tickets_state_idx      ON tickets (state);
CREATE INDEX tickets_project_idx    ON tickets (project_id);
CREATE INDEX messages_ticket_idx    ON messages (ticket_id);
CREATE INDEX artifacts_ticket_idx   ON artifacts (ticket_id);
CREATE INDEX sessions_ticket_idx    ON sessions (ticket_id);
CREATE INDEX runs_session_idx       ON runs (session_id);
