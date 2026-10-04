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

// ---- task 6: mergeCheck's own no-run branch, the outside-the-merge read,
// and the check_loops/max_resumes gates --------------------------------

// mergeOtherTxt is a path main's own commit touches that the ticket branch
// never changes (mergeConflictOnMain, despite its name, is just "a commit
// on main"): merging it leaves no conflict at all.
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
	mergeConflictOnMain(t, s, ticket, mergeOtherTxt, []byte("main only\n"))

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
	mergeConflictOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

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
	mergeConflictOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

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
	mergeConflictOnMain(t, s, ticket, "other2.txt", []byte("main only, again\n"))

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
	if !strings.HasPrefix(tried, "base merge ") {
		t.Errorf("Tried = %q, want prefix %q", tried, "base merge ")
	}
	if !strings.Contains(tried, "test command: false") {
		t.Errorf("Tried = %q, want the failing test command's own output", tried)
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
	mergeConflictOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

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
	mergeConflictOnMain(t, s, ticket, "hello.txt", []byte(mergeHelloConflict))

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
