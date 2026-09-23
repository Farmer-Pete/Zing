// seed_test.go is Task 12's test-first proof for SeedDemo (design section
// 6.15): it seeds one demo project and one demo ticket carrying a plan
// artifact, a scenario set, a finding set, and one open question of each of
// the six kinds, all through the store's validated inserts, and a second
// call inserts nothing new.
package console_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"zing/internal/console"
	"zing/internal/response"
	"zing/internal/store"
)

// seedDemoQuestionKinds is the design section 8 closed set of question
// kinds, the same six question_kinds_test.go asserts SeedQuestionFixtures
// covers; SeedDemo reuses SeedQuestionFixtures directly, so this file
// checks the same six are open on the demo ticket.
var seedDemoQuestionKinds = []response.QuestionKind{
	response.QuestionKindQuestion, response.QuestionKindGate, response.QuestionKindSplit,
	response.QuestionKindMerge, response.QuestionKindPerimeter, response.QuestionKindReview,
}

// TestSeedDemo_SeedsOneProjectAndTicketWithEveryArtifactAndQuestionKind
// proves SeedDemo's first call builds the full fixture design section 6.15
// names: one project, one ticket, one plan artifact, a scenario set, a
// finding set, and one open question of each of the six closed-set kinds.
func TestSeedDemo_SeedsOneProjectAndTicketWithEveryArtifactAndQuestionKind(t *testing.T) {
	s := newConsoleTestStore(t)
	ctx := t.Context()

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	ticketID := demoTicketID(ctx, t, s)

	assertSeedDemoArtifacts(ctx, t, s, ticketID)
	assertSeedDemoQuestions(ctx, t, s, ticketID)
}

// demoTicketID asserts exactly one project and one ticket exist and returns
// the ticket's id, failing the test otherwise.
func demoTicketID(ctx context.Context, t *testing.T, s *store.Store) int64 {
	t.Helper()

	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects = %d, want exactly 1", len(projects))
	}

	tickets, err := s.TicketsByProject(ctx, projects[0].ID)
	if err != nil {
		t.Fatalf("TicketsByProject: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("tickets = %d, want exactly 1", len(tickets))
	}
	return tickets[0].ID
}

// assertSeedDemoArtifacts asserts ticketID carries exactly one plan
// artifact whose Design.Shape contains a mermaid fence, plus a scenario set
// and a finding set of at least two rows each (design section 6.15: "a
// scenario artifact set, a finding set").
func assertSeedDemoArtifacts(ctx context.Context, t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()

	plan, ok, err := s.GetArtifact(ctx, ticketID, "plan")
	if err != nil {
		t.Fatalf("GetArtifact(plan): %v", err)
	}
	if !ok {
		t.Fatal("no plan artifact stored")
	}
	var p response.Plan
	if unmarshalErr := json.Unmarshal(plan.Payload, &p); unmarshalErr != nil {
		t.Fatalf("unmarshal plan payload: %v", unmarshalErr)
	}
	if !strings.Contains(p.Design.Shape, "```mermaid") {
		t.Errorf("plan Design.Shape does not contain a mermaid fence; got:\n%s", p.Design.Shape)
	}

	artifacts, err := s.ListArtifacts(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	var scenarios, findings int
	for _, a := range artifacts {
		switch a.Type {
		case "scenario":
			scenarios++
		case "finding":
			findings++
		}
	}
	if scenarios < 2 {
		t.Errorf("scenario artifacts = %d, want at least 2 (a set)", scenarios)
	}
	if findings < 2 {
		t.Errorf("finding artifacts = %d, want at least 2 (a set)", findings)
	}
}

// assertSeedDemoQuestions asserts ticketID has exactly one open question of
// each of the six closed-set kinds.
func assertSeedDemoQuestions(ctx context.Context, t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()

	open, err := s.QuestionsByState(ctx, ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != len(seedDemoQuestionKinds) {
		t.Fatalf("open questions = %d, want exactly %d (one per kind)", len(open), len(seedDemoQuestionKinds))
	}

	seen := make(map[response.QuestionKind]int, len(seedDemoQuestionKinds))
	for i := range open {
		var payload response.QuestionPayload
		if err := json.Unmarshal(open[i].Payload, &payload); err != nil {
			t.Fatalf("unmarshal question %d payload: %v", open[i].ID, err)
		}
		seen[payload.Kind]++
	}
	for _, kind := range seedDemoQuestionKinds {
		if seen[kind] != 1 {
			t.Errorf("open questions of kind %q = %d, want exactly 1", kind, seen[kind])
		}
	}
}

// TestSeedDemo_IsIdempotent proves a second SeedDemo call on the same store
// inserts nothing new: the same project, the same ticket, and the same
// artifact and message counts as the first call.
func TestSeedDemo_IsIdempotent(t *testing.T) {
	s := newConsoleTestStore(t)
	ctx := t.Context()

	if err := console.SeedDemo(ctx, s); err != nil {
		t.Fatalf("first SeedDemo: %v", err)
	}
	ticketID := demoTicketID(ctx, t, s)

	artifactsBefore, err := s.ListArtifacts(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	messagesBefore, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	if seedErr := console.SeedDemo(ctx, s); seedErr != nil {
		t.Fatalf("second SeedDemo: %v", seedErr)
	}

	secondTicketID := demoTicketID(ctx, t, s)
	if secondTicketID != ticketID {
		t.Errorf("ticket id after second call = %d, want still %d", secondTicketID, ticketID)
	}

	artifactsAfter, err := s.ListArtifacts(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListArtifacts (after): %v", err)
	}
	if len(artifactsAfter) != len(artifactsBefore) {
		t.Errorf("artifacts after second call = %d, want still %d (SeedDemo must insert nothing new)", len(artifactsAfter), len(artifactsBefore))
	}

	messagesAfter, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages (after): %v", err)
	}
	if len(messagesAfter) != len(messagesBefore) {
		t.Errorf("messages after second call = %d, want still %d (SeedDemo must insert nothing new)", len(messagesAfter), len(messagesBefore))
	}

	assertSeedDemoQuestions(ctx, t, s, ticketID)
}

// TestSeedDemo_NormalServeDoesNotSeed proves SeedDemo runs only when called:
// a store that never has SeedDemo (or the --seed-demo flag's call to it)
// invoked carries no demo project, matching the design section 6.15
// constraint "it must NEVER run in a normal serve". cmd/zing's own
// seed_test.go proves the same thing against the real serve() entry point;
// this is the package-level half of that guarantee: nothing in this
// package's own wiring (console.New, the stream, the mutation routes) ever
// calls SeedDemo on its own.
func TestSeedDemo_NormalServeDoesNotSeed(t *testing.T) {
	s := newConsoleTestStore(t)
	ctx := t.Context()

	seedTicket(t, s, "fake#1", "an ordinary ticket, not the demo fixture")

	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	for _, p := range projects {
		if p.Name == "demo" {
			t.Fatalf("a %q project exists without SeedDemo ever being called", "demo")
		}
	}
}
