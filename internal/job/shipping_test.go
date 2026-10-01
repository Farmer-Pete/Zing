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
	"net/http"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/go-github/v92/github"

	"zing/internal/gitfixture"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
	"zing/internal/tracker"
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

	// The fields below back ThreadCommentsContain and ResolveThread (M4 task
	// 5): markerAuthors, keyed threadID+"|"+needle, is the author of the one
	// comment that carries that marker, when any does -- a real
	// ReplyToThread call registers its own marker here under the viewer's
	// own login, so a later idempotency check (ThreadCommentsContain) finds
	// what this same fake already posted; a test seeds an entry directly to
	// simulate a marker a prior, crashed APPLY already posted
	// (TestApplyIsIdempotentAfterCrash, TestApplyFindsMarkerOlderThan100Comments),
	// or under a different login to simulate a spoofed comment
	// (TestApplySpoofedMarkerStillPosts): ThreadCommentsContain only matches
	// when the author it is asked about agrees, exactly as the real,
	// author-gated GraphQL call would. containsCalls records every
	// ThreadCommentsContain call, in order, the same shape as replies and
	// resolves; containsErr, when set, fails every call instead.
	// resolveErrFor, keyed by rawID, fails one thread's own ResolveThread
	// call (TestApplyCrashBetweenReplyAndResolve).
	markerAuthors map[string]string
	containsCalls []string
	containsErr   error
	resolveErrFor map[string]error

	// markReadyErr and convertToDraftErr, when set, fail every MarkReady or
	// ConvertToDraft call instead (M4 task 7's own DraftFlips reads);
	// markReadyCalls and convertToDraftCalls record the pull request node
	// id each call carried, in call order.
	markReadyErr        error
	convertToDraftErr   error
	markReadyCalls      []string
	convertToDraftCalls []string

	// mergeErr, when set, fails every Merge call instead (M4 task 8's own
	// PullRequests.Merge); mergeResultSHA backs its own successful string
	// return (never read by shipHandler.merge, but asserted by a test of
	// its own); mergeCalls records number|sha|method|title, in call order.
	mergeErr       error
	mergeResultSHA string
	mergeCalls     []string
}

// shipViewerLogin is shipGitHub's own default Viewer() result (M4 task 4):
// distinct from any login a test's own comment author uses, so isZingReply
// only matches a comment this package's own tests build with it on purpose.
const shipViewerLogin = "zing-bot"

// shipQuestionResolved is questionStateResolved's own literal (store.go's
// own constant is unexported from that package), named once here so the
// merge tests' own post-commit state checks (M4 task 8) share it (goconst).
const shipQuestionResolved = "resolved"

// shipMergeMethodSquash is MergeRule.Method's own fixed literal every M4
// task 8 merge test configures, named once (goconst) and shared with
// shiprules_test.go's own TestMergeDecision (same package).
const shipMergeMethodSquash = "squash"

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

// Merge gives shipGitHub job.PullRequests.Merge too (M4 task 8): a test
// configures mergeErr to simulate a GitHub refusal (wrap orchestrator.ErrMergeRefused
// for the MERGE row 8.8 covers specially) or any other failure, and reads
// mergeCalls back to assert the exact sha MERGE pinned the call to.
func (g *shipGitHub) Merge(_ context.Context, _, _ string, number int, sha, method, title string) (string, error) {
	g.mergeCalls = append(g.mergeCalls, fmt.Sprintf("%d|%s|%s|%s", number, sha, method, title))
	if g.mergeErr != nil {
		return "", g.mergeErr
	}
	return g.mergeResultSHA, nil
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

func (g *shipGitHub) ThreadCommentsContain(_ context.Context, threadID, needle, author string) (bool, error) {
	g.containsCalls = append(g.containsCalls, threadID+"|"+needle)
	if g.containsErr != nil {
		return false, g.containsErr
	}
	return g.markerAuthors[threadID+"|"+needle] == author, nil
}

// ReplyToThread records the call and, mirroring a real post landing on
// GitHub, registers the marker it carries under the viewer's own login, so
// a later ThreadCommentsContain call (this attempt's own idempotency guard,
// or a retry's) finds it. It refuses a body that does not start with the
// viewer's own disclosure prefix (design section 9.3, N3): every reply
// APPLY posts goes through replyBody first, and TestEveryReplyGoesThroughReplyBody
// proves this fake would catch one that somehow did not.
func (g *shipGitHub) ReplyToThread(_ context.Context, threadID, body string) error {
	login := g.viewerLogin
	if login == "" {
		login = shipViewerLogin
	}
	if !strings.HasPrefix(body, tracker.ReplyPrefix(login)) {
		return fmt.Errorf("shipGitHub: ReplyToThread: body does not start with the disclosure prefix: %q", body)
	}
	g.replies = append(g.replies, threadID+"|"+body)
	if idx := strings.LastIndex(body, "<!-- zing:"); idx >= 0 {
		marker := strings.TrimSpace(body[idx:])
		if g.markerAuthors == nil {
			g.markerAuthors = map[string]string{}
		}
		g.markerAuthors[threadID+"|"+marker] = login
	}
	return nil
}

func (g *shipGitHub) ResolveThread(_ context.Context, threadID string) error {
	if err, ok := g.resolveErrFor[threadID]; ok {
		return err
	}
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

// MarkReady and ConvertToDraft give shipGitHub job.DraftFlips too (M4 task
// 7): POLL's own tests configure markReadyErr/convertToDraftErr the same
// way every other write above is configured, and read markReadyCalls/
// convertToDraftCalls back to assert which pull request node id a row
// actually flipped.
func (g *shipGitHub) MarkReady(_ context.Context, prNodeID string) error {
	if g.markReadyErr != nil {
		return g.markReadyErr
	}
	g.markReadyCalls = append(g.markReadyCalls, prNodeID)
	return nil
}

func (g *shipGitHub) ConvertToDraft(_ context.Context, prNodeID string) error {
	if g.convertToDraftErr != nil {
		return g.convertToDraftErr
	}
	g.convertToDraftCalls = append(g.convertToDraftCalls, prNodeID)
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
	repoGit, ok = pbGitCommonDir(t, orch, localPath)
	if !ok {
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
// very pull request it opened. job.ReviewThreads and job.DraftFlips are
// filled the same conditional way (M4 tasks 4 and 7).
func shipBuildProjects(t *testing.T, s *store.Store, gh orchestrator.GitHub) map[int64]Project {
	t.Helper()
	projects, err := s.ListProjects(t.Context())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	prs, hasPRs := gh.(PullRequests)
	checks, hasChecks := gh.(Checks)
	threads, hasThreads := gh.(ReviewThreads)
	flips, hasFlips := gh.(DraftFlips)

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
		if hasFlips {
			proj.Flips = flips
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

// shipHasMessage reports whether c.Messages carries a message whose Body
// is exactly body, the one-line marker-presence check most of this file's
// merge tests repeat (M4 task 8).
func shipHasMessage(c store.HandlerCommit, body string) bool {
	for _, m := range c.Messages {
		if m.Body == body {
			return true
		}
	}
	return false
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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

// shipPollRunWithRule is shipPollRun with rule in place of Deps' own
// zero-value MergeRule (M4 task 8's own mergeDecision input, design
// section 8.8): a test of the automatic merge gate claims through this
// instead of shipPollRun.
func shipPollRunWithRule(t *testing.T, s *store.Store, ticket store.Ticket, gh *shipGitHub, tr *shipTracker, rule MergeRule) (store.HandlerCommit, error) {
	t.Helper()
	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	deps.MergeRule = rule
	return (shipHandler{}).Run(t.Context(), ticket, deps)
}

// shipAnswerMergeQuestion answers ticketID's one open merge question with
// option ("a" merges now, "b" holds), the same AnswerQuestion path the
// console's own SendBatch uses (clearMatchingWaitTx clears waiting_on at
// answer time, design section 4.2).
func shipAnswerMergeQuestion(t *testing.T, s *store.Store, ticketID int64, option string) {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, questionStateOpen)
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %d questions, want exactly 1", len(open))
	}
	result, err := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: option})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("AnswerQuestion: Accepted = false, Conflict = %q, want accepted", result.Conflict)
	}
}

// shipMergeReadyPR is the common CI-green, zero-thread, ready (not draft)
// pull-request state every row 9 test (M4 task 8) starts from: GetPR's own
// read that lets POLL reach the merge gate at all.
func shipMergeReadyPR(local, nodeID string) orchestrator.PRState {
	return orchestrator.PRState{State: "open", Draft: false, HeadSHA: local, BaseRef: pbFixtureDefaultBranch, NodeID: nodeID}
}

// shipCILogTailText is the canned JobLogTail text every failed-CI test
// below configures gh.logTail with (goconst: repeated across the ci_log
// fix-request test and task 7's own draft-flip tests).
const shipCILogTailText = "FAIL: boom"

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

// seedReReqHandled writes "reviewers re-requested <headSHA>" directly
// through CommitHandlerResult, the shape RE-REQUEST itself writes (design
// section 9.4, respond.go's reRequestedMarkerFor) -- standing in for a
// RE-REQUEST tick this test does not otherwise care about, so a pre-M4-task-6
// test whose own seeded fix landings also satisfy POLL row 2's entry
// condition (M4 task 6, design section 8.5) can isolate the row it exists
// to prove.
func seedReReqHandled(t *testing.T, s *store.Store, ticketID int64, headSHA string) {
	t.Helper()
	owner := "seed-rereq-handled"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("seedReReqHandled: claim: claimed=%v err=%v", claimed, err)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Messages: []store.Message{{TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, Body: reRequestedMarkerFor(headSHA)}},
	})
	if err != nil || !applied {
		t.Fatalf("seedReReqHandled: commit: applied=%v err=%v", applied, err)
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipFailedCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	gh.logTail = func(context.Context, string, string, int64, int) (string, error) { return shipCILogTailText, nil }

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want a fix request: %+v", commit.Escalation.Payload)
	}
	found := false
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, fixRequestedCILogPrefix) && strings.Contains(m.Body, shipCILogTailText) {
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
// The 3 landed fixes also satisfy POLL row 2's own entry condition (M4 task
// 6, design section 8.5): this test seeds the head's own "reviewers
// re-requested" marker up front, so RE-REQUEST does not claim the one tick
// this test drives, keeping its own assertion about the shared gate
// isolated from RE-REQUEST's.
func TestPollSharedGateEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	maxLoops := pbMachine(t).Jobs[jobRespondName].MaxLoops
	seedLandedFixRequests(t, s, ticket.ID, FixKindCILog, maxLoops)

	local := shipHeadSHA(t, s, pbGetTicket(t, s, ticket.ID))
	runs, required := shipFailedCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	seedReReqHandled(t, s, ticket.ID, local)

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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
// Missing differs from the previous poll's. Two required checks, "ci" and
// "deploy", keep CI pending across every poll here (deploy never reports),
// so this test's own three polls stay inside row 7 (8.5) and never reach
// row 8's own ready flip (M4 task 7): a scenario where Missing shrinks to
// empty while still pending cannot arise (CI is never pending with an
// empty Missing, design section 8.4 rule 3), so this is the strongest
// Missing-shrinks case row 7 can exercise on its own.
func TestPollCIWaitingMarkerOnChange(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	gh.required = []orchestrator.RequiredCheck{{Context: "ci"}, {Context: "deploy"}}
	gh.runs = nil // neither reports yet: Missing = ["ci","deploy"]

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (first poll): %v", err)
	}
	wantFirst := ciWaitingPrefix + "ci,deploy"
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

	// "ci" now reports, so Missing shrinks to ["deploy"]: a new marker is
	// written because the set changed. CI stays pending ("deploy" is still
	// missing), so this poll still never reaches row 8.
	gh.runs, _ = shipGreenCI()
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (second poll): %v", err)
	}
	wantSecond := ciWaitingPrefix + "deploy"
	foundSecond := false
	for _, m := range commit2.Messages {
		if m.Body == wantSecond {
			foundSecond = true
		}
	}
	if !foundSecond {
		t.Errorf("commit2.Messages = %+v, want %q (Missing changed to just deploy)", commit2.Messages, wantSecond)
	}
	pbApply(t, s, ticket, commit2)

	// A third poll with the same (still missing "deploy") Missing writes no
	// new marker.
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

// shipApplyThreadA, shipApplyThreadB, and shipApplyReplyText are APPLY's
// own tests' (M4 task 5) shared raw thread ids and reply text, named once
// since goconst flags a literal repeated this many times across one file.
const (
	shipApplyThreadA    = "RT_a"
	shipApplyThreadB    = "RT_b"
	shipApplyReplyText  = "fixed as described"
	shipApplyReplyTextA = "fixed a"
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

// shipApplySeeded publishes a ticket (shipPublished), seeds green CI and
// threads on the fake GitHub, writes a "respond batch 1 started sha <local>
// after run 0" marker in startRespondBatch's own shape, and inserts one
// "respond" artifact {actions, batch 1, sha local, seen} directly through
// Store.InsertArtifact -- APPLY's own tests need full control over which
// threads exist, which comment each one carries, and which actions the
// artifact names, more than driving a real RESPOND run through
// respondScriptsFS would give them. InsertArtifact only enforces the
// artifact's own JSON Schema, not Layer 2's semantic rules, which is also
// how TestApplyChecksEveryBodyBeforeWriting below seeds a reserved-marker
// action the real RESPOND pipeline could never produce in the first place
// (design section 9.3, checkRespondThreadsShape, response/semantics.go) --
// the exact defense-in-depth gap replyBody's own second layer exists to
// close. It does not run APPLY: the caller does, through shipPollRun (Run
// routes to APPLY on its own, applyArtifact, the moment a pending respond
// artifact exists).
func shipApplySeeded(t *testing.T, threads []orchestrator.Thread, actions []response.ThreadAction) (s *store.Store, ticket store.Ticket, gh *shipGitHub, tr *shipTracker, aid int64) {
	t.Helper()
	s, ticket, gh, tr = shipPublished(t)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	local := shipHeadSHA(t, s, ticket)
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	gh.threads = threads

	tids := make([]string, len(threads))
	seenMap := make(map[string]string, len(threads))
	for i, th := range threads {
		id := tid(th.ID)
		tids[i] = id
		if digest, ok := lastHumanCommentDigest(th, shipViewerLogin); ok {
			seenMap[id] = digest
		}
	}
	sort.Strings(tids)
	line2, line3 := respondBatchLines(tids, seenMap)

	ctx := t.Context()
	owner := "ship-apply-seed-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(ctx, ticket.ID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("shipApplySeeded: claim: claimed=%v err=%v", claimed, err)
	}
	startedBody := fmt.Sprintf("respond batch 1 started sha %s after run 0\n%s\n%s", local, line2, line3)
	applied, err := s.CommitHandlerResult(ctx, store.HandlerCommit{
		TicketID: ticket.ID, Owner: owner, Expires: expires,
		Messages: []store.Message{{TicketID: ticket.ID, Type: msgTypeUpdate, Author: authorSystem, Body: startedBody}},
	})
	if err != nil || !applied {
		t.Fatalf("shipApplySeeded: seed started marker: applied=%v err=%v", applied, err)
	}

	seen := make([]response.ThreadSeen, 0, len(tids))
	for _, id := range tids {
		if digest, ok := seenMap[id]; ok {
			seen = append(seen, response.ThreadSeen{TID: id, LastComment: digest})
		}
	}
	artifact := response.RespondArtifact{Threads: actions, Batch: 1, SHA: local, Seen: seen}
	payload, marshalErr := json.Marshal(artifact)
	if marshalErr != nil {
		t.Fatalf("shipApplySeeded: marshal artifact: %v", marshalErr)
	}
	aid, insertErr := s.InsertArtifact(ctx, store.Artifact{TicketID: ticket.ID, Type: artifactTypeRespond, Payload: payload})
	if insertErr != nil {
		t.Fatalf("shipApplySeeded: insert artifact: %v", insertErr)
	}

	return s, pbGetTicket(t, s, ticket.ID), gh, tr, aid
}

// TestPollStartsRespondBatch proves design section 8.5 row 5: an actionable
// thread writes "respond batch 1 started sha <local> after run <R>", its
// own tids sorted on line 2 and their seen digests on line 3, and no run.
func TestPollStartsRespondBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
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

// -----------------------------------------------------------------------
// APPLY (M4 task 5, design section 9.3)
// -----------------------------------------------------------------------

// TestApplyRepliesAndResolves proves design section 9.3 step 2's own reply
// path: the posted body is exactly replyBody's own output, the thread
// resolves, and the closing "respond applied <aid>" marker reports what
// this commit did.
func TestApplyRepliesAndResolves(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", when))
	s, ticket, gh, tr, aid := shipApplySeeded(t, []orchestrator.Thread{thread}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbReply, Text: "Thanks for flagging this -- fixed as described."},
	})

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want a reply: %+v", commit.Escalation.Payload)
	}

	marker := fmt.Sprintf("<!-- zing:reply a%d %s -->", aid, tid(shipApplyThreadA))
	wantBody, bodyErr := replyBody(shipViewerLogin, "Thanks for flagging this -- fixed as described.", marker)
	if bodyErr != nil {
		t.Fatalf("replyBody: %v", bodyErr)
	}
	if len(gh.replies) != 1 || gh.replies[0] != "RT_a|"+wantBody {
		t.Errorf("replies = %+v, want exactly [%q]", gh.replies, "RT_a|"+wantBody)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != shipApplyThreadA {
		t.Errorf("resolves = %+v, want exactly [RT_a]", gh.resolves)
	}

	want := fmt.Sprintf("respond applied %d\nreplied 1 fixing 0 skipped 0", aid)
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestApplyStaleEditedComment proves design section 9.3 step 1a's own first
// freshness check: a human edits the thread's own last comment in place
// (same id, a different body and UpdatedAt), so commentDigest disagrees
// with the batch's own seen snapshot, and APPLY writes "respond batch 1
// stale" instead of posting.
func TestApplyStaleEditedComment(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", when))
	s, ticket, gh, tr, _ := shipApplySeeded(t, []orchestrator.Thread{thread}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbReply, Text: shipApplyReplyText},
	})

	gh.threads[0].Comments[0] = shipHumanComment("c1", "reviewer1", "actually, please also fix this", when.Add(time.Hour))

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "respond batch 1 stale\nthread " + tid(shipApplyThreadA) + " has a new comment"
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
	if len(gh.replies) != 0 || len(gh.resolves) != 0 {
		t.Errorf("replies = %+v resolves = %+v, want none -- the artifact is never applied", gh.replies, gh.resolves)
	}
}

// TestApplyStaleNewComment proves design section 9.3 step 1a's other
// freshness check: a human adds a new comment to a batch thread before
// APPLY runs, so APPLY writes "respond batch 1 stale" and posts nothing,
// and the next poll starts batch 2 from the threads as they now are
// (design section 9.2).
func TestApplyStaleNewComment(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", when))
	s, ticket, gh, tr, _ := shipApplySeeded(t, []orchestrator.Thread{thread}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbReply, Text: shipApplyReplyText},
	})

	gh.threads[0].Comments = append(gh.threads[0].Comments,
		shipHumanComment("c2", "reviewer1", "actually, one more thing", when.Add(time.Hour)))

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "respond batch 1 stale\nthread " + tid(shipApplyThreadA) + " has a new comment"
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
	if len(gh.replies) != 0 || len(gh.resolves) != 0 {
		t.Errorf("replies = %+v resolves = %+v, want none", gh.replies, gh.resolves)
	}
	pbApply(t, s, ticket, commit)

	pollCommit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("poll Run: %v", err)
	}
	found := false
	for _, m := range pollCommit.Messages {
		if strings.HasPrefix(m.Body, "respond batch 2 started sha ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("pollCommit.Messages = %+v, want a respond batch 2 started marker", pollCommit.Messages)
	}
}

// TestApplyStaleHeadMoved proves design section 9.3 step 1a's own head
// check: the pull request head no longer equals the artifact's own sha, so
// APPLY writes "respond batch 1 stale" with the head-moved reason and
// posts nothing.
func TestApplyStaleHeadMoved(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", when))
	s, ticket, gh, tr, _ := shipApplySeeded(t, []orchestrator.Thread{thread}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbReply, Text: shipApplyReplyText},
	})

	gh.prState.HeadSHA = strings.Repeat("a", 40)

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "respond batch 1 stale\n" + respondStaleHeadMovedReason
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
	if len(gh.replies) != 0 || len(gh.resolves) != 0 {
		t.Errorf("replies = %+v resolves = %+v, want none", gh.replies, gh.resolves)
	}
}

// TestApplyChecksEveryBodyBeforeWriting proves design section 9.3 step 1b:
// every reply body is built, through replyBody, before the first GitHub
// write. One bad action's own text (seeded directly, the defense-in-depth
// gap Layer 2 would normally close before the artifact ever existed --
// shipApplySeeded's own doc comment) makes replyBody refuse, and Run
// returns that error with no write at all, not even for the thread that
// would otherwise have been fine.
func TestApplyChecksEveryBodyBeforeWriting(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	threadA := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("ca", "reviewer1", "please fix a", when))
	threadB := shipThread(shipApplyThreadB, "handler.go", 5, shipHumanComment("cb", "reviewer1", "please fix b", when))
	s, ticket, gh, tr, _ := shipApplySeeded(t, []orchestrator.Thread{threadA, threadB}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbReply, Text: shipApplyReplyTextA},
		{ID: tid(shipApplyThreadB), Action: response.ThreadVerbReply, Text: "<!-- zing:forged --> fixed b"},
	})

	_, err := shipPollRun(t, s, ticket, gh, tr)
	if err == nil {
		t.Fatal("Run: want an error for a reserved-marker reply text, got nil")
	}
	if len(gh.replies) != 0 {
		t.Errorf("replies = %+v, want none -- every body must build before the first write", gh.replies)
	}
	if len(gh.resolves) != 0 {
		t.Errorf("resolves = %+v, want none", gh.resolves)
	}
}

// TestApplySkipsResolvedAndMissing proves design section 9.3 step 2's own
// skip branch: a thread the owner already resolved and a thread that no
// longer exists both skip without a GitHub write, and the closing marker's
// own skipped count reports both.
func TestApplySkipsResolvedAndMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	threadA := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("ca", "reviewer1", "please fix a", when))
	threadB := orchestrator.Thread{
		ID: shipApplyThreadB, IsResolved: true, Path: "greet.go", Line: 5,
		Comments: []orchestrator.ThreadComment{shipHumanComment("cb", "reviewer1", "please fix b", when)},
	}
	threadC := shipThread("RT_missing", "main.go", 7, shipHumanComment("cc", "reviewer1", "please fix c", when))

	s, ticket, gh, tr, aid := shipApplySeeded(t, []orchestrator.Thread{threadA, threadB, threadC}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbReply, Text: shipApplyReplyTextA},
		{ID: tid(shipApplyThreadB), Action: response.ThreadVerbReply, Text: "fixed b"},
		{ID: tid("RT_missing"), Action: response.ThreadVerbReply, Text: "fixed missing"},
	})
	// RT_missing disappears from GitHub between the batch starting and
	// APPLY running (deleted, or the review itself withdrawn).
	gh.threads = []orchestrator.Thread{threadA, threadB}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation: %+v", commit.Escalation.Payload)
	}
	if len(gh.replies) != 1 || !strings.HasPrefix(gh.replies[0], "RT_a|") {
		t.Errorf("replies = %+v, want exactly one for RT_a", gh.replies)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != shipApplyThreadA {
		t.Errorf("resolves = %+v, want exactly [RT_a]", gh.resolves)
	}
	want := fmt.Sprintf("respond applied %d\nreplied 1 fixing 0 skipped 2", aid)
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
}

// TestApplyIsIdempotentAfterCrash proves design section 11's own reply-write
// row: a prior, crashed APPLY already posted the reply (its marker visible
// in the thread's own recent comments, authored by the viewer), so this
// attempt posts no second comment and still resolves the thread.
func TestApplyIsIdempotentAfterCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", when))
	s, ticket, gh, tr, aid := shipApplySeeded(t, []orchestrator.Thread{thread}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbReply, Text: shipApplyReplyText},
	})

	marker := fmt.Sprintf("<!-- zing:reply a%d %s -->", aid, tid(shipApplyThreadA))
	replyText, bodyErr := replyBody(shipViewerLogin, shipApplyReplyText, marker)
	if bodyErr != nil {
		t.Fatalf("replyBody: %v", bodyErr)
	}
	gh.threads[0].Comments = append(gh.threads[0].Comments,
		shipHumanComment("c2", shipViewerLogin, replyText, when.Add(time.Minute)))
	gh.markerAuthors = map[string]string{"RT_a|" + marker: shipViewerLogin}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.replies) != 0 {
		t.Errorf("replies = %+v, want none -- the marker is already posted", gh.replies)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != shipApplyThreadA {
		t.Errorf("resolves = %+v, want exactly [RT_a]", gh.resolves)
	}
	want := fmt.Sprintf("respond applied %d\nreplied 1 fixing 0 skipped 0", aid)
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
}

// TestApplyFindsMarkerOlderThan100Comments proves the same idempotency
// guard holds even when the marker sits outside the thread's own last-100
// window (th.Comments, what ListThreads returns): APPLY never scans
// th.Comments for it, only ThreadCommentsContain's own unbounded page
// walk, which this test's own markerAuthors stands in for (design section
// 10.4: "pages every comment of the thread, not only the last 100").
func TestApplyFindsMarkerOlderThan100Comments(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", when))
	s, ticket, gh, tr, aid := shipApplySeeded(t, []orchestrator.Thread{thread}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbReply, Text: shipApplyReplyText},
	})

	marker := fmt.Sprintf("<!-- zing:reply a%d %s -->", aid, tid(shipApplyThreadA))
	gh.markerAuthors = map[string]string{"RT_a|" + marker: shipViewerLogin}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.containsCalls) != 1 || gh.containsCalls[0] != "RT_a|"+marker {
		t.Errorf("containsCalls = %+v, want exactly [%q]", gh.containsCalls, "RT_a|"+marker)
	}
	if len(gh.replies) != 0 {
		t.Errorf("replies = %+v, want none -- ThreadCommentsContain already found the marker", gh.replies)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != shipApplyThreadA {
		t.Errorf("resolves = %+v, want exactly [RT_a]", gh.resolves)
	}
	want := fmt.Sprintf("respond applied %d\nreplied 1 fixing 0 skipped 0", aid)
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
}

// TestApplySpoofedMarkerStillPosts proves design section 9.1's own
// author-gate: a stranger's comment that copies the exact reply marker
// text never satisfies ThreadCommentsContain (author-gated to the viewer),
// so APPLY still posts its own reply.
func TestApplySpoofedMarkerStillPosts(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", when))
	s, ticket, gh, tr, aid := shipApplySeeded(t, []orchestrator.Thread{thread}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbReply, Text: shipApplyReplyText},
	})

	marker := fmt.Sprintf("<!-- zing:reply a%d %s -->", aid, tid(shipApplyThreadA))
	gh.markerAuthors = map[string]string{"RT_a|" + marker: "attacker"}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.replies) != 1 {
		t.Fatalf("replies = %+v, want exactly one -- a spoofed marker must not block a real post", gh.replies)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != shipApplyThreadA {
		t.Errorf("resolves = %+v, want exactly [RT_a]", gh.resolves)
	}
	want := fmt.Sprintf("respond applied %d\nreplied 1 fixing 0 skipped 0", aid)
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
}

// TestApplyCrashBetweenReplyAndResolve proves design section 11's own
// resolve-write row: ResolveThread fails for the second of two threads
// after both replies already posted, so Run returns the error with no
// commit; the retry, with GitHub now reading the first thread back
// resolved and the injected failure gone, posts no second reply for
// either thread (the first is skipped as already resolved, the second's
// own marker is already there) and finishes the batch.
func TestApplyCrashBetweenReplyAndResolve(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	threadA := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("ca", "reviewer1", "please fix a", when))
	threadB := shipThread(shipApplyThreadB, "handler.go", 5, shipHumanComment("cb", "reviewer1", "please fix b", when))
	s, ticket, gh, tr, aid := shipApplySeeded(t, []orchestrator.Thread{threadA, threadB}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbReply, Text: shipApplyReplyTextA},
		{ID: tid(shipApplyThreadB), Action: response.ThreadVerbReply, Text: "fixed b"},
	})

	injected := errors.New("resolve: injected failure")
	gh.resolveErrFor = map[string]error{shipApplyThreadB: injected}

	_, err := shipPollRun(t, s, ticket, gh, tr)
	if err == nil {
		t.Fatal("Run: want an error, got nil")
	}
	if len(gh.replies) != 2 {
		t.Fatalf("replies = %+v, want 2 (both posted before the resolve failure)", gh.replies)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != shipApplyThreadA {
		t.Fatalf("resolves = %+v, want exactly [RT_a]", gh.resolves)
	}
	appliedRows, markerErr := s.MarkersWithPrefix(t.Context(), ticket.ID, "respond applied ")
	if markerErr != nil {
		t.Fatalf("MarkersWithPrefix: %v", markerErr)
	}
	if len(appliedRows) != 0 {
		t.Fatalf("respond applied markers = %d, want none yet (the failed attempt wrote no commit)", len(appliedRows))
	}

	gh.threads[0].IsResolved = true
	gh.resolveErrFor = nil

	// The failed attempt's own Go error, not a HandlerCommit, left the
	// claim held: nothing released it. ExpireClaims is the same reconcile a
	// crash or a dispatcher restart runs for real (building_test.go,
	// fix_test.go give this same pattern).
	if _, expireErr := s.ExpireClaims(t.Context(), time.Now().Add(20*time.Minute)); expireErr != nil {
		t.Fatalf("ExpireClaims: %v", expireErr)
	}

	commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("retry Run: %v", err)
	}
	if len(gh.replies) != 2 {
		t.Errorf("replies after retry = %+v, want still 2 (RT_b's own marker is already posted)", gh.replies)
	}
	if len(gh.resolves) != 2 || gh.resolves[1] != shipApplyThreadB {
		t.Errorf("resolves after retry = %+v, want [RT_a RT_b]", gh.resolves)
	}
	want := fmt.Sprintf("respond applied %d\nreplied 1 fixing 0 skipped 1", aid)
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
}

// TestApplyFixThreadsRequestFix proves design section 9.3 step 3: a
// collected "fix" action never touches GitHub, and becomes one fix request
// naming the thread's own tid, location, and text, with the closing marker
// pointing at it by its own watermark run id.
func TestApplyFixThreadsRequestFix(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please address this properly", when))
	s, ticket, gh, tr, aid := shipApplySeeded(t, []orchestrator.Thread{thread}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbFix, Text: "Validate the input before using it"},
	})

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want a fix request: %+v", commit.Escalation.Payload)
	}
	if len(gh.replies) != 0 || len(gh.resolves) != 0 {
		t.Errorf("replies = %+v resolves = %+v, want none -- a fix action never posts to GitHub", gh.replies, gh.resolves)
	}
	if len(commit.Messages) != 2 {
		t.Fatalf("commit.Messages = %+v, want exactly 2 (the fix request, then the applied marker)", commit.Messages)
	}
	if !strings.HasPrefix(commit.Messages[0].Body, fixRequestedThreadsPrefix) {
		t.Errorf("Messages[0] = %q, want prefix %q", commit.Messages[0].Body, fixRequestedThreadsPrefix)
	}
	wantText := fmt.Sprintf("Review threads from respond batch 1:\nthread %s (greet.go:3): Validate the input before using it", tid(shipApplyThreadA))
	if !strings.HasSuffix(commit.Messages[0].Body, wantText) {
		t.Errorf("Messages[0] = %q, want suffix %q", commit.Messages[0].Body, wantText)
	}

	wantApplied := fmt.Sprintf("respond applied %d\nreplied 0 fixing 1 skipped 0", aid)
	if !strings.HasPrefix(commit.Messages[1].Body, wantApplied) {
		t.Errorf("Messages[1] = %q, want prefix %q", commit.Messages[1].Body, wantApplied)
	}
	if !strings.Contains(commit.Messages[1].Body, "\nfix request after run ") {
		t.Errorf("Messages[1] = %q, want a \"fix request after run \" line", commit.Messages[1].Body)
	}
}

// TestApplyFixThreadsHitGate proves design section 8.7's own shared gate,
// reached through APPLY: the ticket already holds max_loops fix requests,
// so the collected fix action escalates loops_exhausted instead of writing
// a fourth request, Tried names the respond artifact the gate held open,
// and no "respond applied <aid>" marker lands -- but the batch's own reply
// action, already decided before the gate, still posts and resolves.
func TestApplyFixThreadsHitGate(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	threadFix := shipThread("RT_fix", "greet.go", 3, shipHumanComment("cf", "reviewer1", "please address this", when))
	threadReply := shipThread("RT_reply", "main.go", 9, shipHumanComment("cr", "reviewer1", "small nit", when))

	s, ticket, gh, tr, aid := shipApplySeeded(t, []orchestrator.Thread{threadFix, threadReply}, []response.ThreadAction{
		{ID: tid("RT_fix"), Action: response.ThreadVerbFix, Text: "Validate the input"},
		{ID: tid("RT_reply"), Action: response.ThreadVerbReply, Text: "Good catch, thanks"},
	})
	seedLandedFixRequests(t, s, ticket.ID, FixKindCILog, 3) // jobs.respond.max_loops

	commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("Run: want an escalation, got none")
	}
	if commit.Escalation.Payload.Code != string(response.EscalationCodeLoopsExhausted) {
		t.Errorf("Code = %q, want %q", commit.Escalation.Payload.Code, response.EscalationCodeLoopsExhausted)
	}
	wantTriedPrefix := fmt.Sprintf("threads\nrespond %d\n", aid)
	if !strings.HasPrefix(commit.Escalation.Payload.Tried, wantTriedPrefix) {
		t.Errorf("Tried = %q, want prefix %q", commit.Escalation.Payload.Tried, wantTriedPrefix)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}

	if len(gh.replies) != 1 || !strings.HasPrefix(gh.replies[0], "RT_reply|") {
		t.Errorf("replies = %+v, want exactly one for RT_reply", gh.replies)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != "RT_reply" {
		t.Errorf("resolves = %+v, want exactly [RT_reply]", gh.resolves)
	}
	appliedRows, markerErr := s.MarkersWithPrefix(t.Context(), ticket.ID, fmt.Sprintf("respond applied %d", aid))
	if markerErr != nil {
		t.Fatalf("MarkersWithPrefix: %v", markerErr)
	}
	if len(appliedRows) != 0 {
		t.Errorf("respond applied markers = %d, want none -- the gate held the artifact open", len(appliedRows))
	}
}

// TestEveryReplyGoesThroughReplyBody proves N3 (disclosure) holds even
// against this package's own test double: shipGitHub.ReplyToThread itself
// refuses a body that does not start with the viewer's own disclosure
// prefix, and a real APPLY run's own reply -- built through replyBody --
// passes it.
func TestEveryReplyGoesThroughReplyBody(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	gh := &shipGitHub{}
	if err := gh.ReplyToThread(t.Context(), "RT_x", "not disclosed"); err == nil {
		t.Fatal("ReplyToThread: want an error for a body missing the disclosure prefix, got nil")
	}

	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", when))
	s, ticket, gh2, tr, _ := shipApplySeeded(t, []orchestrator.Thread{thread}, []response.ThreadAction{
		{ID: tid(shipApplyThreadA), Action: response.ThreadVerbReply, Text: shipApplyReplyText},
	})
	commit, err := shipPollRun(t, s, ticket, gh2, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation: %+v", commit.Escalation.Payload)
	}
	if len(gh2.replies) != 1 {
		t.Fatalf("replies = %d, want 1 (a well-formed reply body passes the fake's own disclosure guard)", len(gh2.replies))
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

// -----------------------------------------------------------------------
// FIX-REPLIES and RE-REQUEST (design section 9.4, M4 task 6)
// -----------------------------------------------------------------------

// shipFixThreadLandedText is the one fix text every shipFixThreadLanded
// caller needs (design section 9.4's own tests never vary it): a plain,
// single-sentence instruction a fix unit can act on.
const shipFixThreadLandedText = "Validate the input before using it"

// shipFixLanding is shipFixThreadLanded's own result, bundled (gocritic's
// own too-many-results guard): the store and ticket a caller drives further
// polls through, the fake GitHub and tracker, the fixed respond artifact's
// own id, and the branch's sha before and after the fix landed.
type shipFixLanding struct {
	s                     *store.Store
	ticket                store.Ticket
	gh                    *shipGitHub
	tr                    *shipTracker
	aid                   int64
	preFixSHA, postFixSHA string
}

// shipFixThreadLanded publishes a ticket, seeds one "fix" action respond
// batch over thread (shipApplySeeded), runs APPLY to collect the resulting
// fix request, and drives that fix unit to landing (driveShipFixToLanding).
// It leaves gh.prState.HeadSHA at the pre-fix sha, exactly the state POLL
// sees the instant after the fix lands locally but before anything has told
// GitHub about it: row 1's own entry condition (fixRepliesPending's
// "IsAncestor(S, pr.HeadSHA)") cannot hold yet. The caller advances
// gh.prState.HeadSHA to postFixSHA (and reruns POLL) to simulate the push
// landing on GitHub, the same two-step TestPollPushesLocalAhead already
// drives.
func shipFixThreadLanded(t *testing.T, thread orchestrator.Thread) shipFixLanding {
	t.Helper()
	s, ticket, gh, tr, aid := shipApplySeeded(t, []orchestrator.Thread{thread}, []response.ThreadAction{
		{ID: tid(thread.ID), Action: response.ThreadVerbFix, Text: shipFixThreadLandedText},
	})
	preFixSHA := shipHeadSHA(t, s, ticket)

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("shipFixThreadLanded: apply: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("shipFixThreadLanded: apply escalated: %+v", commit.Escalation.Payload)
	}
	pbApply(t, s, ticket, commit)

	rt := runtime.NewFake(fstest.MapFS{shipFixBuildScriptPath: &fstest.MapFile{Data: []byte(judgeFixBuildScript)}})
	driveShipFixToLanding(t, s, ticket.ID, rt)

	ticket = pbGetTicket(t, s, ticket.ID)
	postFixSHA := shipHeadSHA(t, s, ticket)
	return shipFixLanding{s: s, ticket: ticket, gh: gh, tr: tr, aid: aid, preFixSHA: preFixSHA, postFixSHA: postFixSHA}
}

// shipPollUntilMarker drives shipPollRun, applying every commit, until one
// carries a message whose body starts with prefix or maxTicks is
// exhausted: design section 8.5's own "one row per tick" rule means an
// unrelated row -- RE-REQUEST, most often, once M4 task 6 wires it in --
// can legitimately claim an earlier tick than the one a test is after.
func shipPollUntilMarker(t *testing.T, s *store.Store, ticket store.Ticket, gh *shipGitHub, tr *shipTracker, prefix string, maxTicks int) store.HandlerCommit {
	t.Helper()
	for i := range maxTicks {
		commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
		if err != nil {
			t.Fatalf("shipPollUntilMarker: Run (tick %d): %v", i, err)
		}
		pbApply(t, s, ticket, commit)
		for _, m := range commit.Messages {
			if strings.HasPrefix(m.Body, prefix) {
				return commit
			}
		}
	}
	t.Fatalf("shipPollUntilMarker: %q not seen within %d ticks", prefix, maxTicks)
	return store.HandlerCommit{}
}

// TestFixRepliesAfterPush proves design section 8.5 row 1's own entry
// condition: FIX-REPLIES does not fire while GitHub still reports the
// pre-fix head (POLL instead takes the ordinary push branch, design section
// 8.3 step 4), and does fire once GitHub reports the landed fix's own sha
// -- a disclosed "Fixed in <sha7>." reply, the thread resolved, and the
// closing "fix replies posted <aid>" marker.
func TestFixRepliesAfterPush(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please address this properly", when))
	landing := shipFixThreadLanded(t, thread)
	s, ticket, gh, tr := landing.s, landing.ticket, landing.gh, landing.tr

	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: landing.preFixSHA, BaseRef: pbFixtureDefaultBranch}
	commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (before push): %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation before push: %+v", commit.Escalation.Payload)
	}
	if len(gh.replies) != 0 || len(gh.resolves) != 0 {
		t.Fatalf("replies/resolves before push = %+v/%+v, want none", gh.replies, gh.resolves)
	}
	pbApply(t, s, ticket, commit)

	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: landing.postFixSHA, BaseRef: pbFixtureDefaultBranch}
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (after push): %v", err)
	}
	if commit2.Escalation != nil {
		t.Fatalf("got an escalation after push: %+v", commit2.Escalation.Payload)
	}
	if len(gh.replies) != 1 || !strings.HasPrefix(gh.replies[0], shipApplyThreadA+"|") {
		t.Fatalf("replies = %+v, want exactly one for %q", gh.replies, shipApplyThreadA)
	}
	if !strings.Contains(gh.replies[0], "Fixed in "+landing.postFixSHA[:7]+".") {
		t.Errorf("reply body = %q, want it to mention %q", gh.replies[0], "Fixed in "+landing.postFixSHA[:7])
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != shipApplyThreadA {
		t.Fatalf("resolves = %+v, want exactly [%q]", gh.resolves, shipApplyThreadA)
	}
	want := fmt.Sprintf("fix replies posted %d", landing.aid)
	if len(commit2.Messages) != 1 || commit2.Messages[0].Body != want {
		t.Errorf("commit2.Messages = %+v, want exactly %q", commit2.Messages, want)
	}
	if !commit2.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestFixRepliesStaleNewComment proves design section 9.4's own freshness
// check: a human comments on the fixed thread before FIX-REPLIES' own tick,
// so it writes "respond batch 1 stale" and posts nothing, and (since
// "fix replies posted <aid>" is never written) the thread -- now actionable
// again -- is answered by a fresh batch instead.
func TestFixRepliesStaleNewComment(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please address this properly", when))
	landing := shipFixThreadLanded(t, thread)
	s, ticket, gh, tr := landing.s, landing.ticket, landing.gh, landing.tr

	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: landing.postFixSHA, BaseRef: pbFixtureDefaultBranch}
	gh.threads[0].Comments = append(gh.threads[0].Comments,
		shipHumanComment("c2", "reviewer1", "actually, this still isn't right", when.Add(time.Hour)))

	commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "respond batch 1 stale\nthread " + tid(shipApplyThreadA) + " has a new comment"
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
	if len(gh.replies) != 0 || len(gh.resolves) != 0 {
		t.Errorf("replies/resolves = %+v/%+v, want none", gh.replies, gh.resolves)
	}
	postedRows, err := s.MarkersWithPrefix(t.Context(), ticket.ID, fixRepliesPostedMarkerFor(landing.aid))
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	if len(postedRows) != 0 {
		t.Errorf("fix replies posted markers = %d, want none", len(postedRows))
	}
	pbApply(t, s, ticket, commit)

	shipPollUntilMarker(t, s, pbGetTicket(t, s, ticket.ID), gh, tr, "respond batch 2 started sha ", 4)
}

// TestFixRepliesIdempotent proves design section 11's own reply-to-a-thread
// guard, reused by FIX-REPLIES: a crash that already posted the "fixed"
// marker but never reached the commit is found by ThreadCommentsContain, so
// the retry posts no second reply, still resolves the thread, and still
// writes the closing marker.
func TestFixRepliesIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please address this properly", when))
	landing := shipFixThreadLanded(t, thread)
	s, ticket, gh, tr := landing.s, landing.ticket, landing.gh, landing.tr

	marker := fmt.Sprintf("<!-- zing:fixed a%d %s -->", landing.aid, tid(shipApplyThreadA))
	gh.markerAuthors = map[string]string{shipApplyThreadA + "|" + marker: shipViewerLogin}
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: landing.postFixSHA, BaseRef: pbFixtureDefaultBranch}

	commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.replies) != 0 {
		t.Errorf("replies = %+v, want none -- the marker is already posted", gh.replies)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != shipApplyThreadA {
		t.Errorf("resolves = %+v, want exactly [%q]", gh.resolves, shipApplyThreadA)
	}
	want := fmt.Sprintf("fix replies posted %d", landing.aid)
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
}

// TestFixRepliesSpoofedMarkerStillPosts proves design section 9.1's own
// author-gate, reused by FIX-REPLIES (deferred here from M4 task 5): a
// stranger's comment that copies the exact "fixed" marker text never
// satisfies ThreadCommentsContain (author-gated to the viewer), so
// FIX-REPLIES still posts its own disclosed reply.
func TestFixRepliesSpoofedMarkerStillPosts(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	thread := shipThread(shipApplyThreadA, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please address this properly", when))
	landing := shipFixThreadLanded(t, thread)
	s, ticket, gh, tr := landing.s, landing.ticket, landing.gh, landing.tr

	marker := fmt.Sprintf("<!-- zing:fixed a%d %s -->", landing.aid, tid(shipApplyThreadA))
	gh.markerAuthors = map[string]string{shipApplyThreadA + "|" + marker: "attacker"}
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: landing.postFixSHA, BaseRef: pbFixtureDefaultBranch}

	commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.replies) != 1 || !strings.HasPrefix(gh.replies[0], shipApplyThreadA+"|") {
		t.Fatalf("replies = %+v, want exactly one for %q -- a spoofed marker from another author never satisfies the guard", gh.replies, shipApplyThreadA)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != shipApplyThreadA {
		t.Errorf("resolves = %+v, want exactly [%q]", gh.resolves, shipApplyThreadA)
	}
	want := fmt.Sprintf("fix replies posted %d", landing.aid)
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
}

// shipStaleReviewLogin, shipReviewUserType, shipReviewApproved,
// shipReviewChangesRequested, and shipStaleReviewCommitID are
// TestReRequest*'s own shared review fixture literals (goconst): one stale
// login and the fields a stale review's own newest row carries.
const (
	shipStaleReviewLogin       = "alice"
	shipReviewUserType         = "User"
	shipReviewApproved         = "APPROVED"
	shipReviewChangesRequested = "CHANGES_REQUESTED"
	shipStaleReviewCommitID    = "deadbeef"
)

// shipReReqReady publishes a ticket, seeds one landed ci_log fix request
// (seedLandedFixRequests -- RE-REQUEST's own entry condition, design
// section 8.5 row 2, reads only the marker order, never a real git
// ancestor relationship, unlike row 1's), and configures green CI so POLL
// reaches row 2 cleanly.
func shipReReqReady(t *testing.T) (s *store.Store, ticket store.Ticket, gh *shipGitHub, tr *shipTracker, local string) {
	t.Helper()
	s, ticket, gh, tr = shipPublished(t)
	seedLandedFixRequests(t, s, ticket.ID, FixKindCILog, 1)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	local = shipHeadSHA(t, s, pbGetTicket(t, s, ticket.ID))
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	return s, pbGetTicket(t, s, ticket.ID), gh, tr, local
}

// TestReRequestStaleReviewers proves design section 9.4's own RE-REQUEST
// selection: a login whose newest review names an older commit is stale; a
// review on the current head, a bot's review, and the viewer's own review
// are all excluded.
func TestReRequestStaleReviewers(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr, local := shipReReqReady(t)
	gh.reviews = []orchestrator.Review{
		{Login: shipStaleReviewLogin, UserType: shipReviewUserType, State: shipReviewApproved, CommitID: shipStaleReviewCommitID},
		{Login: "bob", UserType: shipReviewUserType, State: shipReviewChangesRequested, CommitID: local},
		{Login: "carol-bot", UserType: "Bot", State: shipReviewApproved, CommitID: shipStaleReviewCommitID},
		{Login: shipViewerLogin, UserType: shipReviewUserType, State: shipReviewApproved, CommitID: shipStaleReviewCommitID},
		{Login: "dave", UserType: shipReviewUserType, State: "DISMISSED", CommitID: shipStaleReviewCommitID},
	}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.requestedReviewers) != 1 || gh.requestedReviewers[0] != shipStaleReviewLogin {
		t.Errorf("requestedReviewers = %+v, want exactly [%s]", gh.requestedReviewers, shipStaleReviewLogin)
	}
	want := "reviewers re-requested " + local + "\n" + shipStaleReviewLogin
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestReRequestOncePerHead proves design section 9.4's own closing marker:
// once "reviewers re-requested <head>" lands, a second poll at the same
// head calls RequestReviewers no further times and writes no second
// marker.
func TestReRequestOncePerHead(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr, _ := shipReReqReady(t)
	gh.reviews = []orchestrator.Review{
		{Login: shipStaleReviewLogin, UserType: shipReviewUserType, State: shipReviewApproved, CommitID: shipStaleReviewCommitID},
	}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (first): %v", err)
	}
	pbApply(t, s, ticket, commit)
	if len(gh.requestedReviewers) != 1 {
		t.Fatalf("requestedReviewers after the first poll = %+v, want exactly one request", gh.requestedReviewers)
	}

	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (second): %v", err)
	}
	if len(gh.requestedReviewers) != 1 {
		t.Errorf("requestedReviewers after the second poll = %+v, want still exactly one -- this head is already handled", gh.requestedReviewers)
	}
	for _, m := range commit2.Messages {
		if strings.HasPrefix(m.Body, "reviewers re-requested ") {
			t.Errorf("commit2.Messages = %+v, want no second reviewers re-requested marker", commit2.Messages)
		}
	}
}

// TestReRequestSkipsUnrequestable proves design section 9.4's own 422
// handling: GitHub refusing one login (not a collaborator, say) is logged
// and skipped, and every other stale login is still requested in the same
// commit.
func TestReRequestSkipsUnrequestable(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr, local := shipReReqReady(t)
	gh.reviews = []orchestrator.Review{
		{Login: shipStaleReviewLogin, UserType: shipReviewUserType, State: shipReviewApproved, CommitID: shipStaleReviewCommitID},
		{Login: "ex-collaborator", UserType: shipReviewUserType, State: shipReviewChangesRequested, CommitID: shipStaleReviewCommitID},
	}
	gh.requestReviewersErr = map[string]error{
		"ex-collaborator": &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnprocessableEntity}},
	}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.requestedReviewers) != 1 || gh.requestedReviewers[0] != shipStaleReviewLogin {
		t.Errorf("requestedReviewers = %+v, want exactly [%s] -- ex-collaborator's 422 is skipped", gh.requestedReviewers, shipStaleReviewLogin)
	}
	want := "reviewers re-requested " + local + "\n" + shipStaleReviewLogin
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
}

// -----------------------------------------------------------------------
// POLL: ready, draft, and leftovers (M4 task 7, design section 8.5 rows 3,
// 6, 6a, 8, and 8.9)
// -----------------------------------------------------------------------

// shipSeedMergeAsked seeds ticketID with an open "merge" question and the
// matching "merge asked <sha>" marker directly through
// CommitHandlerResult, the shape design section 8.8's own MERGE question
// takes (M4 task 8, not yet built in this worktree): task 7's own tests
// need a loop that reopens on an already-asked head without driving
// MERGE-ANSWER's own code to get there. It returns the question's own
// message id, since a ticket that already passed through planning,
// building, reviewing, and judging (shipTicketReady) carries other,
// already-resolved questions of its own: a caller checks this one
// specifically, not a ticket-wide resolved count.
func shipSeedMergeAsked(t *testing.T, s *store.Store, ticketID int64, sha string) int64 {
	t.Helper()
	ctx := t.Context()
	owner := "seed-merge-asked"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(ctx, ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("shipSeedMergeAsked: claim: claimed=%v err=%v", claimed, err)
	}
	payload, marshalErr := json.Marshal(response.QuestionPayload{
		Kind: response.QuestionKindMerge, State: response.QuestionStateOpen,
		Recommended: "a",
		Options:     []response.Option{{Key: "a", Text: "Merge now"}, {Key: "b", Text: "Hold"}},
	})
	if marshalErr != nil {
		t.Fatalf("shipSeedMergeAsked: marshal question payload: %v", marshalErr)
	}
	state := questionStateOpen
	waiting := "merge"
	applied, err := s.CommitHandlerResult(ctx, store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires, Waiting: &waiting,
		Messages: []store.Message{
			{TicketID: ticketID, Type: msgTypeQuestion, Author: authorZing, State: &state, Body: "Merge pull request?", Payload: payload},
			{TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, Body: "merge asked " + sha},
		},
	})
	if err != nil || !applied {
		t.Fatalf("shipSeedMergeAsked: commit: applied=%v err=%v", applied, err)
	}

	open, err := s.QuestionsByState(ctx, ticketID, questionStateOpen)
	if err != nil || len(open) == 0 {
		t.Fatalf("shipSeedMergeAsked: QuestionsByState(open): rows=%d err=%v", len(open), err)
	}
	return open[len(open)-1].ID
}

// TestReadyWhenGreenAndNoThreads proves design section 8.5 row 8: a draft
// pull request with CI green and no threads at all is marked ready, and
// the informational "pr ready <sha>" marker names the head.
func TestReadyWhenGreenAndNoThreads(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch, NodeID: "PR_node_ready"}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want the ready flip: %+v", commit.Escalation.Payload)
	}
	if len(gh.markReadyCalls) != 1 || gh.markReadyCalls[0] != "PR_node_ready" {
		t.Fatalf("markReadyCalls = %+v, want exactly [%q]", gh.markReadyCalls, "PR_node_ready")
	}
	want := prReadyPrefix + local
	if len(commit.Messages) != 1 || commit.Messages[0].Body != want {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestNoReadyWithUnresolvedThread proves row 8's own "zero unresolved
// threads" guard: an actionable thread takes row 5 instead (starting a
// respond batch), and the ready flip never fires.
func TestNoReadyWithUnresolvedThread(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	gh.threads = []orchestrator.Thread{
		shipThread(shipRespondThreadID, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", when)),
	}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.markReadyCalls) != 0 {
		t.Errorf("markReadyCalls = %+v, want none", gh.markReadyCalls)
	}
	found := false
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, "respond batch 1 started sha "+local+" after run ") {
			found = true
		}
	}
	if !found {
		t.Errorf("commit.Messages = %+v, want a respond batch 1 started marker", commit.Messages)
	}
}

// TestNoReadyWithUnclassifiedThread proves row 8's same guard against the
// unclassified class (design section 9.1): a thread with zero comments and
// one with an odd (empty) raw id each block the ready flip, idle (row 6a)
// instead of marking ready.
func TestNoReadyWithUnclassifiedThread(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		thread orchestrator.Thread
	}{
		{"zero comments", orchestrator.Thread{ID: "RT_unclassified_zero_comments"}},
		{"odd id", orchestrator.Thread{ID: "", Comments: []orchestrator.ThreadComment{shipHumanComment("c1", "reviewer1", "???", time.Now())}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ticket, gh, tr := shipPublished(t)
			local := shipHeadSHA(t, s, ticket)
			runs, required := shipGreenCI()
			gh.runs, gh.required = runs, required
			gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
			gh.threads = []orchestrator.Thread{tc.thread}

			commit, err := shipPollRun(t, s, ticket, gh, tr)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(gh.markReadyCalls) != 0 {
				t.Errorf("markReadyCalls = %+v, want none", gh.markReadyCalls)
			}
			if commit.Poll == nil {
				t.Fatal("commit.Poll is nil, want the idle backoff commit (row 6a)")
			}
			found := false
			for _, m := range commit.Messages {
				if strings.HasPrefix(m.Body, threadsBlockingPrefix) {
					found = true
				}
			}
			if !found {
				t.Errorf("commit.Messages = %+v, want a %q marker", commit.Messages, threadsBlockingPrefix)
			}
		})
	}
}

// TestDraftWhenLoopReopens proves design section 8.5 row 3: a failed
// required check converts an already-ready pull request back to draft,
// whoever made it ready -- draft and ready state are read from GitHub
// every poll (design D27's own section 8.9), never from a Zing-only
// marker, so this fires exactly the same way whether Zing's own row 8
// marked it ready earlier or the owner did it by hand on GitHub.
func TestDraftWhenLoopReopens(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipFailedCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: false, HeadSHA: local, BaseRef: pbFixtureDefaultBranch, NodeID: "PR_node_reopen"}
	gh.logTail = func(context.Context, string, string, int64, int) (string, error) { return shipCILogTailText, nil }

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("got an escalation, want the draft flip: %+v", commit.Escalation.Payload)
	}
	if len(gh.convertToDraftCalls) != 1 || gh.convertToDraftCalls[0] != "PR_node_reopen" {
		t.Fatalf("convertToDraftCalls = %+v, want exactly [%q]", gh.convertToDraftCalls, "PR_node_reopen")
	}
	want := prDraftPrefix + local
	found := false
	for _, m := range commit.Messages {
		if m.Body == want {
			found = true
		}
		if strings.HasPrefix(m.Body, fixRequestedCILogPrefix) {
			t.Errorf("commit.Messages = %+v, want no ci_log fix request: row 3 outranks row 4", commit.Messages)
		}
	}
	if !found {
		t.Errorf("commit.Messages = %+v, want exactly %q", commit.Messages, want)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestReadyPRFlipsBackOnZeroCommentThread and TestReadyPRFlipsBackOnUnclassifiedThread
// both prove design section 8.9: starting from a ready pull request with an
// open merge question, an unclassified thread (zero comments, or an odd
// id) reopens the loop. One commit converts the pull request back to
// draft and withdraws the merge question -- ResolveAll, the "pr draft
// <sha>" marker, and the "merge withdrawn <sha>" marker all land together.
func testReadyPRFlipsBackOnUnclassified(t *testing.T, thread orchestrator.Thread) {
	t.Helper()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: false, HeadSHA: local, BaseRef: pbFixtureDefaultBranch, NodeID: "PR_node_flip_back"}
	questionID := shipSeedMergeAsked(t, s, ticket.ID, local)
	gh.threads = []orchestrator.Thread{thread}

	commit, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.convertToDraftCalls) != 1 || gh.convertToDraftCalls[0] != "PR_node_flip_back" {
		t.Fatalf("convertToDraftCalls = %+v, want exactly [%q]", gh.convertToDraftCalls, "PR_node_flip_back")
	}
	if !commit.ResolveAll {
		t.Error("ResolveAll = false, want true (the open merge question is withdrawn)")
	}
	var sawDraft, sawWithdrawn bool
	for _, m := range commit.Messages {
		switch m.Body {
		case prDraftPrefix + local:
			sawDraft = true
		case "merge withdrawn " + local:
			sawWithdrawn = true
		}
	}
	if !sawDraft {
		t.Errorf("commit.Messages = %+v, want %q", commit.Messages, prDraftPrefix+local)
	}
	if !sawWithdrawn {
		t.Errorf("commit.Messages = %+v, want %q", commit.Messages, "merge withdrawn "+local)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}

	pbApply(t, s, ticket, commit)
	question, err := s.GetMessage(t.Context(), questionID)
	if err != nil {
		t.Fatalf("GetMessage(%d): %v", questionID, err)
	}
	if question.State == nil || *question.State != shipQuestionResolved {
		t.Errorf("merge question %d state = %v, want %q", questionID, question.State, shipQuestionResolved)
	}
}

func TestReadyPRFlipsBackOnZeroCommentThread(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	testReadyPRFlipsBackOnUnclassified(t, orchestrator.Thread{ID: "RT_flipback_zero_comments"})
}

func TestReadyPRFlipsBackOnUnclassifiedThread(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	testReadyPRFlipsBackOnUnclassified(t, orchestrator.Thread{ID: "", Comments: []orchestrator.ThreadComment{shipHumanComment("c1", "reviewer1", "???", time.Now())}})
}

// TestReadyCrashConverges proves design section 8.9's own convergence
// rule: draft and ready state are read from GitHub every poll, never from
// a marker, so a crash after a real MarkReady call but before its own
// commit converges cleanly -- the next poll reads the pull request
// already ready and calls MarkReady no further times -- and a later red
// CI still flips it to draft.
func TestReadyCrashConverges(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch, NodeID: "PR_node_crash"}

	// A crash right after MarkReady succeeded on GitHub, before its own
	// commit: the write happens directly (TestPublishFindsExistingPRAfterCrash,
	// above, simulates PUBLISH's own pre-commit crash the same way), and the
	// claim is released with no commit applied.
	preCrashDeps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	if err := gh.MarkReady(t.Context(), gh.prState.NodeID); err != nil {
		t.Fatalf("MarkReady (pre-crash): %v", err)
	}
	shipReleaseClaim(t, s, ticket.ID, preCrashDeps)
	if len(gh.markReadyCalls) != 1 {
		t.Fatalf("markReadyCalls after the pre-crash write = %d, want 1", len(gh.markReadyCalls))
	}

	// The next poll reads GitHub fresh: GetPR already reports the pull
	// request ready (the real write succeeded), so row 8's own condition no
	// longer matches, and this tick calls MarkReady no further times. CI
	// green, zero threads, and a ready pull request is row 9's own
	// condition: with merge.auto off (deps2's own zero-value MergeRule),
	// row 9 asks the merge question instead (design section 8.8, M4 task
	// 8).
	gh.prState.Draft = false
	deps2 := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	commit2, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2)
	if err != nil {
		t.Fatalf("Run (converged poll): %v", err)
	}
	if commit2.Waiting == nil || *commit2.Waiting != waitingMerge {
		t.Errorf("Waiting = %v, want %q (the merge question)", commit2.Waiting, waitingMerge)
	}
	if want := "merge asked " + local; !shipHasMessage(commit2, want) {
		t.Errorf("commit.Messages = %+v, want %q", commit2.Messages, want)
	}
	if len(gh.markReadyCalls) != 1 {
		t.Errorf("markReadyCalls after the converged poll = %d, want still 1 (no duplicate call)", len(gh.markReadyCalls))
	}
	shipReleaseClaim(t, s, ticket.ID, deps2)

	// A later red CI flips the (still not-draft) pull request back to draft.
	gh.runs, gh.required = shipFailedCI()
	gh.logTail = func(context.Context, string, string, int64, int) (string, error) { return shipCILogTailText, nil }
	commit3, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (CI failed poll): %v", err)
	}
	if len(gh.convertToDraftCalls) != 1 {
		t.Errorf("convertToDraftCalls = %+v, want exactly one call", gh.convertToDraftCalls)
	}
	want := prDraftPrefix + local
	found := false
	for _, m := range commit3.Messages {
		if m.Body == want {
			found = true
		}
	}
	if !found {
		t.Errorf("commit3.Messages = %+v, want %q", commit3.Messages, want)
	}
}

// TestLeftoverResolvedEachPoll proves LEFTOVER (design section 8.5 row 6,
// 9.5): a thread whose last comment is Zing's own disclosed reply resolves
// every poll, with no marker of its own.
func TestLeftoverResolvedEachPoll(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	leftoverID := "RT_leftover_resolved"
	gh.threads = []orchestrator.Thread{
		shipThread(leftoverID, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", when), zingReplyComment(shipViewerLogin)),
	}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != leftoverID {
		t.Fatalf("resolves = %+v, want exactly [%q]", gh.resolves, leftoverID)
	}
	if len(commit.Messages) != 0 {
		t.Errorf("commit.Messages = %+v, want none (9.5: no marker records a leftover resolve)", commit.Messages)
	}
	if !commit.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestLeftoverCrashConverges proves design section 11's own leftover-resolve
// row: a crash after the resolve call but before the commit leaves no
// marker behind, so the next poll -- reading the same, still-unresolved
// thread back off the fake, which never flips IsResolved on its own --
// resolves it again, exactly 9.5's own "sees it resolved again"
// convergence.
func TestLeftoverCrashConverges(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	leftoverID := "RT_leftover_crash"
	gh.threads = []orchestrator.Thread{
		shipThread(leftoverID, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", when), zingReplyComment(shipViewerLogin)),
	}

	preCrashDeps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	_, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), preCrashDeps)
	if err != nil {
		t.Fatalf("Run (pre-crash): %v", err)
	}
	if len(gh.resolves) != 1 || gh.resolves[0] != leftoverID {
		t.Fatalf("resolves (pre-crash) = %+v, want exactly [%q]", gh.resolves, leftoverID)
	}
	shipReleaseClaim(t, s, ticket.ID, preCrashDeps)

	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (converged poll): %v", err)
	}
	if len(gh.resolves) != 2 || gh.resolves[1] != leftoverID {
		t.Errorf("resolves (converged poll) = %+v, want a second %q", gh.resolves, leftoverID)
	}
	if !commit2.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
}

// TestThreadsBlockingMarkerOnChange proves design section 8.5 row 6a's own
// change-only marker: "threads blocking <tids>" is written only when the
// unclassified set differs from the previous poll's own newest such
// marker, the same shape ciWaitingPrefix already gives row 7 (
// TestPollCIWaitingMarkerOnChange, above).
func TestThreadsBlockingMarkerOnChange(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	gh.threads = []orchestrator.Thread{{ID: "RT_unclass_a"}}

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (first poll): %v", err)
	}
	wantFirst := threadsBlockingPrefix + tid("RT_unclass_a")
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

	// A second poll with the same unclassified set writes no new marker.
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (second poll): %v", err)
	}
	for _, m := range commit2.Messages {
		if strings.HasPrefix(m.Body, threadsBlockingPrefix) {
			t.Errorf("commit2.Messages = %+v, want no new %q marker (set unchanged)", commit2.Messages, threadsBlockingPrefix)
		}
	}
	pbApply(t, s, ticket, commit2)

	// A third poll where the set changes (a second unclassified thread
	// joins) writes a new marker naming both tids, sorted.
	gh.threads = []orchestrator.Thread{{ID: "RT_unclass_a"}, {ID: "RT_unclass_b"}}
	commit3, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (third poll): %v", err)
	}
	wantTIDs := []string{tid("RT_unclass_a"), tid("RT_unclass_b")}
	sort.Strings(wantTIDs)
	wantThird := threadsBlockingPrefix + strings.Join(wantTIDs, ",")
	foundThird := false
	for _, m := range commit3.Messages {
		if m.Body == wantThird {
			foundThird = true
		}
	}
	if !foundThird {
		t.Errorf("commit3.Messages = %+v, want %q", commit3.Messages, wantThird)
	}
}

// -----------------------------------------------------------------------
// Merge (M4 task 8, design section 8.8 and 8.9)
// -----------------------------------------------------------------------

// shipQuestionMessage returns commit's own "question" message, failing the
// test if it carries none.
func shipQuestionMessage(t *testing.T, commit store.HandlerCommit) store.Message {
	t.Helper()
	for i := range commit.Messages {
		if commit.Messages[i].Type == msgTypeQuestion {
			return commit.Messages[i]
		}
	}
	t.Fatalf("commit.Messages = %+v, want a question message", commit.Messages)
	return store.Message{}
}

// TestMergeQuestionPosted proves design section 8.8's own "The merge
// question": with merge.auto off (the zero-value MergeRule shipPollRun's
// own Deps carries), row 9 asks instead of merging -- the question's own
// options and recommendation, Waiting "merge", the 30s backoff Poll
// commit, and the "merge asked <sha>" marker.
func TestMergeQuestionPosted(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_ask")

	commit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Waiting == nil || *commit.Waiting != waitingMerge {
		t.Fatalf("Waiting = %v, want %q", commit.Waiting, waitingMerge)
	}
	if commit.Poll == nil {
		t.Fatal("Poll is nil, want the merge question's own backoff commit")
	}
	if !shipHasMessage(commit, "merge asked "+local) {
		t.Errorf("commit.Messages = %+v, want a %q marker", commit.Messages, "merge asked "+local)
	}

	q := shipQuestionMessage(t, commit)
	var payload response.QuestionPayload
	if err := json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	if payload.Kind != response.QuestionKindMerge {
		t.Errorf("payload.Kind = %q, want %q", payload.Kind, response.QuestionKindMerge)
	}
	if payload.Recommended != "a" {
		t.Errorf("payload.Recommended = %q, want %q", payload.Recommended, "a")
	}
	wantOptions := []response.Option{{Key: "a", Text: "Merge now"}, {Key: "b", Text: "Hold"}}
	if len(payload.Options) != len(wantOptions) || payload.Options[0] != wantOptions[0] || payload.Options[1] != wantOptions[1] {
		t.Errorf("payload.Options = %+v, want %+v", payload.Options, wantOptions)
	}
	if !strings.Contains(q.Body, "merge.auto is off") {
		t.Errorf("question body = %q, want it to name the reason", q.Body)
	}
}

// TestMergeNowMerges proves MERGE-ANSWER's own option a (design section
// 8.8): the owner's Merge now re-reads CI and threads pinned to the asked
// sha, finds everything still clean, and calls PullRequests.Merge with
// that sha -- "pr merged <sha>", ClearPoll, and the round resolved through
// WithdrawQuestions.
func TestMergeNowMerges(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_merge_now")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	pbApply(t, s, ticket, commit1)

	shipAnswerMergeQuestion(t, s, ticket.ID, "a")
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (merge now): %v", err)
	}
	if !shipHasMessage(commit2, "pr merged "+local) {
		t.Errorf("commit2.Messages = %+v, want %q", commit2.Messages, "pr merged "+local)
	}
	if !commit2.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
	if len(gh.mergeCalls) != 1 || !strings.HasPrefix(gh.mergeCalls[0], "1|"+local+"|") {
		t.Errorf("mergeCalls = %+v, want exactly one call pinned to %q", gh.mergeCalls, local)
	}
	if len(commit2.WithdrawQuestions) != 1 {
		t.Fatalf("WithdrawQuestions = %+v, want exactly one id", commit2.WithdrawQuestions)
	}
	pbApply(t, s, ticket, commit2)

	question, err := s.GetMessage(t.Context(), commit2.WithdrawQuestions[0])
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if question.State == nil || *question.State != shipQuestionResolved {
		t.Errorf("merge question state = %v, want resolved", question.State)
	}
}

// TestMergeNowRefusedWhenHeadMoved proves MERGE's own same-tick re-read
// (N4): between the ask and the owner's Merge now answer, the pull
// request's head moved -- MERGE refuses with "the head moved", pinned to
// the sha the question was actually asked about, not the new head; "merge
// withdrawn <sha>" lands in the same commit, and the round still resolves.
func TestMergeNowRefusedWhenHeadMoved(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_head_moved")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	pbApply(t, s, ticket, commit1)
	shipAnswerMergeQuestion(t, s, ticket.ID, "a")

	gh.prState.HeadSHA = strings.Repeat("f", 40)
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (merge now): %v", err)
	}
	if len(gh.mergeCalls) != 0 {
		t.Errorf("mergeCalls = %+v, want none (the head moved before Merge was called)", gh.mergeCalls)
	}
	if !shipHasMessage(commit2, "merge refused "+local+"\n"+mergeReasonHeadMoved) {
		t.Errorf("commit2.Messages = %+v, want the %q refusal", commit2.Messages, mergeReasonHeadMoved)
	}
	if !shipHasMessage(commit2, "merge withdrawn "+local) {
		t.Errorf("commit2.Messages = %+v, want %q", commit2.Messages, "merge withdrawn "+local)
	}
	if !commit2.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
	if len(commit2.WithdrawQuestions) != 1 {
		t.Errorf("WithdrawQuestions = %+v, want exactly one id (the Merge now round)", commit2.WithdrawQuestions)
	}
}

// TestMergeNowRefusedWhenThreadOpen proves MERGE's own re-read of threads:
// an actionable thread reappeared between the ask and the answer, so MERGE
// refuses with "a review thread is open".
func TestMergeNowRefusedWhenThreadOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_thread_open")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	pbApply(t, s, ticket, commit1)
	shipAnswerMergeQuestion(t, s, ticket.ID, "a")

	gh.threads = []orchestrator.Thread{
		shipThread(shipRespondThreadID, "greet.go", 3, shipHumanComment("c1", "reviewer1", "one more thing", time.Now())),
	}
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (merge now): %v", err)
	}
	if len(gh.mergeCalls) != 0 {
		t.Errorf("mergeCalls = %+v, want none (a thread reopened before Merge was called)", gh.mergeCalls)
	}
	if !shipHasMessage(commit2, "merge refused "+local+"\n"+mergeReasonThreadOpen) {
		t.Errorf("commit2.Messages = %+v, want the %q refusal", commit2.Messages, mergeReasonThreadOpen)
	}
}

// testMergeGateBlockedByUnclassified is TestMergeGateBlockedByZeroCommentThread's
// and TestMergeGateBlockedByOddThreadID's shared body: an unclassified
// thread (design section 9.1) reappears between the ask and the Merge now
// answer, and MERGE's own anyUnresolved check blocks it exactly as an
// actionable or leftover thread would.
func testMergeGateBlockedByUnclassified(t *testing.T, thread orchestrator.Thread) {
	t.Helper()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_gate_unclass")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	pbApply(t, s, ticket, commit1)
	shipAnswerMergeQuestion(t, s, ticket.ID, "a")

	gh.threads = []orchestrator.Thread{thread}
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (merge now): %v", err)
	}
	if len(gh.mergeCalls) != 0 {
		t.Errorf("mergeCalls = %+v, want none", gh.mergeCalls)
	}
	if !shipHasMessage(commit2, "merge refused "+local+"\n"+mergeReasonThreadOpen) {
		t.Errorf("commit2.Messages = %+v, want the %q refusal", commit2.Messages, mergeReasonThreadOpen)
	}
}

func TestMergeGateBlockedByZeroCommentThread(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	testMergeGateBlockedByUnclassified(t, orchestrator.Thread{ID: "RT_gate_zero_comments"})
}

func TestMergeGateBlockedByOddThreadID(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	testMergeGateBlockedByUnclassified(t, orchestrator.Thread{ID: "", Comments: []orchestrator.ThreadComment{shipHumanComment("c1", "reviewer1", "???", time.Now())}})
}

// TestMergeGateBlockedBySpoofedCheck proves MERGE's own re-read of CI uses
// EvaluateCI's app-id matching (design section 8.4): a same-name check
// run from an app other than the one the required check is bound to
// reappears between the ask and the answer (a spoof), so the required
// check reads as missing and MERGE refuses with "CI is not green".
func TestMergeGateBlockedBySpoofedCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	const boundAppID = int64(15368)
	gh.required = []orchestrator.RequiredCheck{{Context: "ci", AppID: new(boundAppID)}}
	gh.runs = []orchestrator.CheckRun{{ID: 1, Name: "ci", Status: ghCompleted, Conclusion: ghSuccess, AppID: boundAppID, AppSlug: ghGitHubActions}}
	gh.prState = shipMergeReadyPR(local, "PR_node_spoofed")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	pbApply(t, s, ticket, commit1)
	shipAnswerMergeQuestion(t, s, ticket.ID, "a")

	gh.runs = []orchestrator.CheckRun{{ID: 2, Name: "ci", Status: ghCompleted, Conclusion: ghSuccess, AppID: 99, AppSlug: ghGitHubActions}}
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (merge now): %v", err)
	}
	if len(gh.mergeCalls) != 0 {
		t.Errorf("mergeCalls = %+v, want none (the required check's own app no longer matches)", gh.mergeCalls)
	}
	if !shipHasMessage(commit2, "merge refused "+local+"\n"+mergeReasonCINotGreen) {
		t.Errorf("commit2.Messages = %+v, want the %q refusal", commit2.Messages, mergeReasonCINotGreen)
	}
}

// TestMergeCrashConverges proves design section 11's own convergence rule
// for MERGE: a crash after the real GitHub Merge call but before its own
// commit leaves tickets.pr_url's state unchanged, but the next poll's own
// GetPR already reports the pull request merged, so it runs DONE -- Merge
// is not called a second time.
func TestMergeCrashConverges(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_crash_merge")

	deps := shipClaim(t, s, pbFakeRuntime(t), ticket.ID, gh, tr)
	deps.MergeRule = MergeRule{Auto: true, Method: shipMergeMethodSquash}
	commit, err := (shipHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (auto merge): %v", err)
	}
	if !shipHasMessage(commit, "pr merged "+local) {
		t.Fatalf("commit.Messages = %+v, want %q", commit.Messages, "pr merged "+local)
	}
	if len(gh.mergeCalls) != 1 {
		t.Fatalf("mergeCalls = %+v, want exactly 1", gh.mergeCalls)
	}
	// Crash: the real GitHub Merge call above already landed; this commit
	// is never applied.
	shipReleaseClaim(t, s, ticket.ID, deps)

	gh.prState.Merged = true
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (converged poll): %v", err)
	}
	if commit2.Next != stateDone {
		t.Errorf("Next = %q, want %q", commit2.Next, stateDone)
	}
	if len(gh.mergeCalls) != 1 {
		t.Errorf("mergeCalls after the converged poll = %d, want still 1 (no duplicate call)", len(gh.mergeCalls))
	}
}

// TestMergeHold proves MERGE-ANSWER's own option b (design section 8.8):
// no GitHub call, the informational "merge held <sha>" marker, the round
// resolved, ClearPoll.
func TestMergeHold(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_hold")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	pbApply(t, s, ticket, commit1)
	shipAnswerMergeQuestion(t, s, ticket.ID, "b")

	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (hold): %v", err)
	}
	if !shipHasMessage(commit2, "merge "+mergeMarkerHeld+" "+local) {
		t.Errorf("commit2.Messages = %+v, want %q", commit2.Messages, "merge held "+local)
	}
	if !commit2.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
	if len(commit2.WithdrawQuestions) != 1 {
		t.Errorf("WithdrawQuestions = %+v, want exactly one id", commit2.WithdrawQuestions)
	}
	if len(gh.mergeCalls) != 0 {
		t.Errorf("mergeCalls = %+v, want none (Hold never merges)", gh.mergeCalls)
	}
}

// TestHeldShaNotAskedAgain proves design section 8.8's own "A held sha is
// not asked about again": after Hold, a clean poll on the same head is
// row 10's own idle wait, not a fresh ask.
func TestHeldShaNotAskedAgain(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_held_not_again")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	pbApply(t, s, ticket, commit1)
	shipAnswerMergeQuestion(t, s, ticket.ID, "b")
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (hold): %v", err)
	}
	pbApply(t, s, ticket, commit2)

	commit3, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (poll after hold): %v", err)
	}
	if shipHasMessage(commit3, "merge asked "+local) {
		t.Errorf("commit3.Messages = %+v, want no fresh ask (held sha)", commit3.Messages)
	}
	if commit3.Poll == nil {
		t.Fatal("Poll is nil, want row 10's own idle backoff commit")
	}
	if commit3.Waiting != nil {
		t.Errorf("Waiting = %v, want nil (held already cleared it at answer time)", commit3.Waiting)
	}
}

// TestMergeAskedAgainAfterReopenSameHead proves design section 8.8's own
// "the merge question can be asked again after a withdraw on the same
// head": asked, a failed check reopens the loop (converting the pull
// request back to draft and withdrawing the question in the same commit,
// design section 8.5 row 3), then a clean poll re-marks it ready (row 8)
// and asks again (row 9) once the head is askable again.
func TestMergeAskedAgainAfterReopenSameHead(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	greenRuns, required := shipGreenCI()
	gh.runs, gh.required = greenRuns, required
	gh.prState = shipMergeReadyPR(local, "PR_node_reopen_same_head")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	if !shipHasMessage(commit1, "merge asked "+local) {
		t.Fatalf("commit1.Messages = %+v, want %q", commit1.Messages, "merge asked "+local)
	}
	pbApply(t, s, ticket, commit1)

	failedRuns, _ := shipFailedCI()
	gh.runs = failedRuns
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (CI fails): %v", err)
	}
	if !shipHasMessage(commit2, "merge withdrawn "+local) {
		t.Fatalf("commit2.Messages = %+v, want %q", commit2.Messages, "merge withdrawn "+local)
	}
	if len(gh.convertToDraftCalls) != 1 {
		t.Fatalf("convertToDraftCalls = %+v, want exactly one call", gh.convertToDraftCalls)
	}
	pbApply(t, s, ticket, commit2)
	gh.prState.Draft = true

	gh.runs = greenRuns
	commit3, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (CI green again): %v", err)
	}
	if len(gh.markReadyCalls) != 1 {
		t.Fatalf("markReadyCalls = %+v, want exactly one call", gh.markReadyCalls)
	}
	pbApply(t, s, ticket, commit3)
	gh.prState.Draft = false

	commit4, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (clean again): %v", err)
	}
	if !shipHasMessage(commit4, "merge asked "+local) {
		t.Errorf("commit4.Messages = %+v, want a fresh %q marker", commit4.Messages, "merge asked "+local)
	}
}

// TestLoopReopenWithdrawsMergeQuestion proves the same reopen, checked at
// the store layer: once commit2's own withdrawal commit (above) is
// applied, the open merge question itself ends "resolved", not left
// dangling.
func TestLoopReopenWithdrawsMergeQuestion(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	greenRuns, required := shipGreenCI()
	gh.runs, gh.required = greenRuns, required
	gh.prState = shipMergeReadyPR(local, "PR_node_loop_reopen")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	pbApply(t, s, ticket, commit1)
	open, err := s.QuestionsByState(t.Context(), ticket.ID, questionStateOpen)
	if err != nil || len(open) == 0 {
		t.Fatalf("QuestionsByState(open): rows=%d err=%v", len(open), err)
	}
	questionID := open[len(open)-1].ID

	failedRuns, _ := shipFailedCI()
	gh.runs = failedRuns
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (CI fails): %v", err)
	}
	if !shipHasMessage(commit2, "merge withdrawn "+local) {
		t.Fatalf("commit2.Messages = %+v, want %q", commit2.Messages, "merge withdrawn "+local)
	}
	pbApply(t, s, ticket, commit2)

	question, err := s.GetMessage(t.Context(), questionID)
	if err != nil {
		t.Fatalf("GetMessage(%d): %v", questionID, err)
	}
	if question.State == nil || *question.State != shipQuestionResolved {
		t.Errorf("merge question state = %v, want resolved", question.State)
	}
}

// TestRefusedAutoMergeAsks proves design section 8.8's own ErrMergeRefused
// row on the automatic path: GitHub refuses the merge, so row 9 falls
// back to the merge question with "GitHub refused the merge: <GitHub's
// own message>", waiting "merge"; polling continues, and the next tick
// does not call Merge again (the head is not askable until answered).
func TestRefusedAutoMergeAsks(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_auto_refused")
	gh.mergeErr = fmt.Errorf("%w: a required review is missing", orchestrator.ErrMergeRefused)

	rule := MergeRule{Auto: true, Method: shipMergeMethodSquash}
	commit, err := shipPollRunWithRule(t, s, ticket, gh, tr, rule)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.mergeCalls) != 1 {
		t.Fatalf("mergeCalls = %+v, want exactly 1 (the refused attempt)", gh.mergeCalls)
	}
	if !shipHasMessage(commit, "merge refused "+local) {
		t.Errorf("commit.Messages = %+v, want %q", commit.Messages, "merge refused "+local)
	}
	if commit.Waiting == nil || *commit.Waiting != waitingMerge {
		t.Errorf("Waiting = %v, want %q", commit.Waiting, waitingMerge)
	}
	q := shipQuestionMessage(t, commit)
	if !strings.Contains(q.Body, "GitHub refused the merge: a required review is missing") {
		t.Errorf("question body = %q, want the GitHub refusal reason", q.Body)
	}
	if commit.Poll == nil {
		t.Fatal("Poll is nil, want the merge question's own backoff commit")
	}
	pbApply(t, s, ticket, commit)

	commit2, err := shipPollRunWithRule(t, s, pbGetTicket(t, s, ticket.ID), gh, tr, rule)
	if err != nil {
		t.Fatalf("Run (second poll): %v", err)
	}
	if len(gh.mergeCalls) != 1 {
		t.Errorf("mergeCalls after the second poll = %d, want still 1 (not askable yet)", len(gh.mergeCalls))
	}
	if commit2.Waiting == nil || *commit2.Waiting != waitingMerge {
		t.Errorf("Waiting (second poll) = %v, want %q", commit2.Waiting, waitingMerge)
	}
}

// TestRefusedMergeNowAsksAgain is TestRefusedAutoMergeAsks' own Merge now
// twin: the owner answered a, GitHub refused anyway, so MERGE re-asks the
// same way, and the next unanswered poll does not call Merge a second
// time.
func TestRefusedMergeNowAsksAgain(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_now_refused")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	pbApply(t, s, ticket, commit1)
	shipAnswerMergeQuestion(t, s, ticket.ID, "a")

	gh.mergeErr = fmt.Errorf("%w: a required review is missing", orchestrator.ErrMergeRefused)
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (merge now, refused): %v", err)
	}
	if len(gh.mergeCalls) != 1 {
		t.Fatalf("mergeCalls = %+v, want exactly 1", gh.mergeCalls)
	}
	if !shipHasMessage(commit2, "merge refused "+local) {
		t.Errorf("commit2.Messages = %+v, want %q", commit2.Messages, "merge refused "+local)
	}
	if len(commit2.WithdrawQuestions) != 1 {
		t.Errorf("WithdrawQuestions = %+v, want exactly one id (the Merge now round)", commit2.WithdrawQuestions)
	}
	q := shipQuestionMessage(t, commit2)
	if !strings.Contains(q.Body, "GitHub refused the merge: a required review is missing") {
		t.Errorf("question body = %q, want the GitHub refusal reason", q.Body)
	}
	pbApply(t, s, ticket, commit2)

	if _, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr); err != nil {
		t.Fatalf("Run (third poll, unanswered): %v", err)
	}
	if len(gh.mergeCalls) != 1 {
		t.Errorf("mergeCalls after the third poll = %d, want still 1", len(gh.mergeCalls))
	}
}

// testMergeNowWithdraws is TestMergeNowCIPendingWithdraws' and
// TestMergeNowDraftWithdraws' shared body (design section 8.8's own "once
// clean, the question is asked again"): the owner chose Merge now, but by
// the time the commit runs the precondition arrange breaks; MERGE never
// calls GitHub's own Merge, refuses with reason, withdraws, and resolves
// the round; once arrange is undone, the next clean poll asks again.
func testMergeNowWithdraws(t *testing.T, reason string, arrange, undo func(gh *shipGitHub)) {
	t.Helper()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_now_withdraws")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	pbApply(t, s, ticket, commit1)
	shipAnswerMergeQuestion(t, s, ticket.ID, "a")

	arrange(gh)
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (merge now, blocked): %v", err)
	}
	if len(gh.mergeCalls) != 0 {
		t.Errorf("mergeCalls = %+v, want none", gh.mergeCalls)
	}
	if !shipHasMessage(commit2, "merge refused "+local+"\n"+reason) {
		t.Errorf("commit2.Messages = %+v, want the %q refusal", commit2.Messages, reason)
	}
	if !shipHasMessage(commit2, "merge withdrawn "+local) {
		t.Errorf("commit2.Messages = %+v, want %q", commit2.Messages, "merge withdrawn "+local)
	}
	if !commit2.ClearPoll {
		t.Error("ClearPoll = false, want true")
	}
	if len(commit2.WithdrawQuestions) != 1 {
		t.Errorf("WithdrawQuestions = %+v, want exactly one id", commit2.WithdrawQuestions)
	}
	pbApply(t, s, ticket, commit2)

	undo(gh)
	commit3, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (clean again): %v", err)
	}
	if !shipHasMessage(commit3, "merge asked "+local) {
		t.Errorf("commit3.Messages = %+v, want a fresh %q marker", commit3.Messages, "merge asked "+local)
	}
}

func TestMergeNowCIPendingWithdraws(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	pendingRuns := []orchestrator.CheckRun{{ID: 1, Name: "ci", Status: "in_progress", AppSlug: ghGitHubActions}}
	var greenRuns []orchestrator.CheckRun
	testMergeNowWithdraws(t, mergeReasonCINotGreen,
		func(gh *shipGitHub) { greenRuns = gh.runs; gh.runs = pendingRuns },
		func(gh *shipGitHub) { gh.runs = greenRuns })
}

func TestMergeNowDraftWithdraws(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	testMergeNowWithdraws(t, mergeReasonDraft,
		func(gh *shipGitHub) { gh.prState.Draft = true },
		func(gh *shipGitHub) { gh.prState.Draft = false })
}

// TestHeldAskedAgainAfterReopen is TestMergeAskedAgainAfterReopenSameHead's
// own Held twin: a held sha is withdrawn exactly like an asked one
// (withdrawMergeQuestionIfAsked's own "asked" or "held" switch) once the
// loop reopens, and asked again once it closes clean.
func TestHeldAskedAgainAfterReopen(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	greenRuns, required := shipGreenCI()
	gh.runs, gh.required = greenRuns, required
	gh.prState = shipMergeReadyPR(local, "PR_node_held_reopen")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	pbApply(t, s, ticket, commit1)
	shipAnswerMergeQuestion(t, s, ticket.ID, "b")
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (hold): %v", err)
	}
	if !shipHasMessage(commit2, "merge held "+local) {
		t.Fatalf("commit2.Messages = %+v, want %q", commit2.Messages, "merge held "+local)
	}
	pbApply(t, s, ticket, commit2)

	gh.threads = []orchestrator.Thread{{ID: "RT_held_reopen"}}
	commit3, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (reopen): %v", err)
	}
	if !shipHasMessage(commit3, "merge withdrawn "+local) {
		t.Fatalf("commit3.Messages = %+v, want %q", commit3.Messages, "merge withdrawn "+local)
	}
	if len(gh.convertToDraftCalls) != 1 {
		t.Fatalf("convertToDraftCalls = %+v, want exactly one call", gh.convertToDraftCalls)
	}
	pbApply(t, s, ticket, commit3)
	gh.prState.Draft = true

	gh.threads = nil
	commit4, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (clean, ready again): %v", err)
	}
	if len(gh.markReadyCalls) != 1 {
		t.Fatalf("markReadyCalls = %+v, want exactly one call", gh.markReadyCalls)
	}
	pbApply(t, s, ticket, commit4)
	gh.prState.Draft = false

	commit5, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (clean, asked again): %v", err)
	}
	if !shipHasMessage(commit5, "merge asked "+local) {
		t.Errorf("commit5.Messages = %+v, want a fresh %q marker", commit5.Messages, "merge asked "+local)
	}
}

// TestAutoMergeWhenAllowed proves design section 8.8's own happy path:
// merge.auto on, no manual-deploy or dependency path in the diff, so row 9
// merges directly with no question asked at all.
func TestAutoMergeWhenAllowed(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_auto_allowed")

	commit, err := shipPollRunWithRule(t, s, ticket, gh, tr, MergeRule{Auto: true, Method: shipMergeMethodSquash})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !shipHasMessage(commit, "pr merged "+local) {
		t.Errorf("commit.Messages = %+v, want %q", commit.Messages, "pr merged "+local)
	}
	if len(gh.mergeCalls) != 1 {
		t.Errorf("mergeCalls = %+v, want exactly 1", gh.mergeCalls)
	}
	if commit.Waiting != nil {
		t.Errorf("Waiting = %v, want nil (no question asked)", commit.Waiting)
	}
}

// TestAutoMergeBlockedByDependencyFile proves mergeDecision's own
// DependencyFiles rule end to end: merge.auto is on, but the ticket's own
// real build landed hello.txt (shipTicketReady's own fixture), named here
// as a dependency file, so row 9 asks instead of merging, naming the path
// in its own reason.
func TestAutoMergeBlockedByDependencyFile(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_auto_blocked")

	rule := MergeRule{Auto: true, Method: shipMergeMethodSquash, DependencyFiles: []string{"hello.txt"}}
	commit, err := shipPollRunWithRule(t, s, ticket, gh, tr, rule)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.mergeCalls) != 0 {
		t.Errorf("mergeCalls = %+v, want none (the dependency file blocks it)", gh.mergeCalls)
	}
	q := shipQuestionMessage(t, commit)
	if !strings.Contains(q.Body, "the diff changes a dependency file: hello.txt") {
		t.Errorf("question body = %q, want the dependency-file reason naming hello.txt", q.Body)
	}
}

// TestMergeWaitPollsAndSeesMerge proves D21: a ticket waiting on "merge"
// stays a dispatch candidate, so a merge GitHub itself reports -- the
// owner merged it there directly, with the question still open and
// unanswered -- is seen on the next poll and moves the ticket to done.
func TestMergeWaitPollsAndSeesMerge(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, gh, tr := shipPublished(t)
	local := shipHeadSHA(t, s, ticket)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	gh.prState = shipMergeReadyPR(local, "PR_node_wait_merge")

	commit1, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("Run (ask): %v", err)
	}
	pbApply(t, s, ticket, commit1)

	gh.prState.Merged = true
	commit2, err := shipPollRun(t, s, pbGetTicket(t, s, ticket.ID), gh, tr)
	if err != nil {
		t.Fatalf("Run (sees the merge): %v", err)
	}
	if commit2.Next != stateDone {
		t.Errorf("Next = %q, want %q", commit2.Next, stateDone)
	}
	if !commit2.ResolveAll {
		t.Error("ResolveAll = false, want true (the still-open merge question resolves too)")
	}
}
