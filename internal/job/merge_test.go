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
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"zing/internal/gitfixture"
	"zing/internal/orchestrator"
	"zing/internal/runtime"
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

// ---- task 5: the merge unit itself ---------------------------------------

// mergeHelloConflict is the content main's own conflicting commit on
// hello.txt carries (mergeConflictOnMain): different from the ticket
// branch's own "hello, world\n" (fixtures/scripts/build/1/1.tree/hello.txt),
// so merging main into the ticket branch leaves a real "both added"
// conflict on the one file both sides touch.
const mergeHelloConflict = "hello, main\n"

// mergeHelloResolved is the fake merge agent's own resolution of that
// conflict (mergeAgentFS's own build/merge/1.tree/hello.txt sibling): both
// sides' text, with no conflict markers left.
const mergeHelloResolved = "hello, world\nhello, main\n"

// mergeAgentScript is the fake merge agent's own turn 1: an ok build
// document claiming it resolved hello.txt, the one path main's own
// conflicting commit and the ticket's own task 1 both touch.
const mergeAgentScript = `<zing job="build" outcome="ok">
  <claims>
    <files_changed>
      <path>hello.txt</path>
    </files_changed>
  </claims>
  <report>Kept both sides: the ticket's own greeting, then main's, one per line.</report>
  <notes></notes>
</zing>`

// mergeAgentFS is the fake runtime's own scripts tree for the merge job's
// first turn (runtime.Fake's own "<job>/<label>/<turn>.xml" key: job
// response.JobBuild "build", label mergeRunLabel "merge").
func mergeAgentFS() fstest.MapFS {
	return fstest.MapFS{
		"build/merge/1.xml":            &fstest.MapFile{Data: []byte(mergeAgentScript)},
		"build/merge/1.tree/hello.txt": &fstest.MapFile{Data: []byte(mergeHelloResolved)},
	}
}

// mergeConflictOnMain commits path (with content) on the project's own
// checkout -- still on the project's default branch, exactly where
// gitfixture.NewSigningRepo left it, since building's own worktree is a
// separate git-worktree directory -- and pushes that branch to origin, so
// a later FetchBase reads a base sha that conflicts with the ticket's own
// edit of the same path (overview design's own demo: "main gets a commit
// that edits the same line of a file the ticket branch edits and is
// pushed to the bare origin"). It returns the new commit's own sha.
func mergeConflictOnMain(t *testing.T, s *store.Store, ticket store.Ticket, path string, content []byte) string {
	t.Helper()
	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	if addErr := gitfixture.AddFile(t.Context(), proj.LocalPath, path, content); addErr != nil {
		t.Fatalf("gitfixture.AddFile: %v", addErr)
	}
	if out, pushErr := gitfixture.Git(t.Context(), proj.LocalPath, "push", "origin", pbFixtureDefaultBranch); pushErr != nil {
		t.Fatalf("git push origin %s: %v: %s", pbFixtureDefaultBranch, pushErr, out)
	}
	sha, err := gitfixture.Git(t.Context(), proj.LocalPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(sha))
}

// shipHasMergeLanded reports whether c.Messages carries a "base merge
// landed " marker.
func shipHasMergeLanded(c store.HandlerCommit) bool {
	for _, m := range c.Messages {
		if strings.HasPrefix(m.Body, "base merge landed ") {
			return true
		}
	}
	return false
}

// TestMergeResolvesConflictEndToEnd is the overview design's own demo: a
// real git fixture where main changes a file the ticket also changed, so
// the published pull request goes dirty. POLL writes the conflict notice
// and the request; the merge unit runs the fake merge agent, which
// resolves the one conflicting file; CHECK passes and Zing makes the
// signed merge commit and records it; a final POLL, with the pull
// request's own head still at the ticket's pre-merge commit, pushes the
// merged branch to origin.
func TestMergeResolvesConflictEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, remoteDir := shipTicketReady(t)
	baseSHA := mergeConflictOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(mergeAgentFS())

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // PUBLISH
	if err != nil {
		t.Fatalf("PUBLISH: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("PUBLISH escalated: %+v", commit.Escalation.Payload)
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	preMergeHead := shipHeadSHA(t, s, ticket)
	gh.prState = orchestrator.PRState{
		State: questionStateOpen, Draft: true, HeadSHA: preMergeHead,
		BaseRef: pbFixtureDefaultBranch, MergeableState: mergeableStateDirty,
	}

	landed := false
	for i := 0; i < 5 && !landed; i++ {
		ticket = pbGetTicket(t, s, ticket.ID)
		deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
		commit, err = (shipHandler{}).Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if commit.Escalation != nil {
			t.Fatalf("tick %d escalated: %+v", i, commit.Escalation.Payload)
		}
		pbApply(t, s, ticket, commit)
		landed = shipHasMergeLanded(commit)
	}
	if !landed {
		t.Fatal("base merge did not land within 5 ticks")
	}
	ticket = pbGetTicket(t, s, ticket.ID)

	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	mergeSHA := shipHeadSHA(t, s, ticket)

	parents, err := proj.Orch.CommitParents(t.Context(), wt, mergeSHA)
	if err != nil {
		t.Fatalf("CommitParents: %v", err)
	}
	t.Logf("merge sha %s, parents %v", mergeSHA, parents)
	if len(parents) != 2 || parents[0] != preMergeHead || parents[1] != baseSHA {
		t.Errorf("CommitParents(%s) = %v, want [%s %s]", mergeSHA, parents, preMergeHead, baseSHA)
	}

	signed, err := proj.Orch.SignedStatus(t.Context(), wt, mergeSHA)
	if err != nil {
		t.Fatalf("SignedStatus: %v", err)
	}
	if !signed {
		t.Error("SignedStatus(merge commit) = false, want true")
	}

	reports, err := s.BuildReports(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("BuildReports: %v", err)
	}
	foundMergeReport := false
	for _, r := range reports {
		if r.Report.CommitSHA != nil && *r.Report.CommitSHA == mergeSHA {
			foundMergeReport = true
		}
	}
	if !foundMergeReport {
		t.Errorf("BuildReports has no row with commit_sha %s", mergeSHA)
	}

	branchShas, err := proj.Orch.BranchCommits(t.Context(), wt)
	if err != nil {
		t.Fatalf("BranchCommits: %v", err)
	}
	if !slices.Equal(recordedShas(reports), branchShas) {
		t.Errorf("recordedShas(reports) = %v, want BranchCommits %v", recordedShas(reports), branchShas)
	}

	// A final POLL, with the pull request's own head still at the
	// ticket's pre-merge commit, pushes the merged branch.
	gh.prState.HeadSHA = preMergeHead
	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	commit, err = (shipHandler{}).Run(t.Context(), ticket, deps) // POLL: push
	if err != nil {
		t.Fatalf("final POLL: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("final POLL escalated: %+v", commit.Escalation.Payload)
	}
	pbApply(t, s, ticket, commit)

	out, err := gitfixture.Git(t.Context(), remoteDir, "rev-parse", "refs/heads/"+*ticket.Branch)
	if err != nil {
		t.Fatalf("git rev-parse refs/heads/%s in origin: %v", *ticket.Branch, err)
	}
	if got := strings.TrimSpace(string(out)); got != mergeSHA {
		t.Errorf("origin's %s = %s, want the merge sha %s", *ticket.Branch, got, mergeSHA)
	}
}
