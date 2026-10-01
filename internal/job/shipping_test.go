package job

// shipping_test.go tests M3 task 6's PUBLISH (design section 8.2),
// shipHandler's own first step. A real git fixture (gitfixture,
// WithBareOrigin) stands in for the ticket's branch and its GitHub remote;
// shipGitHub is a configurable, stateful orchestrator.GitHub double for
// OpenDraftPR's own create-then-find idempotency (a second CreateDraftPR
// against a head that already has an open PR fails, the same way GitHub
// itself refuses a duplicate, so a test can replay PUBLISH without a
// special case); shipTracker is a configurable ShipTracker double for
// PostPRLink. shipTicketReady drives a ticket through the real planning,
// building, reviewing, and judging state machines (reusing judging_test.go's
// own judgeTicketReady and judge-round helpers, same package) to a real
// "judge round 1 passed" commit on a real worktree, exactly the state
// PUBLISH expects; shipSeedPassedRound builds the same marker and verdict
// shape directly, for the three finalVerdicts anomaly tests the real judge
// flow can never produce on its own (CheckCoverage and JudgePasses already
// refuse an incomplete or unknown cohort before EVALUATE ever writes
// "passed").

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"zing/internal/gitfixture"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// Compile-time assertions: *orchestrator.GitHubClient satisfies every
// interface shipping.go declares, so serve can fill job.Project.PullRequests,
// job.Project.Flips, and job.Project.Checks from the one shared client
// (PKG9-PLAN.md section 10.3). They live here, not in
// internal/orchestrator/github_test.go: that file's tests are internal to
// package orchestrator, and package job already imports package
// orchestrator in its regular code, so an internal orchestrator test
// importing job would be an import cycle. respond_test.go holds the matching
// assertion for ReviewThreads, declared in respond.go.
var (
	_ PullRequests = (*orchestrator.GitHubClient)(nil)
	_ DraftFlips   = (*orchestrator.GitHubClient)(nil)
	_ Checks       = (*orchestrator.GitHubClient)(nil)
)

// TestGitHubClientSatisfiesInterfaces documents the var block above: a
// failure here is a compile failure, not a test failure, so the body only
// has to exist.
func TestGitHubClientSatisfiesInterfaces(t *testing.T) {
	t.Parallel()
}

// ---- harness: a configurable fake GitHub and tracker ----------------------

// shipPreCrashTitle is the only title the three "pre-crash" OpenDraftPR
// calls below need: they stand in for a tick that crashed right after
// opening the pull request, before its own commit, so nothing reads this
// title back.
const shipPreCrashTitle = "pre-crash title"

// shipGitHubPR is one pull request shipGitHub has created, keyed by head
// and base so FindPRByHead's own filter (PKG9-PLAN.md section 10.3) can be
// honored for real.
type shipGitHubPR struct {
	url, head, base string
	number          int
}

// shipGitHub is a configurable, stateful orchestrator.GitHub double. Unlike
// every other package's own never-called GitHub stub (pbGitHub, dispatchTestGitHub,
// resumeE2EGitHub, selftestGitHub), CreateDraftPR and FindPRByHead here are
// real enough to exercise OpenDraftPR's own idempotent fallback (Package 5):
// a second CreateDraftPR against a head that already has an open PR fails
// with errShipGitHubPRExists, the same shape a real "a pull request already
// exists" refusal takes, so FindPRByHead is what a retried OpenDraftPR call
// actually reaches, exactly as a crash-and-retry tick would for real.
type shipGitHub struct {
	createErr error // when set, CreateDraftPR always fails with this instead
	findErr   error

	pr       *shipGitHubPR // nil until a PR has been created
	nextNum  int
	creates  int
	finds    int
	lastBody string // the body the newest CreateDraftPR call carried
}

var (
	errShipGitHub         = errors.New("shipGitHub: not implemented")
	errShipGitHubPRExists = errors.New("shipGitHub: a pull request already exists for this head")
)

func (g *shipGitHub) RepoDefaultBranch(context.Context, string, string) (string, error) {
	return "", errShipGitHub
}

func (g *shipGitHub) RequiredChecks(context.Context, string, string, string) ([]string, error) {
	return nil, errShipGitHub
}

func (g *shipGitHub) CreateDraftPR(_ context.Context, _, _, head, base, _, body string) (url string, number int, err error) {
	g.creates++
	g.lastBody = body
	if g.createErr != nil {
		return "", 0, g.createErr
	}
	if g.pr != nil {
		return "", 0, errShipGitHubPRExists
	}
	g.nextNum++
	g.pr = &shipGitHubPR{url: fmt.Sprintf("https://github.com/fixture/fixture/pull/%d", g.nextNum), head: head, base: base, number: g.nextNum}
	return g.pr.url, g.pr.number, nil
}

func (g *shipGitHub) FindPRByHead(_ context.Context, _, _, head, base string) (url string, number int, ok bool, err error) {
	g.finds++
	if g.findErr != nil {
		return "", 0, false, g.findErr
	}
	if g.pr == nil || g.pr.head != head || g.pr.base != base {
		return "", 0, false, nil
	}
	return g.pr.url, g.pr.number, true, nil
}

// shipTracker is a configurable ShipTracker double: PostPRLink posts at
// most once, mirroring the dispatcher's own hidden-marker guard
// (internal/dispatch/dispatch.go's postMarkedOnce), so a test can call
// PUBLISH twice and assert the comment never duplicates.
type shipTracker struct {
	prLinkErr   error
	prPosted    bool
	prPostCount int

	doneErr    error
	donePosted bool
}

func (tr *shipTracker) PostPRLink(_ context.Context, _ int64, _, _ string) error {
	if tr.prLinkErr != nil {
		return tr.prLinkErr
	}
	if !tr.prPosted {
		tr.prPosted = true
		tr.prPostCount++
	}
	return nil
}

func (tr *shipTracker) PostDone(_ context.Context, _ int64, _, _ string) error {
	if tr.doneErr != nil {
		return tr.doneErr
	}
	tr.donePosted = true
	return nil
}

// shipOrchestratorFor builds an *orchestrator.Orchestrator over localPath
// wired to gh (pbOrchestratorFor's own twin, postbuild_test.go, which
// hardcodes pbGitHub{} and so cannot stand in for PUBLISH's own real
// OpenDraftPR call).
func shipOrchestratorFor(t *testing.T, localPath string, gh orchestrator.GitHub) (orch *orchestrator.Orchestrator, repoGit string, ok bool) {
	t.Helper()
	orch, orchErr := orchestrator.New(
		orchestrator.Project{Owner: pbFixtureOwner, Repo: pbFixtureOwner, LocalPath: localPath, DefaultBranch: pbFixtureDefaultBranch},
		gh, orchestrator.NewRunner(), nil)
	if orchErr != nil {
		return nil, "", false
	}
	repoGit, gitErr := orch.GitCommonDir(t.Context())
	if gitErr != nil {
		return nil, "", false
	}
	return orch, repoGit, true
}

// shipBuildProjects is pbBuildProjects' own twin, wired to gh instead of
// pbGitHub{}.
func shipBuildProjects(t *testing.T, s *store.Store, gh orchestrator.GitHub) map[int64]Project {
	t.Helper()
	projects, err := s.ListProjects(t.Context())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	out := make(map[int64]Project, len(projects))
	for _, p := range projects {
		orch, repoGit, ok := shipOrchestratorFor(t, p.LocalPath, gh)
		if !ok {
			continue
		}
		out[p.ID] = Project{Orch: orch, RepoGit: repoGit, TestCmd: "test -f " + pbHelloTxt, LintCmd: pbNoopShellCmd}
	}
	return out
}

// shipClaim is pbClaim's own twin (postbuild_test.go), wired to gh and tr
// instead of a never-called GitHub stub and a nil Tracker: the only shape
// PUBLISH's own tests need pbClaim does not already give them.
func shipClaim(t *testing.T, s *store.Store, rt runtime.Runtime, ticketID int64, gh orchestrator.GitHub, tr ShipTracker) Deps {
	t.Helper()
	owner := "ship-test-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)

	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}

	set, err := runtime.NewSet(map[string]runtime.Runtime{pbRuntimeClaude: rt, pbRuntimeCodex: rt, runtimeFake: rt})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}
	return Deps{
		Store: s, Runtimes: set, Machine: pbMachine(t), Models: pbModels, Budget: pbBudget, Floor: pbFloor,
		Owner: owner, Expires: expires,
		Reserve: func(ctx context.Context, ticketID int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
			return s.Reserve(ctx, ticketID, owner, expires, su, seed)
		},
		Sandboxes: sandbox.OffSet(), RequireSandbox: false,
		Commands:       NewCommandRunner(sandbox.Off(), false),
		Projects:       shipBuildProjects(t, s, gh),
		DataDir:        t.TempDir(),
		LensesParallel: 7,
		Tracker:        tr,
	}
}

// shipReleaseClaim releases the claim deps was built under (a no-op
// HandlerCommit carrying only the fence, dispatch.go's own releaseClaim),
// so a test that drives the orchestrator directly through deps' own
// Projects, to simulate a tick that "crashed" before ever reaching a real
// commit, can still claim ticketID again for the real tick that follows.
func shipReleaseClaim(t *testing.T, s *store.Store, ticketID int64, deps Deps) {
	t.Helper()
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{TicketID: ticketID, Owner: deps.Owner, Expires: deps.Expires})
	if err != nil || !applied {
		t.Fatalf("shipReleaseClaim: applied=%v err=%v", applied, err)
	}
}

// shipTicketReady drives a ticket all the way to "shipping" through the
// real planning, building, reviewing, and judging state machines (reusing
// judging_test.go's own judgeTicketReady, judgeAdvanceStart,
// judgeOkBothScript, and judgeScriptedCheckCommands, same package), exactly
// TestJudgePassMovesToShipping's own sequence, then adds a bare git remote
// (gitfixture.WithBareOrigin) so a real Push/OpenDraftPR has somewhere to
// land. remoteDir is that bare repository's own directory.
func shipTicketReady(t *testing.T) (s *store.Store, ticket store.Ticket, remoteDir string) {
	t.Helper()
	s, judgingTicket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	judgingTicket = judgeAdvanceStart(t, s, rt, judgingTicket) // START round 1

	deps := pbClaim(t, s, rt, judgingTicket.ID)
	runCommit, err := (judgeHandler{}).Run(t.Context(), judgingTicket, deps) // RUN
	if err != nil {
		t.Fatalf("shipTicketReady: judge RUN: %v", err)
	}
	pbApply(t, s, judgingTicket, runCommit)

	checks := &judgeScriptedCheckCommands{steps: []judgeCheckStep{{exit: 0}}}
	deps2 := pbClaim(t, s, rt, judgingTicket.ID)
	deps2.Commands = checks
	checkCommit, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, judgingTicket.ID), deps2) // CHECK
	if err != nil {
		t.Fatalf("shipTicketReady: judge CHECK: %v", err)
	}
	pbApply(t, s, judgingTicket, checkCommit)

	deps3 := pbClaim(t, s, rt, judgingTicket.ID)
	evalCommit, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, judgingTicket.ID), deps3) // EVALUATE: pass
	if err != nil {
		t.Fatalf("shipTicketReady: judge EVALUATE: %v", err)
	}
	pbApply(t, s, judgingTicket, evalCommit)

	ticket = pbGetTicket(t, s, judgingTicket.ID)
	if ticket.State != stateShipping {
		t.Fatalf("shipTicketReady: ticket state = %q, want shipping", ticket.State)
	}

	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("shipTicketReady: ProjectForTicket: %v", err)
	}
	remoteDir, err = gitfixture.WithBareOrigin(t.Context(), proj.LocalPath)
	if err != nil {
		t.Fatalf("shipTicketReady: gitfixture.WithBareOrigin: %v", err)
	}

	return s, ticket, remoteDir
}

// shipSeedPassedRound seeds ticketID (already in the shape judgeTicketReady
// leaves a ticket in: a real worktree, a stored plan, a sealed cohort)
// directly with one verdict artifact per row, the "judge round <round>
// passed" marker, and the transition into "shipping" -- bypassing the real
// judge run entirely. The three finalVerdicts anomaly tests below need
// this: the real judge flow can never produce them (judgerules.go's
// CheckCoverage refuses an incomplete round before RUN ever finishes, and
// EVALUATE writes exactly one "passed" marker per pass), so they build the
// anomaly directly instead of pretending a scripted judge run could.
func shipSeedPassedRound(t *testing.T, s *store.Store, ticketID int64, round int, rows []response.VerdictArtifact) {
	t.Helper()
	ctx := t.Context()
	owner := "ship-seed-passed-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(ctx, ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("shipSeedPassedRound: claim: claimed=%v err=%v", claimed, err)
	}

	for _, row := range rows {
		payload, marshalErr := json.Marshal(row)
		if marshalErr != nil {
			t.Fatalf("shipSeedPassedRound: marshal verdict: %v", marshalErr)
		}
		if _, insertErr := s.InsertArtifact(ctx, store.Artifact{
			TicketID: ticketID, Type: "verdict", Version: 1, Payload: payload,
		}); insertErr != nil {
			t.Fatalf("shipSeedPassedRound: insert verdict: %v", insertErr)
		}
	}

	applied, err := s.CommitHandlerResult(ctx, store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Next: stateShipping, Reason: "ship test setup",
		Messages: []store.Message{{
			TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("judge round %d passed", round),
		}},
	})
	if err != nil || !applied {
		t.Fatalf("shipSeedPassedRound: commit: applied=%v err=%v", applied, err)
	}
}

// shipHeadSHA reads ticket's own real worktree head sha, through the same
// never-called-GitHub orchestrator pbOrchestratorFor already builds
// (postbuild_test.go): the three finalVerdicts anomaly tests need a real
// sha their seeded verdict rows can carry, so publishChecks' own branch
// checks (step 1) agree with them.
func shipHeadSHA(t *testing.T, s *store.Store, ticket store.Ticket) string {
	t.Helper()
	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	orch, _, ok := pbOrchestratorFor(t, proj.LocalPath, orchestrator.NewRunner())
	if !ok {
		t.Fatal("pbOrchestratorFor: not ok")
	}
	wt, _, err := orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	sha, err := orch.HeadSHA(t.Context(), wt)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	return sha
}

// driveShipFixToLanding drives an already-open fix request through the fix
// driver's own RUN then CHECK-and-LAND ticks while ticketID is in
// "shipping" (fix.go's own DriveFix, reached through shipHandler.Run's own
// postBuildPrelude): judging_test.go's own driveJudgeFixToLanding, shipping
// instead of judging, and reusing its exact build fixture (judgeFixBuildScript,
// judgeFixTestCmd) since the fix driver cares about neither state.
func driveShipFixToLanding(t *testing.T, s *store.Store, ticketID int64, rt runtime.Runtime) {
	t.Helper()
	for i := range 4 {
		ticket := pbGetTicket(t, s, ticketID)
		deps := pbWithTestCmd(pbClaim(t, s, rt, ticketID), ticket, judgeFixTestCmd)
		commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("driveShipFixToLanding: Run (step %d): %v", i, err)
		}
		pbApply(t, s, ticket, commit)
		for _, m := range commit.Messages {
			if strings.HasPrefix(m.Body, "fix landed ") {
				return
			}
		}
	}
	t.Fatal("driveShipFixToLanding: fix did not land within 4 ticks")
}

// -----------------------------------------------------------------------
// PUBLISH
// -----------------------------------------------------------------------

// TestPublishPushesAndOpensDraft proves PUBLISH's own steps 1 to 3 (design
// section 8.2): the ticket branch actually lands on the bare remote, and
// OpenDraftPR's own CreateDraftPR is called exactly once.
func TestPublishPushesAndOpensDraft(t *testing.T) {
	t.Parallel()
	s, ticket, remoteDir := shipTicketReady(t)

	gh := &shipGitHub{}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)

	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want a clean publish: %+v", commit.Escalation.Payload)
	}
	if gh.creates != 1 {
		t.Errorf("gh.creates = %d, want 1", gh.creates)
	}
	if ticket.Branch == nil {
		t.Fatal("ticket.Branch is nil, want the branch building committed")
	}
	if out, refErr := gitfixture.Git(t.Context(), remoteDir, "show-ref", "--verify", "refs/heads/"+*ticket.Branch); refErr != nil {
		t.Errorf("bare remote did not receive refs/heads/%s: %v: %s", *ticket.Branch, refErr, out)
	}
}

// TestPublishStoresURLAndComment proves PUBLISH's own steps 5 and 6 (design
// section 8.2): the tracker's PR-link comment posts exactly once, and the
// commit sets pr_url, the "pr opened <n>" marker, and ClearPoll.
func TestPublishStoresURLAndComment(t *testing.T) {
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)

	gh := &shipGitHub{}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)

	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.SetPRURL == nil || *commit.SetPRURL != gh.pr.url {
		t.Fatalf("commit.SetPRURL = %v, want %q", commit.SetPRURL, gh.pr.url)
	}
	if !commit.ClearPoll {
		t.Error("commit.ClearPoll = false, want true")
	}
	wantMarker := fmt.Sprintf("pr opened %d", gh.pr.number)
	found := false
	for _, m := range commit.Messages {
		if m.Body == wantMarker {
			found = true
		}
	}
	if !found {
		t.Errorf("commit.Messages = %+v, want a %q marker", commit.Messages, wantMarker)
	}
	if !tr.prPosted || tr.prPostCount != 1 {
		t.Errorf("tr.prPosted = %v, tr.prPostCount = %d, want true and 1", tr.prPosted, tr.prPostCount)
	}

	pbApply(t, s, ticket, commit)
	final := pbGetTicket(t, s, ticket.ID)
	if final.PRURL == nil || *final.PRURL != gh.pr.url {
		t.Errorf("final.PRURL = %v, want %q", final.PRURL, gh.pr.url)
	}
}

// TestPublishFindsExistingPRAfterCrash proves OpenDraftPR's own idempotent
// fallback (Package 5) is what a retried PUBLISH actually takes: a PR
// already open at this head (simulated by calling OpenDraftPR directly,
// standing in for a first tick that crashed after opening it but before
// its own commit) makes the real tick's own CreateDraftPR fail and its
// FindPRByHead fallback find the very same PR, so the retried tick stores
// the same URL rather than opening a second one.
func TestPublishFindsExistingPRAfterCrash(t *testing.T) {
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)

	gh := &shipGitHub{}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)

	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if _, _, openErr := proj.Orch.OpenDraftPR(t.Context(), wt, orchestrator.PullRequest{Title: shipPreCrashTitle}); openErr != nil {
		t.Fatalf("OpenDraftPR (pre-crash): %v", openErr)
	}
	wantURL, wantNumber := gh.pr.url, gh.pr.number
	shipReleaseClaim(t, s, ticket.ID, deps)

	deps2 := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gh.creates != 2 || gh.finds != 1 {
		t.Errorf("gh.creates = %d, gh.finds = %d, want 2 and 1 (the second create fails, so OpenDraftPR falls back to find)", gh.creates, gh.finds)
	}
	if commit.SetPRURL == nil || *commit.SetPRURL != wantURL {
		t.Errorf("commit.SetPRURL = %v, want %q", commit.SetPRURL, wantURL)
	}
	wantMarker := fmt.Sprintf("pr opened %d", wantNumber)
	found := false
	for _, m := range commit.Messages {
		if m.Body == wantMarker {
			found = true
		}
	}
	if !found {
		t.Errorf("commit.Messages = %+v, want a %q marker", commit.Messages, wantMarker)
	}
}

// TestPublishCrashBeforePRComment proves design section 11's own row for
// PUBLISH's PR comment, the "before the post" half: a crash right after
// OpenDraftPR succeeds but before PostPRLink ever ran leaves no marker, so
// the next tick posts the comment exactly once and commits.
func TestPublishCrashBeforePRComment(t *testing.T) {
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)

	gh := &shipGitHub{}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)

	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if _, _, openErr := proj.Orch.OpenDraftPR(t.Context(), wt, orchestrator.PullRequest{Title: shipPreCrashTitle}); openErr != nil {
		t.Fatalf("OpenDraftPR (pre-crash): %v", openErr)
	}
	if tr.prPostCount != 0 {
		t.Fatalf("tr.prPostCount = %d before the real tick, want 0", tr.prPostCount)
	}
	shipReleaseClaim(t, s, ticket.ID, deps)

	deps2 := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tr.prPostCount != 1 {
		t.Errorf("tr.prPostCount = %d, want exactly 1", tr.prPostCount)
	}
	pbApply(t, s, ticket, commit)

	final := pbGetTicket(t, s, ticket.ID)
	if final.PRURL == nil || *final.PRURL != gh.pr.url {
		t.Errorf("final.PRURL = %v, want %q", final.PRURL, gh.pr.url)
	}
}

// TestPublishCrashAfterPRComment proves design section 11's own row for
// PUBLISH's PR comment, the "after the post" half: the comment already
// posted (simulated by calling the tracker directly) but no commit ever
// landed leaves the marker behind, so the next tick finds it, posts
// nothing a second time, and still commits.
func TestPublishCrashAfterPRComment(t *testing.T) {
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)

	gh := &shipGitHub{}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)

	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if _, _, openErr := proj.Orch.OpenDraftPR(t.Context(), wt, orchestrator.PullRequest{Title: shipPreCrashTitle}); openErr != nil {
		t.Fatalf("OpenDraftPR (pre-crash): %v", openErr)
	}
	if linkErr := tr.PostPRLink(t.Context(), ticket.ProjectID, ticket.TrackerRef, gh.pr.url); linkErr != nil {
		t.Fatalf("PostPRLink (pre-crash): %v", linkErr)
	}
	if tr.prPostCount != 1 {
		t.Fatalf("tr.prPostCount = %d before the real tick, want 1", tr.prPostCount)
	}
	shipReleaseClaim(t, s, ticket.ID, deps)

	deps2 := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tr.prPostCount != 1 {
		t.Errorf("tr.prPostCount = %d after the real tick, want still 1 (no duplicate)", tr.prPostCount)
	}
	pbApply(t, s, ticket, commit)

	final := pbGetTicket(t, s, ticket.ID)
	if final.PRURL == nil || *final.PRURL != gh.pr.url {
		t.Errorf("final.PRURL = %v, want %q", final.PRURL, gh.pr.url)
	}
}

// TestPublishPRCommentErrorNoCommit proves design section 8.2 step 5's own
// rule: a PostPRLink error returns with no commit, so a retried tick
// repeats PUBLISH from scratch.
func TestPublishPRCommentErrorNoCommit(t *testing.T) {
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)

	gh := &shipGitHub{}
	tr := &shipTracker{prLinkErr: errors.New("boom: tracker unavailable")}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)

	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err == nil {
		t.Fatal("Run: want an error when PostPRLink fails, got nil")
	}
	if commit.TicketID != 0 || commit.Next != "" || commit.SetPRURL != nil {
		t.Errorf("commit = %+v, want the zero value (no commit on a PostPRLink error)", commit)
	}
	final := pbGetTicket(t, s, ticket.ID)
	if final.PRURL != nil {
		t.Errorf("final.PRURL = %v, want nil (no commit landed)", final.PRURL)
	}
}

// TestPublishPushErrorEscalates proves design section 8.2 step 4's own push
// row: a push failure (no "origin" remote configured) escalates
// environment, origin shipping, What pushFailedWhat, Tried the error text,
// and never reaches CreateDraftPR.
func TestPublishPushErrorEscalates(t *testing.T) {
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)

	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	if _, rmErr := gitfixture.Git(t.Context(), proj.LocalPath, "remote", "remove", "origin"); rmErr != nil {
		t.Fatalf("remove origin: %v", rmErr)
	}

	gh := &shipGitHub{}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)

	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("want an escalation")
	}
	if commit.Escalation.Payload.What != pushFailedWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, pushFailedWhat)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginShipping) {
		t.Errorf("Origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginShipping)
	}
	if commit.Escalation.Payload.Tried == "" {
		t.Error("Tried is empty, want the push error's own text")
	}
	if gh.creates != 0 {
		t.Errorf("gh.creates = %d, want 0 (push must fail before create is ever attempted)", gh.creates)
	}
}

// TestPublishAuthErrorEscalates proves design section 8.2 step 4's own
// ErrGitHubAuth row: escalates environment, origin shipping, What
// prOpenRefusedWhat.
func TestPublishAuthErrorEscalates(t *testing.T) {
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)

	gh := &shipGitHub{createErr: orchestrator.ErrGitHubAuth}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)

	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("want an escalation")
	}
	if commit.Escalation.Payload.What != prOpenRefusedWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, prOpenRefusedWhat)
	}
	if !strings.Contains(commit.Escalation.Payload.Tried, orchestrator.ErrGitHubAuth.Error()) {
		t.Errorf("Tried = %q, want it to mention %q", commit.Escalation.Payload.Tried, orchestrator.ErrGitHubAuth)
	}
}

// TestPublishUnavailableRetries proves design section 8.2 step 4's own
// ErrGitHubUnavailable row: PUBLISH returns the error itself, with no
// commit, so the dispatcher releases the claim and the next tick tries
// again.
func TestPublishUnavailableRetries(t *testing.T) {
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)

	gh := &shipGitHub{createErr: orchestrator.ErrGitHubUnavailable}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)

	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err == nil {
		t.Fatal("Run: want an error (ErrGitHubUnavailable), got nil")
	}
	if !errors.Is(err, orchestrator.ErrGitHubUnavailable) {
		t.Errorf("err = %v, want it to wrap orchestrator.ErrGitHubUnavailable", err)
	}
	if commit.TicketID != 0 || commit.Escalation != nil {
		t.Errorf("commit = %+v, want the zero value", commit)
	}
}

// TestPublishRequiresJudgePassOnHead proves design section 8.2 step 2's own
// SHA check: a CI fix landed in shipping, after the judge passed, moves the
// head past what the judge actually verified (D23: a fix landed in
// shipping is never re-judged), so PUBLISH escalates judgeNotPassedWhat
// rather than opening a pull request for an unverified commit.
func TestPublishRequiresJudgePassOnHead(t *testing.T) {
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)

	maxRunID, err := s.MaxRunID(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	msg, err := fixRequestMessage(ticket, FixKindCILog, "check \"ci\" failed: boom", maxRunID)
	if err != nil {
		t.Fatalf("fixRequestMessage: %v", err)
	}
	owner := "ship-seed-fix-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticket.ID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticket.ID, Owner: owner, Expires: expires, Messages: []store.Message{msg},
	})
	if err != nil || !applied {
		t.Fatalf("seed fix request: applied=%v err=%v", applied, err)
	}

	rt := runtime.NewFake(fstest.MapFS{"build/fix/1.xml": &fstest.MapFile{Data: []byte(judgeFixBuildScript)}})
	driveShipFixToLanding(t, s, ticket.ID, rt)

	gh := &shipGitHub{}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("want an escalation")
	}
	if commit.Escalation.Payload.What != judgeNotPassedWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, judgeNotPassedWhat)
	}
}

// -----------------------------------------------------------------------
// finalVerdicts, through PUBLISH
// -----------------------------------------------------------------------

// TestFinalVerdictsAfterFailFixPass is design section 19.4 task 6's own
// integration case: round 1 fails a scenario, a fix lands, round 2 passes
// with a check override -- judging_test.go's own TestJudgeFixThenPass, one
// PUBLISH further (the fixture's own check-bearing scenario is s1, not s2:
// the planning fixture's s1 carries check="curl -sf localhost:8080/hello",
// per M2 task 8's own handoff note). finalVerdicts must read round 2's own
// verdict rows, not round 1's failing ones, even though round 1's rows are
// still on the ticket (Verdicts is append-only): the rendered pull request
// body is judged on round 2's own sha, never round 1's.
func TestFinalVerdictsAfterFailFixPass(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)

	scripts := judgeScriptsFS(judgeOkBothScript)
	scripts["judge/2/1.xml"] = &fstest.MapFile{Data: []byte(judgeOkBothScript)}
	scripts["build/fix/1.xml"] = &fstest.MapFile{Data: []byte(judgeFixBuildScript)}
	rt := runtime.NewFake(scripts)

	checks := &judgeScriptedCheckCommands{real: NewCommandRunner(sandbox.Off(), false), steps: []judgeCheckStep{{exit: 1}, {exit: 0}}}

	ticket = judgeAdvanceStart(t, s, rt, ticket) // START round 1

	deps := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), ticket, judgeFixTestCmd)
	deps.Commands = checks
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps) // RUN round 1
	if err != nil {
		t.Fatalf("RUN round 1: %v", err)
	}
	pbApply(t, s, ticket, runCommit)

	deps2 := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), pbGetTicket(t, s, ticket.ID), judgeFixTestCmd)
	deps2.Commands = checks
	checkCommit, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2) // CHECK s1: exit 1
	if err != nil {
		t.Fatalf("CHECK round 1: %v", err)
	}
	pbApply(t, s, ticket, checkCommit)

	deps3 := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), pbGetTicket(t, s, ticket.ID), judgeFixTestCmd)
	deps3.Commands = checks
	evalCommit, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps3) // EVALUATE round 1: fail, fix request
	if err != nil {
		t.Fatalf("EVALUATE round 1: %v", err)
	}
	pbApply(t, s, ticket, evalCommit)

	driveJudgeFixToLanding(t, s, ticket.ID, rt, checks)

	ticket = pbGetTicket(t, s, ticket.ID)
	deps4 := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), ticket, judgeFixTestCmd)
	deps4.Commands = checks
	startCommit2, err := (judgeHandler{}).Run(t.Context(), ticket, deps4) // START round 2
	if err != nil {
		t.Fatalf("START round 2: %v", err)
	}
	pbApply(t, s, ticket, startCommit2)

	deps5 := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), pbGetTicket(t, s, ticket.ID), judgeFixTestCmd)
	deps5.Commands = checks
	runCommit2, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps5) // RUN round 2
	if err != nil {
		t.Fatalf("RUN round 2: %v", err)
	}
	pbApply(t, s, ticket, runCommit2)

	deps6 := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), pbGetTicket(t, s, ticket.ID), judgeFixTestCmd)
	deps6.Commands = checks
	checkCommit2, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps6) // CHECK s1: exit 0
	if err != nil {
		t.Fatalf("CHECK round 2: %v", err)
	}
	pbApply(t, s, ticket, checkCommit2)

	deps7 := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), pbGetTicket(t, s, ticket.ID), judgeFixTestCmd)
	deps7.Commands = checks
	evalCommit2, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps7) // EVALUATE round 2: pass
	if err != nil {
		t.Fatalf("EVALUATE round 2: %v", err)
	}
	pbApply(t, s, ticket, evalCommit2)

	ticket = pbGetTicket(t, s, ticket.ID)
	if ticket.State != stateShipping {
		t.Fatalf("ticket state = %q, want shipping", ticket.State)
	}

	wantSHA := shipHeadSHA(t, s, ticket)

	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	if _, wboErr := gitfixture.WithBareOrigin(t.Context(), proj.LocalPath); wboErr != nil {
		t.Fatalf("gitfixture.WithBareOrigin: %v", wboErr)
	}

	gh := &shipGitHub{}
	tr := &shipTracker{}
	publishDeps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, publishDeps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want a clean publish: %+v", commit.Escalation.Payload)
	}
	if commit.SetPRURL == nil {
		t.Fatal("commit.SetPRURL is nil, want the opened pr's url")
	}
	if !strings.Contains(gh.lastBody, wantSHA[:7]) {
		t.Errorf("the pull request body does not mention round 2's own sha7 %q:\n%s", wantSHA[:7], gh.lastBody)
	}
}

// TestFinalVerdictsMissingScenario proves design section 8.2 step 2's own
// missing-verdict row: a sealed cohort scenario with no verdict row at all
// under the passing round escalates environment, origin shipping, What
// "the passing judge round has no verdict for scenario <id>".
func TestFinalVerdictsMissingScenario(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	sha := shipHeadSHA(t, s, ticket)

	shipSeedPassedRound(t, s, ticket.ID, 1, []response.VerdictArtifact{
		{Verdict: response.Verdict{Scenario: "s1", Result: response.ResultPass, Evidence: "ok"}, Kind: response.ScenarioKindBehavior, Round: 1, SHA: sha}, //nolint:modernize // embedlit's own suggestion (eliding response.Verdict here) does not compile: a struct field's composite literal cannot elide its type the way a slice element can.
		// s2 is missing on purpose: judgeTicketReady's own cohort is [s1, s2].
	})

	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	if _, wboErr := gitfixture.WithBareOrigin(t.Context(), proj.LocalPath); wboErr != nil {
		t.Fatalf("gitfixture.WithBareOrigin: %v", wboErr)
	}

	gh := &shipGitHub{}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("want an escalation")
	}
	want := "the passing judge round has no verdict for scenario s2"
	if commit.Escalation.Payload.What != want {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, want)
	}
}

// TestFinalVerdictsUnknownScenario proves design section 8.2 step 2's own
// unknown-scenario row: a round n verdict row naming a scenario outside the
// sealed cohort escalates environment, origin shipping, What "the passing
// judge round has a verdict for unknown scenario <id>".
func TestFinalVerdictsUnknownScenario(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	sha := shipHeadSHA(t, s, ticket)

	shipSeedPassedRound(t, s, ticket.ID, 1, []response.VerdictArtifact{
		{Verdict: response.Verdict{Scenario: "s1", Result: response.ResultPass, Evidence: "ok"}, Kind: response.ScenarioKindBehavior, Round: 1, SHA: sha}, //nolint:modernize // embedlit's own suggestion (eliding response.Verdict here) does not compile: a struct field's composite literal cannot elide its type the way a slice element can.
		{Verdict: response.Verdict{Scenario: "s2", Result: response.ResultPass, Evidence: "ok"}, Kind: response.ScenarioKindBehavior, Round: 1, SHA: sha}, //nolint:modernize // embedlit's own suggestion (eliding response.Verdict here) does not compile: a struct field's composite literal cannot elide its type the way a slice element can.
		{Verdict: response.Verdict{Scenario: "s3", Result: response.ResultPass, Evidence: "ok"}, Kind: response.ScenarioKindBehavior, Round: 1, SHA: sha}, //nolint:modernize // embedlit's own suggestion (eliding response.Verdict here) does not compile: a struct field's composite literal cannot elide its type the way a slice element can.
	})

	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	if _, wboErr := gitfixture.WithBareOrigin(t.Context(), proj.LocalPath); wboErr != nil {
		t.Fatalf("gitfixture.WithBareOrigin: %v", wboErr)
	}

	gh := &shipGitHub{}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("want an escalation")
	}
	want := "the passing judge round has a verdict for unknown scenario s3"
	if commit.Escalation.Payload.What != want {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, want)
	}
}

// TestFinalVerdictsPassedTwice proves design section 8.2 step 2's own
// duplicate-marker row: two "judge round <n> passed" markers naming the
// same round -- a replay or a retried commit, never the real judge flow,
// since EVALUATE writes exactly one -- escalates environment, origin
// shipping, What "judge round <n> passed twice".
func TestFinalVerdictsPassedTwice(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	sha := shipHeadSHA(t, s, ticket)

	shipSeedPassedRound(t, s, ticket.ID, 1, []response.VerdictArtifact{
		{Verdict: response.Verdict{Scenario: "s1", Result: response.ResultPass, Evidence: "ok"}, Kind: response.ScenarioKindBehavior, Round: 1, SHA: sha}, //nolint:modernize // embedlit's own suggestion (eliding response.Verdict here) does not compile: a struct field's composite literal cannot elide its type the way a slice element can.
		{Verdict: response.Verdict{Scenario: "s2", Result: response.ResultPass, Evidence: "ok"}, Kind: response.ScenarioKindBehavior, Round: 1, SHA: sha}, //nolint:modernize // embedlit's own suggestion (eliding response.Verdict here) does not compile: a struct field's composite literal cannot elide its type the way a slice element can.
	})

	owner := "ship-seed-dup-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticket.ID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticket.ID, Owner: owner, Expires: expires,
		Messages: []store.Message{{TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem, Body: "judge round 1 passed"}},
	})
	if err != nil || !applied {
		t.Fatalf("seed dup marker: applied=%v err=%v", applied, err)
	}

	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	if _, wboErr := gitfixture.WithBareOrigin(t.Context(), proj.LocalPath); wboErr != nil {
		t.Fatalf("gitfixture.WithBareOrigin: %v", wboErr)
	}

	gh := &shipGitHub{}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("want an escalation")
	}
	want := "judge round 1 passed twice"
	if commit.Escalation.Payload.What != want {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, want)
	}
}
