// merge_test.go tests merge.go. Task 4 covers POLL's own dirty row
// (pollConflict): mergePublished publishes a ticket exactly as shipPublished
// does, then sets gh.prState to the published PR's own open, draft state at
// its current head on the project's default branch, with MergeableState
// "dirty" -- the state every test below starts from, overriding BaseRef or
// MergeableState for its own case.
package job

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"zing/internal/orchestrator"
	"zing/internal/store"
)

func mergePublished(t *testing.T) (s *store.Store, ticket store.Ticket, gh *shipGitHub, tr *shipTracker) {
	t.Helper()
	s, ticket, gh, tr = shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	gh.prState = orchestrator.PRState{
		State: questionStateOpen, Draft: true, HeadSHA: local,
		BaseRef: pbFixtureDefaultBranch, MergeableState: mergeableStateDirty,
	}
	return s, ticket, gh, tr
}

// mergeBaseRefSHA reads refs/zing/base/<branch> directly out of the
// ticket's own project checkout, the ref FetchBase (internal/orchestrator)
// advances, so a test can prove a request's own BaseSHA is exactly what
// FetchBase read.
func mergeBaseRefSHA(t *testing.T, s *store.Store, ticket store.Ticket, branch string) string {
	t.Helper()
	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "refs/zing/base/"+branch)
	cmd.Dir = proj.LocalPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse refs/zing/base/%s: %v", branch, err)
	}
	return strings.TrimSpace(string(out))
}

// TestPollDirtyWritesMergeRequest proves POLL's own dirty row (overview
// design, "Detect it"): a dirty pull request whose base is the project's
// own default branch writes the conflict notice and the request marker
// together, with ClearPoll set and no idle poll scheduled.
func TestPollDirtyWritesMergeRequest(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := mergePublished(t)

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("poll escalated: %+v", commit.Escalation.Payload)
	}
	if !commit.ClearPoll {
		t.Error("commit.ClearPoll = false, want true")
	}
	if commit.Poll != nil {
		t.Errorf("commit.Poll = %+v, want nil", commit.Poll)
	}
	if len(commit.Messages) != 2 {
		t.Fatalf("commit.Messages = %+v, want 2 messages", commit.Messages)
	}

	wantWhat := "PR #1 conflicts with " + pbFixtureDefaultBranch
	if commit.Messages[0].Body != wantWhat {
		t.Errorf("commit.Messages[0].Body = %q, want %q", commit.Messages[0].Body, wantWhat)
	}

	req, err := parseBaseMergeRequest(store.MessageRow{ID: 1, Body: commit.Messages[1].Body})
	if err != nil {
		t.Fatalf("parseBaseMergeRequest: %v", err)
	}
	if req.BaseBranch != pbFixtureDefaultBranch {
		t.Errorf("req.BaseBranch = %q, want %q", req.BaseBranch, pbFixtureDefaultBranch)
	}
	wantSHA := mergeBaseRefSHA(t, s, ticket, pbFixtureDefaultBranch)
	if req.BaseSHA != wantSHA {
		t.Errorf("req.BaseSHA = %s, want %s (refs/zing/base/%s)", req.BaseSHA, wantSHA, pbFixtureDefaultBranch)
	}

	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, ciWaitingPrefix) {
			t.Errorf("commit.Messages has a %q marker, want none", m.Body)
		}
	}
}

// TestPollDirtyAfterMaxLoopsEscalates proves jobs.merge.max_loops bounds
// POLL's own dirty row (overview design, "POLL opens at most
// jobs.merge.max_loops ... merge requests per ticket"): with two closed
// requests already on the ticket, a third dirty poll escalates instead of
// opening another one.
func TestPollDirtyAfterMaxLoopsEscalates(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := mergePublished(t)

	for _, sha := range []string{strings.Repeat("1", 40), strings.Repeat("2", 40)} {
		req := baseMergeRequest{BaseBranch: pbFixtureDefaultBranch, BaseSHA: sha}
		id, err := s.InsertMessage(t.Context(), store.Message{
			TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem, Body: req.body(),
		})
		if err != nil {
			t.Fatalf("InsertMessage(request): %v", err)
		}
		if _, err := s.InsertMessage(t.Context(), store.Message{
			TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("base merge closed %d", id),
		}); err != nil {
			t.Fatalf("InsertMessage(closed): %v", err)
		}
	}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want an escalation")
	}
	wantWhat := "PR #1 conflicts with " + pbFixtureDefaultBranch
	if commit.Escalation.Payload.What != wantWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, wantWhat)
	}
	wantWhy := fmt.Sprintf("Zing already merged %s into this branch 2 times and the pull request conflicts again; jobs.merge.max_loops is 2", pbFixtureDefaultBranch)
	if commit.Escalation.Payload.Why != wantWhy {
		t.Errorf("Why = %q, want %q", commit.Escalation.Payload.Why, wantWhy)
	}
	if len(commit.Messages) != 0 {
		t.Errorf("commit.Messages = %+v, want none", commit.Messages)
	}
}

// TestPollDirtyOtherBaseEscalates proves POLL's dirty row only ever merges
// the project's own default branch (overview design, nongoal "Merging a
// base branch other than the project's default branch ... escalates").
func TestPollDirtyOtherBaseEscalates(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := mergePublished(t)
	gh.prState.BaseRef = "release"

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want an escalation")
	}
	wantWhat := "PR #1 conflicts with release"
	if commit.Escalation.Payload.What != wantWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, wantWhat)
	}
	wantWhy := fmt.Sprintf("Zing merges only the project's default branch, %s, into a ticket branch", pbFixtureDefaultBranch)
	if commit.Escalation.Payload.Why != wantWhy {
		t.Errorf("Why = %q, want %q", commit.Escalation.Payload.Why, wantWhy)
	}
	if len(commit.Messages) != 0 {
		t.Errorf("commit.Messages = %+v, want none", commit.Messages)
	}
}

// TestPollUnknownMergeableStateIdles proves a non-"dirty" MergeableState
// never trips the dirty row: with a required check CI has not yet matched,
// POLL still takes its ordinary idle row.
func TestPollUnknownMergeableStateIdles(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	gh.prState = orchestrator.PRState{
		State: questionStateOpen, Draft: true, HeadSHA: local,
		BaseRef: pbFixtureDefaultBranch, MergeableState: "unknown",
	}
	gh.required = []orchestrator.RequiredCheck{{Context: "ci"}}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("poll escalated: %+v", commit.Escalation.Payload)
	}
	if commit.Poll == nil {
		t.Fatal("commit.Poll is nil, want the backoff commit")
	}

	found := false
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, ciWaitingPrefix) {
			found = true
		}
		if strings.HasPrefix(m.Body, baseMergePrefix) || strings.Contains(m.Body, "conflicts with") {
			t.Errorf("unexpected base merge message: %q", m.Body)
		}
	}
	if !found {
		t.Errorf("commit.Messages = %+v, want a %q marker", commit.Messages, ciWaitingPrefix)
	}
}

// TestOpenBaseMergeFindsTheOpenRequest proves openBaseMerge reads the one
// open request a ticket's own "base merge " markers carry, for
// driveOpenMerge (task 5) and retryMerge (task 7).
func TestOpenBaseMergeFindsTheOpenRequest(t *testing.T) {
	t.Parallel()
	s := newFixTestStore(t)
	ticket := pbSeedTicketInState(t, s, stateShipping)

	want := baseMergeRequest{BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("3", 40)}
	if _, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem, Body: want.body(),
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	got, open, err := openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge: %v", err)
	}
	if !open {
		t.Fatal("open = false, want true")
	}
	if got.BaseBranch != want.BaseBranch || got.BaseSHA != want.BaseSHA {
		t.Errorf("openBaseMerge = %+v, want BaseBranch %q BaseSHA %q", got, want.BaseBranch, want.BaseSHA)
	}

	_, insertErr := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("base merge closed %d", got.MessageID),
	})
	if insertErr != nil {
		t.Fatalf("InsertMessage(closed): %v", insertErr)
	}
	_, open, err = openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge (after close): %v", err)
	}
	if open {
		t.Error("open = true after a closed marker, want false")
	}
}

// TestMergeEscalationSetsTriedAndClearPoll proves mergeTried and
// mergeEscalation's own shape: Tried names the request so a later owner
// retry can find its way back to it (baseMergeTriedID), and the commit
// always clears the poll schedule.
func TestMergeEscalationSetsTriedAndClearPoll(t *testing.T) {
	t.Parallel()
	req := baseMergeRequest{MessageID: 42, BaseBranch: pbFixtureDefaultBranch, BaseSHA: strings.Repeat("4", 40)}

	if got, want := mergeTried(req, ""), "base merge 42"; got != want {
		t.Errorf("mergeTried(no detail) = %q, want %q", got, want)
	}
	if got, want := mergeTried(req, "detail"), "base merge 42\ndetail"; got != want {
		t.Errorf("mergeTried(detail) = %q, want %q", got, want)
	}

	c := mergeEscalation(store.Ticket{ID: 1}, Deps{}, req, "what", "why", "detail")
	if !c.ClearPoll {
		t.Error("c.ClearPoll = false, want true")
	}
	if c.Escalation == nil {
		t.Fatal("c.Escalation is nil")
	}
	if c.Escalation.Payload.What != "what" || c.Escalation.Payload.Why != "why" {
		t.Errorf("Escalation.Payload = %+v, want What %q Why %q", c.Escalation.Payload, "what", "why")
	}
	if want := "base merge 42\ndetail"; c.Escalation.Payload.Tried != want {
		t.Errorf("Escalation.Payload.Tried = %q, want %q", c.Escalation.Payload.Tried, want)
	}
	id, ok := baseMergeTriedID(c.Escalation.Payload.Tried)
	if !ok || id != req.MessageID {
		t.Errorf("baseMergeTriedID(Tried) = (%d, %v), want (%d, true)", id, ok, req.MessageID)
	}
}
