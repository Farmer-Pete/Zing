package tracker_test

import (
	"context"
	"testing"

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

func TestFixture_CommentReturnsNil(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	if err := f.Comment(context.Background(), fixtureProject, "fake#1", "looks good"); err != nil {
		t.Errorf("Comment: %v, want nil", err)
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
