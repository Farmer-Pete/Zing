package store

import (
	"path/filepath"
	"testing"
)

// Table and type name literals repeated across this package's tests.
const (
	testTableMessages  = "messages"
	testTableArtifacts = "artifacts"
	testTypeQuestion   = "question"
	testTypeAnswer     = "answer"
	testTypeState      = "state"
	testTypePlanreview = "planreview"
	testTypeClaims     = "claims"
	testTypeScenario   = "scenario"
	testAuthorZing     = "zing"
	testAuthorYou      = "you"
)

var wantTables = []string{
	"projects", "tickets", "sessions", "runs", testTableMessages,
	testTableArtifacts, "push_subscriptions", "settings",
}

func TestOpen_CreatesAllTables(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "zing.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	for _, table := range wantTables {
		var name string
		err := s.db.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

func TestOpen_WALMode(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "zing.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestOpen_SecondOpenIsNoOp(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "zing.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = second.Close() }()

	for _, table := range wantTables {
		var name string
		err := second.db.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing after second Open: %v", table, err)
		}
	}
}

// TestOpen_FailingOpenLeavesNothingWedged forces a deterministic failure by
// passing an existing directory as the database path: MkdirAll on its parent
// succeeds, but the driver cannot open a directory as a SQLite file, so
// PingContext (or the migration step) fails. This exercises the close-on-error
// path in Open. A later Open on a good path must still succeed, proving the
// failed attempt leaked no connection.
func TestOpen_FailingOpenLeavesNothingWedged(t *testing.T) {
	ctx := t.Context()
	dirAsPath := t.TempDir()

	s, err := Open(ctx, dirAsPath)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open with a directory path: want error, got nil")
	}
	if s != nil {
		t.Errorf("Open with a directory path: want nil Store, got %v", s)
	}

	goodPath := filepath.Join(t.TempDir(), "zing.db")
	good, err := Open(ctx, goodPath)
	if err != nil {
		t.Fatalf("Open on a good path after a failed Open: %v", err)
	}
	_ = good.Close()
}

// seedProjectAndTicket inserts a project (id 1) and a ticket on it (id 1),
// the shared setup every other table's row references by foreign key.
func seedProjectAndTicket(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (id, name, repo_url, local_path, tracker) VALUES (1, 'zing', 'https://github.com/x/zing', '/tmp/zing', 'github')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tickets (id, project_id, tracker_ref, title, state) VALUES (1, 1, '42', 'fix the bug', 'queued')`,
	); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
}

// TestStore_RoundTripAllTables inserts into, and reads back from, every one
// of the eight tables, exercising each table's foreign keys, CHECK
// constraints, and unique indexes on a valid row.
func TestStore_RoundTripAllTables(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedProjectAndTicket(t, s)

	if _, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, ticket_id, job, runtime) VALUES (1, 1, 'planning', 'claude')`,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if _, err = s.db.ExecContext(ctx,
		`INSERT INTO runs (id, session_id, turn, outcome) VALUES (1, 1, 0, 'ok')`,
	); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	stateID, err := s.InsertMessage(ctx, Message{
		TicketID: 1, Type: testTypeState, Author: testAuthorZing,
		Payload: []byte(`{"from":"queued","to":"planning","reason":"started"}`),
	})
	if err != nil {
		t.Fatalf("insert state message: %v", err)
	}
	if _, err := s.InsertMessage(ctx, Message{
		TicketID: 1, ParentID: &stateID, Type: "update", Author: testAuthorZing, Body: "progress",
	}); err != nil {
		t.Fatalf("insert update message (no payload): %v", err)
	}

	if _, err := s.InsertArtifact(ctx, Artifact{
		TicketID: 1, Type: testTypeScenario,
		Payload: []byte(`{"id":"s1","kind":"behavior","check_cmd":"go test","given":"g","when":"w","then":"t"}`),
	}); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO push_subscriptions (endpoint, keys_json) VALUES ('https://push.example/1', '{}')`,
	); err != nil {
		t.Fatalf("insert push subscription: %v", err)
	}

	var draining string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'draining'").Scan(&draining); err != nil {
		t.Fatalf("read seeded settings: %v", err)
	}
	if draining != "false" {
		t.Errorf("settings.draining = %q, want %q", draining, "false")
	}
}

// TestStore_ConstraintViolations proves the CHECK, foreign key, and unique
// constraints in the migration actually reject a bad row.
func TestStore_ConstraintViolations(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedProjectAndTicket(t, s)

	t.Run("bad foreign key", func(t *testing.T) {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO sessions (ticket_id, job, runtime) VALUES (999, 'planning', 'claude')`)
		if err == nil {
			t.Error("insert with a nonexistent ticket_id: want error, got nil")
		}
	})

	t.Run("bad enum", func(t *testing.T) {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO tickets (project_id, tracker_ref, title, state) VALUES (1, '99', 't', 'bogus')`)
		if err == nil {
			t.Error("insert with an invalid state: want error, got nil")
		}
	})

	t.Run("duplicate unique key", func(t *testing.T) {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO projects (name, repo_url, local_path, tracker) VALUES ('zing', 'x', 'y', 'github')`)
		if err == nil {
			t.Error("insert with a duplicate project name: want error, got nil")
		}
	})
}

// TestInsertMessage_BindsPayloadAsText and TestInsertArtifact_BindsPayloadAsText
// prove payload rides as string(payload), never the raw []byte: modernc/sqlite
// binds a []byte as a BLOB, which would break the TEXT storage contract.
func TestInsertMessage_BindsPayloadAsText(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedProjectAndTicket(t, s)

	id, err := s.InsertMessage(ctx, Message{
		TicketID: 1, Type: testTypeState, Author: testAuthorZing,
		Payload: []byte(`{"from":"queued","to":"planning","reason":"started"}`),
	})
	if err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	var sqlType string
	if err := s.db.QueryRowContext(ctx, "SELECT typeof(payload) FROM messages WHERE id = ?", id).Scan(&sqlType); err != nil {
		t.Fatalf("query typeof(payload): %v", err)
	}
	if sqlType != "text" {
		t.Errorf("typeof(payload) = %q, want %q", sqlType, "text")
	}
}

func TestInsertArtifact_BindsPayloadAsText(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedProjectAndTicket(t, s)

	id, err := s.InsertArtifact(ctx, Artifact{
		TicketID: 1, Type: testTypeScenario,
		Payload: []byte(`{"id":"s1","kind":"behavior","check_cmd":"go test","given":"g","when":"w","then":"t"}`),
	})
	if err != nil {
		t.Fatalf("InsertArtifact: %v", err)
	}

	var sqlType string
	if err := s.db.QueryRowContext(ctx, "SELECT typeof(payload) FROM artifacts WHERE id = ?", id).Scan(&sqlType); err != nil {
		t.Fatalf("query typeof(payload): %v", err)
	}
	if sqlType != "text" {
		t.Errorf("typeof(payload) = %q, want %q", sqlType, "text")
	}
}

// TestVerifyTables_DetectsMissingTable proves the selftest table-existence
// check (plan section 6.9 step 2) actually catches an absent table, rather
// than only ever seeing a freshly migrated, complete schema.
func TestVerifyTables_DetectsMissingTable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err = s.VerifyTables(ctx); err != nil {
		t.Fatalf("VerifyTables on a freshly migrated database: %v, want nil", err)
	}

	if _, err = s.db.ExecContext(ctx, "DROP TABLE push_subscriptions"); err != nil {
		t.Fatalf("drop push_subscriptions: %v", err)
	}

	if err = s.VerifyTables(ctx); err == nil {
		t.Error("VerifyTables after dropping a table: want error, got nil")
	}
}

// TestStore_MessagesStateCheck proves the composite messages.state CHECK in
// migration 0001_init.sql: question, answer, and reply each require a
// non-NULL state from their own closed set, and every other message type
// requires NULL. The IS NOT NULL guards in the CHECK are what this test
// exercises: SQLite passes a CHECK that evaluates to NULL, so a NULL state
// on a question/answer/reply row would otherwise slip through.
func TestStore_MessagesStateCheck(t *testing.T) {
	t.Parallel()

	questionPayload := []byte(`{"key":"Q1","kind":"question","state":"open","recommended":"x","options":[]}`)
	answerPayload := []byte(`{}`)
	statePayload := []byte(`{"from":"queued","to":"planning","reason":"go"}`)

	strPtr := func(s string) *string { return &s }

	tests := []struct {
		name    string
		msg     Message
		wantErr bool
	}{
		{"question with NULL state is rejected", Message{TicketID: 1, Type: testTypeQuestion, Author: testAuthorZing, Payload: questionPayload}, true},
		{"question with a valid state succeeds", Message{TicketID: 1, Type: testTypeQuestion, Author: testAuthorZing, Payload: questionPayload, State: strPtr("open")}, false},
		{"answer with NULL state is rejected", Message{TicketID: 1, Type: testTypeAnswer, Author: testAuthorYou, Payload: answerPayload}, true},
		{"answer with a valid state succeeds", Message{TicketID: 1, Type: testTypeAnswer, Author: testAuthorYou, Payload: answerPayload, State: strPtr("draft")}, false},
		{"reply with NULL state is rejected", Message{TicketID: 1, Type: "reply", Author: testAuthorZing}, true},
		{"reply with a valid state succeeds", Message{TicketID: 1, Type: "reply", Author: testAuthorZing, State: strPtr("sent")}, false},
		{"a state-type message with a non-NULL state is rejected", Message{TicketID: 1, Type: testTypeState, Author: testAuthorZing, Payload: statePayload, State: strPtr("open")}, true},
		{"a state-type message with NULL state succeeds", Message{TicketID: 1, Type: testTypeState, Author: testAuthorZing, Payload: statePayload}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			s, err := Open(ctx, dbPath(t))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = s.Close() }()
			seedProjectAndTicket(t, s)

			_, err = s.InsertMessage(ctx, tt.msg)
			if tt.wantErr && err == nil {
				t.Error("InsertMessage: want error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("InsertMessage: %v, want nil", err)
			}
		})
	}
}

// TestStore_ArtifactWholeDocUniqueIndex proves the partial unique index
// artifacts_whole_doc_uk: a whole-document type (claims, plan, planreview,
// children) may not repeat at the same (ticket_id, type, version), but a
// per-row type such as scenario may.
func TestStore_ArtifactWholeDocUniqueIndex(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	seedProjectAndTicket(t, s)

	claims := []byte(`[{"kind":"code","verdict":"true","evidence":"x","text":"y"}]`)
	if _, err = s.InsertArtifact(ctx, Artifact{TicketID: 1, Type: testTypeClaims, Version: 1, Payload: claims}); err != nil {
		t.Fatalf("first claims insert: %v", err)
	}
	if _, err = s.InsertArtifact(ctx, Artifact{TicketID: 1, Type: testTypeClaims, Version: 1, Payload: claims}); err == nil {
		t.Error("second claims insert at the same (ticket_id, type, version): want error, got nil")
	}

	scenario1 := []byte(`{"id":"s1","kind":"behavior","check_cmd":"go test","given":"g","when":"w","then":"t"}`)
	if _, err = s.InsertArtifact(ctx, Artifact{TicketID: 1, Type: testTypeScenario, Version: 1, Payload: scenario1}); err != nil {
		t.Fatalf("first scenario insert: %v", err)
	}
	scenario2 := []byte(`{"id":"s2","kind":"behavior","check_cmd":"go test","given":"g","when":"w","then":"t"}`)
	if _, err = s.InsertArtifact(ctx, Artifact{TicketID: 1, Type: testTypeScenario, Version: 1, Payload: scenario2}); err != nil {
		t.Errorf("second scenario insert at the same ticket (a per-row type): %v, want nil", err)
	}
}

// TestStore_TicketsWaitingOnCheck proves tickets.waiting_on rejects a value
// outside its closed set.
func TestStore_TicketsWaitingOnCheck(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err = s.db.ExecContext(ctx,
		`INSERT INTO projects (id, name, repo_url, local_path, tracker) VALUES (1, 'zing', 'https://github.com/x/zing', '/tmp/zing', 'github')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO tickets (project_id, tracker_ref, title, state, waiting_on) VALUES (1, '99', 't', 'queued', 'bogus')`)
	if err == nil {
		t.Error("insert with an invalid waiting_on: want error, got nil")
	}
}

// TestStore_RunsChecks proves runs.lens and runs.outcome each reject a value
// outside their closed set.
func TestStore_RunsChecks(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// t.Cleanup, not defer: the subtests below call t.Parallel, so this
	// function returns (and a defer would fire) before they actually run.
	t.Cleanup(func() { _ = s.Close() })
	seedProjectAndTicket(t, s)

	if _, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, ticket_id, job, runtime) VALUES (1, 1, 'planning', 'claude')`,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	t.Run("invalid lens is rejected", func(t *testing.T) {
		t.Parallel()
		_, runErr := s.db.ExecContext(ctx, `INSERT INTO runs (session_id, turn, lens) VALUES (1, 0, 'bogus')`)
		if runErr == nil {
			t.Error("insert with an invalid lens: want error, got nil")
		}
	})

	t.Run("invalid outcome is rejected", func(t *testing.T) {
		t.Parallel()
		_, runErr := s.db.ExecContext(ctx, `INSERT INTO runs (session_id, turn, outcome) VALUES (1, 1, 'bogus')`)
		if runErr == nil {
			t.Error("insert with an invalid outcome: want error, got nil")
		}
	})
}

// TestStore_TicketsProjectTrackerRefUnique proves UNIQUE(project_id, tracker_ref).
func TestStore_TicketsProjectTrackerRefUnique(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	seedProjectAndTicket(t, s) // seeds tracker_ref '42' on project 1

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO tickets (project_id, tracker_ref, title, state) VALUES (1, '42', 'a duplicate ticket', 'queued')`)
	if err == nil {
		t.Error("insert with a duplicate (project_id, tracker_ref): want error, got nil")
	}
}

// TestStore_PushSubscriptionsEndpointUnique proves push_subscriptions.endpoint UNIQUE.
func TestStore_PushSubscriptionsEndpointUnique(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err = s.db.ExecContext(ctx,
		`INSERT INTO push_subscriptions (endpoint, keys_json) VALUES ('https://push.example/1', '{}')`,
	); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO push_subscriptions (endpoint, keys_json) VALUES ('https://push.example/1', '{}')`)
	if err == nil {
		t.Error("insert with a duplicate endpoint: want error, got nil")
	}
}

// TestStore_RoundTrip_ReadsBackStoredValues completes the section 21 round
// trip: after inserting a row into every one of the eight tables, it reads
// each back and asserts the stored values, not just that the insert
// succeeded.
func TestStore_RoundTrip_ReadsBackStoredValues(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	seedProjectAndTicket(t, s)

	if _, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, ticket_id, job, runtime) VALUES (1, 1, 'planning', 'claude')`,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err = s.db.ExecContext(ctx,
		`INSERT INTO runs (id, session_id, turn, outcome) VALUES (1, 1, 0, 'ok')`,
	); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	msgID, err := s.InsertMessage(ctx, Message{
		TicketID: 1, Type: testTypeState, Author: testAuthorZing, Body: "started building",
		Payload: []byte(`{"from":"queued","to":"planning","reason":"started"}`),
	})
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	artID, err := s.InsertArtifact(ctx, Artifact{
		TicketID: 1, Type: testTypeScenario,
		Payload: []byte(`{"id":"s1","kind":"behavior","check_cmd":"go test","given":"g","when":"w","then":"t"}`),
	})
	if err != nil {
		t.Fatalf("insert artifact: %v", err)
	}

	if _, err = s.db.ExecContext(ctx,
		`INSERT INTO push_subscriptions (endpoint, keys_json) VALUES ('https://push.example/rt', '{"p256dh":"x"}')`,
	); err != nil {
		t.Fatalf("insert push subscription: %v", err)
	}

	var pName, pRepo, pTracker string
	if err = s.db.QueryRowContext(ctx, "SELECT name, repo_url, tracker FROM projects WHERE id = 1").
		Scan(&pName, &pRepo, &pTracker); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if pName != "zing" || pRepo != "https://github.com/x/zing" || pTracker != "github" {
		t.Errorf("project = (%q, %q, %q), want (zing, https://github.com/x/zing, github)", pName, pRepo, pTracker)
	}

	var tTitle, tState string
	if err = s.db.QueryRowContext(ctx, "SELECT title, state FROM tickets WHERE id = 1").
		Scan(&tTitle, &tState); err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	if tTitle != "fix the bug" || tState != "queued" {
		t.Errorf("ticket = (%q, %q), want (fix the bug, queued)", tTitle, tState)
	}

	var sJob, sRuntime string
	if err = s.db.QueryRowContext(ctx, "SELECT job, runtime FROM sessions WHERE id = 1").
		Scan(&sJob, &sRuntime); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sJob != "planning" || sRuntime != "claude" {
		t.Errorf("session = (%q, %q), want (planning, claude)", sJob, sRuntime)
	}

	var rOutcome string
	if err = s.db.QueryRowContext(ctx, "SELECT outcome FROM runs WHERE id = 1").Scan(&rOutcome); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if rOutcome != "ok" {
		t.Errorf("run.outcome = %q, want ok", rOutcome)
	}

	var mType, mBody, mPayload string
	if err = s.db.QueryRowContext(ctx, "SELECT type, body, payload FROM messages WHERE id = ?", msgID).
		Scan(&mType, &mBody, &mPayload); err != nil {
		t.Fatalf("read message: %v", err)
	}
	if mType != testTypeState || mBody != "started building" {
		t.Errorf("message = (%q, %q), want (state, started building)", mType, mBody)
	}
	if mPayload != `{"from":"queued","to":"planning","reason":"started"}` {
		t.Errorf("message.payload = %q, want the inserted JSON verbatim", mPayload)
	}

	var aType, aPayload string
	if err = s.db.QueryRowContext(ctx, "SELECT type, payload FROM artifacts WHERE id = ?", artID).
		Scan(&aType, &aPayload); err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if aType != testTypeScenario {
		t.Errorf("artifact.type = %q, want scenario", aType)
	}
	if aPayload != `{"id":"s1","kind":"behavior","check_cmd":"go test","given":"g","when":"w","then":"t"}` {
		t.Errorf("artifact.payload = %q, want the inserted JSON verbatim", aPayload)
	}

	var keysJSON string
	if err = s.db.QueryRowContext(ctx,
		"SELECT keys_json FROM push_subscriptions WHERE endpoint = 'https://push.example/rt'").
		Scan(&keysJSON); err != nil {
		t.Fatalf("read push subscription: %v", err)
	}
	if keysJSON != `{"p256dh":"x"}` {
		t.Errorf(`push_subscriptions.keys_json = %q, want {"p256dh":"x"}`, keysJSON)
	}

	var logLevel string
	if err = s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'log_level'").
		Scan(&logLevel); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if logLevel != "info" {
		t.Errorf("settings.log_level = %q, want info", logLevel)
	}
}
