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

// shipFixBuildScriptPath is the fake runtime's own path for judgeFixBuildScript
// (judging_test.go), the one build script every ci_log fix this file lands
// through driveShipFixToLanding runs.
const shipFixBuildScriptPath = "build/fix/1.xml"

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

	// The fields below are POLL's own configurable reads (task 7): GetPR,
	// ListCheckRuns, ListStatuses, and RequiredCheckRules each return their
	// own *Err when set, instead of the configured value.
	prState    orchestrator.PRState
	prErr      error
	getPRCount int

	runs        []orchestrator.CheckRun
	runsErr     error
	statuses    []orchestrator.CommitStatus
	statusesErr error
	required    []orchestrator.RequiredCheck
	requiredErr error

	// logTail, when set, backs JobLogTail; nil returns "", nil (ciLogText's
	// own tests, shiprules_test.go, cover JobLogTail's real shape).
	logTail func(ctx context.Context, owner, repo string, jobID int64, lines int) (string, error)

	// The fields below are respond.go's own configurable reads (M4 task 4):
	// threads backs ListThreads (nil is "no threads", every pre-task-4 POLL
	// test's own implicit expectation); viewerLogin backs Viewer, defaulting
	// to shipViewerLogin so isZingReply has a login to compare against
	// without every test configuring one. replies, resolves, and
	// requestedReviewers record ReplyToThread, ResolveThread, and
	// RequestReviewers calls respectively, for a later task's own tests.
	threads     []orchestrator.Thread
	threadsErr  error
	viewerLogin string
	viewerErr   error
	reviews     []orchestrator.Review
	reviewsErr  error

	replies             []string // threadID+"|"+body, in call order
	resolves            []string // threadID, in call order
	requestedReviewers  []string // login, in call order
	requestReviewersErr map[string]error
}

// shipViewerLogin is shipGitHub's own default Viewer() result (M4 task 4):
// distinct from any login a test's own comment author uses, so isZingReply
// only matches a comment this package's own tests build with it on purpose.
const shipViewerLogin = "zing-bot"

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

// GetPR, Merge, ListCheckRuns, ListStatuses, RequiredCheckRules, and
// JobLogTail give shipGitHub job.PullRequests and job.Checks too (task 7):
// POLL's own tests configure prState/prErr/runs/statuses/required/logTail
// directly on the struct literal, the same way PUBLISH's own tests
// configure createErr/findErr.
func (g *shipGitHub) GetPR(context.Context, string, string, int) (orchestrator.PRState, error) {
	g.getPRCount++
	if g.prErr != nil {
		return orchestrator.PRState{}, g.prErr
	}
	return g.prState, nil
}

func (g *shipGitHub) Merge(context.Context, string, string, int, string, string, string) (string, error) {
	return "", errors.New("shipGitHub: Merge not implemented (M4)")
}

func (g *shipGitHub) ListCheckRuns(context.Context, string, string, string) ([]orchestrator.CheckRun, error) {
	if g.runsErr != nil {
		return nil, g.runsErr
	}
	return g.runs, nil
}

func (g *shipGitHub) ListStatuses(context.Context, string, string, string) ([]orchestrator.CommitStatus, error) {
	if g.statusesErr != nil {
		return nil, g.statusesErr
	}
	return g.statuses, nil
}

func (g *shipGitHub) RequiredCheckRules(context.Context, string, string, string) ([]orchestrator.RequiredCheck, error) {
	if g.requiredErr != nil {
		return nil, g.requiredErr
	}
	return g.required, nil
}

func (g *shipGitHub) JobLogTail(ctx context.Context, owner, repo string, jobID int64, lines int) (string, error) {
	if g.logTail != nil {
		return g.logTail(ctx, owner, repo, jobID, lines)
	}
	return "", nil
}

// ListThreads, ThreadCommentsContain, ReplyToThread, ResolveThread,
// ListReviews, RequestReviewers, and Viewer give shipGitHub job.ReviewThreads
// too (M4 task 4): respond.go's own tests configure threads/threadsErr and
// viewerLogin/viewerErr directly on the struct literal, the same way every
// other read above is configured.
func (g *shipGitHub) ListThreads(context.Context, string, string, int) ([]orchestrator.Thread, error) {
	if g.threadsErr != nil {
		return nil, g.threadsErr
	}
	return g.threads, nil
}

func (g *shipGitHub) ThreadCommentsContain(context.Context, string, string, string) (bool, error) {
	return false, errShipGitHub
}

func (g *shipGitHub) ReplyToThread(_ context.Context, threadID, body string) error {
	g.replies = append(g.replies, threadID+"|"+body)
	return nil
}

func (g *shipGitHub) ResolveThread(_ context.Context, threadID string) error {
	g.resolves = append(g.resolves, threadID)
	return nil
}

func (g *shipGitHub) ListReviews(context.Context, string, string, int) ([]orchestrator.Review, error) {
	if g.reviewsErr != nil {
		return nil, g.reviewsErr
	}
	return g.reviews, nil
}

func (g *shipGitHub) RequestReviewers(_ context.Context, _, _ string, _ int, login string) error {
	if err, ok := g.requestReviewersErr[login]; ok {
		return err
	}
	g.requestedReviewers = append(g.requestedReviewers, login)
	return nil
}

func (g *shipGitHub) Viewer(context.Context) (string, error) {
	if g.viewerErr != nil {
		return "", g.viewerErr
	}
	if g.viewerLogin != "" {
		return g.viewerLogin, nil
	}
	return shipViewerLogin, nil
}

// shipTracker is a configurable ShipTracker double: PostPRLink posts at
// most once, mirroring the dispatcher's own hidden-marker guard
// (internal/dispatch/dispatch.go's postMarkedOnce), so a test can call
// PUBLISH twice and assert the comment never duplicates.
type shipTracker struct {
	prLinkErr   error
	prPosted    bool
	prPostCount int

	doneErr       error
	donePosted    bool
	donePostCount int
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
	if !tr.donePosted {
		tr.donePosted = true
		tr.donePostCount++
	}
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
// pbGitHub{}. When gh also implements job.PullRequests and job.Checks (as
// *shipGitHub does, task 7), every project's own Owner, Repo, PullRequests,
// and Checks are filled from it too, exactly as serve's own
// buildJobProjects fills all three from one *orchestrator.GitHubClient
// (PKG9-PLAN.md section 10.3) -- so POLL's own tests can share the one
// *shipGitHub PUBLISH's own OpenDraftPR call already used, and see the
// very pull request it opened.
func shipBuildProjects(t *testing.T, s *store.Store, gh orchestrator.GitHub) map[int64]Project {
	t.Helper()
	projects, err := s.ListProjects(t.Context())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	prs, hasPRs := gh.(PullRequests)
	checks, hasChecks := gh.(Checks)
	threads, hasThreads := gh.(ReviewThreads)

	out := make(map[int64]Project, len(projects))
	for _, p := range projects {
		orch, repoGit, ok := shipOrchestratorFor(t, p.LocalPath, gh)
		if !ok {
			continue
		}
		proj := Project{Orch: orch, RepoGit: repoGit, TestCmd: "test -f " + pbHelloTxt, LintCmd: pbNoopShellCmd}
		if hasPRs && hasChecks {
			proj.Owner, proj.Repo = pbFixtureOwner, pbFixtureOwner
			proj.PullRequests, proj.Checks = prs, checks
		}
		if hasThreads {
			proj.Threads = threads
		}
		out[p.ID] = proj
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

	rt := runtime.NewFake(fstest.MapFS{shipFixBuildScriptPath: &fstest.MapFile{Data: []byte(judgeFixBuildScript)}})
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
	scripts[shipFixBuildScriptPath] = &fstest.MapFile{Data: []byte(judgeFixBuildScript)}
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

// -----------------------------------------------------------------------
// POLL (task 7)
// -----------------------------------------------------------------------

// shipPublished drives a ticket all the way through PUBLISH (shipTicketReady,
// then one real shipHandler.Run that opens the draft pull request) so its
// own tickets.pr_url is set -- the state every POLL test starts from. gh
// and tr are the same doubles PUBLISH used, so a caller configures gh's own
// prState, runs, statuses, and required (POLL's own reads, task 7) before
// claiming the ticket again for a POLL call.
func shipPublished(t *testing.T) (s *store.Store, ticket store.Ticket, gh *shipGitHub, tr *shipTracker) {
	t.Helper()
	s, ticket, _ = shipTicketReady(t)
	gh = &shipGitHub{}
	tr = &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("shipPublished: PUBLISH Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("shipPublished: PUBLISH escalated: %+v", commit.Escalation.Payload)
	}
	pbApply(t, s, ticket, commit)
	return s, pbGetTicket(t, s, ticket.ID), gh, tr
}

// shipPollRun claims ticket afresh and runs shipHandler.Run once (POLL,
// since pr_url is already set by shipPublished).
func shipPollRun(t *testing.T, s *store.Store, ticket store.Ticket, gh *shipGitHub, tr *shipTracker) (store.HandlerCommit, error) {
	t.Helper()
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	return (shipHandler{}).Run(t.Context(), ticket, deps)
}

// shipGreenCI is the Checks reads a ticket.ID's own POLL needs to reach
// EvaluateCI's green branch: one required "ci" context, matched by one
// completed/success run of the same name. Statuses are never used by any
// of this file's own cases, so, unlike runs and required, there is no
// third return here.
func shipGreenCI() (runs []orchestrator.CheckRun, required []orchestrator.RequiredCheck) {
	return []orchestrator.CheckRun{{ID: 1, Name: "ci", Status: "completed", Conclusion: "success", AppSlug: "github-actions"}},
		[]orchestrator.RequiredCheck{{Context: "ci"}}
}

// shipFailedCI is shipGreenCI's own failed twin: the one required "ci" run
// completed with conclusion failure.
func shipFailedCI() (runs []orchestrator.CheckRun, required []orchestrator.RequiredCheck) {
	return []orchestrator.CheckRun{{
			ID: 1, Name: "ci", Status: "completed", Conclusion: "failure", AppSlug: "github-actions",
			DetailsURL: "https://github.com/fixture/fixture/actions/runs/1/job/2",
		}},
		[]orchestrator.RequiredCheck{{Context: "ci"}}
}

// seedLandedFixRequests writes n complete "fix requested <kind> after run
// 0" / "fix landed <mid> sha <fakesha>" marker pairs directly through
// CommitHandlerResult, each in its own two commits (the landed marker's own
// mid is the requested marker's own row id, assigned by the database only
// once the first commit lands) -- standing in for n real fix cycles without
// driving the fix unit three times over (design section 8.7's shared gate
// only ever counts the marker text, never how it got there).
func seedLandedFixRequests(t *testing.T, s *store.Store, ticketID int64, kind FixKind, n int) {
	t.Helper()
	ctx := t.Context()
	for i := range n {
		reqOwner := fmt.Sprintf("seed-fix-req-%s-%d", kind, i)
		expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
		claimed, err := s.Claim(ctx, ticketID, reqOwner, expires)
		if err != nil || !claimed {
			t.Fatalf("seedLandedFixRequests: claim %d: claimed=%v err=%v", i, claimed, err)
		}
		msg, msgErr := fixRequestMessage(store.Ticket{ID: ticketID}, kind, fmt.Sprintf("log %d", i), 0)
		if msgErr != nil {
			t.Fatalf("seedLandedFixRequests: fixRequestMessage: %v", msgErr)
		}
		applied, err := s.CommitHandlerResult(ctx, store.HandlerCommit{
			TicketID: ticketID, Owner: reqOwner, Expires: expires, Messages: []store.Message{msg},
		})
		if err != nil || !applied {
			t.Fatalf("seedLandedFixRequests: request commit %d: applied=%v err=%v", i, applied, err)
		}

		prefix := fixRequestedCILogPrefix
		if kind == FixKindThreads {
			prefix = fixRequestedThreadsPrefix
		}
		rows, err := s.MarkersWithPrefix(ctx, ticketID, prefix)
		if err != nil || len(rows) == 0 {
			t.Fatalf("seedLandedFixRequests: markers after request %d: rows=%d err=%v", i, len(rows), err)
		}
		mid := rows[len(rows)-1].ID

		landOwner := reqOwner + "-land"
		claimed, err = s.Claim(ctx, ticketID, landOwner, expires)
		if err != nil || !claimed {
			t.Fatalf("seedLandedFixRequests: claim land %d: claimed=%v err=%v", i, claimed, err)
		}
		landedBody := fmt.Sprintf("fix landed %d sha %040x", mid, i+1)
		applied, err = s.CommitHandlerResult(ctx, store.HandlerCommit{
			TicketID: ticketID, Owner: landOwner, Expires: expires,
			Messages: []store.Message{{TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, Body: landedBody}},
		})
		if err != nil || !applied {
			t.Fatalf("seedLandedFixRequests: land commit %d: applied=%v err=%v", i, applied, err)
		}
	}
}

// TestPollMergedGoesDone proves design section 8.3 step 3 and 8.6's DONE:
// a merged PR moves the ticket straight to done, posting the done comment
// and closing the issue before the commit.
func TestPollMergedGoesDone(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	gh.prState = orchestrator.PRState{Merged: true, Draft: true}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != stateDone || commit.Reason != reasonMerged {
		t.Errorf("commit = (Next=%q, Reason=%q), want (done, %q)", commit.Next, commit.Reason, reasonMerged)
	}
	if !commit.ResolveAll {
		t.Error("ResolveAll = false, want true")
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
	if !tr.donePosted || tr.donePostCount != 1 {
		t.Errorf("tr.donePosted = %v, tr.donePostCount = %d, want true and 1", tr.donePosted, tr.donePostCount)
	}

	pbApply(t, s, ticket, commit)
	final := pbGetTicket(t, s, ticket.ID)
	if final.State != stateDone {
		t.Errorf("final.State = %q, want done", final.State)
	}
}

// TestDoneCrashBeforeCommitConverges proves design section 11's own DONE
// row, "after the post" half: PostDone already posted (simulated by calling
// the tracker directly) but no commit ever landed; the next tick finds the
// marker, posts nothing a second time, and still commits done.
func TestDoneCrashBeforeCommitConverges(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	gh.prState = orchestrator.PRState{Merged: true, Draft: true}

	if err := tr.PostDone(t.Context(), ticket.ProjectID, ticket.TrackerRef, *ticket.PRURL); err != nil {
		t.Fatalf("PostDone (pre-crash): %v", err)
	}
	if tr.donePostCount != 1 {
		t.Fatalf("tr.donePostCount = %d before the real tick, want 1", tr.donePostCount)
	}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tr.donePostCount != 1 {
		t.Errorf("tr.donePostCount = %d after the real tick, want still 1 (no duplicate)", tr.donePostCount)
	}
	pbApply(t, s, ticket, commit)
	final := pbGetTicket(t, s, ticket.ID)
	if final.State != stateDone {
		t.Errorf("final.State = %q, want done", final.State)
	}
}

// TestShipTrackerDoneErrorNoCommit proves design section 8.6 step 2: a
// PostDone error returns with no commit, so a retried tick repeats step 1.
func TestShipTrackerDoneErrorNoCommit(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	gh.prState = orchestrator.PRState{Merged: true, Draft: true}
	tr.doneErr = errors.New("boom: tracker unavailable")

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err == nil {
		t.Fatal("Run: want an error when PostDone fails, got nil")
	}
	if commit.TicketID != 0 || commit.Next != "" {
		t.Errorf("commit = %+v, want the zero value (no commit on a PostDone error)", commit)
	}
	final := pbGetTicket(t, s, ticket.ID)
	if final.State != stateShipping {
		t.Errorf("final.State = %q, want still shipping", final.State)
	}
}

// TestPollClosedEscalatesPRClosed proves design section 8.6's CLOSED row:
// a closed, unmerged PR escalates pr_closed, origin shipping.
func TestPollClosedEscalatesPRClosed(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	gh.prState = orchestrator.PRState{State: "closed", Merged: false}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("want an escalation")
	}
	if commit.Escalation.Payload.Code != string(response.EscalationCodePRClosed) {
		t.Errorf("Code = %q, want %q", commit.Escalation.Payload.Code, response.EscalationCodePRClosed)
	}
	if commit.Escalation.Payload.What != prClosedWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, prClosedWhat)
	}
	if !strings.Contains(commit.Escalation.Payload.Why, "is closed") {
		t.Errorf("Why = %q, want it to mention the pull request is closed", commit.Escalation.Payload.Why)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestPollPushesLocalAhead proves design section 8.3 step 4's own ancestor
// row: the PR head is an ancestor of the local branch (a landed fix has not
// been pushed yet), so POLL pushes and clears the poll, with no escalation.
func TestPollPushesLocalAhead(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	oldSHA := shipHeadSHA(t, s, ticket)

	maxRunID, err := s.MaxRunID(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	msg, msgErr := fixRequestMessage(ticket, FixKindCILog, "check \"ci\" failed: boom", maxRunID)
	if msgErr != nil {
		t.Fatalf("fixRequestMessage: %v", msgErr)
	}
	owner := "ship-push-ahead-owner"
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

	rt := runtime.NewFake(fstest.MapFS{shipFixBuildScriptPath: &fstest.MapFile{Data: []byte(judgeFixBuildScript)}})
	driveShipFixToLanding(t, s, ticket.ID, rt)

	newSHA := shipHeadSHA(t, s, pbGetTicket(t, s, ticket.ID))
	if newSHA == oldSHA {
		t.Fatal("the landed fix did not move the branch head")
	}

	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: oldSHA, BaseRef: pbFixtureDefaultBranch}
	commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want a clean push: %+v", commit.Escalation.Payload)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestPollForeignHeadEscalates proves design section 8.3 step 4's own
// non-ancestor row: a PR head that is not an ancestor of the local branch
// (a human pushed to or rewrote the pull request branch) escalates.
func TestPollForeignHeadEscalates(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)

	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	if addErr := gitfixture.AddFile(t.Context(), proj.LocalPath, "unrelated.txt", []byte("x\n")); addErr != nil {
		t.Fatalf("AddFile: %v", addErr)
	}
	out, revErr := gitfixture.Git(t.Context(), proj.LocalPath, "rev-parse", "HEAD")
	if revErr != nil {
		t.Fatalf("rev-parse HEAD: %v: %s", revErr, out)
	}
	foreignSHA := strings.TrimSpace(string(out))

	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: foreignSHA, BaseRef: pbFixtureDefaultBranch}
	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("want an escalation")
	}
	if commit.Escalation.Payload.What != foreignHeadWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, foreignHeadWhat)
	}
	if !strings.Contains(commit.Escalation.Payload.Tried, foreignSHA) {
		t.Errorf("Tried = %q, want it to mention %q", commit.Escalation.Payload.Tried, foreignSHA)
	}
}

// TestPollCIFailedRequestsFix proves design section 8.5 row 4: a failed
// required check writes a ci_log fix request carrying the log tail.
func TestPollCIFailedRequestsFix(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipFailedCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	gh.logTail = func(context.Context, string, string, int64, int) (string, error) { return "FAIL: boom", nil }

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want a fix request: %+v", commit.Escalation.Payload)
	}
	found := false
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, fixRequestedCILogPrefix) && strings.Contains(m.Body, "FAIL: boom") {
			found = true
		}
	}
	if !found {
		t.Errorf("commit.Messages = %+v, want a %q marker carrying the log tail", commit.Messages, fixRequestedCILogPrefix)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestPollSharedGateEscalates proves design section 8.7's shared gate: with
// jobs.respond.max_loops (3) ci_log fix requests already landed, the next
// CI failure escalates loops_exhausted instead of requesting a fourth fix.
func TestPollSharedGateEscalates(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	maxLoops := pbMachine(t).Jobs[jobRespondName].MaxLoops
	seedLandedFixRequests(t, s, ticket.ID, FixKindCILog, maxLoops)

	local := shipHeadSHA(t, s, pbGetTicket(t, s, ticket.ID))
	runs, required := shipFailedCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}

	commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("want an escalation")
	}
	if commit.Escalation.Payload.Code != string(response.EscalationCodeLoopsExhausted) {
		t.Errorf("Code = %q, want %q", commit.Escalation.Payload.Code, response.EscalationCodeLoopsExhausted)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginShipping) {
		t.Errorf("Origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginShipping)
	}
}

// TestThreadsGateRetryKeepsCheckpoint proves design section 5.6's own
// "shipping, loops_exhausted" retry row for kind threads: the retry writes
// both the fix request and "respond applied <aid>" with its own watermark
// in the same commit, and driving that fix to landing lands cleanly, with
// no second fix request ever written. FIX-REPLIES itself (8.5 row 1) is
// M4's: this test proves the checkpoint the retry leaves behind, not a
// reply actually posted to a thread, since nothing in M3 implements
// FIX-REPLIES yet (shipping.go's own file comment).
func TestThreadsGateRetryKeepsCheckpoint(t *testing.T) {
	t.Parallel()
	s, ticket, _ := shipTicketReady(t)

	payload := response.EscalationPayload{
		Code:    string(response.EscalationCodeLoopsExhausted),
		What:    "shipping needed more than 3 fix runs",
		Why:     "CI fixes and review-thread fixes share a limit of 3",
		Tried:   "threads\nrespond 42\nAddress the stale comment on greet.go:3",
		Options: escalationOptions, Origin: string(response.EscalationOriginShipping),
	}
	owner := "seed-threads-gate-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticket.ID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	waiting := waitingFlagQuestions
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticket.ID, Owner: owner, Expires: expires, Waiting: &waiting,
		Escalation: &store.EscalationCommit{Body: payload.Code + ": " + payload.What, Payload: payload},
	})
	if err != nil || !applied {
		t.Fatalf("seed escalation: applied=%v err=%v", applied, err)
	}
	open, err := s.QuestionsByState(t.Context(), ticket.ID, "open")
	if err != nil || len(open) == 0 {
		t.Fatalf("QuestionsByState(open): rows=%d err=%v", len(open), err)
	}
	pbAnswerEscalation(t, s, ticket.ID, open[len(open)-1].ID, "a")

	gh := &shipGitHub{}
	tr := &shipTracker{}
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawRequest, sawApplied bool
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, fixRequestedThreadsPrefix) && strings.Contains(m.Body, "Address the stale comment") {
			sawRequest = true
		}
		if strings.HasPrefix(m.Body, "respond applied 42\n") && strings.Contains(m.Body, "fix request after run ") {
			sawApplied = true
		}
	}
	if !sawRequest {
		t.Errorf("commit.Messages = %+v, want a %q marker", commit.Messages, fixRequestedThreadsPrefix)
	}
	if !sawApplied {
		t.Errorf("commit.Messages = %+v, want a \"respond applied 42\" marker naming the fix request's watermark", commit.Messages)
	}
	pbApply(t, s, ticket, commit)

	rt := runtime.NewFake(fstest.MapFS{shipFixBuildScriptPath: &fstest.MapFile{Data: []byte(judgeFixBuildScript)}})
	driveShipFixToLanding(t, s, ticket.ID, rt)

	reqRows, err := s.MarkersWithPrefix(t.Context(), ticket.ID, fixRequestedThreadsPrefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	if len(reqRows) != 1 {
		t.Errorf("fix requested threads markers = %d, want exactly 1 (no duplicate request)", len(reqRows))
	}
	appliedRows, err := s.MarkersWithPrefix(t.Context(), ticket.ID, "respond applied ")
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	if len(appliedRows) != 1 {
		t.Errorf("respond applied markers = %d, want exactly 1", len(appliedRows))
	}
}

// TestPollPendingBacksOff proves design section 8.3's own backoff: a
// pending CI commits Poll with interval 30 on the first poll, doubling to
// 60 on the next poll with the same fingerprint.
func TestPollPendingBacksOff(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	gh.runs = []orchestrator.CheckRun{{ID: 1, Name: "ci", Status: testInProgress}}
	gh.required = []orchestrator.RequiredCheck{{Context: "ci"}}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (first poll): %v", err)
	}
	if commit.Poll == nil {
		t.Fatal("commit.Poll is nil, want the backoff commit")
	}
	if commit.Poll.IntervalS != 30 {
		t.Errorf("first poll interval = %d, want 30", commit.Poll.IntervalS)
	}
	pbApply(t, s, ticket, commit)

	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (second poll): %v", err)
	}
	if commit2.Poll == nil {
		t.Fatal("commit2.Poll is nil, want the backoff commit")
	}
	if commit2.Poll.IntervalS != 60 {
		t.Errorf("second poll interval = %d, want 60 (doubled)", commit2.Poll.IntervalS)
	}
}

// TestPollRateLimitWaitsForReset proves design section 8.3's own
// schedule-only rule: a RateLimitedError schedules the next poll at
// max(now+iv, resetAt+5s), picking the later reset time when it wins.
func TestPollRateLimitWaitsForReset(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	resetAt := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	gh.prErr = orchestrator.RateLimitedError{ResetAt: resetAt}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.PollSchedule == nil {
		t.Fatal("commit.PollSchedule is nil, want the schedule-only commit")
	}
	wantAfter := resetAt.Add(4 * time.Second)
	if !commit.PollSchedule.NextAt.After(wantAfter) {
		t.Errorf("PollSchedule.NextAt = %v, want after resetAt+5s (%v)", commit.PollSchedule.NextAt, resetAt.Add(5*time.Second))
	}
}

// TestFirstPollFailureSchedules proves design section 8.3's own first-read
// failure row: 429, 502, and a network error each schedule interval 30 with
// no stored interval or fingerprint, and the fingerprint stays NULL.
func TestFirstPollFailureSchedules(t *testing.T) {
	t.Parallel()
	for name, prErr := range map[string]error{
		"429":           orchestrator.RateLimitedError{},
		"502":           orchestrator.ErrGitHubUnavailable,
		"network error": fmt.Errorf("dial tcp: connection refused: %w", orchestrator.ErrGitHubUnavailable),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, ticket, gh, tr := shipPublished(t)
			gh.prErr = prErr

			commit, err := shipPollRun(t, s, ticket, gh, tr)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if commit.PollSchedule == nil {
				t.Fatal("commit.PollSchedule is nil, want the schedule-only commit")
			}
			if commit.PollSchedule.IntervalS != 30 {
				t.Errorf("IntervalS = %d, want 30", commit.PollSchedule.IntervalS)
			}
			pbApply(t, s, ticket, commit)
			final := pbGetTicket(t, s, ticket.ID)
			if final.PollFingerprint != nil {
				t.Errorf("final.PollFingerprint = %v, want nil (still NULL)", *final.PollFingerprint)
			}
		})
	}
}

// TestPollFailureDoublesInterval proves the schedule-only rule's own
// doubling: a stored interval of 120 becomes 240, and a stored interval
// already at the 300 cap stays there.
func TestPollFailureDoublesInterval(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		stored  int
		wantNew int
	}{
		{"120 doubles to 240", 120, 240},
		{"300 stays capped at 300", 300, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ticket, gh, tr := shipPublished(t)

			seedOwner := "seed-poll-interval"
			expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
			claimed, err := s.Claim(t.Context(), ticket.ID, seedOwner, expires)
			if err != nil || !claimed {
				t.Fatalf("claim: claimed=%v err=%v", claimed, err)
			}
			applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
				TicketID: ticket.ID, Owner: seedOwner, Expires: expires,
				PollSchedule: &store.PollSchedule{NextAt: time.Now().UTC().Truncate(time.Second), IntervalS: tc.stored},
			})
			if err != nil || !applied {
				t.Fatalf("seed poll interval: applied=%v err=%v", applied, err)
			}

			gh.prErr = orchestrator.ErrGitHubUnavailable
			commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if commit.PollSchedule == nil {
				t.Fatal("commit.PollSchedule is nil, want the schedule-only commit")
			}
			if commit.PollSchedule.IntervalS != tc.wantNew {
				t.Errorf("IntervalS = %d, want %d", commit.PollSchedule.IntervalS, tc.wantNew)
			}
		})
	}
}

// TestPushCrashConverges proves design section 11's own push row: the next
// poll, reading the pull request's head already equal to local (the push
// itself succeeded; only the commit confirming it was lost), takes no push
// branch and no escalation.
func TestPushCrashConverges(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	gh.runs, gh.required = shipGreenCI()

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want convergence: %+v", commit.Escalation.Payload)
	}
}

// TestPollUnprotectedEscalates proves design section 8.4's own unprotected
// row: no required check at all escalates environment.
func TestPollUnprotectedEscalates(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	gh.required = nil

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("want an escalation")
	}
	if commit.Escalation.Payload.What != unprotectedWhat {
		t.Errorf("What = %q, want %q", commit.Escalation.Payload.What, unprotectedWhat)
	}
}

// TestPollCIWaitingMarkerOnChange proves design section 8.4's own
// informational marker: an idle poll writes "ci waiting <names>" only when
// Missing differs from the previous poll's.
func TestPollCIWaitingMarkerOnChange(t *testing.T) {
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	gh.required = []orchestrator.RequiredCheck{{Context: "ci"}}
	gh.runs = nil // nothing reports "ci" yet: Missing = ["ci"]

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (first poll): %v", err)
	}
	wantFirst := ciWaitingPrefix + "ci"
	foundFirst := false
	for _, m := range commit.Messages {
		if m.Body == wantFirst {
			foundFirst = true
		}
	}
	if !foundFirst {
		t.Errorf("commit.Messages = %+v, want %q", commit.Messages, wantFirst)
	}
	pbApply(t, s, ticket, commit)

	// The required check now reports, so Missing becomes empty: a new
	// marker is written because the set changed.
	gh.runs, _ = shipGreenCI()
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (second poll): %v", err)
	}
	wantSecond := ciWaitingPrefix
	foundSecond := false
	for _, m := range commit2.Messages {
		if m.Body == wantSecond {
			foundSecond = true
		}
	}
	if !foundSecond {
		t.Errorf("commit2.Messages = %+v, want %q (Missing changed to empty)", commit2.Messages, wantSecond)
	}
	pbApply(t, s, ticket, commit2)

	// A third poll with the same (empty) Missing writes no new marker.
	commit3, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (third poll): %v", err)
	}
	for _, m := range commit3.Messages {
		if strings.HasPrefix(m.Body, ciWaitingPrefix) {
			t.Errorf("commit3.Messages = %+v, want no %q marker (Missing unchanged)", commit3.Messages, ciWaitingPrefix)
		}
	}
}

// TestShippingEscalationRetries proves design section 5.6's own shipping
// rows for pr_closed and any other code: both write the plain "retry
// requested" marker plus ClearPoll.
func TestShippingEscalationRetries(t *testing.T) {
	t.Parallel()
	for _, code := range []response.EscalationCode{response.EscalationCodePRClosed, response.EscalationCodeEnvironment} {
		t.Run(string(code), func(t *testing.T) {
			t.Parallel()
			s, ticket, _ := shipTicketReady(t)

			qID := pbEscalateDirect(t, s, ticket.ID, nil, nil, code, response.EscalationOriginShipping)
			pbAnswerEscalation(t, s, ticket.ID, qID, "a")

			gh := &shipGitHub{}
			tr := &shipTracker{}
			deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
			commit, runErr := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps)
			if runErr != nil {
				t.Fatalf("Run: %v", runErr)
			}
			if !commit.ClearPoll {
				t.Error("ClearPoll = false, want true")
			}
			found := false
			for _, m := range commit.Messages {
				if m.Body == markerRetryRequested {
					found = true
				}
			}
			if !found {
				t.Errorf("commit.Messages = %+v, want a %q marker", commit.Messages, markerRetryRequested)
			}
		})
	}
}

// -----------------------------------------------------------------------
// RESPOND (M4 task 4)
// -----------------------------------------------------------------------

// shipRespondThreadID and shipRespondTID are one actionable thread's own
// raw GraphQL id and its tid (threadrules.go's tid, sha256 of the raw id):
// computed once here rather than inline, since every test below that
// starts or drives a batch needs them to agree with each other and with
// fixtures/scripts/respond/1/1.xml, which answers this exact tid.
const (
	shipRespondThreadID = "RT_thread_1"
	shipRespondTID      = "t9d07af1e65f013d5"
)

// shipThread builds one orchestrator.Thread for a respond test: unresolved,
// at the given path and line, carrying comments in order.
func shipThread(rawID, path string, line int, comments ...orchestrator.ThreadComment) orchestrator.Thread {
	return orchestrator.Thread{ID: rawID, Path: path, Line: line, Comments: comments}
}

// shipHumanComment builds one orchestrator.ThreadComment authored by
// someone other than shipGitHub's own Viewer login, so classifyThreads
// (threadrules.go) counts it as the thread's own last human comment.
func shipHumanComment(id, author, body string, at time.Time) orchestrator.ThreadComment {
	return orchestrator.ThreadComment{ID: id, Author: author, Body: body, CreatedAt: at, UpdatedAt: at}
}

// respondScriptsFS builds an in-memory fs.FS carrying one or more batch-1
// respond turns, keyed "respond/1/<turn>.xml" (runtime.Fake's own
// scriptKey, design section 9.2): scripts[0] is turn 1, scripts[1] turn 2,
// and so on, judgeScriptsFS's own twin for job "respond". Every test below
// drives batch 1, a cap_resumes retry of batch 1 included (design section
// 5.6: a retry reruns the same batch number), so this never needs a batch
// parameter of its own.
func respondScriptsFS(scripts ...string) fstest.MapFS {
	m := make(fstest.MapFS, len(scripts))
	for i, text := range scripts {
		m[fmt.Sprintf("respond/1/%d.xml", i+1)] = &fstest.MapFile{Data: []byte(text)}
	}
	return m
}

// shipRespondReplyScript is a minimal respond "ok" document (design
// section 9.2) that answers shipRespondTID with a reply, the same shape
// fixtures/scripts/respond/1/1.xml gives it.
const shipRespondReplyScript = `<zing job="respond" outcome="ok">
  <thread id="` + shipRespondTID + `" action="reply">Thanks for flagging this -- fixed as described.</thread>
</zing>`

// shipRespondWrongThreadScript is an "ok" document that answers a thread id
// outside the batch instead of shipRespondTID, so CheckRespondCoverage
// (threadrules.go) reports both "thread <wrong id> is not in this batch"
// and "missing action for thread <shipRespondTID>" -- RESPOND's own
// coverage-failed branch (design section 9.2).
const shipRespondWrongThreadScript = `<zing job="respond" outcome="ok">
  <thread id="tnotinthisbatch0" action="reply">Wrong thread entirely.</thread>
</zing>`

// shipRespondQuestionScript is a minimal respond "question" document
// (design section 9.2, N1: a plain agent question, the same universal
// shape every other job's own first-turn question takes).
const shipRespondQuestionScript = `<zing job="respond" outcome="question">
  <question key="Q1">
    <title>Should this reply mention the follow-up ticket?</title>
    <body>The thread references a separate cleanup; say whether to link it.</body>
    <option key="a">Yes, link it</option>
    <recommended>a</recommended>
  </question>
</zing>`

// shipRespondReady drives a published ticket (shipPublished) through POLL
// once with green CI and one actionable thread (shipRespondThreadID at
// greet.go:3), so its own "respond batch 1 started sha <local> after run
// <R>" marker lands via startRespondBatch (design section 8.5 row 5).
// commentAt lets a caller control the thread's own last-human-comment time,
// since UpdatedAt feeds commentDigest.
func shipRespondReady(t *testing.T, commentAt time.Time) (s *store.Store, ticket store.Ticket, gh *shipGitHub, tr *shipTracker, local string) {
	t.Helper()
	s, ticket, gh, tr = shipPublished(t)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	local = shipHeadSHA(t, s, ticket)
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	gh.threads = []orchestrator.Thread{
		shipThread(shipRespondThreadID, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", commentAt)),
	}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("shipRespondReady: poll: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("shipRespondReady: poll escalated: %+v", commit.Escalation.Payload)
	}
	found := false
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, "respond batch 1 started sha "+local+" after run ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("shipRespondReady: commit.Messages = %+v, want a respond batch 1 started marker", commit.Messages)
	}
	pbApply(t, s, ticket, commit)
	return s, pbGetTicket(t, s, ticket.ID), gh, tr, local
}

// TestPollStartsRespondBatch proves design section 8.5 row 5: an actionable
// thread writes "respond batch 1 started sha <local> after run <R>", its
// own tids sorted on line 2 and their seen digests on line 3, and no run.
func TestPollStartsRespondBatch(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, _, _, local := shipRespondReady(t, when)

	rows, err := s.MarkersWithPrefix(t.Context(), ticket.ID, respondBatchMarkerPrefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("respond batch markers = %d, want 1", len(rows))
	}
	lines := strings.Split(rows[0].Body, "\n")
	if len(lines) != 3 {
		t.Fatalf("marker has %d lines, want 3: %q", len(lines), rows[0].Body)
	}
	wantPrefix := "respond batch 1 started sha " + local + " after run "
	if !strings.HasPrefix(lines[0], wantPrefix) {
		t.Errorf("first line = %q, want it to start %q", lines[0], wantPrefix)
	}
	if lines[1] != shipRespondTID {
		t.Errorf("tids line = %q, want %q", lines[1], shipRespondTID)
	}
	if !strings.HasPrefix(lines[2], "seen "+shipRespondTID+"=") {
		t.Errorf("seen line = %q, want it to start %q", lines[2], "seen "+shipRespondTID+"=")
	}
}

// TestRespondRunStoresArtifact proves design section 9.2's own first turn
// and ok outcome: one "respond" artifact {threads, batch 1, sha, seen}, its
// own seen copied from the batch marker, never the model.
func TestRespondRunStoresArtifact(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr, local := shipRespondReady(t, when)

	rt := runtime.NewFake(respondScriptsFS(shipRespondReplyScript))
	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want a stored artifact: %+v", commit.Escalation.Payload)
	}
	if len(commit.Artifacts) != 1 {
		t.Fatalf("Artifacts = %+v, want exactly one", commit.Artifacts)
	}
	var artifact response.RespondArtifact
	if unmarshalErr := json.Unmarshal(commit.Artifacts[0].Payload, &artifact); unmarshalErr != nil {
		t.Fatalf("unmarshal artifact: %v", unmarshalErr)
	}
	if artifact.Batch != 1 {
		t.Errorf("Batch = %d, want 1", artifact.Batch)
	}
	if artifact.SHA != local {
		t.Errorf("SHA = %q, want %q", artifact.SHA, local)
	}
	if len(artifact.Threads) != 1 || artifact.Threads[0].ID != shipRespondTID {
		t.Errorf("Threads = %+v, want exactly one action for %q", artifact.Threads, shipRespondTID)
	}
	if len(artifact.Seen) != 1 || artifact.Seen[0].TID != shipRespondTID {
		t.Errorf("Seen = %+v, want exactly one entry for %q", artifact.Seen, shipRespondTID)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestRespondCoverageResumes proves design section 9.2's own coverage gate:
// a first turn that answers the wrong thread terminalizes ok and writes
// "respond coverage failed run <rid>"; the resume, answering correctly,
// stores the artifact and writes "respond coverage delivered run <rid>".
func TestRespondCoverageResumes(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr, _ := shipRespondReady(t, when)

	rt := runtime.NewFake(respondScriptsFS(shipRespondWrongThreadScript, shipRespondReplyScript))
	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	firstCommit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if firstCommit.Escalation != nil {
		t.Fatalf("got an escalation on the first coverage failure: %+v", firstCommit.Escalation.Payload)
	}
	if len(firstCommit.Artifacts) != 0 {
		t.Errorf("first commit stored %d artifacts, want 0", len(firstCommit.Artifacts))
	}
	found := false
	for _, m := range firstCommit.Messages {
		if strings.HasPrefix(m.Body, "respond coverage failed run ") && strings.Contains(m.Body, "is not in this batch") {
			found = true
		}
	}
	if !found {
		t.Fatalf("firstCommit.Messages = %+v, want a %q marker", firstCommit.Messages, "respond coverage failed run")
	}
	pbApply(t, s, ticket, firstCommit)

	deps2 := shipClaim(t, s, rt, ticket.ID, gh, tr)
	resumeCommit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2)
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if resumeCommit.Escalation != nil {
		t.Fatalf("got an escalation on the resume: %+v", resumeCommit.Escalation.Payload)
	}
	if len(resumeCommit.Artifacts) != 1 {
		t.Fatalf("resumeCommit.Artifacts = %+v, want exactly one", resumeCommit.Artifacts)
	}
	deliveredFound := false
	for _, m := range resumeCommit.Messages {
		if strings.HasPrefix(m.Body, "respond coverage delivered run ") {
			deliveredFound = true
		}
	}
	if !deliveredFound {
		t.Errorf("resumeCommit.Messages = %+v, want a %q marker", resumeCommit.Messages, "respond coverage delivered run")
	}
}

// TestRespondSecondCoverageEscalates proves design section 9.2's own second
// branch: a coverage resume whose own ok outcome is incomplete too
// escalates response_invalid instead of resuming a third time.
func TestRespondSecondCoverageEscalates(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr, _ := shipRespondReady(t, when)

	rt := runtime.NewFake(respondScriptsFS(shipRespondWrongThreadScript, shipRespondWrongThreadScript))
	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	firstCommit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	pbApply(t, s, ticket, firstCommit)

	deps2 := shipClaim(t, s, rt, ticket.ID, gh, tr)
	resumeCommit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2)
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if resumeCommit.Escalation == nil {
		t.Fatal("want an escalation on the second coverage failure")
	}
	if resumeCommit.Escalation.Payload.Code != string(response.EscalationCodeResponseInvalid) {
		t.Errorf("Code = %q, want %q", resumeCommit.Escalation.Payload.Code, response.EscalationCodeResponseInvalid)
	}
	if resumeCommit.Escalation.Payload.Origin != string(response.EscalationOriginRespond) {
		t.Errorf("Origin = %q, want %q", resumeCommit.Escalation.Payload.Origin, response.EscalationOriginRespond)
	}
}

// TestRespondQuestionResumes proves design section 9.2's own "question"
// outcome and decision tree step (1)'s "job respond" branch: a first turn
// that asks waits on "questions"; once answered, the same session resumes
// (shipHandler.resumeRespondAnswered) and the next ok outcome stores the
// artifact.
func TestRespondQuestionResumes(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr, _ := shipRespondReady(t, when)

	rt := runtime.NewFake(respondScriptsFS(shipRespondQuestionScript, shipRespondReplyScript))
	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	askCommit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("ask Run: %v", err)
	}
	if askCommit.Waiting == nil || *askCommit.Waiting != waitingFlagQuestions {
		t.Fatalf("askCommit.Waiting = %+v, want %q", askCommit.Waiting, waitingFlagQuestions)
	}
	if len(askCommit.Messages) != 1 {
		t.Fatalf("askCommit.Messages = %+v, want exactly one question", askCommit.Messages)
	}
	pbApply(t, s, ticket, askCommit)

	open, err := s.QuestionsByState(t.Context(), ticket.ID, "open")
	if err != nil || len(open) == 0 {
		t.Fatalf("QuestionsByState(open): rows=%d err=%v", len(open), err)
	}
	pbAnswerEscalation(t, s, ticket.ID, open[len(open)-1].ID, "a")

	deps2 := shipClaim(t, s, rt, ticket.ID, gh, tr)
	resumeCommit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2)
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if resumeCommit.Escalation != nil {
		t.Fatalf("got an escalation: %+v", resumeCommit.Escalation.Payload)
	}
	if len(resumeCommit.Artifacts) != 1 {
		t.Fatalf("resumeCommit.Artifacts = %+v, want exactly one", resumeCommit.Artifacts)
	}
	if resumeCommit.Session == nil || resumeCommit.Session.ID == nil || askCommit.Session == nil || askCommit.Session.ID == nil ||
		*resumeCommit.Session.ID != *askCommit.Session.ID {
		t.Errorf("resumeCommit.Session = %+v, want the same session askCommit opened (%+v)", resumeCommit.Session, askCommit.Session)
	}
}

// TestRespondBatchSkippedWhenThreadsGone proves design section 9.2: when
// none of a batch's own tids still match a thread GitHub returns, RESPOND
// writes "respond batch 1 skipped" and starts no run.
func TestRespondBatchSkippedWhenThreadsGone(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr, _ := shipRespondReady(t, when)
	gh.threads = nil // the owner resolved it, or it vanished, before RESPOND ran

	rt := runtime.NewFake(respondScriptsFS(shipRespondReplyScript))
	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(commit.Runs) != 0 {
		t.Errorf("commit.Runs = %+v, want none", commit.Runs)
	}
	if len(commit.Messages) != 1 || commit.Messages[0].Body != "respond batch 1 skipped" {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, "respond batch 1 skipped")
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestSkippedBatchIsClosed proves the tick after a skip does not reach
// decision tree step (2) again (design section 8.1): it falls through to
// POLL.
func TestSkippedBatchIsClosed(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr, local := shipRespondReady(t, when)
	gh.threads = nil

	rt := runtime.NewFake(respondScriptsFS(shipRespondReplyScript))
	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	skipCommit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("skip Run: %v", err)
	}
	pbApply(t, s, ticket, skipCommit)

	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	deps2 := shipClaim(t, s, rt, ticket.ID, gh, tr)
	nextCommit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2)
	if err != nil {
		t.Fatalf("next Run: %v", err)
	}
	if len(nextCommit.Runs) != 0 {
		t.Errorf("nextCommit.Runs = %+v, want none (POLL makes no runtime call)", nextCommit.Runs)
	}
	for _, m := range nextCommit.Messages {
		if strings.HasPrefix(m.Body, "respond batch 1 started") {
			t.Errorf("nextCommit.Messages = %+v, want no second %q marker for batch 1", nextCommit.Messages, "respond batch 1 started")
		}
	}
}

// TestRespondErrorRetry proves design section 5.6's own "respond, with no
// run" row: escalationCommit with RunID nil resolves to the plain "retry
// requested" marker plus ClearPoll, the same shape shipRetryMarkerCommit
// gives every other no-run retry.
func TestRespondErrorRetry(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr, _ := shipRespondReady(t, when)

	qID := pbEscalateDirect(t, s, ticket.ID, nil, nil, response.EscalationCodeEnvironment, response.EscalationOriginRespond)
	pbAnswerEscalation(t, s, ticket.ID, qID, "a")

	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
	found := false
	for _, m := range commit.Messages {
		if m.Body == markerRetryRequested {
			found = true
		}
	}
	if !found {
		t.Errorf("commit.Messages = %+v, want a %q marker", commit.Messages, markerRetryRequested)
	}
}

// TestRespondCapResumesRetryStartsFresh proves design section 5.6's own
// "cap_resumes, exhausted session of job respond" row: the retry marker
// copies the started marker's own sha, tids, and seen line byte for byte
// into "respond batch 1 retry sha <sha> after run <R>"; the next tick runs
// a first turn in a brand-new session, not the exhausted one.
func TestRespondCapResumesRetryStartsFresh(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr, local := shipRespondReady(t, when)
	started, err := s.MarkersWithPrefix(t.Context(), ticket.ID, respondBatchMarkerPrefix)
	if err != nil || len(started) != 1 {
		t.Fatalf("MarkersWithPrefix: rows=%d err=%v", len(started), err)
	}
	wantLine2, wantLine3, splitErr := respondBatchRawLines(started[0].Body)
	if splitErr != nil {
		t.Fatalf("respondBatchRawLines: %v", splitErr)
	}

	// The exhausted session's own first turn answers the wrong thread (a
	// coverage failure, not ok): an ok turn would store batch 1's own
	// artifact immediately, and decision tree step (2)'s own guard ("no
	// respond artifact of batch n yet", design section 8.1) would then
	// never fire again for this batch, which is not what an exhausted,
	// still-mid-batch session looks like.
	rt := runtime.NewFake(respondScriptsFS(shipRespondWrongThreadScript))
	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	runCommit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if runCommit.Session == nil || runCommit.Session.ID == nil {
		t.Fatalf("runCommit.Session = %+v, want a freshly reserved session id", runCommit.Session)
	}
	sessionID := *runCommit.Session.ID
	pbApply(t, s, ticket, runCommit)

	owner := "ship-respond-cap-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	maxResumes := pbMachine(t).Jobs[jobRespondName].MaxResumes
	for range maxResumes {
		claimed, claimErr := s.Claim(t.Context(), ticket.ID, owner, expires)
		if claimErr != nil || !claimed {
			t.Fatalf("bump claim: claimed=%v err=%v", claimed, claimErr)
		}
		applied, bumpErr := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
			TicketID: ticket.ID, Owner: owner, Expires: expires,
			Session: &store.SessionUpsert{ID: &sessionID, BumpResumes: true},
		})
		if bumpErr != nil || !applied {
			t.Fatalf("bump CommitHandlerResult: applied=%v err=%v", applied, bumpErr)
		}
	}

	qID := pbEscalateDirect(t, s, ticket.ID, nil, &sessionID, response.EscalationCodeResumesExhausted, response.EscalationOriginCapResumes)
	pbAnswerEscalation(t, s, ticket.ID, qID, "a")

	deps2 := shipClaim(t, s, rt, ticket.ID, gh, tr)
	retryCommit, handled := pbRunPrelude(t, s, deps2, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(retryCommit.Runs) != 0 {
		t.Errorf("retryCommit.Runs = %+v, want none (deferred to the next tick)", retryCommit.Runs)
	}
	wantFirst := "respond batch 1 retry sha " + local + " after run "
	if len(retryCommit.Messages) != 1 || !strings.HasPrefix(retryCommit.Messages[0].Body, wantFirst) {
		t.Fatalf("retryCommit.Messages = %+v, want exactly one %q marker", retryCommit.Messages, wantFirst)
	}
	gotLine2, gotLine3, splitErr2 := respondBatchRawLines(retryCommit.Messages[0].Body)
	if splitErr2 != nil {
		t.Fatalf("respondBatchRawLines: %v", splitErr2)
	}
	if gotLine2 != wantLine2 {
		t.Errorf("retry tids line = %q, want %q (copied from the started marker)", gotLine2, wantLine2)
	}
	if gotLine3 != wantLine3 {
		t.Errorf("retry seen line = %q, want %q (copied from the started marker)", gotLine3, wantLine3)
	}

	// The retry reruns batch 1 itself (design section 5.6: "n the batch of
	// the exhausted session"), not a new, higher batch number, so the fresh
	// session's own first turn is still labeled "1" -- a separate Fake
	// instance is what makes it a fresh session, not a new batch number.
	rt2 := runtime.NewFake(respondScriptsFS(shipRespondReplyScript))
	deps3 := shipClaim(t, s, rt2, ticket.ID, gh, tr)
	freshCommit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps3)
	if err != nil {
		t.Fatalf("Run after retry: %v", err)
	}
	if len(freshCommit.Runs) != 1 {
		t.Fatalf("freshCommit.Runs = %+v, want exactly one (a fresh first turn)", freshCommit.Runs)
	}
	if freshCommit.Session == nil || freshCommit.Session.ID == nil || *freshCommit.Session.ID == sessionID {
		t.Errorf("freshCommit.Session = %+v, want a freshly minted session (not the exhausted one, %d)", freshCommit.Session, sessionID)
	}
}

// TestRespondRetryStaleOnNewComment proves design section 9.2's own
// freshness check, reached through a "respond batch 1 retry ..." marker: a
// human comments on the batch's own thread before the retry's own tick
// runs, so the next tick writes "respond batch 1 stale" instead of a run.
func TestRespondRetryStaleOnNewComment(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr, local := shipRespondReady(t, when)

	owner := "ship-respond-stale-comment-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticket.ID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	line2, line3, splitErr := respondBatchRawLines(mustNewestMarker(t, s, ticket.ID, respondBatchMarkerPrefix))
	if splitErr != nil {
		t.Fatalf("respondBatchRawLines: %v", splitErr)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticket.ID, Owner: owner, Expires: expires,
		Messages: []store.Message{{
			TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("respond batch 1 retry sha %s after run 0\n%s\n%s", local, line2, line3),
		}},
	})
	if err != nil || !applied {
		t.Fatalf("seed retry marker: applied=%v err=%v", applied, err)
	}

	gh.threads = []orchestrator.Thread{
		shipThread(shipRespondThreadID, "greet.go", 3,
			shipHumanComment("c1", "reviewer1", "please fix this", when),
			shipHumanComment("c2", "reviewer1", "actually, one more thing", when.Add(time.Hour))),
	}

	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(commit.Runs) != 0 {
		t.Errorf("commit.Runs = %+v, want none", commit.Runs)
	}
	want := "respond batch 1 stale\nthread " + shipRespondTID + " has a new comment"
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestRespondRetryStaleOnHeadMoved proves design section 9.2's own other
// freshness check, reached through a "respond batch 1 retry ..." marker:
// the pull request head moves before the retry's own tick runs, so the
// next tick writes "respond batch 1 stale" with the head-moved reason.
func TestRespondRetryStaleOnHeadMoved(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr, local := shipRespondReady(t, when)

	owner := "ship-respond-stale-head-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticket.ID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	line2, line3, splitErr := respondBatchRawLines(mustNewestMarker(t, s, ticket.ID, respondBatchMarkerPrefix))
	if splitErr != nil {
		t.Fatalf("respondBatchRawLines: %v", splitErr)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticket.ID, Owner: owner, Expires: expires,
		Messages: []store.Message{{
			TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("respond batch 1 retry sha %s after run 0\n%s\n%s", local, line2, line3),
		}},
	})
	if err != nil || !applied {
		t.Fatalf("seed retry marker: applied=%v err=%v", applied, err)
	}

	foreignSHA := strings.Repeat("a", 40)
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: foreignSHA, BaseRef: pbFixtureDefaultBranch}

	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(commit.Runs) != 0 {
		t.Errorf("commit.Runs = %+v, want none", commit.Runs)
	}
	want := "respond batch 1 stale\n" + respondStaleHeadMovedReason
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
}

// mustNewestMarker returns the newest marker of ticketID whose first line
// starts with prefix, its own body, failing the test when there is none.
func mustNewestMarker(t *testing.T, s *store.Store, ticketID int64, prefix string) string {
	t.Helper()
	rows, err := s.MarkersWithPrefix(t.Context(), ticketID, prefix)
	if err != nil || len(rows) == 0 {
		t.Fatalf("MarkersWithPrefix(%q): rows=%d err=%v", prefix, len(rows), err)
	}
	return rows[len(rows)-1].Body
}
