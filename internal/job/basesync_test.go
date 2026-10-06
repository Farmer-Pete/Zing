// basesync_test.go tests task 3: baseSync's own review point
// (reviewingHandler.enterRound), wired through the post-build prelude's own
// new driveOpenMerge call. It reuses reviewing_test.go's own
// reviewTicketReady and reviewScriptsFS, and merge_test.go's own
// mergeCommitOnMain and shipHasMergeLanded, all package job.
package job

import (
	"strings"
	"testing"

	"zing/internal/gitfixture"
	"zing/internal/orchestrator"
	"zing/internal/runtime"
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

// basesyncReviewTick runs one reviewingHandler.Run tick, fails the test if
// it escalates, applies the commit, and returns the refreshed ticket plus
// the commit itself (mergeRunTick's own shape, merge_test.go, for
// reviewing's handler instead of shipping's).
func basesyncReviewTick(t *testing.T, s *store.Store, deps Deps, ticket store.Ticket, label string) (store.Ticket, store.HandlerCommit) {
	t.Helper()
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
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
		ticket, last = basesyncReviewTick(t, s, deps, ticket, "merge tick")
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
