// merge_test.go tests merge.go. Task 4 covers POLL's own dirty row
// (pollConflict): mergePublished publishes a ticket exactly as shipPublished
// does, then sets gh.prState to the published PR's own open, draft state at
// its current head on the project's default branch, with MergeableState
// "dirty" -- the state every test below starts from, overriding BaseRef or
// MergeableState for its own case.
package job

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"zing/internal/gitfixture"
	"zing/internal/orchestrator"
	"zing/internal/response"
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
	s, ticket, gh, tr := mergePublished(t)
	gh.prState.MergeableState = "unknown"
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

// TestPollDirtyFetchBaseFailureEscalates proves pollConflict's own
// FetchBase-failure branch (overview design "Detect it"): with the
// project's local checkout missing the default branch ref entirely -- so
// neither the real fetch (no origin in this fixture) nor FetchBase's own
// local-seed fallback can produce a sha -- it escalates instead of writing
// a request with no base sha to merge. This calls pollConflict directly,
// since deleting the local default branch ref also breaks BranchCommits,
// which the generic poll() flow this ticket never reaches calls for
// reasons of its own.
func TestPollDirtyFetchBaseFailureEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(fstest.MapFS{})

	ticket = mergeDirtyAfterPublish(t, s, ticket, rt, gh, tr)

	storeProj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	// Clear any base sha an earlier planning/build/judge tick already
	// cached, then remove the local default branch ref too: with both
	// gone, neither FetchBase's own real fetch (no origin remote in this
	// fixture) nor its local-seed fallback (which reads this very ref)
	// can produce a sha.
	if out, delErr := gitfixture.Git(t.Context(), storeProj.LocalPath, "update-ref", "-d", "refs/zing/base/"+pbFixtureDefaultBranch); delErr != nil {
		t.Fatalf("git update-ref -d refs/zing/base: %v: %s", delErr, out)
	}
	if out, delErr := gitfixture.Git(t.Context(), storeProj.LocalPath, "update-ref", "-d", "refs/heads/"+pbFixtureDefaultBranch); delErr != nil {
		t.Fatalf("git update-ref -d refs/heads: %v: %s", delErr, out)
	}

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	pr := orchestrator.PRState{
		State: questionStateOpen, Draft: true, HeadSHA: shipHeadSHA(t, s, ticket),
		BaseRef: pbFixtureDefaultBranch, MergeableState: mergeableStateDirty,
	}

	commit, err := (shipHandler{}).pollConflict(t.Context(), ticket, deps, proj, wt, pr, 1)
	if err != nil {
		t.Fatalf("pollConflict: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want an escalation")
	}
	wantWhat := "PR #1 conflicts with " + pbFixtureDefaultBranch
	if commit.Escalation.Payload.What != wantWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, wantWhat)
	}
	if commit.Escalation.Payload.Why != baseNotFetchedWhy {
		t.Errorf("Why = %q, want %q", commit.Escalation.Payload.Why, baseNotFetchedWhy)
	}
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, "base merge requested after run ") {
			t.Errorf("commit.Messages = %+v, want no request marker", commit.Messages)
		}
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
// hello.txt carries (mergeCommitOnMain): different from the ticket
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

// mergeTurn1XMLKey and mergeTurn1HelloTreeKey are the fake runtime's own
// scripts-tree keys for the merge job's first turn (runtime.Fake's own
// "<job>/<label>/<turn>.xml" and ".tree/<path>" keys: job
// response.JobBuild "build", label mergeRunLabel "merge"), shared by every
// fixture below whose own turn 1 claims hello.txt fixed.
const (
	mergeTurn1XMLKey       = "build/merge/1.xml"
	mergeTurn1HelloTreeKey = "build/merge/1.tree/hello.txt"
)

// mergeAgentFS is the fake runtime's own scripts tree for the merge job's
// first turn.
func mergeAgentFS() fstest.MapFS {
	return fstest.MapFS{
		mergeTurn1XMLKey:       &fstest.MapFile{Data: []byte(mergeAgentScript)},
		mergeTurn1HelloTreeKey: &fstest.MapFile{Data: []byte(mergeHelloResolved)},
	}
}

// mergeCommitOnMain commits path (with content) on the project's own
// checkout -- still on the project's default branch, exactly where
// gitfixture.NewSigningRepo left it, since building's own worktree is a
// separate git-worktree directory -- and pushes that branch to origin, so
// a later FetchBase reads a base sha that conflicts with the ticket's own
// edit of the same path (overview design's own demo: "main gets a commit
// that edits the same line of a file the ticket branch edits and is
// pushed to the bare origin"). It returns the new commit's own sha.
func mergeCommitOnMain(t *testing.T, s *store.Store, ticket store.Ticket, path string, content []byte) string {
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
	baseSHA := mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(mergeAgentFS())

	ticket = mergeDirtyAfterPublish(t, s, ticket, rt, gh, tr)
	preMergeHead := gh.prState.HeadSHA

	var deps Deps
	landed := false
	var last store.HandlerCommit
	for i := 0; i < 4 && !landed; i++ {
		deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
		ticket, last = mergeRunTick(t, s, deps, ticket, fmt.Sprintf("tick %d", i))
		landed = shipHasMergeLanded(last)
	}
	if !landed {
		t.Fatal("base merge did not land within 4 ticks")
	}

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
	ticket, _ = mergeRunTick(t, s, deps, ticket, "final POLL: push")

	out, err := gitfixture.Git(t.Context(), remoteDir, "rev-parse", "refs/heads/"+*ticket.Branch)
	if err != nil {
		t.Fatalf("git rev-parse refs/heads/%s in origin: %v", *ticket.Branch, err)
	}
	if got := strings.TrimSpace(string(out)); got != mergeSHA {
		t.Errorf("origin's %s = %s, want the merge sha %s", *ticket.Branch, got, mergeSHA)
	}
}

// ---- task 6: mergeCheck's own no-run branch, the outside-the-merge read,
// and the check_loops/max_resumes gates --------------------------------

// mergeOtherTxt is a path main's own commit touches that the ticket branch
// never changes: merging it leaves no conflict at all.
const mergeOtherTxt = "other.txt"

// mergeDirtyAfterPublish runs PUBLISH on a shipTicketReady ticket, then
// reports the published pull request dirty against base, the shape every
// test below starts the merge unit from.
func mergeDirtyAfterPublish(t *testing.T, s *store.Store, ticket store.Ticket, rt runtime.Runtime, gh *shipGitHub, tr *shipTracker) store.Ticket {
	t.Helper()
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

	gh.prState = orchestrator.PRState{
		State: questionStateOpen, Draft: true, HeadSHA: shipHeadSHA(t, s, ticket),
		BaseRef: pbFixtureDefaultBranch, MergeableState: mergeableStateDirty,
	}
	return ticket
}

// mergeRunTick runs one shipHandler.Run tick, fails the test if it
// escalates, applies the commit, and returns the refreshed ticket plus
// the commit itself.
func mergeRunTick(t *testing.T, s *store.Store, deps Deps, ticket store.Ticket, label string) (store.Ticket, store.HandlerCommit) {
	t.Helper()
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if commit.Escalation != nil {
		t.Fatalf("%s escalated: %+v", label, commit.Escalation.Payload)
	}
	pbApply(t, s, ticket, commit)
	return pbGetTicket(t, s, ticket.ID), commit
}

// TestMergeCleanSkipsAgent proves mergeCheck's own no-run branch (task 6):
// a clean merge -- main touches a path the ticket never changed, so
// StartBaseMerge finds no conflict -- runs CHECK with no agent turn at
// all, and a green result lands the merge with the synthesized report
// (landMerge's own rid-nil case, wired up in task 5).
func TestMergeCleanSkipsAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, mergeOtherTxt, []byte("main only\n"))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(fstest.MapFS{})

	ticket = mergeDirtyAfterPublish(t, s, ticket, rt, gh, tr)

	landed := false
	var last store.HandlerCommit
	for i := 0; i < 4 && !landed; i++ {
		deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
		ticket, last = mergeRunTick(t, s, deps, ticket, fmt.Sprintf("tick %d", i))
		landed = shipHasMergeLanded(last)
	}
	if !landed {
		t.Fatal("base merge did not land within 4 ticks")
	}

	sessions, err := s.SessionsForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	for _, sess := range sessions {
		if sess.Job == jobMergeName {
			t.Errorf("sessions = %+v, want no %q session", sessions, jobMergeName)
		}
	}

	reports, err := s.BuildReports(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("BuildReports: %v", err)
	}
	found := false
	for _, r := range reports {
		if r.RunID == 0 && len(r.Report.FilesChanged) == 0 && r.Report.CommitSHA != nil {
			found = true
		}
	}
	if !found {
		t.Errorf("BuildReports = %+v, want a synthesized row (RunID 0, empty files_changed, CommitSHA set)", reports)
	}
}

// TestMergeStartFailedEscalates proves driveMerge's own "start_merge_failed"
// row (overview design "One merge tick"): an uncommitted edit to the very
// file the base side also changed makes StartBaseMerge's own "git merge"
// refuse outright, so the tick escalates mergeFailedWhat with Tried naming
// the open request, and lands nothing.
func TestMergeStartFailedEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(fstest.MapFS{})

	ticket = mergeDirtyAfterPublish(t, s, ticket, rt, gh, tr)

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	req, open, err := openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge: %v", err)
	}
	if !open {
		t.Fatal("openBaseMerge: no open request after POLL")
	}

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if writeErr := os.WriteFile(filepath.Join(wt.Dir(), "hello.txt"), []byte("uncommitted local edit\n"), 0o644); writeErr != nil {
		t.Fatalf("WriteFile: %v", writeErr)
	}

	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // StartBaseMerge refuses: dirty tree
	if err != nil {
		t.Fatalf("merge tick: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want a start-merge-failed escalation")
	}
	if commit.Escalation.Payload.What != mergeFailedWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, mergeFailedWhat)
	}
	wantTriedPrefix := fmt.Sprintf("base merge %d\n", req.MessageID)
	if !strings.HasPrefix(commit.Escalation.Payload.Tried, wantTriedPrefix) {
		t.Errorf("Tried = %q, want prefix %q", commit.Escalation.Payload.Tried, wantTriedPrefix)
	}
	if shipHasMergeLanded(commit) {
		t.Error("commit landed the merge, want a start-merge-failed escalation instead")
	}
}

// TestMergeLandSigningFailureEscalates proves landMerge's own CommitMerge
// failure path (overview design "One merge tick"): when CHECK passes but
// the commit itself cannot be signed, the tick escalates
// commitSigningFailedWhat with Tried naming the open request, and lands
// nothing.
func TestMergeLandSigningFailureEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, mergeOtherTxt, []byte("main only\n"))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(fstest.MapFS{})

	ticket = mergeDirtyAfterPublish(t, s, ticket, rt, gh, tr)

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	req, open, err := openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge: %v", err)
	}
	if !open {
		t.Fatal("openBaseMerge: no open request after POLL")
	}

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	badKey := filepath.Join(t.TempDir(), "no-such-signing-key")
	if out, cfgErr := gitfixture.Git(t.Context(), wt.Dir(), "config", "user.signingKey", badKey); cfgErr != nil {
		t.Fatalf("git config user.signingKey: %v: %s", cfgErr, out)
	}

	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // StartBaseMerge + CHECK + LAND: signing fails
	if err != nil {
		t.Fatalf("merge tick: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want a signing-failure escalation")
	}
	if commit.Escalation.Payload.What != commitSigningFailedWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, commitSigningFailedWhat)
	}
	wantTriedPrefix := fmt.Sprintf("base merge %d\n", req.MessageID)
	if !strings.HasPrefix(commit.Escalation.Payload.Tried, wantTriedPrefix) {
		t.Errorf("Tried = %q, want prefix %q", commit.Escalation.Payload.Tried, wantTriedPrefix)
	}
	if shipHasMergeLanded(commit) {
		t.Error("commit landed the merge, want a signing-failure escalation instead")
	}
}

// TestMergeLandAfterRetriedSigningFailureCarriesReport proves
// priorMergeRunID (merge.go): a signing failure escalates after the agent
// already resolved a real conflict; once the owner retries, the reopened
// request's own StartBaseMerge finds the resolution already staged (no
// unmerged paths) and its own SessionAfter finds no session past its fresh
// watermark, so landMerge must not synthesize an empty "no conflicts"
// report -- it carries over the closed request's own agent-authored report
// instead, files changed and all.
func TestMergeLandAfterRetriedSigningFailureCarriesReport(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(mergeAgentFS())

	ticket = mergeDirtyAfterPublish(t, s, ticket, rt, gh, tr)

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	req1, open, err := openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge: %v", err)
	}
	if !open {
		t.Fatal("openBaseMerge: no open request after POLL")
	}

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	var runCommit store.HandlerCommit
	ticket, runCommit = mergeRunTick(t, s, deps, ticket, "merge run") // StartBaseMerge + agent turn 1 (ok, resolves hello.txt)
	if shipHasMergeLanded(runCommit) {
		t.Fatal("the merge run tick already landed, want the agent turn only")
	}

	// Break signing before the next tick's own mergeCheck -> landMerge, so
	// the already-resolved tree is left mid-merge rather than committed.
	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	origKeyOut, err := gitfixture.Git(t.Context(), wt.Dir(), "config", "user.signingKey")
	if err != nil {
		t.Fatalf("git config user.signingKey (read): %v: %s", err, origKeyOut)
	}
	origKey := strings.TrimSpace(string(origKeyOut))
	badKey := filepath.Join(t.TempDir(), "no-such-signing-key")
	if out, cfgErr := gitfixture.Git(t.Context(), wt.Dir(), "config", "user.signingKey", badKey); cfgErr != nil {
		t.Fatalf("git config user.signingKey (break): %v: %s", cfgErr, out)
	}

	escCommit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // CHECK passes, CommitMerge fails signing
	if err != nil {
		t.Fatalf("land tick: %v", err)
	}
	if escCommit.Escalation == nil {
		t.Fatal("escCommit.Escalation is nil, want a signing-failure escalation")
	}
	if escCommit.Escalation.Payload.What != commitSigningFailedWhat {
		t.Errorf("What = %q, want %q", escCommit.Escalation.Payload.What, commitSigningFailedWhat)
	}
	pbApply(t, s, ticket, escCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	openQs, err := s.QuestionsByState(t.Context(), ticket.ID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState: %v", err)
	}
	if len(openQs) == 0 {
		t.Fatal("QuestionsByState(open) = [], want at least one open question")
	}
	qID := openQs[len(openQs)-1].ID
	optA := "a" // escalationChoiceRetry
	if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticket.ID, QuestionID: &qID, Option: &optA}); draftErr != nil {
		t.Fatalf("SaveDraft(option): %v", draftErr)
	}
	if _, sendErr := s.SendBatch(t.Context(), ticket.ID); sendErr != nil {
		t.Fatalf("SendBatch: %v", sendErr)
	}

	// Fix signing before the retry reaches landMerge again.
	if out, cfgErr := gitfixture.Git(t.Context(), wt.Dir(), "config", "user.signingKey", origKey); cfgErr != nil {
		t.Fatalf("git config user.signingKey (restore): %v: %s", cfgErr, out)
	}

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	var reopenCommit store.HandlerCommit
	ticket, reopenCommit = mergeRunTick(t, s, deps, ticket, "retry: close and reopen")
	closedBody := fmt.Sprintf("base merge closed %d", req1.MessageID)
	if !shipHasMessage(reopenCommit, closedBody) {
		t.Fatalf("reopenCommit.Messages = %+v, want %q", reopenCommit.Messages, closedBody)
	}

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	var landCommit store.HandlerCommit
	ticket, landCommit = mergeRunTick(t, s, deps, ticket, "land after reopen")
	if !shipHasMergeLanded(landCommit) {
		t.Fatalf("landCommit.Messages = %+v, want a %q marker", landCommit.Messages, "base merge landed ")
	}

	var landedSHA string
	for _, m := range landCommit.Messages {
		if strings.HasPrefix(m.Body, "base merge landed ") {
			fields := strings.Fields(m.Body)
			landedSHA = fields[len(fields)-1]
		}
	}
	if landedSHA == "" {
		t.Fatalf("landCommit.Messages = %+v, want a parsable %q marker", landCommit.Messages, "base merge landed ")
	}

	reports, err := s.BuildReports(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("BuildReports: %v", err)
	}
	found := false
	for _, r := range reports {
		if r.Report.CommitSHA != nil && *r.Report.CommitSHA == landedSHA {
			found = true
			if len(r.Report.FilesChanged) != 1 || r.Report.FilesChanged[0] != pbHelloTxt {
				t.Errorf("landed report FilesChanged = %v, want [hello.txt] (carried over, not synthesized)", r.Report.FilesChanged)
			}
		}
	}
	if !found {
		t.Errorf("BuildReports has no row with commit_sha %s", landedSHA)
	}
}

// mergeConflictMarkerStill is a hello.txt resolution that still carries
// git's own conflict marker lines: turn 1's own claimed fix for
// TestMergeCheckFailureResumes, so CHECK's own ConflictMarkerPaths read,
// not the test or lint command, is what catches the unfinished work.
const mergeConflictMarkerStill = "<<<<<<< HEAD\nhello, world\n=======\nhello, main\n>>>>>>> main\n"

// mergeAgentFSMarkersThenResolved is TestMergeCheckFailureResumes' own
// fake runtime script: turn 1 claims hello.txt fixed but leaves conflict
// markers behind, turn 2 writes the real resolution.
func mergeAgentFSMarkersThenResolved() fstest.MapFS {
	return fstest.MapFS{
		mergeTurn1XMLKey:               &fstest.MapFile{Data: []byte(mergeAgentScript)},
		mergeTurn1HelloTreeKey:         &fstest.MapFile{Data: []byte(mergeConflictMarkerStill)},
		"build/merge/2.xml":            &fstest.MapFile{Data: []byte(mergeAgentScript)},
		"build/merge/2.tree/hello.txt": &fstest.MapFile{Data: []byte(mergeHelloResolved)},
	}
}

// TestMergeCheckFailureResumes proves mergeCheck's own charged resume
// (task 6): a first turn that leaves conflict markers behind fails CHECK,
// which resumes the session with a check input naming the file, charged
// (Resumes becomes 1); the second turn resolves it for real and the next
// CHECK lands the merge.
func TestMergeCheckFailureResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rec := &recordingRuntime{inner: runtime.NewFake(mergeAgentFSMarkersThenResolved())}

	ticket = mergeDirtyAfterPublish(t, s, ticket, rec, gh, tr)

	deps := shipClaim(t, s, rec, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	deps = shipClaim(t, s, rec, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "run first") // runMergeFirst: turn 1

	deps = shipClaim(t, s, rec, ticket.ID, gh, tr)
	var checkOne store.HandlerCommit
	ticket, checkOne = mergeRunTick(t, s, deps, ticket, "check 1") // CHECK finds markers, resumes: turn 2
	if shipHasMergeLanded(checkOne) {
		t.Fatal("check 1 landed the merge, want a charged resume instead")
	}

	gotPrompt := rec.lastRequest(t).Prompt
	wantText := "conflict markers remain in: hello.txt"
	if !strings.Contains(gotPrompt, wantText) {
		t.Errorf("resume prompt = %q, want it to contain %q", gotPrompt, wantText)
	}

	sessions, err := s.SessionsForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	mergeResumes := -1
	for _, sess := range sessions {
		if sess.Job == jobMergeName {
			mergeResumes = sess.Resumes
		}
	}
	if mergeResumes != 1 {
		t.Errorf("merge session Resumes = %d, want 1", mergeResumes)
	}

	deps = shipClaim(t, s, rec, ticket.ID, gh, tr)
	var checkTwo store.HandlerCommit
	ticket, checkTwo = mergeRunTick(t, s, deps, ticket, "check 2") // CHECK on turn 2's resolution: lands
	if !shipHasMergeLanded(checkTwo) {
		t.Fatal("check 2 did not land the merge")
	}
	_ = ticket
}

// mergeNotesExtra is a path neither side of the merge touches, written by
// TestMergeOutsidePathFailsCheck's own turn 1 alongside its real
// resolution of hello.txt.
const mergeNotesExtra = "scratch notes, not part of the merge\n"

// mergeAgentFSExtraThenDeleted is TestMergeOutsidePathFailsCheck's own
// fake runtime script: turn 1 resolves hello.txt cleanly but also writes
// notes.txt; turn 2 deletes it.
func mergeAgentFSExtraThenDeleted() fstest.MapFS {
	return fstest.MapFS{
		mergeTurn1XMLKey:               &fstest.MapFile{Data: []byte(mergeAgentScript)},
		mergeTurn1HelloTreeKey:         &fstest.MapFile{Data: []byte(mergeHelloResolved)},
		"build/merge/1.tree/notes.txt": &fstest.MapFile{Data: []byte(mergeNotesExtra)},
		"build/merge/2.xml":            &fstest.MapFile{Data: []byte(mergeAgentScript)},
		"build/merge/2.delete":         &fstest.MapFile{Data: []byte("notes.txt\n")},
	}
}

// TestMergeOutsidePathFailsCheck proves mergeCheck's own outside-the-merge
// read (task 6): a turn that resolves every conflict but also leaves a
// path neither side of the merge touched fails CHECK, naming that path;
// once it is removed, the merge lands.
func TestMergeOutsidePathFailsCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rec := &recordingRuntime{inner: runtime.NewFake(mergeAgentFSExtraThenDeleted())}

	ticket = mergeDirtyAfterPublish(t, s, ticket, rec, gh, tr)

	deps := shipClaim(t, s, rec, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	deps = shipClaim(t, s, rec, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "run first") // runMergeFirst: turn 1

	deps = shipClaim(t, s, rec, ticket.ID, gh, tr)
	var checkOne store.HandlerCommit
	ticket, checkOne = mergeRunTick(t, s, deps, ticket, "check 1") // CHECK finds notes.txt, resumes: turn 2
	if shipHasMergeLanded(checkOne) {
		t.Fatal("check 1 landed the merge, want a charged resume instead")
	}

	gotPrompt := rec.lastRequest(t).Prompt
	wantText := "these paths are outside the merge; restore or delete them: notes.txt"
	if !strings.Contains(gotPrompt, wantText) {
		t.Errorf("resume prompt = %q, want it to contain %q", gotPrompt, wantText)
	}

	deps = shipClaim(t, s, rec, ticket.ID, gh, tr)
	var checkTwo store.HandlerCommit
	ticket, checkTwo = mergeRunTick(t, s, deps, ticket, "check 2") // CHECK on turn 2's deletion: lands
	if !shipHasMergeLanded(checkTwo) {
		t.Fatal("check 2 did not land the merge")
	}
	_ = ticket
}

// mergeOkNoChangeScript is TestMergeCheckLoopsEscalate's own repeated
// turn: an ok claim with nothing changed, since the fixture's own test
// command fails for a reason no file edit can fix.
const mergeOkNoChangeScript = `<zing job="build" outcome="ok">
  <claims>
    <files_changed>
    </files_changed>
  </claims>
  <report>The project's own test command fails for reasons outside this merge.</report>
  <notes></notes>
</zing>`

// mergeRepeatedOkFS returns n identical ok turns under the merge job's own
// label, "build/merge/<turn>.xml".
func mergeRepeatedOkFS(n int) fstest.MapFS {
	fsys := make(fstest.MapFS, n)
	for i := 1; i <= n; i++ {
		fsys[fmt.Sprintf("build/merge/%d.xml", i)] = &fstest.MapFile{Data: []byte(mergeOkNoChangeScript)}
	}
	return fsys
}

// mergeClaimFailingTest is shipClaim with the ticket's own project test
// command replaced by "false", so CHECK fails deterministically no matter
// what the merge or the agent do to the tree.
func mergeClaimFailingTest(t *testing.T, s *store.Store, rt runtime.Runtime, ticket store.Ticket, gh orchestrator.GitHub, tr ShipTracker) Deps {
	t.Helper()
	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	proj := deps.Projects[ticket.ProjectID]
	proj.TestCmd = "false"
	deps.Projects[ticket.ProjectID] = proj
	return deps
}

// TestMergeCheckLoopsEscalate proves mergeCheck's own check_loops gate
// (task 6): a test command that always fails burns through
// jobs.merge.check_loops (5) charged resumes, then the next CHECK
// escalates instead of resuming again, with Tried holding the failing
// output.
func TestMergeCheckLoopsEscalate(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, "other2.txt", []byte("main only, again\n"))

	checkLoops := pbMachine(t).Jobs[jobMergeName].CheckLoops

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(mergeRepeatedOkFS(checkLoops + 1))

	deps := mergeClaimFailingTest(t, s, rt, ticket, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // PUBLISH
	if err != nil {
		t.Fatalf("PUBLISH: %v", err)
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)
	gh.prState = orchestrator.PRState{
		State: questionStateOpen, Draft: true, HeadSHA: shipHeadSHA(t, s, ticket),
		BaseRef: pbFixtureDefaultBranch, MergeableState: mergeableStateDirty,
	}

	deps = mergeClaimFailingTest(t, s, rt, ticket, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	req, open, err := openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge: %v", err)
	}
	if !open {
		t.Fatal("openBaseMerge: no open request after POLL")
	}

	var last store.HandlerCommit
	for i := 0; i < checkLoops+1; i++ {
		deps = mergeClaimFailingTest(t, s, rt, ticket, gh, tr)
		ticket, last = mergeRunTick(t, s, deps, ticket, fmt.Sprintf("tick %d", i))
		if shipHasMergeLanded(last) {
			t.Fatalf("tick %d landed the merge, want %d charged resumes first", i, checkLoops)
		}
	}

	deps = mergeClaimFailingTest(t, s, rt, ticket, gh, tr)
	commit, err = (shipHandler{}).Run(t.Context(), ticket, deps) // the next CHECK: escalates
	if err != nil {
		t.Fatalf("final check: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("final check did not escalate")
	}
	if commit.Escalation.Payload.What != mergeCheckWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, mergeCheckWhat)
	}
	wantWhy := fmt.Sprintf(mergeCheckWhyFmt, checkLoops)
	if commit.Escalation.Payload.Why != wantWhy {
		t.Errorf("Why = %q, want %q", commit.Escalation.Payload.Why, wantWhy)
	}
	tried := commit.Escalation.Payload.Tried
	wantTriedPrefix := fmt.Sprintf("base merge %d\n", req.MessageID)
	if !strings.HasPrefix(tried, wantTriedPrefix) {
		t.Errorf("Tried = %q, want prefix %q", tried, wantTriedPrefix)
	}
	if !strings.Contains(tried, "test command: false") {
		t.Errorf("Tried = %q, want the failing test command's own output", tried)
	}
}

// mergeClaimFailingTestLowMaxResumes is mergeClaimFailingTest plus
// jobs.merge.max_resumes capped at maxResumes, with check_loops pushed
// well above it: under the real config (check_loops 5, max_resumes 6)
// mergeCheck's own max_resumes gate can never fire, since check_loops
// always gates first, so TestMergeCheckMaxResumesEscalates needs its own
// configuration to reach it at all.
func mergeClaimFailingTestLowMaxResumes(t *testing.T, s *store.Store, rt runtime.Runtime, ticket store.Ticket, gh orchestrator.GitHub, tr ShipTracker, maxResumes int) Deps {
	t.Helper()
	deps := mergeClaimFailingTest(t, s, rt, ticket, gh, tr)
	cfg := deps.Machine.Jobs[jobMergeName]
	cfg.CheckLoops = maxResumes + 10
	cfg.MaxResumes = maxResumes
	deps.Machine.Jobs[jobMergeName] = cfg
	return deps
}

// TestMergeCheckMaxResumesEscalates proves mergeCheck's own max_resumes
// gate (task 6), reachable only once check_loops is pushed above it: a
// test command that always fails burns through jobs.merge.max_resumes
// charged resumes, then the next CHECK escalates "the merge session ran
// out of resumes" instead of resuming or hitting the check_loops text.
func TestMergeCheckMaxResumesEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, "other3.txt", []byte("main only, a third time\n"))

	const maxResumes = 2
	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(mergeRepeatedOkFS(maxResumes + 1))

	claim := func() Deps { return mergeClaimFailingTestLowMaxResumes(t, s, rt, ticket, gh, tr, maxResumes) }

	deps := claim()
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // PUBLISH
	if err != nil {
		t.Fatalf("PUBLISH: %v", err)
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)
	gh.prState = orchestrator.PRState{
		State: questionStateOpen, Draft: true, HeadSHA: shipHeadSHA(t, s, ticket),
		BaseRef: pbFixtureDefaultBranch, MergeableState: mergeableStateDirty,
	}

	deps = claim()
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	var last store.HandlerCommit
	for i := range maxResumes + 1 {
		deps = claim()
		ticket, last = mergeRunTick(t, s, deps, ticket, fmt.Sprintf("tick %d", i))
		if shipHasMergeLanded(last) {
			t.Fatalf("tick %d landed the merge, want %d charged resumes first", i, maxResumes)
		}
	}

	deps = claim()
	commit, err = (shipHandler{}).Run(t.Context(), ticket, deps) // the next CHECK: escalates
	if err != nil {
		t.Fatalf("final check: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("final check did not escalate")
	}
	if commit.Escalation.Payload.What != mergeResumesWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, mergeResumesWhat)
	}
	wantWhy := fmt.Sprintf(mergeResumesWhyFmt, maxResumes)
	if commit.Escalation.Payload.Why != wantWhy {
		t.Errorf("Why = %q, want %q", commit.Escalation.Payload.Why, wantWhy)
	}
}

// TestMergeAfterErrorResumesExhaustedEscalates proves mergeAfterError's own
// max_resumes gate (merge.go, the n == 1 branch): a charged resume already
// spent the merge session's own jobs.merge.max_resumes budget on a failing
// CHECK, so when that very resumed run comes back unparseable instead of
// ok, mergeAfterError must not spend a further charged resume retrying it
// -- it escalates "the merge session ran out of resumes" instead.
func TestMergeAfterErrorResumesExhaustedEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, mergeOtherTxt, []byte("main only\n"))

	const maxResumes = 1
	const mergeResumeExhaustedSessionID = "merge-resume-exhausted-sess"
	const invalidReason = "no zing element in final message"
	scripted := &pbScriptedRuntime{t: t, steps: []pbScriptedStep{
		pbBuildStep(nil, nil, mergeResumeExhaustedSessionID),
		{
			res: runtime.RunResult{SessionID: mergeResumeExhaustedSessionID, ExitCode: 0, AgentTime: time.Second},
			err: &runtime.InvalidOutputError{Reason: invalidReason},
		},
	}}

	gh := &shipGitHub{}
	tr := &shipTracker{}
	claim := func() Deps { return mergeClaimFailingTestLowMaxResumes(t, s, scripted, ticket, gh, tr, maxResumes) }

	ticket = mergeDirtyAfterPublish(t, s, ticket, scripted, gh, tr)

	deps := claim()
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	deps = claim()
	ticket, _ = mergeRunTick(t, s, deps, ticket, "check with no run yet: runs the agent (ok), CHECK still fails") // turn 1

	deps = claim()
	ticket, _ = mergeRunTick(t, s, deps, ticket, "charged resume: comes back invalid, Resumes -> 1") // turn 2

	deps = claim()
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // mergeAfterError: resumes already exhausted
	if err != nil {
		t.Fatalf("mergeAfterError tick: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want an escalation")
	}
	if commit.Escalation.Payload.What != mergeResumesWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, mergeResumesWhat)
	}
	wantWhy := fmt.Sprintf(mergeResumesWhyFmt, maxResumes)
	if commit.Escalation.Payload.Why != wantWhy {
		t.Errorf("Why = %q, want %q", commit.Escalation.Payload.Why, wantWhy)
	}
}

// ---- task 7: mergeAfterError, retryMerge, reopenMerge ---------------------

// mergeQuestionScript is a minimal merge "question" turn (overview design,
// nongoal "A resumable owner question inside a merge session"): the merge
// unit never resumes a question itself -- mergeSuccessCommit's own
// QuestionResponse branch escalates it straight to the owner, so this
// script's own options are never read back as the agent's own choice,
// only as something a human reading the escalation's Tried text would see.
const mergeQuestionScript = `<zing job="build" outcome="question">
  <question key="Q1">
    <title>Keep the ticket's error type or main's?</title>
    <body>Both sides redefine the same error type differently; say which one to keep.</body>
    <option key="a">Keep the ticket's own type</option>
    <option key="b">Keep main's type</option>
    <recommended>a</recommended>
  </question>
</zing>`

// mergeAgentFSQuestion is the fake runtime's own scripts tree for a merge
// job's first turn that asks a question instead of resolving anything.
func mergeAgentFSQuestion() fstest.MapFS {
	return fstest.MapFS{mergeTurn1XMLKey: &fstest.MapFile{Data: []byte(mergeQuestionScript)}}
}

// TestMergeFirstPromptCarriesConflictsAndBaseLog proves runMergeFirst
// actually puts the conflicting paths and the base branch's own commit
// log into the agent's prompt (overview design goal 3): a prompt without
// either would still pass every other test here, since they only check
// the check text and the notes input.
func TestMergeFirstPromptCarriesConflictsAndBaseLog(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	baseSHA := mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rec := &recordingRuntime{inner: runtime.NewFake(mergeAgentFS())}

	ticket = mergeDirtyAfterPublish(t, s, ticket, rec, gh, tr)

	deps := shipClaim(t, s, rec, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	deps = shipClaim(t, s, rec, ticket.ID, gh, tr)
	if _, err := (shipHandler{}).Run(t.Context(), ticket, deps); err != nil { // runMergeFirst
		t.Fatalf("run first: %v", err)
	}

	gotPrompt := rec.lastRequest(t).Prompt
	if !strings.Contains(gotPrompt, "conflicts:\n") || !strings.Contains(gotPrompt, "hello.txt") {
		t.Errorf("prompt = %q, want a %q labeled input containing %q", gotPrompt, "conflicts:\n", "hello.txt")
	}
	if !strings.Contains(gotPrompt, "base_log:\n") || !strings.Contains(gotPrompt, baseSHA) || !strings.Contains(gotPrompt, "add hello.txt") {
		t.Errorf("prompt = %q, want a %q labeled input containing %q and %q", gotPrompt, "base_log:\n", baseSHA, "add hello.txt")
	}
}

// TestMergeQuestionEscalatesAndRetryCarriesNotes proves mergeSuccessCommit's
// own question escalation, and retryMerge/reopenMerge's own request
// lifecycle (overview design "Request lifecycle"): a merge agent's question
// escalates immediately, naming the open request in Tried; the owner's
// retry (option "a" plus a free-text note) closes that request and opens
// its successor carrying the note; the next tick reserves a fresh merge
// session whose prompt carries it.
func TestMergeQuestionEscalatesAndRetryCarriesNotes(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rec := &recordingRuntime{inner: runtime.NewFake(mergeAgentFSQuestion())}

	ticket = mergeDirtyAfterPublish(t, s, ticket, rec, gh, tr)

	deps := shipClaim(t, s, rec, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	deps = shipClaim(t, s, rec, ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // runMergeFirst: the agent asks a question
	if err != nil {
		t.Fatalf("run first: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want the merge question to escalate")
	}
	if commit.Escalation.Payload.What != mergeDecisionWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, mergeDecisionWhat)
	}
	tried := commit.Escalation.Payload.Tried
	firstID, ok := baseMergeTriedID(tried)
	if !ok {
		t.Fatalf("baseMergeTriedID(%q) = (_, false), want a %q prefix", tried, "base merge ")
	}
	if len(commit.Runs) != 1 || runOutcome(commit.Runs[0]) != string(response.OutcomeQuestion) {
		t.Errorf("commit.Runs = %+v, want exactly one run with outcome %q", commit.Runs, response.OutcomeQuestion)
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	open, err := s.QuestionsByState(t.Context(), ticket.ID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState: %v", err)
	}
	if len(open) == 0 {
		t.Fatal("QuestionsByState(open) = [], want at least one open question")
	}
	qID := open[len(open)-1].ID

	const retryNote = "keep main's error type"
	optA := "a"
	if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticket.ID, QuestionID: &qID, Option: &optA}); draftErr != nil {
		t.Fatalf("SaveDraft(option): %v", draftErr)
	}
	if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticket.ID, QuestionID: &qID, Text: retryNote}); draftErr != nil {
		t.Fatalf("SaveDraft(text): %v", draftErr)
	}
	if _, sendErr := s.SendBatch(t.Context(), ticket.ID); sendErr != nil {
		t.Fatalf("SendBatch: %v", sendErr)
	}

	deps = shipClaim(t, s, rec, ticket.ID, gh, tr)
	var reopenCommit store.HandlerCommit
	ticket, reopenCommit = mergeRunTick(t, s, deps, ticket, "retry: close and reopen")

	if !reopenCommit.ClearPoll {
		t.Error("reopenCommit.ClearPoll = false, want true")
	}
	closedBody := fmt.Sprintf("base merge closed %d", firstID)
	foundClosed, foundRequest := false, false
	var newReqBody string
	for _, m := range reopenCommit.Messages {
		switch {
		case m.Body == closedBody:
			foundClosed = true
		case strings.HasPrefix(m.Body, "base merge requested after run "):
			foundRequest = true
			newReqBody = m.Body
		}
	}
	if !foundClosed {
		t.Errorf("reopenCommit.Messages = %+v, want %q", reopenCommit.Messages, closedBody)
	}
	if !foundRequest {
		t.Fatalf("reopenCommit.Messages = %+v, want a fresh request marker", reopenCommit.Messages)
	}

	newReq, err := parseBaseMergeRequest(store.MessageRow{ID: 999, Body: newReqBody})
	if err != nil {
		t.Fatalf("parseBaseMergeRequest: %v", err)
	}
	if newReq.RetryOf != firstID {
		t.Errorf("newReq.RetryOf = %d, want %d", newReq.RetryOf, firstID)
	}
	if !strings.Contains(newReq.Notes, retryNote) {
		t.Errorf("newReq.Notes = %q, want it to contain %q", newReq.Notes, retryNote)
	}

	open, err = s.QuestionsByState(t.Context(), ticket.ID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState (after retry): %v", err)
	}
	if len(open) != 0 {
		t.Errorf("open questions after retry = %+v, want none (the round resolved)", open)
	}

	// The tick after that reserves a fresh merge session whose prompt
	// carries the owner's own note.
	deps = shipClaim(t, s, rec, ticket.ID, gh, tr)
	if _, runErr := (shipHandler{}).Run(t.Context(), ticket, deps); runErr != nil {
		t.Fatalf("fresh run: %v", runErr)
	}
	gotPrompt := rec.lastRequest(t).Prompt
	if !strings.Contains(gotPrompt, "notes:\n") {
		t.Errorf("fresh run prompt = %q, want a %q labeled input", gotPrompt, "notes:\n")
	}
	if !strings.Contains(gotPrompt, retryNote) {
		t.Errorf("fresh run prompt = %q, want it to contain %q", gotPrompt, retryNote)
	}
}

// mergeErrorScript is a minimal merge "error" document (internal/response/
// examples/build-error.xml's own shape, job "build" since the merge unit
// answers in the build response shape): the agent could not resolve the
// conflict at all.
const mergeErrorScript = `<zing job="build" outcome="error">
  <error code="cannot_run">
    <what>the conflict could not be resolved</what>
    <why>the sandbox denied write access to hello.txt</why>
    <tried>edited hello.txt, re-ran git diff</tried>
  </error>
</zing>`

// mergeAgentFSError is the fake runtime's own scripts tree for a merge
// job's first turn that errors out instead of resolving anything.
func mergeAgentFSError() fstest.MapFS {
	return fstest.MapFS{mergeTurn1XMLKey: &fstest.MapFile{Data: []byte(mergeErrorScript)}}
}

// TestMergeErrorEscalatesAndRetryReopens proves mergeSuccessCommit's own
// ErrorResponse branch (overview design "One merge tick"): the agent's own
// error outcome escalates immediately under its own code, What and Why,
// with Tried carrying both the open request's id (mergeTried) and the
// agent's own Tried text, and the run stored as error; the owner's retry
// then routes through retryMerge (isBaseMergeTried) and reopens the
// request.
func TestMergeErrorEscalatesAndRetryReopens(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(mergeAgentFSError())

	ticket = mergeDirtyAfterPublish(t, s, ticket, rt, gh, tr)

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	req, open, err := openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge: %v", err)
	}
	if !open {
		t.Fatal("openBaseMerge: no open request after POLL")
	}

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // runMergeFirst: the agent errors out
	if err != nil {
		t.Fatalf("run first: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want the merge error to escalate")
	}
	if commit.Escalation.Payload.Code != "cannot_run" {
		t.Errorf("Code = %q, want %q", commit.Escalation.Payload.Code, "cannot_run")
	}
	if commit.Escalation.Payload.What != "the conflict could not be resolved" {
		t.Errorf("What = %q, want the agent's own What", commit.Escalation.Payload.What)
	}
	if commit.Escalation.Payload.Why != "the sandbox denied write access to hello.txt" {
		t.Errorf("Why = %q, want the agent's own Why", commit.Escalation.Payload.Why)
	}
	wantTried := fmt.Sprintf("base merge %d\nedited hello.txt, re-ran git diff", req.MessageID)
	if commit.Escalation.Payload.Tried != wantTried {
		t.Errorf("Tried = %q, want %q", commit.Escalation.Payload.Tried, wantTried)
	}
	if !isBaseMergeTried(commit.Escalation.Payload.Tried) {
		t.Errorf("isBaseMergeTried(%q) = false, want true", commit.Escalation.Payload.Tried)
	}
	if len(commit.Runs) != 1 || runOutcome(commit.Runs[0]) != string(response.OutcomeError) {
		t.Errorf("commit.Runs = %+v, want exactly one run with outcome %q", commit.Runs, response.OutcomeError)
	}
	if !commit.ClearPoll {
		t.Error("commit.ClearPoll = false, want true")
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	openQs, err := s.QuestionsByState(t.Context(), ticket.ID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState: %v", err)
	}
	if len(openQs) == 0 {
		t.Fatal("QuestionsByState(open) = [], want at least one open question")
	}
	qID := openQs[len(openQs)-1].ID
	optRetry := escalationChoiceRetry
	if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticket.ID, QuestionID: &qID, Option: &optRetry}); draftErr != nil {
		t.Fatalf("SaveDraft(option): %v", draftErr)
	}
	if _, sendErr := s.SendBatch(t.Context(), ticket.ID); sendErr != nil {
		t.Fatalf("SendBatch: %v", sendErr)
	}

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	_, reopenCommit := mergeRunTick(t, s, deps, ticket, "retry: close and reopen")
	closedBody := fmt.Sprintf("base merge closed %d", req.MessageID)
	if !shipHasMessage(reopenCommit, closedBody) {
		t.Fatalf("reopenCommit.Messages = %+v, want %q", reopenCommit.Messages, closedBody)
	}
}

// TestMergeInterruptedRunResumesFree proves mergeAfterError's own
// interrupted branch (overview design "One merge tick"): a merge session
// whose newest run was cut short with no answer at all (a shutdown or
// dead-serve reclaim, store.Run.Interrupted) resumes the very same session,
// free -- the resume carries the raw "interrupted" input and Reserve's own
// BumpResumes stays false, so sessions.resumes is unchanged.
func TestMergeInterruptedRunResumesFree(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(fstest.MapFS{})

	ticket = mergeDirtyAfterPublish(t, s, ticket, rt, gh, tr)

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	// A session reserved directly through the store, its external id set
	// by its own terminalizing commit (freshSessionRecord's own shape) but
	// its one run left reserved (outcome NULL): the shape a process death
	// right after the runtime answered with a session id, but before this
	// code ever read that answer, would leave behind.
	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	rsv, reserveErr := s.Reserve(t.Context(), ticket.ID, deps.Owner, deps.Expires, store.SessionUpsert{Job: jobMergeName, Runtime: pbRuntimeClaude}, store.RunSeed{Model: pbModelClaudeX})
	if reserveErr != nil {
		t.Fatalf("Reserve: %v", reserveErr)
	}
	const mergeInterruptedSessionID = "merge-interrupted-sess"
	ext := mergeInterruptedSessionID
	applied, commitErr := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticket.ID, Owner: deps.Owner, Expires: deps.Expires,
		Session: &store.SessionUpsert{ID: &rsv.SessionID, ExternalID: &ext},
	})
	if commitErr != nil || !applied {
		t.Fatalf("CommitHandlerResult(external id): applied=%v err=%v", applied, commitErr)
	}

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	interrupted, interruptErr := s.InterruptRuns(t.Context(), ticket.ID, deps.Owner, deps.Expires)
	if interruptErr != nil {
		t.Fatalf("InterruptRuns: %v", interruptErr)
	}
	if !interrupted {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	req, open, err := openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge: %v", err)
	}
	if !open {
		t.Fatal("open = false, want true")
	}
	maxResumes := pbMachine(t).Jobs[jobMergeName].MaxResumes
	sess, state, _, found, sessErr := s.SessionAfter(t.Context(), ticket.ID, jobMergeName, req.AfterRunID, maxResumes)
	if sessErr != nil {
		t.Fatalf("SessionAfter: %v", sessErr)
	}
	if !found || state != store.SessionOpen {
		t.Fatalf("SessionAfter = (found %v, state %v), want (true, SessionOpen)", found, state)
	}
	if sess.Resumes != 0 {
		t.Fatalf("session Resumes before the interrupted resume = %d, want 0", sess.Resumes)
	}

	resumeRT := &pbScriptedRuntime{t: t, steps: []pbScriptedStep{pbBuildStep(nil, nil, mergeInterruptedSessionID)}}
	deps = shipClaim(t, s, resumeRT, ticket.ID, gh, tr)
	ticket, commit := mergeRunTick(t, s, deps, ticket, "resume: interrupted, free")
	_ = ticket

	if len(resumeRT.reqs) != 1 {
		t.Fatalf("resumeRT.reqs = %+v, want exactly one", resumeRT.reqs)
	}
	lastReq := resumeRT.reqs[0]
	if !strings.Contains(lastReq.Prompt, interruptedResumeText) {
		t.Errorf("resume prompt = %q, want the interrupted input", lastReq.Prompt)
	}
	if lastReq.SessionID != mergeInterruptedSessionID {
		t.Errorf("resume request SessionID = %q, want %q", lastReq.SessionID, mergeInterruptedSessionID)
	}
	if len(commit.Runs) != 1 {
		t.Errorf("commit.Runs = %+v, want exactly one", commit.Runs)
	}

	sessions, err := s.SessionsForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	mergeResumes := -1
	for _, sv := range sessions {
		if sv.Job == jobMergeName {
			mergeResumes = sv.Resumes
		}
	}
	if mergeResumes != 0 {
		t.Errorf("session Resumes after the free interrupted resume = %d, want 0 (unchanged)", mergeResumes)
	}
}

// TestMergeInvalidOutputResumesCharged proves mergeAfterError's own charged
// resume after a first invalid output (overview design "One merge tick"):
// the agent's first turn returns an unparseable document, which only marks
// "response invalid run <rid>" rather than escalating; the next tick
// resumes the very same session with the invalid input, charged (Resumes
// becomes 1).
func TestMergeInvalidOutputResumesCharged(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	const mergeInvalidSessionID = "merge-invalid-sess"
	const invalidReason = "no zing element in final message"
	scripted := &pbScriptedRuntime{t: t, steps: []pbScriptedStep{{
		res: runtime.RunResult{SessionID: mergeInvalidSessionID, ExitCode: 0, AgentTime: time.Second},
		err: &runtime.InvalidOutputError{Reason: invalidReason},
	}}}

	ticket = mergeDirtyAfterPublish(t, s, ticket, scripted, gh, tr)

	deps := shipClaim(t, s, scripted, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	deps = shipClaim(t, s, scripted, ticket.ID, gh, tr)
	firstCommit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // runMergeFirst: invalid output
	if err != nil {
		t.Fatalf("run first: %v", err)
	}
	if firstCommit.Escalation != nil {
		t.Fatalf("first invalid output escalated: %+v", firstCommit.Escalation.Payload)
	}
	pbApply(t, s, ticket, firstCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	scripted.steps = append(scripted.steps, pbBuildStep([]string{pbHelloTxt}, nil, mergeInvalidSessionID))
	deps = shipClaim(t, s, scripted, ticket.ID, gh, tr)
	resumeCommit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // mergeAfterError: charged resume
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumeCommit.Escalation != nil {
		t.Fatalf("resume escalated: %+v", resumeCommit.Escalation.Payload)
	}

	if len(scripted.reqs) != 2 {
		t.Fatalf("scripted.reqs = %+v, want exactly two", scripted.reqs)
	}
	resumeReq := scripted.reqs[1]
	if !strings.Contains(resumeReq.Prompt, invalidReason) {
		t.Errorf("resume prompt = %q, want it to contain %q", resumeReq.Prompt, invalidReason)
	}
	if resumeReq.SessionID != mergeInvalidSessionID {
		t.Errorf("resume request SessionID = %q, want %q", resumeReq.SessionID, mergeInvalidSessionID)
	}

	sessions, err := s.SessionsForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	mergeResumes := -1
	for _, sv := range sessions {
		if sv.Job == jobMergeName {
			mergeResumes = sv.Resumes
		}
	}
	if mergeResumes != 1 {
		t.Errorf("session Resumes after the charged invalid-output resume = %d, want 1", mergeResumes)
	}
}

// TestMergeRetrySettledRequestFallsBackToGenericMarker proves retryMerge's
// own fallback (overview design "Request lifecycle"): a request that
// settled -- closed here, but landed would look identical -- behind the
// owner's back before the owner ever answered its escalation gets the
// generic shipping "retry requested" marker, resolves the question, and
// opens no fresh request of its own.
func TestMergeRetrySettledRequestFallsBackToGenericMarker(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(mergeAgentFSQuestion())

	ticket = mergeDirtyAfterPublish(t, s, ticket, rt, gh, tr)

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // runMergeFirst: the agent asks a question
	if err != nil {
		t.Fatalf("run first: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want the merge question to escalate")
	}
	firstID, ok := baseMergeTriedID(commit.Escalation.Payload.Tried)
	if !ok {
		t.Fatalf("baseMergeTriedID(%q) = (_, false), want a %q prefix", commit.Escalation.Payload.Tried, "base merge ")
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	// Settle the request behind the owner's back, exactly as a later tick
	// might (landed or already-merged would look the same to retryMerge),
	// before the owner ever answers the escalation.
	if _, insertErr := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("base merge closed %d", firstID),
	}); insertErr != nil {
		t.Fatalf("InsertMessage(closed): %v", insertErr)
	}

	open, err := s.QuestionsByState(t.Context(), ticket.ID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState: %v", err)
	}
	if len(open) == 0 {
		t.Fatal("QuestionsByState(open) = [], want at least one open question")
	}
	qID := open[len(open)-1].ID
	optA := "a" // escalationChoiceRetry
	if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticket.ID, QuestionID: &qID, Option: &optA}); draftErr != nil {
		t.Fatalf("SaveDraft(option): %v", draftErr)
	}
	if _, sendErr := s.SendBatch(t.Context(), ticket.ID); sendErr != nil {
		t.Fatalf("SendBatch: %v", sendErr)
	}

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket = pbGetTicket(t, s, ticket.ID)
	retryCommit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // retryMerge: settled, falls back
	if err != nil {
		t.Fatalf("retry tick: %v", err)
	}
	if !shipHasMessage(retryCommit, markerRetryRequested) {
		t.Errorf("retryCommit.Messages = %+v, want %q", retryCommit.Messages, markerRetryRequested)
	}
	if !slices.Contains(retryCommit.ResolveQuestions, qID) {
		t.Errorf("retryCommit.ResolveQuestions = %v, want it to include %d", retryCommit.ResolveQuestions, qID)
	}
	for _, m := range retryCommit.Messages {
		if strings.HasPrefix(m.Body, "base merge requested after run ") {
			t.Errorf("retryCommit.Messages = %+v, want no fresh request marker", retryCommit.Messages)
		}
	}

	stillOpen, err := s.QuestionsByState(t.Context(), ticket.ID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState (after retry): %v", err)
	}
	if len(stillOpen) != 0 {
		t.Errorf("open questions after retry = %+v, want none (the round resolved)", stillOpen)
	}
}

// TestMergeRetriedErrorReopens proves mergeAfterError's own fall-through
// (overview design "One merge tick"): once a merge session's own error run
// has already escalated and the owner retried it through the generic
// shipping row (not a merge-specific one), a later tick finds the session
// still sitting on that same settled error run and reopens the request
// rather than resuming it again.
func TestMergeRetriedErrorReopens(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(fstest.MapFS{})

	ticket = mergeDirtyAfterPublish(t, s, ticket, rt, gh, tr)

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	req, open, err := openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge: %v", err)
	}
	if !open {
		t.Fatal("openBaseMerge: no open request after POLL")
	}

	// A session reserved directly through the store, its one run already
	// terminalized as an error (outcome error, not interrupted): the shape
	// a merge run's own escalation, already answered once through the
	// generic shipping retry row, leaves behind once the owner's retry
	// resolved the escalation but never reopened the merge-specific
	// request itself.
	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	rsv, reserveErr := s.Reserve(t.Context(), ticket.ID, deps.Owner, deps.Expires, store.SessionUpsert{Job: jobMergeName, Runtime: pbRuntimeClaude}, store.RunSeed{Model: pbModelClaudeX})
	if reserveErr != nil {
		t.Fatalf("Reserve: %v", reserveErr)
	}
	const mergeRetriedSessionID = "merge-retried-sess"
	ext := mergeRetriedSessionID
	outcome, exitCode, agentSeconds := string(response.OutcomeError), 1, 1
	applied, commitErr := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticket.ID, Owner: deps.Owner, Expires: deps.Expires,
		Session: &store.SessionUpsert{ID: &rsv.SessionID, ExternalID: &ext},
		Runs:    []store.Run{{ID: rsv.RunID, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
	})
	if commitErr != nil || !applied {
		t.Fatalf("CommitHandlerResult(settled error run): applied=%v err=%v", applied, commitErr)
	}

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket = pbGetTicket(t, s, ticket.ID)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // mergeAfterError: fall-through reopen
	if err != nil {
		t.Fatalf("reopen tick: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("reopen tick escalated: %+v", commit.Escalation.Payload)
	}
	if !commit.ClearPoll {
		t.Error("commit.ClearPoll = false, want true")
	}
	closedBody := fmt.Sprintf("base merge closed %d", req.MessageID)
	foundClosed, foundRequest := false, false
	for _, m := range commit.Messages {
		switch {
		case m.Body == closedBody:
			foundClosed = true
		case strings.HasPrefix(m.Body, "base merge requested after run "):
			foundRequest = true
		}
	}
	if !foundClosed {
		t.Errorf("commit.Messages = %+v, want %q", commit.Messages, closedBody)
	}
	if !foundRequest {
		t.Errorf("commit.Messages = %+v, want a fresh request marker", commit.Messages)
	}
}

// ---- task 8: adoptMerge, and StartBaseMerge's own already-merged branch --

// TestMergeAdoptsCommitAfterCrash proves adoptMerge (overview design
// "Request lifecycle"): a merge commit git already holds -- landed by a
// tick that committed it through CommitMerge directly and then crashed
// before recording "base merge landed" -- is adopted on the next tick
// without git ever committing again: the commit's own two parents and its
// signature are enough to trust it, and the merge session's own newest ok
// run (the agent turn that resolved hello.txt) gives its report.
func TestMergeAdoptsCommitAfterCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)
	baseSHA := mergeCommitOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(mergeAgentFS())

	ticket = mergeDirtyAfterPublish(t, s, ticket, rt, gh, tr)
	preMergeHead := shipHeadSHA(t, s, ticket)

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	req, open, err := openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge: %v", err)
	}
	if !open {
		t.Fatal("openBaseMerge: no open request after POLL")
	}

	// StartBaseMerge finds the conflict and runs the fake merge agent's
	// first turn, which resolves hello.txt and records an ok run plus its
	// own build_report -- but nothing is committed yet.
	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	var runCommit store.HandlerCommit
	ticket, runCommit = mergeRunTick(t, s, deps, ticket, "merge run")
	if shipHasMergeLanded(runCommit) {
		t.Fatal("the merge run tick already landed, want the agent turn only")
	}

	// Simulate a crash right after a tick committed the merge for real
	// (CommitMerge, direct through the orchestrator) but before it could
	// record "base merge landed": the claim is released with no commit
	// applied, exactly as TestReadyCrashConverges (shipping_test.go) does
	// for MarkReady.
	preCrashDeps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	proj := preCrashDeps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	mergeSHA, commitErr := proj.Orch.CommitMerge(t.Context(), wt, orchestrator.CommitMessage{
		Title: mergeTitle(req), FuncLines: mergeFuncLines(req, []string{pbHelloTxt}),
	}, req.BaseSHA)
	if commitErr != nil {
		t.Fatalf("CommitMerge (pre-crash): %v", commitErr)
	}
	shipReleaseClaim(t, s, ticket.ID, preCrashDeps)

	// The next tick's driveMerge finds mergeSHA as the one unrecorded
	// commit at the tip and adopts it: no new git commit, just the
	// "landed" marker and build_report.
	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket = pbGetTicket(t, s, ticket.ID)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("adopt tick: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("adopt tick escalated: %+v", commit.Escalation.Payload)
	}
	want := fmt.Sprintf("base merge landed %d sha %s", req.MessageID, mergeSHA)
	if !shipHasMessage(commit, want) {
		t.Errorf("commit.Messages = %+v, want %q", commit.Messages, want)
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	if got := shipHeadSHA(t, s, ticket); got != mergeSHA {
		t.Errorf("HEAD after the adopt tick = %s, want it unchanged at the pre-crash merge sha %s", got, mergeSHA)
	}

	parents, err := proj.Orch.CommitParents(t.Context(), wt, mergeSHA)
	if err != nil {
		t.Fatalf("CommitParents: %v", err)
	}
	if len(parents) != 2 || parents[0] != preMergeHead || parents[1] != baseSHA {
		t.Errorf("CommitParents(%s) = %v, want [%s %s]", mergeSHA, parents, preMergeHead, baseSHA)
	}

	reports, err := s.BuildReports(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("BuildReports: %v", err)
	}
	found := false
	for _, r := range reports {
		if r.Report.CommitSHA != nil && *r.Report.CommitSHA == mergeSHA {
			found = true
			if len(r.Report.FilesChanged) != 1 || r.Report.FilesChanged[0] != pbHelloTxt {
				t.Errorf("adopted report FilesChanged = %v, want [hello.txt] (the agent's own report, not synthesized)", r.Report.FilesChanged)
			}
		}
	}
	if !found {
		t.Errorf("BuildReports has no row with commit_sha %s", mergeSHA)
	}
}

// TestMergeAdoptForeignOrdinaryCommitEscalates proves adoptMerge refuses an
// ordinary, single-parent commit left at the tip by anyone with bash in the
// worktree (overview design nongoal "Adopting a merge commit the owner made
// by hand"): it is signed, but it is not a merge at all, so the parents
// check fails before any of adoptMerge's other gates ever run.
func TestMergeAdoptForeignOrdinaryCommitEscalates(t *testing.T) {
	mergeAdoptEscalationCase(t, mergeOtherTxt, []byte("main only\n"),
		func(t *testing.T, dir, _, _ string) {
			t.Helper()
			if writeErr := os.WriteFile(filepath.Join(dir, "foreign.txt"), []byte("not a merge\n"), 0o644); writeErr != nil {
				t.Fatalf("write foreign.txt: %v", writeErr)
			}
			if out, addErr := gitfixture.Git(t.Context(), dir, "add", "foreign.txt"); addErr != nil {
				t.Fatalf("git add: %v: %s", addErr, out)
			}
			if out, commitErr := gitfixture.Git(t.Context(), dir, "commit", "-q", "-S", "-m", "a foreign commit, not a merge"); commitErr != nil {
				t.Fatalf("git commit: %v: %s", commitErr, out)
			}
		},
		"not this request's merge commit")
}

// TestMergeAdoptWrongSecondParentEscalates proves adoptMerge refuses a
// two-parent commit whose second parent is not req.BaseSHA: shape alone
// (signed, two parents) is not enough to trust it as this request's own
// merge.
func TestMergeAdoptWrongSecondParentEscalates(t *testing.T) {
	mergeAdoptEscalationCase(t, mergeOtherTxt, []byte("main only\n"),
		func(t *testing.T, dir, head, _ string) {
			t.Helper()
			// otherParent is an ancestor already reachable from HEAD (the
			// merge base with the fetched base branch), not a brand new
			// commit: using an unreachable commit here would itself add a
			// second unrecorded commit, tripping driveMerge's own
			// foreign_commits branch before adoptMerge ever runs.
			mergeBaseOut, mergeBaseErr := gitfixture.Git(t.Context(), dir, "merge-base", "refs/zing/base/"+pbFixtureDefaultBranch, "HEAD")
			if mergeBaseErr != nil {
				t.Fatalf("git merge-base: %v: %s", mergeBaseErr, mergeBaseOut)
			}
			otherParent := strings.TrimSpace(string(mergeBaseOut))
			fakeMergeOut, fakeMergeErr := gitfixture.Git(t.Context(), dir, "commit-tree", "HEAD^{tree}", "-p", head, "-p", otherParent, "-S", "-m", "a merge that is not this request's own")
			if fakeMergeErr != nil {
				t.Fatalf("git commit-tree (fake merge): %v: %s", fakeMergeErr, fakeMergeOut)
			}
			fakeMerge := strings.TrimSpace(string(fakeMergeOut))
			if out, updateErr := gitfixture.Git(t.Context(), dir, "update-ref", "HEAD", fakeMerge); updateErr != nil {
				t.Fatalf("git update-ref HEAD: %v: %s", updateErr, out)
			}
		},
		"not this request's merge commit")
}

// mergeAdoptEscalationCase proves one of adoptMerge's own refusal checks
// (overview design, adoptMerge's own doc comment): main commits mainPath
// (mainContent) on the default branch, POLL opens a request, then build
// writes a commit at the tip -- given the worktree, the pre-merge head, and
// the request's own base sha -- that fails exactly one of adoptMerge's
// gates. The next tick's driveMerge must then escalate unverifiable_commit
// with Tried ending "\n"+wantDetail, never land the merge.
func mergeAdoptEscalationCase(t *testing.T, mainPath string, mainContent []byte, build func(t *testing.T, dir, head, baseSHA string), wantDetail string) {
	t.Helper()
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	s, ticket, _ := shipTicketReady(t)
	mergeCommitOnMain(t, s, ticket, mainPath, mainContent)

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(fstest.MapFS{})

	ticket = mergeDirtyAfterPublish(t, s, ticket, rt, gh, tr)

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket, _ = mergeRunTick(t, s, deps, ticket, "poll writes request") // POLL

	req, open, err := openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge: %v", err)
	}
	if !open {
		t.Fatal("openBaseMerge: no open request after POLL")
	}

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	headOut, headErr := gitfixture.Git(t.Context(), wt.Dir(), "rev-parse", "HEAD")
	if headErr != nil {
		t.Fatalf("git rev-parse HEAD: %v", headErr)
	}
	head := strings.TrimSpace(string(headOut))

	build(t, wt.Dir(), head, req.BaseSHA)
	shipReleaseClaim(t, s, ticket.ID, deps)

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	ticket = pbGetTicket(t, s, ticket.ID)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("adopt tick: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want an unverifiable_commit escalation")
	}
	if commit.Escalation.Payload.What != unverifiableCommitWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, unverifiableCommitWhat)
	}
	tried := commit.Escalation.Payload.Tried
	wantTried := fmt.Sprintf("base merge %d\n%s", req.MessageID, wantDetail)
	if tried != wantTried {
		t.Errorf("Tried = %q, want %q", tried, wantTried)
	}
	if shipHasMergeLanded(commit) {
		t.Error("commit landed the merge, want an escalation instead")
	}
}

// TestMergeAdoptUnsignedEscalates proves adoptMerge refuses a correctly
// shaped merge commit (right parents, right paths) that simply carries no
// signature: shape is not enough, since "git commit-tree" with no "-S"
// never signs at all, unlike "git commit", which commit.gpgsign can force.
func TestMergeAdoptUnsignedEscalates(t *testing.T) {
	mergeAdoptEscalationCase(t, mergeOtherTxt, []byte("main only\n"),
		func(t *testing.T, dir, head, baseSHA string) {
			t.Helper()
			commitOut, err := gitfixture.Git(t.Context(), dir, "commit-tree", head+"^{tree}", "-p", head, "-p", baseSHA, "-m", "an unsigned merge")
			if err != nil {
				t.Fatalf("git commit-tree: %v: %s", err, commitOut)
			}
			sha := strings.TrimSpace(string(commitOut))
			if out, updateErr := gitfixture.Git(t.Context(), dir, "update-ref", "HEAD", sha); updateErr != nil {
				t.Fatalf("git update-ref HEAD: %v: %s", updateErr, out)
			}
		},
		"unsigned")
}

// TestMergeAdoptCommandsFailedEscalates proves adoptMerge re-runs the
// project's own commands before trusting a shape- and signature-valid
// commit: dropping hello.txt from the tree -- a path the merge's own side
// set covers, since mergeCommitOnMain also edits it -- makes the project's
// own "test -f hello.txt" fail.
func TestMergeAdoptCommandsFailedEscalates(t *testing.T) {
	mergeAdoptEscalationCase(t, "hello.txt", []byte(mergeHelloConflict),
		func(t *testing.T, dir, head, baseSHA string) {
			t.Helper()
			if out, err := gitfixture.Git(t.Context(), dir, "rm", "-f", "--quiet", "hello.txt"); err != nil {
				t.Fatalf("git rm: %v: %s", err, out)
			}
			treeOut, err := gitfixture.Git(t.Context(), dir, "write-tree")
			if err != nil {
				t.Fatalf("git write-tree: %v: %s", err, treeOut)
			}
			tree := strings.TrimSpace(string(treeOut))
			commitOut, err := gitfixture.Git(t.Context(), dir, "commit-tree", tree, "-p", head, "-p", baseSHA, "-S", "-m", "drop hello.txt")
			if err != nil {
				t.Fatalf("git commit-tree: %v: %s", err, commitOut)
			}
			sha := strings.TrimSpace(string(commitOut))
			if out, resetErr := gitfixture.Git(t.Context(), dir, "reset", "--hard", sha); resetErr != nil {
				t.Fatalf("git reset --hard: %v: %s", resetErr, out)
			}
		},
		"commands failed")
}

// TestMergeAdoptTreeNotCleanEscalates proves adoptMerge refuses to trust a
// shape-, signature-, and command-valid commit while the worktree itself
// still carries an uncommitted change.
func TestMergeAdoptTreeNotCleanEscalates(t *testing.T) {
	mergeAdoptEscalationCase(t, mergeOtherTxt, []byte("main only\n"),
		func(t *testing.T, dir, head, baseSHA string) {
			t.Helper()
			commitOut, err := gitfixture.Git(t.Context(), dir, "commit-tree", head+"^{tree}", "-p", head, "-p", baseSHA, "-S", "-m", "a proper merge shape")
			if err != nil {
				t.Fatalf("git commit-tree: %v: %s", err, commitOut)
			}
			sha := strings.TrimSpace(string(commitOut))
			if out, updateErr := gitfixture.Git(t.Context(), dir, "update-ref", "HEAD", sha); updateErr != nil {
				t.Fatalf("git update-ref HEAD: %v: %s", updateErr, out)
			}
			if writeErr := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("uncommitted\n"), 0o644); writeErr != nil {
				t.Fatalf("write hello.txt: %v", writeErr)
			}
		},
		"tree not clean")
}

// TestMergeAdoptOutsideMergeEscalates proves adoptMerge refuses a commit
// that, alongside the two sides' own real changes, also touches a path
// neither side ever changed.
func TestMergeAdoptOutsideMergeEscalates(t *testing.T) {
	mergeAdoptEscalationCase(t, mergeOtherTxt, []byte("main only\n"),
		func(t *testing.T, dir, head, baseSHA string) {
			t.Helper()
			if writeErr := os.WriteFile(filepath.Join(dir, "unexpected.txt"), []byte("neither side touched this\n"), 0o644); writeErr != nil {
				t.Fatalf("write unexpected.txt: %v", writeErr)
			}
			if out, addErr := gitfixture.Git(t.Context(), dir, "add", "unexpected.txt"); addErr != nil {
				t.Fatalf("git add: %v: %s", addErr, out)
			}
			treeOut, err := gitfixture.Git(t.Context(), dir, "write-tree")
			if err != nil {
				t.Fatalf("git write-tree: %v: %s", err, treeOut)
			}
			tree := strings.TrimSpace(string(treeOut))
			commitOut, err := gitfixture.Git(t.Context(), dir, "commit-tree", tree, "-p", head, "-p", baseSHA, "-S", "-m", "adds a path neither side touched")
			if err != nil {
				t.Fatalf("git commit-tree: %v: %s", err, commitOut)
			}
			sha := strings.TrimSpace(string(commitOut))
			if out, resetErr := gitfixture.Git(t.Context(), dir, "reset", "--hard", sha); resetErr != nil {
				t.Fatalf("git reset --hard: %v: %s", resetErr, out)
			}
		},
		"outside the merge")
}

// TestMergeAdoptConflictMarkersRemainEscalates proves adoptMerge refuses a
// commit that touches only the merge's own side-set paths, but leaves a
// conflict marker inside one of them.
func TestMergeAdoptConflictMarkersRemainEscalates(t *testing.T) {
	mergeAdoptEscalationCase(t, "hello.txt", []byte(mergeHelloConflict),
		func(t *testing.T, dir, head, baseSHA string) {
			t.Helper()
			marked := "<<<<<<< HEAD\nhello, world\n=======\nhello, main\n>>>>>>> " + baseSHA + "\n"
			if writeErr := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte(marked), 0o644); writeErr != nil {
				t.Fatalf("write hello.txt: %v", writeErr)
			}
			if out, addErr := gitfixture.Git(t.Context(), dir, "add", "hello.txt"); addErr != nil {
				t.Fatalf("git add: %v: %s", addErr, out)
			}
			treeOut, err := gitfixture.Git(t.Context(), dir, "write-tree")
			if err != nil {
				t.Fatalf("git write-tree: %v: %s", err, treeOut)
			}
			tree := strings.TrimSpace(string(treeOut))
			commitOut, err := gitfixture.Git(t.Context(), dir, "commit-tree", tree, "-p", head, "-p", baseSHA, "-S", "-m", "leaves conflict markers in hello.txt")
			if err != nil {
				t.Fatalf("git commit-tree: %v: %s", err, commitOut)
			}
			sha := strings.TrimSpace(string(commitOut))
			if out, resetErr := gitfixture.Git(t.Context(), dir, "reset", "--hard", sha); resetErr != nil {
				t.Fatalf("git reset --hard: %v: %s", resetErr, out)
			}
		},
		"conflict markers remain")
}

// TestMergeAlreadyMergedClosesRequest proves StartBaseMerge's own
// ErrAlreadyMerged branch (overview design "Request lifecycle"): a request
// whose own base sha is already an ancestor of the ticket branch -- the
// ordinary shape right after POLL writes the very first request, before
// main ever diverges a second time -- closes with no git merge and no
// agent run, and ClearPoll, so the next tick runs POLL again.
func TestMergeAlreadyMergedClosesRequest(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := mergePublished(t)
	rt := runtime.NewFake(fstest.MapFS{})

	pollCommit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if pollCommit.Escalation != nil {
		t.Fatalf("poll escalated: %+v", pollCommit.Escalation.Payload)
	}
	pbApply(t, s, ticket, pollCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	req, open, err := openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge: %v", err)
	}
	if !open {
		t.Fatal("openBaseMerge: no open request after POLL")
	}

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("merge tick: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("merge tick escalated: %+v", commit.Escalation.Payload)
	}
	wantClosed := fmt.Sprintf("base merge closed %d", req.MessageID)
	if len(commit.Messages) != 1 || commit.Messages[0].Body != wantClosed {
		t.Errorf("commit.Messages = %+v, want exactly one message %q", commit.Messages, wantClosed)
	}
	if !commit.ClearPoll {
		t.Error("commit.ClearPoll = false, want true")
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	_, stillOpen, err := openBaseMerge(t.Context(), ticket, Deps{Store: s})
	if err != nil {
		t.Fatalf("openBaseMerge (after close): %v", err)
	}
	if stillOpen {
		t.Error("openBaseMerge: a request is still open after the close marker, want none")
	}

	// The next tick runs POLL again (driveOpenMerge finds nothing open):
	// the same dirty PR opens a fresh request rather than idling forever.
	next, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("poll (after close): %v", err)
	}
	if next.Escalation != nil {
		t.Fatalf("poll (after close) escalated: %+v", next.Escalation.Payload)
	}
	wantWhat := "PR #1 conflicts with " + pbFixtureDefaultBranch
	if !shipHasMessage(next, wantWhat) {
		t.Errorf("commit.Messages = %+v, want a fresh %q notice", next.Messages, wantWhat)
	}
}
