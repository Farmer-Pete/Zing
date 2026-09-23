package tracker_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"zing/fixtures"
	"zing/internal/tracker"
)

const fixtureProject = "zing"

func newFixture(t *testing.T) *tracker.Fixture {
	t.Helper()
	f, err := tracker.NewFixture(fixtures.FS, "tickets.toml")
	if err != nil {
		t.Fatalf("NewFixture: %v", err)
	}
	return f
}

func TestFixture_IntakeReturnsTheFixtureTicket(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	got, err := f.Intake(context.Background(), fixtureProject, tracker.IntakeRule{})
	if err != nil {
		t.Fatalf("Intake: %v", err)
	}

	want := tracker.Ticket{
		Ref:   "fake#1",
		Title: "Add a hello endpoint",
		Body:  "Serve GET /hello with a plain-text greeting.",
	}
	if len(got) != 1 {
		t.Fatalf("Intake returned %d tickets, want 1: %+v", len(got), got)
	}
	if got[0] != want {
		t.Errorf("Intake ticket = %+v, want %+v", got[0], want)
	}
}

func TestFixture_IntakeIsStableAcrossReads(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	first, err := f.Intake(context.Background(), fixtureProject, tracker.IntakeRule{})
	if err != nil {
		t.Fatalf("Intake (first): %v", err)
	}
	second, err := f.Intake(context.Background(), fixtureProject, tracker.IntakeRule{})
	if err != nil {
		t.Fatalf("Intake (second): %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("first read returned %d tickets, second returned %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("ticket %d changed between reads: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func TestFixture_IntakeReturnsNoneForAnUnmatchedProject(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	got, err := f.Intake(context.Background(), "not-zing", tracker.IntakeRule{})
	if err != nil {
		t.Fatalf("Intake: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Intake for an unmatched project returned %d tickets, want 0", len(got))
	}
}

func TestFixture_FetchFindsAFiledTicketByRef(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	got, err := f.Fetch(context.Background(), fixtureProject, "fake#1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Ref != "fake#1" {
		t.Errorf("Fetch ref = %q, want fake#1", got.Ref)
	}

	if _, err := f.Fetch(context.Background(), fixtureProject, "fake#999"); err == nil {
		t.Error("Fetch of an unknown ref returned nil error, want one")
	}
}

func TestFixture_FileTicketAppendsAndGeneratesARef(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	ref, err := f.FileTicket(context.Background(), fixtureProject, tracker.NewTicket{
		Title: "A second ticket",
		Body:  "Filed in memory.",
	})
	if err != nil {
		t.Fatalf("FileTicket: %v", err)
	}
	if ref != "fake#2" {
		t.Errorf("FileTicket ref = %q, want fake#2", ref)
	}

	got, err := f.Intake(context.Background(), fixtureProject, tracker.IntakeRule{})
	if err != nil {
		t.Fatalf("Intake: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Intake after FileTicket returned %d tickets, want 2: %+v", len(got), got)
	}
	if got[1].Ref != ref || got[1].Title != "A second ticket" {
		t.Errorf("Intake did not include the filed ticket: %+v", got[1])
	}
}

// nonSequentialTicketsTOML loads two tickets whose refs are not
// sequential from 1: "fake#1" and "fake#9". Only two entries exist, so
// seeding nextRef from len(tickets)+1 would mint "fake#3" next, which never
// collides with anything loaded here -- the point of this fixture is the
// next test, which proves the generated ref is instead based on the ref
// values themselves (max 9, so the next must be "fake#10").
const nonSequentialTicketsTOML = `
project = "zing"

[[ticket]]
ref = "fake#1"
title = "first"
body = "first body"

[[ticket]]
ref = "fake#9"
title = "ninth"
body = "ninth body"
`

// TestFixture_FileTicketAvoidsCollidingWithANonSequentialLoadedRef proves
// FileTicket seeds its generated ref from the highest numeric ref already
// loaded, not from the loaded ticket count (design section "Tracker fixture"
// fix 14): loading two tickets whose refs are "fake#1" and "fake#9" must
// still generate "fake#10" next, never "fake#3" (len+1), which would be a
// fresh, uncolliding ref only by coincidence here and could collide with a
// real fixture whose refs carry gaps.
func TestFixture_FileTicketAvoidsCollidingWithANonSequentialLoadedRef(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{"tickets.toml": {Data: []byte(nonSequentialTicketsTOML)}}
	f, err := tracker.NewFixture(fsys, "tickets.toml")
	if err != nil {
		t.Fatalf("NewFixture: %v", err)
	}

	ref, err := f.FileTicket(context.Background(), "zing", tracker.NewTicket{Title: "new", Body: "new body"})
	if err != nil {
		t.Fatalf("FileTicket: %v", err)
	}
	if ref != "fake#10" {
		t.Errorf("FileTicket ref = %q, want fake#10 (one past the highest loaded ref, fake#9)", ref)
	}
}

// TestFixture_CommentReturnsNil proves Comment succeeds and, since a
// comment body can carry sensitive text (the repo rule is never log a
// secret), that its log line carries the body's length, never the body
// itself. It swaps the process-wide slog default logger to capture that
// line, so it does not run in parallel with the other subtests here, none
// of which touch slog.
func TestFixture_CommentReturnsNil(t *testing.T) {
	f := newFixture(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	const secretBody = "the deploy key is sk-super-secret-value"
	if err := f.Comment(context.Background(), fixtureProject, "fake#1", secretBody); err != nil {
		t.Errorf("Comment: %v, want nil", err)
	}

	logged := buf.String()
	if strings.Contains(logged, secretBody) {
		t.Errorf("Comment logged the raw body; want only its length, got log line: %s", logged)
	}
	if !strings.Contains(logged, "body_len") {
		t.Errorf("Comment's log line is missing body_len; got: %s", logged)
	}
}

func TestFixture_CollaboratorsReturnsAFixedName(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	got, err := f.Collaborators(context.Background(), fixtureProject)
	if err != nil {
		t.Fatalf("Collaborators: %v", err)
	}
	if len(got) != 1 || got[0] == "" {
		t.Errorf("Collaborators = %+v, want one fixed name", got)
	}
}
