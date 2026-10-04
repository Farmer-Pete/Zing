package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// seedRunForEvidence seeds a session on ticket 1 and one bare run under it,
// returning the run's id.
func seedRunForEvidence(t *testing.T, s *Store) int64 {
	t.Helper()
	ctx := t.Context()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (ticket_id, job, runtime) VALUES (1, 'planning', 'claude')`)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	sessionID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	runRes, err := s.db.ExecContext(ctx, `INSERT INTO runs (session_id, turn) VALUES (?, 0)`, sessionID)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	runID, err := runRes.LastInsertId()
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}

// TestRunEvidence_RecordAndReadBack proves RecordRunEvidence writes all
// three evidence columns, that RunEvidenceByID reads them back along with
// the seeded ticket id, and that RunEvidenceForTicket returns exactly the
// seeded run ids. A nil field round-trips as nil, not an empty string.
func TestRunEvidence_RecordAndReadBack(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	seedProjectAndTicket(t, s)

	run1 := seedRunForEvidence(t, s)
	run2 := seedRunForEvidence(t, s)

	full := RunEvidence{
		FinalMessage:   new("the agent said this"),
		StderrPath:     new("/data/runs/run-1-stderr.log"),
		TranscriptPath: new("/home/u/.claude/projects/-a-b/s1.jsonl"),
	}
	if recErr := s.RecordRunEvidence(ctx, run1, full); recErr != nil {
		t.Fatalf("RecordRunEvidence(run1, full): %v", recErr)
	}

	partial := RunEvidence{StderrPath: new("/data/runs/run-2-stderr.log")}
	if recErr := s.RecordRunEvidence(ctx, run2, partial); recErr != nil {
		t.Fatalf("RecordRunEvidence(run2, partial): %v", recErr)
	}

	ticketID, ev1, err := s.RunEvidenceByID(ctx, run1)
	if err != nil {
		t.Fatalf("RunEvidenceByID(run1): %v", err)
	}
	if ticketID != 1 {
		t.Errorf("RunEvidenceByID(run1) ticketID = %d, want 1", ticketID)
	}
	if ev1.FinalMessage == nil || *ev1.FinalMessage != "the agent said this" {
		t.Errorf("RunEvidenceByID(run1).FinalMessage = %v, want %q", ev1.FinalMessage, "the agent said this")
	}
	if ev1.StderrPath == nil || *ev1.StderrPath != "/data/runs/run-1-stderr.log" {
		t.Errorf("RunEvidenceByID(run1).StderrPath = %v, want %q", ev1.StderrPath, "/data/runs/run-1-stderr.log")
	}
	if ev1.TranscriptPath == nil || *ev1.TranscriptPath != "/home/u/.claude/projects/-a-b/s1.jsonl" {
		t.Errorf("RunEvidenceByID(run1).TranscriptPath = %v, want %q", ev1.TranscriptPath, "/home/u/.claude/projects/-a-b/s1.jsonl")
	}

	_, ev2, err := s.RunEvidenceByID(ctx, run2)
	if err != nil {
		t.Fatalf("RunEvidenceByID(run2): %v", err)
	}
	if ev2.FinalMessage != nil {
		t.Errorf("RunEvidenceByID(run2).FinalMessage = %v, want nil", ev2.FinalMessage)
	}
	if ev2.StderrPath == nil || *ev2.StderrPath != "/data/runs/run-2-stderr.log" {
		t.Errorf("RunEvidenceByID(run2).StderrPath = %v, want %q", ev2.StderrPath, "/data/runs/run-2-stderr.log")
	}
	if ev2.TranscriptPath != nil {
		t.Errorf("RunEvidenceByID(run2).TranscriptPath = %v, want nil", ev2.TranscriptPath)
	}

	byTicket, err := s.RunEvidenceForTicket(ctx, 1)
	if err != nil {
		t.Fatalf("RunEvidenceForTicket: %v", err)
	}
	if len(byTicket) != 2 {
		t.Fatalf("RunEvidenceForTicket = %+v, want exactly 2 runs", byTicket)
	}
	if _, ok := byTicket[run1]; !ok {
		t.Errorf("RunEvidenceForTicket missing run %d", run1)
	}
	if _, ok := byTicket[run2]; !ok {
		t.Errorf("RunEvidenceForTicket missing run %d", run2)
	}
}

// TestRunEvidence_UnknownRun proves both RecordRunEvidence and
// RunEvidenceByID report a wrapped sql.ErrNoRows for a run id that does not
// exist, and that RunEvidenceByID's ticketID is 0 in that case.
func TestRunEvidence_UnknownRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(ctx, dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	const unknownRunID = 99999

	if recErr := s.RecordRunEvidence(ctx, unknownRunID, RunEvidence{FinalMessage: new("x")}); !errors.Is(recErr, sql.ErrNoRows) {
		t.Errorf("RecordRunEvidence(unknown run) = %v, want it to wrap sql.ErrNoRows", recErr)
	}

	ticketID, _, err := s.RunEvidenceByID(ctx, unknownRunID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("RunEvidenceByID(unknown run) = %v, want it to wrap sql.ErrNoRows", err)
	}
	if ticketID != 0 {
		t.Errorf("RunEvidenceByID(unknown run) ticketID = %d, want 0", ticketID)
	}
}

// TestStore_DirIsDatabaseDirectory proves Store.Dir() returns the directory
// holding the database file passed to Open, the directory handleRunFile
// resolves stderr paths against.
func TestStore_DirIsDatabaseDirectory(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tmp := t.TempDir()
	s, err := Open(ctx, filepath.Join(tmp, "zing.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got := s.Dir(); got != tmp {
		t.Errorf("Store.Dir() = %q, want %q", got, tmp)
	}
}
