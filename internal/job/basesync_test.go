// basesync_test.go tests task 3: baseSync's own review point
// (reviewingHandler.enterRound), wired through the post-build prelude's own
// new driveOpenMerge call. It reuses reviewing_test.go's own
// reviewTicketReady and reviewScriptsFS, and merge_test.go's own
// mergeCommitOnMain and shipHasMergeLanded, all package job. Task 5 adds the
// CI point (shipHandler.pollCIFailed), reusing shipping_test.go's own
// shipTicketReady, shipGitHub, shipTracker, shipFailedCI and shipHeadSHA,
// and merge_test.go's own shipClaim and mergeRunTick.
package job

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"zing/internal/gitfixture"
	"zing/internal/orchestrator"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// basesyncAddOrigin adds a bare origin remote to ticket's project and
// pushes its current default branch there, so a later FetchBase performs a
// real fetch (and so can really advance refs/zing/base/<default>) instead
// of falling back to the no-origin local-main seed that never moves again
// once seeded (orchestrator.fetchBase's own "no_origin" branch).
func basesyncAddOrigin(t *testing.T, s *store.Store, ticket store.Ticket) (remoteDir string) {
	t.Helper()
	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	remoteDir, err = gitfixture.WithBareOrigin(t.Context(), proj.LocalPath)
	if err != nil {
		t.Fatalf("gitfixture.WithBareOrigin: %v", err)
	}
	if out, pushErr := gitfixture.Git(t.Context(), proj.LocalPath, "push", "origin", pbFixtureDefaultBranch); pushErr != nil {
		t.Fatalf("git push origin %s: %v: %s", pbFixtureDefaultBranch, pushErr, out)
	}
	return remoteDir
}

// basesyncTicketWorktree returns ticket's own real worktree, through the
// same never-called-GitHub orchestrator pbOrchestratorFor already builds
// (postbuild_test.go).
func basesyncTicketWorktree(t *testing.T, s *store.Store, ticket store.Ticket) (orch *orchestrator.Orchestrator, wt orchestrator.Worktree) {
	t.Helper()
	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	orch, _, ok := pbOrchestratorFor(t, proj.LocalPath, orchestrator.NewRunner())
	if !ok {
		t.Fatal("pbOrchestratorFor: not ok")
	}
	wt, _, err = orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	return orch, wt
}

// basesyncNoticePrefix is the start of every syncNotice's own review line
// (basesync.go), named once so the three tests below never repeat the
// literal.
const basesyncNoticePrefix = "Merging " + pbFixtureDefaultBranch + " at"

// basesyncFindMessage returns the first of c.Messages whose Body has
// prefix, so a test can check a tick wrote (or did not write) the notice
// or the request marker it expects.
func basesyncFindMessage(c store.HandlerCommit, prefix string) (string, bool) {
	for i := range c.Messages {
		if strings.HasPrefix(c.Messages[i].Body, prefix) {
			return c.Messages[i].Body, true
		}
	}
	return "", false
}

// basesyncTick runs one tick of h.Run, fails the test if it escalates,
// applies the commit, and returns the refreshed ticket plus the commit
// itself. Shared by the review and judge point tests below (h is
// reviewingHandler{} or judgeHandler{}); mergeRunTick (merge_test.go) is
// the same shape again, for shipping's handler.
func basesyncTick(t *testing.T, s *store.Store, deps Deps, ticket store.Ticket, h Handler, label string) (store.Ticket, store.HandlerCommit) {
	t.Helper()
	commit, err := h.Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if commit.Escalation != nil {
		t.Fatalf("%s escalated: %+v", label, commit.Escalation.Payload)
	}
	pbApply(t, s, ticket, commit)
	return pbGetTicket(t, s, ticket.ID), commit
}

// TestBaseSyncBeforeReviewMergesOverlap proves the review point (overview
// design, first integration test): while the ticket is building, main
// changes hello.txt with exactly the content the ticket branch's own
// hello.txt already carries (an "add/add, identical content" merge, so it
// lands with no conflict and no agent run) and an unrelated other.txt.
// Entering reviewing opens a base merge request with point review before
// round 1 ever reads the diff; once the request lands, a signed merge
// commit records it, and the next tick runs review round 1 for real.
func TestBaseSyncBeforeReviewMergesOverlap(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, beforeRunID := reviewTicketReady(t)
	basesyncAddOrigin(t, s, ticket)

	_, wt := basesyncTicketWorktree(t, s, ticket)
	preMergeHead := shipHeadSHA(t, s, ticket)
	helloContent, err := gitfixture.Git(t.Context(), wt.Dir(), "show", "HEAD:hello.txt")
	if err != nil {
		t.Fatalf("git show HEAD:hello.txt: %v", err)
	}

	mergeCommitOnMain(t, s, ticket, "hello.txt", helloContent)
	baseSHA := mergeCommitOnMain(t, s, ticket, "other.txt", []byte("main only\n"))

	rt := runtime.NewFake(reviewScriptsFS(nil))

	deps := pbClaim(t, s, rt, ticket.ID)
	commit1, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if commit1.Escalation != nil {
		t.Fatalf("tick 1 escalated: %+v", commit1.Escalation.Payload)
	}
	if commit1.Next != "" {
		t.Errorf("tick 1 commit.Next = %q, want none (the merge runs before round 1)", commit1.Next)
	}
	if _, found := basesyncFindMessage(commit1, basesyncNoticePrefix); !found {
		t.Errorf("tick 1 commit.Messages = %+v, want a message starting %q", commit1.Messages, basesyncNoticePrefix)
	}
	reqBody, found := basesyncFindMessage(commit1, "base merge requested after run ")
	if !found {
		t.Fatalf("tick 1 commit.Messages = %+v, want a base merge request", commit1.Messages)
	}
	req, err := parseBaseMergeRequest(store.MessageRow{ID: 1, Body: reqBody})
	if err != nil {
		t.Fatalf("parseBaseMergeRequest: %v", err)
	}
	if req.Point != syncPointReview {
		t.Errorf("req.Point = %q, want %q", req.Point, syncPointReview)
	}
	if req.BaseSHA != baseSHA {
		t.Errorf("req.BaseSHA = %s, want %s", req.BaseSHA, baseSHA)
	}

	afterRunID, err := s.MaxRunID(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	if afterRunID != beforeRunID {
		t.Errorf("MaxRunID moved from %d to %d; review round 1 must not run before the merge lands", beforeRunID, afterRunID)
	}

	pbApply(t, s, ticket, commit1)
	ticket = pbGetTicket(t, s, ticket.ID)

	landed := false
	var last store.HandlerCommit
	for i := 0; i < 4 && !landed; i++ {
		deps = pbClaim(t, s, rt, ticket.ID)
		ticket, last = basesyncTick(t, s, deps, ticket, reviewingHandler{}, "merge tick")
		landed = shipHasMergeLanded(last)
	}
	if !landed {
		t.Fatal("base merge did not land within 4 ticks")
	}

	proj := deps.Projects[ticket.ProjectID]
	mergeSHA := shipHeadSHA(t, s, ticket)
	parents, err := proj.Orch.CommitParents(t.Context(), wt, mergeSHA)
	if err != nil {
		t.Fatalf("CommitParents: %v", err)
	}
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

	sessions, err := s.SessionsForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	for _, sess := range sessions {
		if sess.Job == jobMergeName {
			t.Errorf("sessions = %+v, want no %q session (a clean merge runs no agent)", sessions, jobMergeName)
		}
	}

	deps = pbClaim(t, s, rt, ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("tick after landed: %v", err)
	}
	if commit2.Escalation != nil {
		t.Fatalf("tick after landed escalated: %+v", commit2.Escalation.Payload)
	}
	if commit2.Next != stateJudging {
		t.Errorf("commit.Next = %q, want %q (review round 1 ran after the merge)", commit2.Next, stateJudging)
	}
}

// TestBaseSyncBeforeJudgeRoundMerges proves the judge point (overview
// design, judgeStartOrSync): after the ticket has entered judging, main
// changes hello.txt with exactly the content the ticket branch already
// carries (a clean merge) and an unrelated other.txt. START opens a base
// merge request with point judge before it ever writes round 1's own
// started marker; once the request lands, the next tick writes "judge
// round 1 started" at the merged sha.
func TestBaseSyncBeforeJudgeRoundMerges(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	basesyncAddOrigin(t, s, ticket)

	orch, wt := basesyncTicketWorktree(t, s, ticket)
	helloContent, err := gitfixture.Git(t.Context(), wt.Dir(), "show", "HEAD:hello.txt")
	if err != nil {
		t.Fatalf("git show HEAD:hello.txt: %v", err)
	}

	mergeCommitOnMain(t, s, ticket, "hello.txt", helloContent)
	baseSHA := mergeCommitOnMain(t, s, ticket, "other.txt", []byte("main only\n"))

	rt := runtime.NewFake(judgeScriptsFS())

	deps := pbClaim(t, s, rt, ticket.ID)
	commit1, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if commit1.Escalation != nil {
		t.Fatalf("tick 1 escalated: %+v", commit1.Escalation.Payload)
	}
	if _, found := basesyncFindMessage(commit1, "judge round 1 started"); found {
		t.Errorf("tick 1 commit.Messages = %+v, want no started marker (the merge runs first)", commit1.Messages)
	}
	wantNoticePrefix := "Merging " + pbFixtureDefaultBranch + " at " + baseSHA[:7] + " before judging:"
	if _, found := basesyncFindMessage(commit1, wantNoticePrefix); !found {
		t.Errorf("tick 1 commit.Messages = %+v, want a message starting %q", commit1.Messages, wantNoticePrefix)
	}
	reqBody, found := basesyncFindMessage(commit1, "base merge requested after run ")
	if !found {
		t.Fatalf("tick 1 commit.Messages = %+v, want a base merge request", commit1.Messages)
	}
	req, err := parseBaseMergeRequest(store.MessageRow{ID: 1, Body: reqBody})
	if err != nil {
		t.Fatalf("parseBaseMergeRequest: %v", err)
	}
	if req.Point != syncPointJudge {
		t.Errorf("req.Point = %q, want %q", req.Point, syncPointJudge)
	}
	if req.BaseSHA != baseSHA {
		t.Errorf("req.BaseSHA = %s, want %s", req.BaseSHA, baseSHA)
	}

	pbApply(t, s, ticket, commit1)
	ticket = pbGetTicket(t, s, ticket.ID)

	landed := false
	var last store.HandlerCommit
	for i := 0; i < 4 && !landed; i++ {
		deps = pbClaim(t, s, rt, ticket.ID)
		ticket, last = basesyncTick(t, s, deps, ticket, judgeHandler{}, "merge tick")
		landed = shipHasMergeLanded(last)
	}
	if !landed {
		t.Fatal("base merge did not land within 4 ticks")
	}

	mergedSHA, err := orch.HeadSHA(t.Context(), wt)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	parents, err := orch.CommitParents(t.Context(), wt, mergedSHA)
	if err != nil {
		t.Fatalf("CommitParents: %v", err)
	}
	if len(parents) != 2 {
		t.Errorf("CommitParents(%s) = %v, want 2 parents", mergedSHA, parents)
	}

	deps = pbClaim(t, s, rt, ticket.ID)
	commit2, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("tick after landed: %v", err)
	}
	if commit2.Escalation != nil {
		t.Fatalf("tick after landed escalated: %+v", commit2.Escalation.Payload)
	}
	wantPrefix := "judge round 1 started sha " + mergedSHA
	if _, found := basesyncFindMessage(commit2, wantPrefix); !found {
		t.Fatalf("tick after landed commit.Messages = %+v, want a message starting %q", commit2.Messages, wantPrefix)
	}
}

// TestBaseSyncBeforeJudgeRoundAfterFailedRound proves the judge point's
// other call site: judging.go's failed-round branch, which restarts round
// m+1 after round m's own "failure" fix lands. Round 1 fails, its fix
// lands (moving HEAD), main then changes hello.txt (with the content the
// branch's own fix already left there, so the merge is clean) while an
// unrelated other.txt also moves. The branch that would otherwise write
// "judge round 2 started" opens a base merge request with point judge
// first, and only once it lands does round 2 actually start, at the
// merged sha.
func TestBaseSyncBeforeJudgeRoundAfterFailedRound(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	basesyncAddOrigin(t, s, ticket)

	scripts := judgeScriptsFS(judgeOkBothScript)
	scripts["build/fix/1.xml"] = &fstest.MapFile{Data: []byte(judgeFixBuildScript)}
	rt := runtime.NewFake(scripts)

	checks := &judgeScriptedCheckCommands{real: NewCommandRunner(sandbox.Off(), false), steps: []judgeCheckStep{{exit: 1}}}

	ticket = judgeFailRoundOne(t, s, ticket, rt, checks) // START, RUN, CHECK (exit 1), EVALUATE round 1
	driveJudgeFixToLanding(t, s, ticket.ID, rt, checks, judgeFixTestCmd)
	ticket = pbGetTicket(t, s, ticket.ID)
	if ticket.State != stateJudging {
		t.Fatalf("after the fix landed: ticket state = %q, want judging", ticket.State)
	}

	orch, wt := basesyncTicketWorktree(t, s, ticket)
	helloContent, err := gitfixture.Git(t.Context(), wt.Dir(), "show", "HEAD:hello.txt")
	if err != nil {
		t.Fatalf("git show HEAD:hello.txt: %v", err)
	}
	mergeCommitOnMain(t, s, ticket, "hello.txt", helloContent)
	baseSHA := mergeCommitOnMain(t, s, ticket, "other.txt", []byte("main only\n"))

	// From here on the ticket's own default test command runs (not
	// judgeFixTestCmd, which appends to hello.txt on every call and would
	// dirty the tree during the merge's own CHECK with a change outside
	// the merge).
	deps := pbClaim(t, s, rt, ticket.ID)
	commit1, err := (judgeHandler{}).Run(t.Context(), ticket, deps) // round 1 failed branch: round 2 start attempt
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if commit1.Escalation != nil {
		t.Fatalf("tick 1 escalated: %+v", commit1.Escalation.Payload)
	}
	if _, found := basesyncFindMessage(commit1, "judge round 2 started"); found {
		t.Errorf("tick 1 commit.Messages = %+v, want no started marker (the merge runs first)", commit1.Messages)
	}
	reqBody, found := basesyncFindMessage(commit1, "base merge requested after run ")
	if !found {
		t.Fatalf("tick 1 commit.Messages = %+v, want a base merge request", commit1.Messages)
	}
	req, err := parseBaseMergeRequest(store.MessageRow{ID: 1, Body: reqBody})
	if err != nil {
		t.Fatalf("parseBaseMergeRequest: %v", err)
	}
	if req.Point != syncPointJudge {
		t.Errorf("req.Point = %q, want %q", req.Point, syncPointJudge)
	}
	if req.BaseSHA != baseSHA {
		t.Errorf("req.BaseSHA = %s, want %s", req.BaseSHA, baseSHA)
	}

	pbApply(t, s, ticket, commit1)
	ticket = pbGetTicket(t, s, ticket.ID)

	landed := false
	var last store.HandlerCommit
	for i := 0; i < 4 && !landed; i++ {
		deps = pbClaim(t, s, rt, ticket.ID)
		ticket, last = basesyncTick(t, s, deps, ticket, judgeHandler{}, "merge tick")
		landed = shipHasMergeLanded(last)
	}
	if !landed {
		t.Fatal("base merge did not land within 4 ticks")
	}

	mergedSHA, err := orch.HeadSHA(t.Context(), wt)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	deps = pbClaim(t, s, rt, ticket.ID)
	commit2, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("tick after landed: %v", err)
	}
	if commit2.Escalation != nil {
		t.Fatalf("tick after landed escalated: %+v", commit2.Escalation.Payload)
	}
	wantPrefix := "judge round 2 started sha " + mergedSHA
	if _, found := basesyncFindMessage(commit2, wantPrefix); !found {
		t.Fatalf("tick after landed commit.Messages = %+v, want a message starting %q", commit2.Messages, wantPrefix)
	}
}

// TestBaseSyncBeforeReviewSkipsUnrelated proves the review point's own
// overlap condition (overview design's second integration test): main
// changes only a path the ticket never touched, so no merge opens and
// review round 1 runs on the unmerged branch.
func TestBaseSyncBeforeReviewSkipsUnrelated(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	basesyncAddOrigin(t, s, ticket)

	preHead := shipHeadSHA(t, s, ticket)
	mergeCommitOnMain(t, s, ticket, "other.txt", []byte("main only\n"))

	rt := runtime.NewFake(reviewScriptsFS(nil))
	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("Run escalated: %+v", commit.Escalation.Payload)
	}
	if commit.Next != stateJudging {
		t.Errorf("commit.Next = %q, want %q", commit.Next, stateJudging)
	}
	if body, found := basesyncFindMessage(commit, "base merge requested after run "); found {
		t.Errorf("commit.Messages has a base merge request %q, want none", body)
	}

	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)
	if got := shipHeadSHA(t, s, ticket); got != preHead {
		t.Errorf("HEAD = %s, want unchanged %s (no merge happened)", got, preHead)
	}
}

// TestBaseSyncBeforeReviewSkipsAtPointLimit proves the review point's own
// jobs.merge.max_loops budget (owner's Q2 decision): with two closed
// point-review requests already on the ticket, the point logs and skips
// even though main has since moved under a shared path, and review round 1
// runs on the unmerged branch.
func TestBaseSyncBeforeReviewSkipsAtPointLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	basesyncAddOrigin(t, s, ticket)

	for _, sha := range []string{strings.Repeat("3", 40), strings.Repeat("4", 40)} {
		req := baseMergeRequest{BaseBranch: pbFixtureDefaultBranch, BaseSHA: sha, Point: syncPointReview}
		id, err := s.InsertMessage(t.Context(), store.Message{
			TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem, Body: req.body(),
		})
		if err != nil {
			t.Fatalf("InsertMessage(request): %v", err)
		}
		if _, err := s.InsertMessage(t.Context(), store.Message{
			TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: baseMergeClosedBody(id),
		}); err != nil {
			t.Fatalf("InsertMessage(closed): %v", err)
		}
	}

	_, wt := basesyncTicketWorktree(t, s, ticket)
	helloContent, err := gitfixture.Git(t.Context(), wt.Dir(), "show", "HEAD:hello.txt")
	if err != nil {
		t.Fatalf("git show HEAD:hello.txt: %v", err)
	}
	mergeCommitOnMain(t, s, ticket, "hello.txt", helloContent)

	rt := runtime.NewFake(reviewScriptsFS(nil))
	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("Run escalated: %+v", commit.Escalation.Payload)
	}
	if commit.Next != stateJudging {
		t.Errorf("commit.Next = %q, want %q", commit.Next, stateJudging)
	}
	if body, found := basesyncFindMessage(commit, "base merge requested after run "); found {
		t.Errorf("commit.Messages has a base merge request %q, want none (the point is at its limit)", body)
	}
}

// TestBaseSyncOnCIFailureMergesBeforeFix proves the CI point (overview
// design, owner's Q1 note): once CI reports failed, pollCIFailed merges a
// moved main first -- with no shared-path condition, unlike review and
// judge -- and asks for a ci_log fix only once CI still fails after the
// merge lands, matching the #91/PR #179 case the ticket names.
func TestBaseSyncOnCIFailureMergesBeforeFix(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, remoteDir := shipTicketReady(t)

	gh := &shipGitHub{}
	tr := &shipTracker{}
	rt := runtime.NewFake(fstest.MapFS{})

	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	publishCommit, err := (shipHandler{}).Run(t.Context(), ticket, deps) // PUBLISH
	if err != nil {
		t.Fatalf("PUBLISH: %v", err)
	}
	if publishCommit.Escalation != nil {
		t.Fatalf("PUBLISH escalated: %+v", publishCommit.Escalation.Payload)
	}
	pbApply(t, s, ticket, publishCommit)
	ticket = pbGetTicket(t, s, ticket.ID)
	preMergeHead := shipHeadSHA(t, s, ticket)

	baseSHA := mergeCommitOnMain(t, s, ticket, "other.txt", []byte("main only\n"))

	runs, required := shipFailedCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: preMergeHead, BaseRef: pbFixtureDefaultBranch}
	gh.logTail = func(context.Context, string, string, int64, int) (string, error) { return shipCILogTailText, nil }

	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	commit1, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if commit1.Escalation != nil {
		t.Fatalf("tick 1 escalated: %+v", commit1.Escalation.Payload)
	}
	if !commit1.ClearPoll {
		t.Error("tick 1 ClearPoll = false, want true")
	}
	if body, found := basesyncFindMessage(commit1, fixRequestedCILogPrefix); found {
		t.Errorf("tick 1 commit.Messages has a %q marker %q, want none (the merge runs first)", fixRequestedCILogPrefix, body)
	}
	reqBody, found := basesyncFindMessage(commit1, "base merge requested after run ")
	if !found {
		t.Fatalf("tick 1 commit.Messages = %+v, want a base merge request", commit1.Messages)
	}
	req, err := parseBaseMergeRequest(store.MessageRow{ID: 1, Body: reqBody})
	if err != nil {
		t.Fatalf("parseBaseMergeRequest: %v", err)
	}
	if req.Point != syncPointCI {
		t.Errorf("req.Point = %q, want %q", req.Point, syncPointCI)
	}
	if req.BaseSHA != baseSHA {
		t.Errorf("req.BaseSHA = %s, want %s", req.BaseSHA, baseSHA)
	}
	wantNotice := syncNotice(req, nil)
	foundNotice := false
	for _, m := range commit1.Messages {
		if m.Body == wantNotice {
			foundNotice = true
		}
	}
	if !foundNotice {
		t.Errorf("tick 1 commit.Messages = %+v, want a message equal to %q", commit1.Messages, wantNotice)
	}

	pbApply(t, s, ticket, commit1)
	ticket = pbGetTicket(t, s, ticket.ID)

	landed := false
	var last store.HandlerCommit
	for i := 0; i < 4 && !landed; i++ {
		deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
		ticket, last = mergeRunTick(t, s, deps, ticket, fmt.Sprintf("merge tick %d", i))
		landed = shipHasMergeLanded(last)
	}
	if !landed {
		t.Fatal("base merge did not land within 4 ticks")
	}
	mergedSHA := shipHeadSHA(t, s, ticket)

	// gh.prState.HeadSHA is still the pre-merge sha: the next POLL pushes
	// the merged branch to the pull request, the same head-mismatch path
	// every other POLL push takes.
	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	pushCommit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("push tick: %v", err)
	}
	if pushCommit.Escalation != nil {
		t.Fatalf("push tick escalated: %+v", pushCommit.Escalation.Payload)
	}
	pbApply(t, s, ticket, pushCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	out, err := gitfixture.Git(t.Context(), remoteDir, "rev-parse", "refs/heads/"+*ticket.Branch)
	if err != nil {
		t.Fatalf("git rev-parse refs/heads/%s in origin: %v", *ticket.Branch, err)
	}
	if got := strings.TrimSpace(string(out)); got != mergedSHA {
		t.Errorf("origin's %s = %s, want the merged sha %s", *ticket.Branch, got, mergedSHA)
	}

	// CI still fails at the merged head: the next POLL writes the ci_log
	// fix request, carrying the log tail, and opens no second base merge
	// request (the base is already an ancestor of HEAD).
	gh.prState.HeadSHA = mergedSHA
	deps = shipClaim(t, s, rt, ticket.ID, gh, tr)
	commit2, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("tick after merged: %v", err)
	}
	if commit2.Escalation != nil {
		t.Fatalf("tick after merged escalated: %+v", commit2.Escalation.Payload)
	}
	fixBody, found := basesyncFindMessage(commit2, fixRequestedCILogPrefix)
	if !found {
		t.Fatalf("tick after merged commit.Messages = %+v, want a %q marker", commit2.Messages, fixRequestedCILogPrefix)
	}
	if !strings.Contains(fixBody, shipCILogTailText) {
		t.Errorf("fix request body = %q, want it to contain %q", fixBody, shipCILogTailText)
	}
	if body, found := basesyncFindMessage(commit2, "base merge requested after run "); found {
		t.Errorf("tick after merged commit.Messages has a base merge request %q, want none", body)
	}
}
