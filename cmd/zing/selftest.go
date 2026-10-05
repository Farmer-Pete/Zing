package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	zing "zing"
	"zing/fixtures"
	"zing/internal/bus"
	"zing/internal/console"
	zdispatch "zing/internal/dispatch"
	"zing/internal/gitfixture"
	"zing/internal/job"
	"zing/internal/lens"
	"zing/internal/machine"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/schemagen"
	"zing/internal/store"
	"zing/internal/tracker"
)

// selftestGitHub is a never-called orchestrator.GitHub, enough to satisfy
// orchestrator.New's required parameter (PKG8-PLAN.md section 10): the
// building state machine's own tests never push or open a pull request, so
// every method here is unreachable in this suite.
type selftestGitHub struct{}

func (selftestGitHub) RepoDefaultBranch(context.Context, string, string) (string, error) {
	return "", errors.New("selftestGitHub: not implemented")
}

func (selftestGitHub) RequiredChecks(context.Context, string, string, string) ([]string, error) {
	return nil, errors.New("selftestGitHub: not implemented")
}

func (selftestGitHub) CreateDraftPR(context.Context, string, string, string, string, string, string) (prURL string, number int, err error) {
	return "", 0, errors.New("selftestGitHub: not implemented")
}

func (selftestGitHub) FindPRByHead(context.Context, string, string, string, string) (prURL string, number int, ok bool, err error) {
	return "", 0, false, errors.New("selftestGitHub: not implemented")
}

// selftestShipGitHub is selftestResumeE2E's own working GitHub double (M3
// tasks 6, 7, 8; M4 task 8): unlike selftestGitHub above, PUBLISH and POLL
// both reach it for real, so CreateDraftPR and FindPRByHead actually track
// one pull request. GetPR and ListCheckRuns are scripted to carry that
// pull request through a CI failure, a landed ci_log fix, and the push
// that follows; redSHA is the commit PUBLISH first pushed, read straight
// off the real bare origin remoteDir rather than a canned field this
// double would otherwise have to be told about, so a real git push -- not
// a call back into this double -- is what actually moves what GetPR and
// ListCheckRuns next report: every check run on redSHA always fails, every
// other sha always succeeds. draft and merged are this double's own real
// state, moved only by MarkReady, ConvertToDraft, and Merge, so the e2e
// walks the real design section 8.5/8.8 path end to end -- CI green, then
// row 8's ready flip, then row 9's merge question (merge.auto is off, the
// suite's own dispatcher carries the zero-value MergeRule), then the
// owner's "Merge now" answer, then MERGE's own GetPR/ListCheckRuns re-read
// and PullRequests.Merge call -- rather than GetPR synthesizing Merged on
// its own after a fixed number of reads, the way an earlier M3-only build
// of this double did before MERGE existed.
type selftestShipGitHub struct {
	mu        sync.Mutex
	remoteDir string
	pr        *selftestShipPR
	nextNum   int
	redSHA    string
	draft     bool
	merged    bool
	threads   []orchestrator.Thread
	replies   map[string]string // raw thread id -> the body ReplyToThread posted
}

type selftestShipPR struct {
	url, head, base string
	number          int
}

// selftestShipGHViewerLogin is the token owner Viewer reports for this
// double, reused everywhere a thread comment needs an author (M4 task 10):
// isZingReply (threadrules.go) and this suite's own verify step both
// compare a comment's author against exactly this login.
const selftestShipGHViewerLogin = "zing-selftest-bot"

// selftestShipReplyThreadID and selftestShipFixThreadID are the two review
// threads newSelftestShipGitHub seeds (M4 task 10, design section 19.5 task
// 10): one a plain "reply" fixtures/scripts/respond/1/1.xml answers, the
// other a "fix" it collects into a consolidated fix request (design section
// 9.3 step 3), so this suite's one fake-runtime e2e walks RESPOND, APPLY,
// the shared fix driver, and FIX-REPLIES (design section 9.4) in a single
// pass, not only the one-reply shape M3's own worked example (13.3) shows.
// Their own tids (threadrules.go's tid, sha256 of the raw id) are what
// fixtures/scripts/respond/1/1.xml answers; computed once and asserted by
// TestSelftestShipThreadTIDsMatchFixture so a renamed raw id here is caught
// at test time, not by a cryptic "no script for respond/1/1.xml" failure.
const (
	selftestShipReplyThreadID = "RT_thread_1"
	selftestShipFixThreadID   = "RT_thread_2"
)

// newSelftestShipGitHub returns a selftestShipGitHub reading branch heads
// off remoteDir, the gitfixture bare origin selftestResumeE2E adds to the
// fixture project (gitfixture.WithBareOrigin), seeded with the two review
// threads above: both unresolved, each with one comment from a reviewer
// login other than selftestShipGHViewerLogin, so classifyThreads
// (threadrules.go) counts both as actionable the first time POLL reads
// them (design section 9.1).
func newSelftestShipGitHub(remoteDir string) *selftestShipGitHub {
	commentAt := time.Now().UTC()
	return &selftestShipGitHub{
		remoteDir: remoteDir,
		replies:   make(map[string]string),
		threads: []orchestrator.Thread{
			{
				ID: selftestShipReplyThreadID, Path: "cmd/zing/main.go", Line: 1,
				Comments: []orchestrator.ThreadComment{{
					ID: "c1", Author: "reviewer-bot", Body: "What does this line do?",
					CreatedAt: commentAt, UpdatedAt: commentAt,
				}},
			},
			{
				ID: selftestShipFixThreadID, Path: "hello.txt", Line: 1,
				Comments: []orchestrator.ThreadComment{{
					ID: "c2", Author: "reviewer-bot", Body: "Validate this before using it.",
					CreatedAt: commentAt, UpdatedAt: commentAt,
				}},
			},
		},
	}
}

// headSHA reads branch's own current commit straight off g's real bare
// origin ("git rev-parse refs/heads/<branch>"), so a real git push is what
// moves what GetPR and ListCheckRuns see, not a field this double would
// otherwise have to be told to update.
func (g *selftestShipGitHub) headSHA(ctx context.Context, branch string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", g.remoteDir, "rev-parse", "refs/heads/"+branch) //nolint:gosec // argv-only, no shell; remoteDir is this suite's own gitfixture bare origin and branch is git's own zing/<id>-<slug> branch name, never outside input
	// Scrubbed, so a GIT_DIR a git hook exported cannot redirect "-C".
	cmd.Env = gitfixture.Environ()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("selftestShipGitHub: rev-parse %s: %w", branch, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (*selftestShipGitHub) RepoDefaultBranch(context.Context, string, string) (string, error) {
	return "", errors.New("selftestShipGitHub: not implemented")
}

func (*selftestShipGitHub) RequiredChecks(context.Context, string, string, string) ([]string, error) {
	return nil, errors.New("selftestShipGitHub: not implemented")
}

func (g *selftestShipGitHub) CreateDraftPR(ctx context.Context, _, _, head, base, _, _ string) (prURL string, number int, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pr != nil {
		return "", 0, errors.New("selftestShipGitHub: a pull request already exists for this head")
	}
	sha, shaErr := g.headSHA(ctx, head)
	if shaErr != nil {
		return "", 0, shaErr
	}
	g.nextNum++
	g.pr = &selftestShipPR{url: fmt.Sprintf("https://github.com/%s/%s/pull/%d", e2eFixtureGitHubOwner, e2eFixtureGitHubOwner, g.nextNum), head: head, base: base, number: g.nextNum}
	g.redSHA = sha
	g.draft = true
	return g.pr.url, g.pr.number, nil
}

func (g *selftestShipGitHub) FindPRByHead(_ context.Context, _, _, head, base string) (prURL string, number int, ok bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pr == nil || g.pr.head != head || g.pr.base != base {
		return "", 0, false, nil
	}
	return g.pr.url, g.pr.number, true, nil
}

func (g *selftestShipGitHub) GetPR(ctx context.Context, _, _ string, _ int) (orchestrator.PRState, error) {
	g.mu.Lock()
	pr := g.pr
	g.mu.Unlock()
	if pr == nil {
		return orchestrator.PRState{}, errors.New("selftestShipGitHub: GetPR before CreateDraftPR")
	}
	sha, err := g.headSHA(ctx, pr.head)
	if err != nil {
		return orchestrator.PRState{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return orchestrator.PRState{
		Number: pr.number, State: "open", Draft: g.draft, HeadSHA: sha, BaseRef: pr.base,
		NodeID: pr.url, Merged: g.merged,
	}, nil
}

// Merge is MERGE's own real GitHub write (M4 task 8, design section 8.8):
// it records that a merge happened, so the next GetPR reports Merged
// true and POLL's own row (design section 8.3 step 3) moves the ticket to
// done. This double never refuses: every precondition MERGE itself must
// hold (open, not draft, head pinned, CI green, no open thread) is already
// proved by the real dispatcher tick that reached here.
func (g *selftestShipGitHub) Merge(_ context.Context, _, _ string, _ int, _, _, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.merged = true
	return "", nil
}

func (g *selftestShipGitHub) ListCheckRuns(_ context.Context, _, _, sha string) ([]orchestrator.CheckRun, error) {
	g.mu.Lock()
	redSHA := g.redSHA
	g.mu.Unlock()
	conclusion := "success"
	if sha == redSHA {
		conclusion = "failure"
	}
	return []orchestrator.CheckRun{{ID: 1, Name: "ci", Status: "completed", Conclusion: conclusion, AppSlug: "github-actions", AppID: 1}}, nil
}

func (*selftestShipGitHub) ListStatuses(context.Context, string, string, string) ([]orchestrator.CommitStatus, error) {
	return nil, nil
}

func (*selftestShipGitHub) RequiredCheckRules(context.Context, string, string, string) ([]orchestrator.RequiredCheck, error) {
	return []orchestrator.RequiredCheck{{Context: "ci"}}, nil
}

func (*selftestShipGitHub) JobLogTail(context.Context, string, string, int64, int) (string, error) {
	return "", nil
}

// MarkReady and ConvertToDraft give selftestShipGitHub job.DraftFlips too
// (M4 task 7, task 8): this fake's own draft field is real state, read
// back by the very next GetPR (design section 8.9's own convergence
// rule), so row 8's ready flip and row 9's merge gate both see it for
// real rather than a canned value.
func (g *selftestShipGitHub) MarkReady(context.Context, string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.draft = false
	return nil
}

func (g *selftestShipGitHub) ConvertToDraft(context.Context, string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.draft = true
	return nil
}

// ListThreads, ThreadCommentsContain, ReplyToThread, and ResolveThread give
// selftestShipGitHub job.ReviewThreads too (M4 tasks 4, 10): unlike the
// stub shape ListStatuses and JobLogTail above still give POLL's other
// unused reads, this suite's own fixture ticket does open two real review
// threads (newSelftestShipGitHub), so these four track and mutate real
// state the same way draft and merged do above -- RESPOND, APPLY, and
// FIX-REPLIES (design section 9.2 to 9.4) all read back what the others
// wrote, not a canned answer.
func (g *selftestShipGitHub) ListThreads(context.Context, string, string, int) ([]orchestrator.Thread, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]orchestrator.Thread, len(g.threads))
	copy(out, g.threads)
	return out, nil
}

// ThreadCommentsContain reports whether a reply already posted to rawID
// carries needle, the same idempotent marker check ReplyToThread's own real
// GraphQL sibling makes (APPLY's and FIX-REPLIES' own guard, design
// sections 9.3, 9.4): author is unchecked, since this double only ever
// posts its own replies under selftestShipGHViewerLogin.
func (g *selftestShipGitHub) ThreadCommentsContain(_ context.Context, rawID, needle, _ string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Contains(g.replies[rawID], needle), nil
}

// ReplyToThread records body as rawID's own posted reply and appends it to
// that thread's own comments, authored by selftestShipGHViewerLogin, so a
// later ListThreads shows Zing's own reply as the thread's last comment the
// same way a real GitHub reply would (design section 9.1's own leftover
// class, had anything in this suite relied on it).
func (g *selftestShipGitHub) ReplyToThread(_ context.Context, rawID, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	i := slices.IndexFunc(g.threads, func(th orchestrator.Thread) bool { return th.ID == rawID })
	if i < 0 {
		return fmt.Errorf("selftestShipGitHub: ReplyToThread: unknown thread %s", rawID)
	}
	g.replies[rawID] = body
	now := time.Now().UTC()
	g.threads[i].Comments = append(g.threads[i].Comments, orchestrator.ThreadComment{
		ID: "zing-reply-" + rawID, Author: selftestShipGHViewerLogin, Body: body, CreatedAt: now, UpdatedAt: now,
	})
	return nil
}

// ResolveThread marks rawID resolved, so the next ListThreads a poll makes
// no longer counts it against row 8's "zero unresolved threads" rule
// (design section 8.5, 8.9).
func (g *selftestShipGitHub) ResolveThread(_ context.Context, rawID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	i := slices.IndexFunc(g.threads, func(th orchestrator.Thread) bool { return th.ID == rawID })
	if i < 0 {
		return fmt.Errorf("selftestShipGitHub: ResolveThread: unknown thread %s", rawID)
	}
	g.threads[i].IsResolved = true
	return nil
}

func (*selftestShipGitHub) ListReviews(context.Context, string, string, int) ([]orchestrator.Review, error) {
	return nil, nil
}

func (*selftestShipGitHub) RequestReviewers(context.Context, string, string, int, string) error {
	return errors.New("selftestShipGitHub: not implemented")
}

func (*selftestShipGitHub) Viewer(context.Context) (string, error) {
	return selftestShipGHViewerLogin, nil
}

func (*selftestShipGitHub) CommentOnPR(context.Context, string, string, int, string) error {
	return errors.New("selftestShipGitHub: not implemented")
}

// runSelftest proves the foundation on an empty machine: it migrates a fresh
// temporary database and checks it. It prints "selftest: OK" and returns 0
// when every step passes, or prints "selftest: <detail>" for the first
// failure and returns 1.
func runSelftest() int {
	if err := selftest(); err != nil {
		fmt.Fprintf(os.Stderr, "selftest: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stdout, "selftest: OK")
	return 0
}

func selftest() error {
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "zing-selftest")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	s, err := store.Open(ctx, filepath.Join(dir, "zing.db"))
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	if err = s.VerifyTables(ctx); err != nil {
		return fmt.Errorf("verify tables: %w", err)
	}

	diffs, err := schemagen.Diff(store.SchemaFS())
	if err != nil {
		return fmt.Errorf("schema drift: %w", err)
	}
	if len(diffs) > 0 {
		return fmt.Errorf("committed schema differs from the generator: %s", diffs[0])
	}

	if _, err = machine.Load(zing.Assets, "machine.toml"); err != nil {
		return err
	}

	lenses, err := lens.Load(zing.Assets, "prompts/lenses")
	if err != nil {
		return err
	}
	if len(lenses) != 8 {
		return fmt.Errorf("expected 8 lenses, found %d", len(lenses))
	}

	if err := s.ValidateExamples(); err != nil {
		return err
	}

	if err := checkResponseTemplates(); err != nil {
		return err
	}

	if err := checkResponseExamples(response.ExampleFS); err != nil {
		return err
	}

	if err := checkSandboxProfile(dir); err != nil {
		return err
	}

	if err := selftestE2E(ctx); err != nil {
		return fmt.Errorf("end-to-end: %w", err)
	}

	return nil
}

// e2eMaxTicks bounds selftestResumeE2E's tick loop: enough ticks for intake
// plus one handler call per pipeline transition (design section 7.1: queued,
// planning x2, building, reviewing, judging, shipping is 7 handler calls),
// plus judging's own round 1 (START, RUN, one CHECK per scenario with a
// check command, EVALUATE) failing once and driving a fix to landing
// (RUN, CHECK, LAND) before judging's own round 2 (START, RUN, CHECK,
// EVALUATE) passes it on to shipping (PKG9-PLAN.md section 19.3 task 9:
// "the e2e passes review, a judge failure, a fix, and a judge pass"), with
// generous headroom, so a stuck dispatcher fails the selftest promptly
// instead of hanging.
const e2eMaxTicks = 80

// e2eJudgeCheckCmd is the one shell command fixtures/scripts/planning/2.xml's
// own scenario s1 carries as its check_cmd: CHECK (design section 7.5)
// re-runs it for real, but this suite's fixture project never starts a
// real HTTP server on port 8080, so a live curl would always fail.
// selftestCommands intercepts exactly this one command, never a live
// network call, and reports what a real server would have, at the same
// CommandRunner seam building's own CHECK step already takes its commands
// through (job.Deps.Commands).
const e2eJudgeCheckCmd = "curl -sf localhost:8080/hello"

// selftestCommands wraps the real CommandRunner so the judge's own CHECK
// step never dials out: every command but e2eJudgeCheckCmd runs for real
// (the building state's own "test -f hello.txt" and "true" included, so
// selftest still proves those run for real). e2eJudgeCheckCmd itself is
// stateful (PKG9-PLAN.md section 19.3 task 9): its first call reports the
// exit 1 a server not yet listening would give, failing judge round 1 and
// driving fixtures/scripts/build/fix/1.xml's own fix to landing; every
// later call -- round 2's own re-run, after the fix -- reports the exit 0
// a live server would, matching fixtures/scripts/judge/2/1.xml's own
// scripted pass verdict for s1. checkCalls is a pointer, not a plain int,
// so every job.Deps copy this value is handed into still shares the one
// counter underneath it.
type selftestCommands struct {
	real       job.CommandRunner
	checkCalls *int32
}

// newSelftestCommands returns a selftestCommands wrapping real, its own
// e2eJudgeCheckCmd call counter freshly zeroed.
func newSelftestCommands(realRunner job.CommandRunner) selftestCommands {
	return selftestCommands{real: realRunner, checkCalls: new(int32)}
}

func (c selftestCommands) Run(ctx context.Context, dir, repoGit, shellCmd string, timeout time.Duration, cio job.CommandIO) (int, error) {
	if shellCmd == e2eJudgeCheckCmd {
		if atomic.AddInt32(c.checkCalls, 1) == 1 {
			return 1, nil // round 1: the scenario's own check fails, forcing a fix
		}
		return 0, nil // round 2, after the fix lands: the check passes
	}
	return c.real.Run(ctx, dir, repoGit, shellCmd, timeout, cio)
}

// e2eOwner is this selftest run's claim owner id (design section 7.2's
// shape is <hostname>-<pid>; a fixed literal is simpler and just as unique
// within one selftest process, which claims nothing concurrently).
const e2eOwner = "selftest-e2e"

// e2eFixtureGitHubOwner is the owner and repo name both the orchestrator's
// own Project and job.Project's own shipping fields (M3 tasks 6, 7) use for
// this e2e's one fixture project: a placeholder, never a real GitHub
// repository, since selftestShipGH never calls the real API.
const e2eFixtureGitHubOwner = "zing-fixture"

// e2eBudget and e2eFloor are the fixture job.Deps.Budget and job.Deps.Floor
// selftestResumeE2E wires the dispatcher with (design section 4.4): the
// same values internal/config's own applyDefaults would produce from an
// empty zing.toml (agent_minutes_per_ticket 240, review.floor "minor").
// Nothing in this M1 task reads Budget or Floor yet; wiring them here now
// only means task 4's runJob and the review floor split find them already
// in place.
var (
	e2eBudget = 240 * time.Minute
	e2eFloor  = response.SeverityMinor
)

// e2eLensesParallel is the fixture job.Deps.LensesParallel selftestResumeE2E
// wires the dispatcher with (PKG9-PLAN.md section 4.3, 6.2): the same
// default internal/config's own applyDefaults would produce from an empty
// zing.toml (review.max_lenses_parallel 7), so ROUND runs every one of its
// seven lenses in one tick, the way the real demo (section 19.2 task 10)
// does.
const e2eLensesParallel = 7

// e2eModels is the fixture job.Deps.Models alias table selftestResumeE2E
// wires the dispatcher with: the same model ids internal/config's own
// applyDefaults would produce from an empty zing.toml's [models] table.
var e2eModels = map[string]string{
	modelAliasSonnet: "claude-sonnet-5",
	modelAliasOpus:   "claude-opus-5-5",
	modelAliasFable:  "claude-fable-5-1",
	modelAliasCodex:  "gpt-6-luna",
}

// e2eWantStates is the ordered "to" state of every state message the
// silent ring plus the one question write, in order (design section 7.1):
// queued -> planning carries no message at intake, so the first message is
// planning itself.
var e2eWantStates = []string{"planning", "building", "reviewing", "judging", "shipping", "done"}

// e2eFrameTimeout bounds every SSE read and every settle-poll this suite
// makes, matching internal/console's own test frameTimeout: long enough for
// a slow CI box, short enough that a hung stream fails selftest instead of
// hanging it.
const e2eFrameTimeout = 5 * time.Second

// e2ePushToken is never checked: neither phase below calls /push/key or
// /push/subscribe, so any value satisfies console.New's signature.
const e2ePushToken = "selftest-e2e-push-token" //nolint:gosec // not a credential: a fixed placeholder no route in this suite ever checks

// selftestE2E proves the design section 11 end-to-end suite plus its Task
// 12 extension (design section 6.15, section 12 row 12), entirely on
// temp-dir stores (never ~/.zing), as the one suite that must never be
// skipped: selftestSeedDemo seeds the demo fixture and proves every
// closed-set question kind renders in the console's real live thread, and
// selftestResumeE2E drives the fixture ticket from intake through planning
// with the real dispatcher, the fixture tracker, the fake runtime, the real
// store, the real bus, and the real console endpoints -- including its own
// live GET /stream -- answers the planning question through POST /draft
// then POST /send exactly as a browser's chip click and send chord would,
// and asserts the resume advances the ticket to done with every state
// message in order and no goroutine or bus-subscription leak.
func selftestE2E(ctx context.Context) error {
	if err := selftestSeedDemo(ctx); err != nil {
		return fmt.Errorf("seed demo: %w", err)
	}
	return selftestResumeE2E(ctx)
}

// selftestSeedDemo seeds the design section 6.15 demo fixture on its own
// temp-dir store, calling SeedDemo twice to also prove it is idempotent,
// then opens the console's real live GET /stream on the demo ticket's
// thread -- the same path a browser's thread view reads -- and asserts all
// six closed-set question kinds (design section 8) render there.
func selftestSeedDemo(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "zing-selftest-seed-demo")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	st, err := store.Open(ctx, filepath.Join(dir, "zing.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if seedErr := console.SeedDemo(ctx, st); seedErr != nil {
		return fmt.Errorf("first SeedDemo: %w", seedErr)
	}
	if seedErr := console.SeedDemo(ctx, st); seedErr != nil {
		return fmt.Errorf("second SeedDemo (idempotency): %w", seedErr)
	}

	ticketID, err := demoTicketID(ctx, st)
	if err != nil {
		return err
	}

	logHandler := console.NewHandler(io.Discard, new(slog.LevelVar), nil)
	srv, err := newSelftestConsoleServer(ctx, st, bus.New(), nil, logHandler)
	if err != nil {
		return fmt.Errorf("start console server: %w", err)
	}
	defer srv.Close()

	streamResp, streamReader, cancelStream, err := getThreadStream(ctx, srv.URL, ticketID)
	if err != nil {
		return fmt.Errorf("open demo thread stream: %w", err)
	}
	defer cancelStream()
	defer func() { _ = streamResp.Body.Close() }()

	_, main, _, err := readInitialSelftestFrames(streamReader)
	if err != nil {
		return fmt.Errorf("read demo thread frames: %w", err)
	}

	return assertEveryQuestionKindRenders(main)
}

// demoTicketID returns SeedDemo's demo ticket id: the one ticket under the
// one project SeedDemo names "demo".
func demoTicketID(ctx context.Context, st *store.Store) (int64, error) {
	projects, err := st.ListProjects(ctx)
	if err != nil {
		return 0, fmt.Errorf("list projects: %w", err)
	}
	var demoProjectID int64
	found := false
	for _, p := range projects {
		if p.Name == "demo" {
			demoProjectID = p.ID
			found = true
			break
		}
	}
	if !found {
		return 0, fmt.Errorf("no %q project after SeedDemo", "demo")
	}

	tickets, err := st.TicketsByProject(ctx, demoProjectID)
	if err != nil {
		return 0, fmt.Errorf("tickets by project: %w", err)
	}
	if len(tickets) != 1 {
		return 0, fmt.Errorf("%d tickets under the demo project, want exactly 1", len(tickets))
	}
	return tickets[0].ID, nil
}

// selftestQuestionKindTitles are the six question-kind fixture titles
// SeedQuestionFixtures writes (internal/console/seed.go's seedQuestionText),
// one per design section 8 closed-set kind, in no particular order: a
// thread frame that shows every title has shown every kind's rendered
// control.
var selftestQuestionKindTitles = []string{
	"How should the greeting read?", // question
	"Approve the plan?",             // gate
	"Split this ticket?",            // split
	"Merge the PR?",                 // merge
	"Confirm the file perimeter",    // perimeter
	"Triage the review findings",    // review
}

// assertEveryQuestionKindRenders fails unless mainFrame -- one rendered
// #main thread frame -- contains every selftestQuestionKindTitles entry and
// exactly that many `<details class="q">` question groups (the same marker
// internal/console's own TestQuestionKindsRenderTheirControls splits on).
func assertEveryQuestionKindRenders(mainFrame string) error {
	for _, title := range selftestQuestionKindTitles {
		if !strings.Contains(mainFrame, title) {
			return fmt.Errorf("thread frame is missing the %q question group", title)
		}
	}
	groups := strings.Count(mainFrame, `<details class="q"`)
	if groups != len(selftestQuestionKindTitles) {
		return fmt.Errorf("thread frame has %d question groups, want exactly %d (one per closed-set kind)", groups, len(selftestQuestionKindTitles))
	}
	return nil
}

// selftestResumeE2E is Task 7's verify-by (design section 6.7, section 12
// row 7), run here as the suite that must never be skipped: the fake
// pipeline drives the fixture ticket from intake through planning on the
// real dispatcher, the fixture tracker, the fake runtime, and the real
// store; this answers its one planning question through the console's own
// live GET /stream plus real POST /draft then POST /send -- the same
// two-step a browser's chip click and send chord take, against a real
// network listener rather than an in-process recorder -- and asserts the
// resume advances the ticket to done, every state message appears in
// order in both the store and the live stream's own frames, and neither
// the stream's goroutine nor its bus subscription survives disconnecting it
// (design section 12 row 12; the technique mirrors Task 7's own
// internal/console/resume_e2e_test.go).
func selftestResumeE2E(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "zing-selftest-e2e")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	st, err := store.Open(ctx, filepath.Join(dir, "zing.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		return err
	}

	scripts, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		return fmt.Errorf("sub scripts fs: %w", err)
	}
	// Production real runtimes arrive in task 14 (Claude and Codex are
	// still stubs); the selftest e2e maps all three machine.toml runtime
	// names to the one Fake (design section 4.1, D2).
	fake := runtime.NewFake(scripts)
	rts, err := runtime.NewSet(map[string]runtime.Runtime{runtimeNameClaude: fake, runtimeNameCodex: fake, runtimeNameFake: fake})
	if err != nil {
		return fmt.Errorf("build runtime set: %w", err)
	}

	tr, err := tracker.NewFixture(fixtures.FS, "tickets.toml")
	if err != nil {
		return err
	}

	// The project path is a real, signed gitfixture repository (PKG8-PLAN.md
	// section 9.4, 10): building's EnsureWorktree, ChangedPaths, CommitTask,
	// and SignedStatus all run real git against it. The planning handler's
	// ready entry point (design section 6.5) opens the same directory
	// through os.OpenRoot and checks the fixture cohort's one code claim
	// (fixtures/scripts/planning/2.xml cites "cmd/zing/main.go:60"), so that
	// path is committed here too, on top of gitfixture's own initial commit,
	// and never shows up as an extra once building starts.
	projDir := filepath.Join(dir, "project")
	if mkErr := os.MkdirAll(projDir, 0o755); mkErr != nil {
		return fmt.Errorf("mkdir project dir: %w", mkErr)
	}
	if fixErr := gitfixture.NewSigningRepo(ctx, projDir); fixErr != nil {
		return fmt.Errorf("build gitfixture repo: %w", fixErr)
	}
	if addErr := gitfixture.AddFile(ctx, projDir, filepath.Join("cmd", "zing", "main.go"), []byte("package main\n")); addErr != nil {
		return fmt.Errorf("add cmd/zing/main.go to gitfixture repo: %w", addErr)
	}
	// A bare origin remote, so shipping's own PUBLISH (M3 task 6) has
	// somewhere real to push the ticket branch before OpenDraftPR asks
	// selftestShipGH to open the draft pull request (PKG9-PLAN.md section
	// 8.2), and selftestShipGH itself (task 8) a real ref to read PR heads
	// and pushes off of.
	remoteDir, wboErr := gitfixture.WithBareOrigin(ctx, projDir)
	if wboErr != nil {
		return fmt.Errorf("add bare origin to gitfixture repo: %w", wboErr)
	}

	projectID, err := st.EnsureProject(ctx, store.Project{
		Name: "zing", RepoURL: "https://example.invalid/zing", LocalPath: projDir, Tracker: "github",
	})
	if err != nil {
		return err
	}

	selftestShipGH := newSelftestShipGitHub(remoteDir)
	orch, err := orchestrator.New(
		orchestrator.Project{Owner: e2eFixtureGitHubOwner, Repo: e2eFixtureGitHubOwner, LocalPath: projDir, DefaultBranch: "main"},
		selftestShipGH, orchestrator.NewRunner(), nil)
	if err != nil {
		return fmt.Errorf("build orchestrator: %w", err)
	}
	repoGit, err := orch.GitCommonDir(ctx)
	if err != nil {
		return fmt.Errorf("git common dir: %w", err)
	}

	// clock is the fake clock design section 17.1 names: the babysit poll's
	// own backoff (section 8.3) schedules a real Poll{NextAt} up to 300s
	// out, and driveToDone (below) advances clock straight to a waiting
	// ticket's own next_poll_at instead of ever letting Tick's own
	// candidate pick actually wait that out.
	clock := newSelftestClock()

	b := bus.New()
	d, err := zdispatch.New(st, tr, b, m, job.Registry(),
		[]zdispatch.Binding{{StoreProjectID: projectID, TrackerProject: "zing"}},
		zdispatch.Config{
			Interval: time.Millisecond, MaxParallel: 2, Owner: e2eOwner,
			Models: e2eModels, Budget: e2eBudget, Floor: e2eFloor,
			// selftest drives the fake runtime, never a real sandboxed
			// process: sandbox.OffSet() is always unavailable, and
			// RequireSandbox false lets a sandboxed job (build, perimeter)
			// run unwrapped instead of refusing (design D5, section 10).
			Sandboxes: sandbox.OffSet(), RequireSandbox: false,
			Commands:       newSelftestCommands(job.NewCommandRunner(sandbox.Off(), false)),
			DataDir:        dir,
			LensesParallel: e2eLensesParallel,
			Now:            clock.Now,
			// Projects carries what the real building handler needs for
			// this one project (PKG8-PLAN.md section 4.3): the fixture
			// project's own test and lint commands (section 9.4). Owner,
			// Repo, PullRequests, and Checks are shipping's own window onto
			// GitHub (PKG9-PLAN.md section 10.3), all filled from the one
			// selftestShipGH built above, the same way serve's own
			// buildJobProjects fills them from one *orchestrator.GitHubClient.
			Projects: map[int64]job.Project{
				projectID: {
					Orch: orch, RepoGit: repoGit, TestCmd: "test -f hello.txt", LintCmd: "true",
					Owner: e2eFixtureGitHubOwner, Repo: e2eFixtureGitHubOwner,
					PullRequests: selftestShipGH, Checks: selftestShipGH, Threads: selftestShipGH, Flips: selftestShipGH,
				},
			},
		}, rts)
	if err != nil {
		return err
	}

	// The real console handler, over a real network listener (design
	// section 11, "stream": a live SSE connection needs one; an
	// httptest.NewRecorder cannot stream a still-running handler). This
	// suite never exercises /loglevel or /debug, so the Task 5/10 log
	// handler console.New now requires is a throwaway one over a discarded
	// sink, not the process's installed default (run's own, serve.go).
	logHandler := console.NewHandler(io.Discard, new(slog.LevelVar), nil)
	srv, err := newSelftestConsoleServer(ctx, st, b, m, logHandler)
	if err != nil {
		return fmt.Errorf("start console server: %w", err)
	}
	defer srv.Close()

	ticketID, err := driveToOpenQuestion(ctx, d, st)
	if err != nil {
		return err
	}

	// The baseline is taken right before the /stream connection opens, with
	// the server and every other steady-state goroutine already running, so
	// the leak check below isolates this one connection's own resources
	// rather than the server's constant overhead (design section 11,
	// "stream" test seam).
	baseline := goruntime.NumGoroutine()

	streamResp, streamReader, cancelStream, err := getThreadStream(ctx, srv.URL, ticketID)
	if err != nil {
		return fmt.Errorf("open thread stream: %w", err)
	}
	frames := newSelftestFrameCollector(streamReader)
	closeStream := func() {
		cancelStream()
		_ = streamResp.Body.Close()
	}

	if err := answerFixtureQuestion(ctx, st, srv.URL, ticketID); err != nil {
		closeStream()
		return err
	}

	if err := driveToDone(ctx, d, st, clock, srv.URL, ticketID); err != nil {
		closeStream()
		return err
	}

	if err := verifySelftestE2E(ctx, st, ticketID); err != nil {
		closeStream()
		return err
	}
	if err := verifySelftestRespondDisclosedReply(selftestShipGH); err != nil {
		closeStream()
		return err
	}

	// Poll (bounded, not a fixed sleep) until the live stream itself has
	// shown every state transition, including "done": the ticket reaching
	// done in the store (asserted above) and the SSE handler's own
	// subsequent wake-and-patch are two different goroutines, so a fixed
	// assertion right after the store read could race a frame that simply
	// has not been flushed yet.
	if err := waitForSelftestFrames(frames, e2eWantStates); err != nil {
		closeStream()
		return err
	}

	closeStream()

	if err := waitForGoroutineBaseline(baseline); err != nil {
		return err
	}

	ch, unsub := b.Subscribe()
	defer unsub()
	b.Publish()
	select {
	case <-ch:
	case <-time.After(e2eFrameTimeout):
		return errors.New("e2e: a fresh subscriber saw no publish after the resume stream disconnected")
	}
	return nil
}

// driveToOpenQuestion ticks d, bounded by e2eMaxTicks, until the fixture
// ticket has appeared and is waiting on its planning question, and returns
// its id.
func driveToOpenQuestion(ctx context.Context, d *zdispatch.Dispatcher, st *store.Store) (int64, error) {
	var ticketID int64
	for i := range e2eMaxTicks {
		if err := d.Tick(ctx); err != nil {
			return 0, fmt.Errorf("tick %d: %w", i, err)
		}
		tickets, err := st.ListAllTickets(ctx)
		if err != nil {
			return 0, err
		}
		if len(tickets) == 0 {
			continue // intake has not run yet, or the fixture ticket is not there
		}
		if len(tickets) != 1 {
			return 0, fmt.Errorf("e2e: %d tickets after intake, want exactly 1", len(tickets))
		}
		ticketID = tickets[0].ID

		ticket, err := st.GetTicket(ctx, ticketID)
		if err != nil {
			return 0, err
		}
		if ticket.WaitingOn != nil && *ticket.WaitingOn == "questions" {
			return ticketID, nil
		}
	}
	if ticketID == 0 {
		return 0, fmt.Errorf("e2e: the fixture ticket never appeared within %d ticks", e2eMaxTicks)
	}
	return 0, fmt.Errorf("e2e: ticket %d never reached its planning question within %d ticks", ticketID, e2eMaxTicks)
}

// answerFixtureQuestion answers ticketID's one open planning question with
// its first offered option, through the console's own real POST /draft then
// POST /send (design section 6.7, section 11), the same two-step a
// browser's chip click and send chord take, and asserts the batch cleared
// the wait.
func answerFixtureQuestion(ctx context.Context, st *store.Store, base string, ticketID int64) error {
	open, err := st.QuestionsByState(ctx, ticketID, "open")
	if err != nil {
		return fmt.Errorf("questions by state: %w", err)
	}
	if len(open) != 1 {
		return fmt.Errorf("e2e: %d open questions on the fixture ticket, want exactly 1", len(open))
	}
	var payload response.QuestionPayload
	if unmarshalErr := json.Unmarshal(open[0].Payload, &payload); unmarshalErr != nil {
		return fmt.Errorf("unmarshal question payload: %w", unmarshalErr)
	}
	if len(payload.Options) == 0 {
		return fmt.Errorf("e2e: question %d has no options", open[0].ID)
	}
	chosen := payload.Options[0].Key

	draftBody := fmt.Sprintf(`{"ticket":%d,"question":%d,"option":%q}`, ticketID, open[0].ID, chosen)
	if postErr := postSelftestConsole(ctx, base, "/draft", draftBody); postErr != nil {
		return fmt.Errorf("draft answer for question %d: %w", open[0].ID, postErr)
	}
	sendBody := fmt.Sprintf(`{"ticket":%d}`, ticketID)
	if postErr := postSelftestConsole(ctx, base, "/send", sendBody); postErr != nil {
		return fmt.Errorf("send batch for ticket %d: %w", ticketID, postErr)
	}

	after, err := st.GetTicket(ctx, ticketID)
	if err != nil {
		return err
	}
	if after.WaitingOn != nil {
		return fmt.Errorf("e2e: ticket.WaitingOn after send = %q, want nil (the batch must clear the wait)", *after.WaitingOn)
	}
	return nil
}

// driveToDone keeps ticking d, bounded by e2eMaxTicks, until ticketID
// reaches state "done". Once the review tick posts the gate (design section
// 6.6, task 7c), it answers that the same console way answerFixtureQuestion
// answered Q1 -- a draft with option "a" then /send, which SendBatch's own
// kindForWaitReason maps straight to the gate kind -- so the dispatcher can
// seal the cohort and carry the ticket the rest of the way. Once POLL asks
// the merge question (design section 8.5 row 9, waiting "merge", M4 task
// 8), it answers that the same way with its own first option, "Merge now"
// (mergeOptions' own closed set, internal/job/shipping.go) -- selftestShipGH's
// own Merge then records the real GitHub write, so the ticket reaches done
// through the real design section 8.8 path, not a canned Merged value.
// After every tick it also advances clock to the ticket's own
// next_poll_at (design section 17.1, task 8): shipping's own babysit poll
// (section 8.3) can leave the ticket waiting on a real backoff up to 300s
// out, and this is what keeps that wait from ever actually happening.
func driveToDone(ctx context.Context, d *zdispatch.Dispatcher, st *store.Store, clock *selftestClock, base string, ticketID int64) error {
	answeredGate := false
	answeredMerge := false
	for i := range e2eMaxTicks {
		if err := d.Tick(ctx); err != nil {
			return fmt.Errorf("post-send tick %d: %w", i, err)
		}
		ticket, err := st.GetTicket(ctx, ticketID)
		if err != nil {
			return err
		}
		if ticket.State == "done" {
			switch {
			case !answeredGate:
				return errors.New("e2e: ticket reached done without ever waiting on the gate")
			case !answeredMerge:
				return errors.New("e2e: ticket reached done without ever waiting on the merge question")
			}
			return nil
		}
		if !answeredGate && ticket.WaitingOn != nil && *ticket.WaitingOn == "gate" {
			if err := answerFixtureQuestion(ctx, st, base, ticketID); err != nil {
				return fmt.Errorf("answer the gate: %w", err)
			}
			answeredGate = true
		}
		if !answeredMerge && ticket.WaitingOn != nil && *ticket.WaitingOn == "merge" {
			if err := answerFixtureQuestion(ctx, st, base, ticketID); err != nil {
				return fmt.Errorf("answer the merge question: %w", err)
			}
			answeredMerge = true
		}
		if ticket.NextPollAt != nil {
			clock.advanceTo(*ticket.NextPollAt)
		}
	}
	return fmt.Errorf("e2e: ticket did not reach done within %d ticks", e2eMaxTicks)
}

// selftestClock is the fake clock cmd/zing/selftest.go injects into
// dispatch.Config.Now (PKG9-PLAN.md section 17.1): it starts at the real
// wall-clock instant and only ever moves forward, through advanceTo, so
// driveToDone can skip a babysit poll's own real backoff wait (design
// section 8.3) without pretending any other part of the suite runs at
// anything but the real time.
type selftestClock struct {
	mu  sync.Mutex
	now time.Time
}

func newSelftestClock() *selftestClock {
	return &selftestClock{now: time.Now()}
}

// Now is dispatch.Config.Now.
func (c *selftestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// advanceTo moves the clock forward to at least t; it never moves it
// backward.
func (c *selftestClock) advanceTo(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.now) {
		c.now = t
	}
}

// newSelftestConsoleServer builds a real console.New handler bound to a
// reserved 127.0.0.1 listener, so its own real port -- not an arbitrary one
// -- is what console.New's mutation guard allowlists (mw.go, design section
// 6.14), and starts it, matching internal/console's own test helper
// technique (mw_test.go's newMutationTestServer). It passes sandbox.Off()'s
// own Reason() as console.New's sandboxReason (design section 9.2, Task
// 15), matching the dispatcher's own sandbox.Off() above: selftest never
// runs a real sandboxed process, so its console honestly shows sandbox: off
// rather than claiming an availability nothing here provides.
func newSelftestConsoleServer(ctx context.Context, st *store.Store, b *bus.Broker, m *machine.Machine, logHandler *console.Handler) (*httptest.Server, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("reserve a listener: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("unexpected listener address type %T", ln.Addr())
	}

	// tracker and user are left zero (nil, ""): selftest's e2e does not
	// exercise POST /projects/{id}/pickup (PKG9-PLAN.md D29).
	handler := console.New(st, b, m, []string{"127.0.0.1"}, addr.Port, logHandler, nil, e2ePushToken, e2eFloor, sandbox.Off().Reason(), nil, "")
	srv := httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		return nil, fmt.Errorf("close the placeholder listener: %w", err)
	}
	srv.Listener = ln
	srv.Start()
	return srv, nil
}

// getThreadStream opens GET /stream?view=thread&open=<ticketID> against
// base and returns the response, a buffered reader over its body, and its
// cancel func, matching internal/console/resume_e2e_test.go's own
// openThreadStream.
func getThreadStream(ctx context.Context, base string, ticketID int64) (*http.Response, *bufio.Reader, context.CancelFunc, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	v := url.Values{}
	v.Set("datastar", fmt.Sprintf(`{"view":"thread","open":%d,"project":0}`, ticketID))
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base+"/stream?"+v.Encode(), http.NoBody)
	if err != nil {
		cancel()
		return nil, nil, nil, fmt.Errorf("build GET /stream request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, nil, nil, fmt.Errorf("GET /stream: %w", err)
	}
	return resp, bufio.NewReader(resp.Body), cancel, nil
}

// postSelftestConsole sends one JSON POST to base+path over a real network
// connection, carrying the Datastar-Request header and an Origin equal to
// base, the same shape a browser's chip click or send chord sends (design
// section 6.4, 6.14). Close is set so the connection does not linger in the
// client's keep-alive pool, which would otherwise show up as apparent
// goroutine growth in selftestResumeE2E's own leak check even though
// nothing leaked. It fails unless the handler reports 204.
func postSelftestConsole(ctx context.Context, base, path, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("build POST %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Origin", base)
	req.Close = true

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 2xx generally, not exactly 204: POST /send answers 200 with a plain
	// result line (bug fix, answer.go's handleSend) while /draft and /read
	// still answer 204, and this helper is shared across all three.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("POST %s: status = %d, and reading the body failed: %w", path, resp.StatusCode, readErr)
		}
		return fmt.Errorf("POST %s: status = %d, body = %s", path, resp.StatusCode, respBody)
	}
	return nil
}

// readSelftestFrame reads one SSE frame from r, bounded by e2eFrameTimeout,
// matching internal/console/console_test.go's own readFrame.
func readSelftestFrame(r *bufio.Reader) (string, error) {
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var sb strings.Builder
		started := false
		for {
			line, err := r.ReadString('\n')
			if !started {
				if line == "\n" && err == nil {
					continue // a stray separator blank line before the frame starts
				}
				started = true
			}
			sb.WriteString(line)
			if err != nil {
				ch <- result{sb.String(), err}
				return
			}
			if line == "\n" {
				ch <- result{sb.String(), nil}
				return
			}
		}
	}()
	select {
	case res := <-ch:
		return res.text, res.err
	case <-time.After(e2eFrameTimeout):
		return "", errors.New("timed out waiting for an SSE frame")
	}
}

// readInitialSelftestFrames reads the three frames one /stream connect
// always sends, in the fixed order patchRegions writes them: #nav, then
// #main, then #rail (matching internal/console/stream_test.go's own
// readInitialFrames).
func readInitialSelftestFrames(r *bufio.Reader) (nav, main, rail string, err error) {
	if nav, err = readSelftestFrame(r); err != nil {
		return "", "", "", fmt.Errorf("read nav frame: %w", err)
	}
	if main, err = readSelftestFrame(r); err != nil {
		return "", "", "", fmt.Errorf("read main frame: %w", err)
	}
	if rail, err = readSelftestFrame(r); err != nil {
		return "", "", "", fmt.Errorf("read rail frame: %w", err)
	}
	return nav, main, rail, nil
}

// selftestFrameCollector accumulates every line read from a live /stream
// connection behind a mutex, so the main goroutine can poll its running
// text (String()) while the background reader is still live, matching
// internal/console/resume_e2e_test.go's own frameCollector.
type selftestFrameCollector struct {
	mu   sync.Mutex
	text strings.Builder
}

// newSelftestFrameCollector spawns one goroutine reading every SSE frame
// from r until it hits an error (the stream closing, on the caller's
// cancel).
func newSelftestFrameCollector(r *bufio.Reader) *selftestFrameCollector {
	c := &selftestFrameCollector{}
	go func() {
		for {
			line, err := r.ReadString('\n')
			c.mu.Lock()
			c.text.WriteString(line)
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return c
}

// String returns everything read so far, safe to call while the background
// reader is still running.
func (c *selftestFrameCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String()
}

// waitForSelftestFrames polls (bounded by e2eFrameTimeout, not a fixed
// sleep) until frames has shown a transition to every one of wantStates.
func waitForSelftestFrames(frames *selftestFrameCollector, wantStates []string) error {
	deadline := time.Now().Add(e2eFrameTimeout)
	for {
		if framesShowEveryStateSelftest(frames.String(), wantStates) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("live stream frames never showed every transition in %v; got:\n%s", wantStates, frames.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// framesShowEveryStateSelftest reports whether text contains a state
// separator (views.go's stateLine: "<from> -> <to>") for every state in
// wantStates, accepting either the raw or HTML-escaped arrow (templ
// auto-escapes text content).
func framesShowEveryStateSelftest(text string, wantStates []string) bool {
	for _, want := range wantStates {
		if !strings.Contains(text, "-&gt; "+want) && !strings.Contains(text, "-> "+want) {
			return false
		}
	}
	return true
}

// waitForGoroutineBaseline polls (bounded by e2eFrameTimeout) until the
// process's goroutine count has settled back to baseline, proving the
// disconnected stream's own goroutine is really gone.
func waitForGoroutineBaseline(baseline int) error {
	deadline := time.Now().Add(e2eFrameTimeout)
	for goruntime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			return fmt.Errorf("goroutine count did not settle back to the pre-stream baseline: got %d, baseline %d",
				goruntime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// verifySelftestE2E asserts the design section 11 end-to-end checkpoints
// once ticketID has reached done: the ordered state messages and exactly
// one question asked, answered, and resolved.
func verifySelftestE2E(ctx context.Context, st *store.Store, ticketID int64) error {
	msgs, err := st.ListMessages(ctx, ticketID)
	if err != nil {
		return err
	}

	var states []string
	var questions, answers, resolved int
	for i := range msgs {
		msg := &msgs[i]
		switch msg.Type {
		case "state":
			var sp response.StatePayload
			if err := json.Unmarshal(msg.Payload, &sp); err != nil {
				return fmt.Errorf("unmarshal state message %d: %w", msg.ID, err)
			}
			states = append(states, string(sp.To))
		case "question":
			questions++
		case "answer":
			answers++
		case "resolved":
			resolved++
		}
	}

	if !slices.Equal(states, e2eWantStates) {
		return fmt.Errorf("state messages = %v, want %v", states, e2eWantStates)
	}
	// Q1 (the fixture's planning question), the gate (design section 6.6,
	// task 7c), and the merge question (design section 8.8, M4 task 8):
	// three of each.
	const wantQuestionsAnswersResolved = 3
	if questions != wantQuestionsAnswersResolved {
		return fmt.Errorf("question messages = %d, want exactly %d", questions, wantQuestionsAnswersResolved)
	}
	if answers != wantQuestionsAnswersResolved {
		return fmt.Errorf("answer messages = %d, want exactly %d", answers, wantQuestionsAnswersResolved)
	}
	if resolved != wantQuestionsAnswersResolved {
		return fmt.Errorf("resolved messages = %d, want exactly %d", resolved, wantQuestionsAnswersResolved)
	}
	if err := verifySelftestJudgeFailedFixedThenPassed(msgs); err != nil {
		return err
	}
	if err := verifySelftestShipCILogFixedThenMerged(msgs); err != nil {
		return err
	}
	if err := verifySelftestRespondAnsweredThenReady(msgs); err != nil {
		return err
	}
	if err := verifySelftestMarkersAllRecognized(msgs); err != nil {
		return err
	}
	return verifySelftestCohortSealed(ctx, st, ticketID)
}

// verifySelftestMarkersAllRecognized is the design report's second guard
// (design/threading-design.md, "Render every message inside the thread it
// belongs to"): after the full pipeline, every "update" marker msgs carries
// must be one console.MarkerRecognized actually registers, not a shape
// nobody told buildThreadRows about. The hand-kept updateMarker* table in
// internal/console/views.go cannot catch a new marker someone's job code
// starts writing but never registers there -- this does, by checking the
// real bodies a real run wrote. msgs is the one ticket
// TestSelftestE2E_TicketReachesDoneWithOneQuestionAnswered drives through
// the whole pipeline (reviewing, judging, shipping, respond), so this
// already sees every marker kind that path writes; a future e2e fixture
// with more than one ticket would need this called per ticket, the same way
// verifySelftestE2E's other checks would.
func verifySelftestMarkersAllRecognized(msgs []store.MessageRow) error {
	for i := range msgs {
		if msgs[i].Type != selftestMsgTypeUpdate {
			continue
		}
		if !console.MarkerRecognized(msgs[i]) {
			return fmt.Errorf("message %d: console does not recognize update marker %q", msgs[i].ID, msgs[i].Body)
		}
	}
	return nil
}

// selftestMsgTypeUpdate is store.MessageRow.Type's own "update" value, the
// kind every marker this file's verify functions scan for is stored under:
// named once so goconst has nothing to flag across them.
const selftestMsgTypeUpdate = "update"

// verifySelftestJudgeFailedFixedThenPassed asserts msgs (ticketID's own, in
// id order) actually walked the path PKG9-PLAN.md section 19.3 task 9 names
// -- "a judge failure, a fix, and a judge pass" -- rather than merely
// reaching done some other way: judge round 1's own "judge round 1 failed"
// marker, a "fix landed" marker for the request it opened, and judge round
// 2's own "judge round 2 passed" marker, each after the one before it.
func verifySelftestJudgeFailedFixedThenPassed(msgs []store.MessageRow) error {
	var sawFailed, sawLanded, sawPassed bool
	for i := range msgs {
		if msgs[i].Type != selftestMsgTypeUpdate {
			continue
		}
		switch {
		case strings.HasPrefix(msgs[i].Body, "judge round 1 failed"):
			sawFailed = true
		case strings.HasPrefix(msgs[i].Body, "fix landed ") && sawFailed && !sawPassed:
			sawLanded = true
		case msgs[i].Body == "judge round 2 passed":
			sawPassed = true
		}
	}
	switch {
	case !sawFailed:
		return errors.New(`e2e: no "judge round 1 failed" marker, want judge round 1 to fail (PKG9-PLAN.md section 19.3 task 9)`)
	case !sawLanded:
		return errors.New(`e2e: no "fix landed" marker after judge round 1 failed, want the fix request to land`)
	case !sawPassed:
		return errors.New(`e2e: no "judge round 2 passed" marker, want round 2 to pass after the fix landed`)
	}
	return nil
}

// verifySelftestShipCILogFixedThenMerged asserts msgs (ticketID's own, in
// id order) actually walked the real path PKG9-PLAN.md section 19.4 task 8
// and section 19.5 task 8 name end to end -- PUBLISH opens a draft pull
// request, POLL sees the pushed commit's checks fail and requests a ci_log
// fix, the fix lands, CI reads green, row 8 marks the pull request ready,
// row 9 asks the merge question, and MERGE-ANSWER's own "Merge now" calls
// the real Merge -- rather than reaching done some other way: a "pr
// opened" marker, a "fix requested ci_log" marker after it, a "fix landed"
// marker after that, a "pr ready" marker, a "merge asked" marker, and
// finally "pr merged", each after the one before it. Done itself (and its
// own "merged" reason) is proved by e2eWantStates and driveToDone's own
// answeredGate and answeredMerge checks above; selftestShipGH's own Merge
// is what actually requires the real design section 8.8 path -- ready,
// asked, answered -- before it ever records the pull request merged (its
// own doc comment).
func verifySelftestShipCILogFixedThenMerged(msgs []store.MessageRow) error {
	var sawPROpened, sawFixRequested, sawFixLanded, sawReady, sawAsked, sawMerged bool
	for i := range msgs {
		if msgs[i].Type != selftestMsgTypeUpdate {
			continue
		}
		switch {
		case strings.HasPrefix(msgs[i].Body, "pr opened "):
			sawPROpened = true
		case strings.HasPrefix(msgs[i].Body, "fix requested ci_log after run ") && sawPROpened:
			sawFixRequested = true
		case strings.HasPrefix(msgs[i].Body, "fix landed ") && sawFixRequested && !sawFixLanded:
			sawFixLanded = true
		case strings.HasPrefix(msgs[i].Body, "pr ready ") && sawFixLanded:
			sawReady = true
		case strings.HasPrefix(msgs[i].Body, "merge asked ") && sawReady:
			sawAsked = true
		case strings.HasPrefix(msgs[i].Body, "pr merged ") && sawAsked:
			sawMerged = true
		}
	}
	switch {
	case !sawPROpened:
		return errors.New(`e2e: no "pr opened" marker, want PUBLISH to open a draft pull request (PKG9-PLAN.md section 19.4 task 8)`)
	case !sawFixRequested:
		return errors.New(`e2e: no "fix requested ci_log" marker after the pull request opened, want POLL to request a fix once CI failed`)
	case !sawFixLanded:
		return errors.New(`e2e: no "fix landed" marker after the ci_log fix request, want the fix to land`)
	case !sawReady:
		return errors.New(`e2e: no "pr ready" marker after the fix landed, want row 8 to mark the pull request ready once CI reads green`)
	case !sawAsked:
		return errors.New(`e2e: no "merge asked" marker after the ready flip, want row 9 to ask the merge question (PKG9-PLAN.md section 19.5 task 8)`)
	case !sawMerged:
		return errors.New(`e2e: no "pr merged" marker after the merge question was asked, want MERGE-ANSWER's own "Merge now" to merge for real`)
	}
	return nil
}

// verifySelftestRespondAnsweredThenReady asserts msgs (ticketID's own, in id
// order) walked the M4 task 10 respond leg before the ready flip: RESPOND
// answers both of newSelftestShipGitHub's own seeded threads (design
// section 9.2), APPLY posts the plain reply and collects the other into a
// consolidated fix request (9.3), the shared fix driver lands that request
// the normal way building's own fix unit already proves (design section
// 5.3), and FIX-REPLIES posts "Fixed in <sha>." and resolves it (9.4) --
// each after the one before it, and all before row 8 ever marks the pull
// request ready -- rather than the respond leg being silently skipped or
// the ready flip racing ahead of it.
func verifySelftestRespondAnsweredThenReady(msgs []store.MessageRow) error {
	var sawBatchStarted, sawApplied, sawThreadsRequested, sawThreadsLanded, sawRepliesPosted, sawReady bool
	landedCount := 0
	for i := range msgs {
		if msgs[i].Type != selftestMsgTypeUpdate {
			continue
		}
		body := msgs[i].Body
		switch {
		case strings.HasPrefix(body, "respond batch 1 started sha "):
			sawBatchStarted = true
		// APPLY's own commit (respond.go's apply) carries the fix request
		// message before the closing "respond applied" message in the same
		// commit (c.Messages = []store.Message{*fixMsg, appliedMsg}), so
		// "fix requested threads" always reaches store.ListMessages' own id
		// order first; neither depends on the other here.
		case strings.HasPrefix(body, "fix requested threads after run ") && sawBatchStarted:
			sawThreadsRequested = true
		case strings.HasPrefix(body, "respond applied ") && sawBatchStarted:
			sawApplied = true
		case strings.HasPrefix(body, "fix landed "):
			landedCount++
			if sawThreadsRequested {
				sawThreadsLanded = true
			}
		case strings.HasPrefix(body, "fix replies posted ") && sawThreadsLanded:
			sawRepliesPosted = true
		case strings.HasPrefix(body, "pr ready ") && sawRepliesPosted:
			sawReady = true
		}
	}
	switch {
	case !sawBatchStarted:
		return errors.New(`e2e: no "respond batch 1 started" marker, want POLL to start a respond batch once an actionable thread exists (PKG9-PLAN.md section 19.5 task 10)`)
	case !sawApplied:
		return errors.New(`e2e: no "respond applied" marker after the batch started, want RESPOND then APPLY to run`)
	case !sawThreadsRequested:
		return errors.New(`e2e: no "fix requested threads" marker after APPLY, want the collected fix action to go through the shared gate`)
	case landedCount < 2:
		return fmt.Errorf(`e2e: %d "fix landed" markers, want at least 2 (the ci_log fix and the threads fix)`, landedCount)
	case !sawThreadsLanded:
		return errors.New(`e2e: no "fix landed" marker after the threads fix request, want it to land`)
	case !sawRepliesPosted:
		return errors.New(`e2e: no "fix replies posted" marker after the threads fix landed, want FIX-REPLIES to close the loop`)
	case !sawReady:
		return errors.New(`e2e: no "pr ready" marker after the respond leg closed, want row 8 to mark the pull request ready only once every thread resolved`)
	}
	return nil
}

// verifySelftestRespondDisclosedReply asserts gh's own posted reply to
// selftestShipReplyThreadID (design section 9.3's own RESPOND answer,
// fixtures/scripts/respond/1/1.xml) actually carries N3's own disclosure --
// "Zing (an AI agent) replying on behalf of @<login>:" -- and its own reply
// marker, and that both of gh's seeded threads ended resolved: the ready
// flip and the merge that followed it (verifySelftestShipCILogFixedThenMerged)
// prove something cleared the loop, but only this double's own recorded
// state proves it was a real disclosed GitHub reply and not some other path.
func verifySelftestRespondDisclosedReply(gh *selftestShipGitHub) error {
	gh.mu.Lock()
	defer gh.mu.Unlock()

	reply, posted := gh.replies[selftestShipReplyThreadID]
	if !posted {
		return errors.New("e2e: no reply recorded for the review thread, want APPLY to post one")
	}
	wantPrefix := "Zing (an AI agent) replying on behalf of @" + selftestShipGHViewerLogin + ":"
	if !strings.HasPrefix(reply, wantPrefix) {
		return fmt.Errorf("e2e: posted reply = %q, want it to start with the disclosure prefix %q (design D10)", reply, wantPrefix)
	}
	if !strings.Contains(reply, "<!-- zing:reply a") {
		return fmt.Errorf("e2e: posted reply = %q, want it to carry a %q marker", reply, "<!-- zing:reply a")
	}

	for _, th := range gh.threads {
		if !th.IsResolved {
			return fmt.Errorf("e2e: thread %s is still unresolved, want both seeded threads resolved by the time the ticket reaches done", th.ID)
		}
	}
	return nil
}

// verifySelftestCohortSealed asserts every scenario artifact of ticketID's
// current plan cohort carries a non-nil sealed_at (design section 6.6
// branch 4, task 7c): the gate's approval seals exactly the cohort the
// review ran against.
func verifySelftestCohortSealed(ctx context.Context, st *store.Store, ticketID int64) error {
	cohort, ok, err := st.CurrentCohort(ctx, ticketID)
	if err != nil {
		return fmt.Errorf("current cohort: %w", err)
	}
	if !ok || cohort.RunID == nil {
		return fmt.Errorf("e2e: current cohort = %+v, ok=%v, want a cohort with a producing run", cohort, ok)
	}
	scenarios, err := st.ScenariosForRun(ctx, ticketID, *cohort.RunID, false)
	if err != nil {
		return fmt.Errorf("scenarios for run: %w", err)
	}
	if len(scenarios) == 0 {
		return errors.New("e2e: ScenariosForRun returned no scenarios, want the fixture cohort's own")
	}
	for i, sc := range scenarios {
		if sc.SealedAt == nil {
			return fmt.Errorf("e2e: scenario artifact %d has no sealed_at, want every cohort scenario sealed after approval", i)
		}
	}
	return nil
}

// checkSandboxProfile proves the checked-in seatbelt profile actually loads
// on this machine (design section 10, task 8): on macOS it runs sandbox.Load
// for real, against dataDir, and fails with the sandbox's own reason when
// it did not load, since an operator running selftest on their own laptop
// should learn now, not at the first real build tick, that a real build run
// would refuse to start (N9). selftest's own dispatcher never uses this
// sandbox -- it always runs sandbox.Off() (below) -- so this is a canary
// check, not a dependency of the e2e suite that follows it. It is a no-op
// off darwin, where the sandbox is always unavailable by definition, and
// when ZING_SANDBOXED is set (design section 5.3, Task 15): macOS refuses a
// nested sandbox, so a selftest already running inside the profile -- for
// example while zing builds itself -- cannot start sandbox-exec to prove
// anything here, the same skip internal/sandbox's own darwin tests apply.
func checkSandboxProfile(dataDir string) error {
	if goruntime.GOOS != "darwin" || os.Getenv("ZING_SANDBOXED") != "" {
		return nil
	}
	profile, err := zing.Assets.ReadFile("sandbox/build.sb")
	if err != nil {
		return fmt.Errorf("read embedded sandbox profile: %w", err)
	}
	sb := sandbox.Load(profile, dataDir, nil, e2eSandboxCheckPort)
	if !sb.Available() {
		return fmt.Errorf("selftest: sandbox profile did not load: %s", sb.Reason())
	}
	return nil
}

// e2eSandboxCheckPort is an arbitrary, valid port checkSandboxProfile's own
// Load call renders into the profile's console-deny rule: it is never
// dialed, so any value in 1-65535 would do.
const e2eSandboxCheckPort = 7420

// checkResponseTemplates renders every registered (job, outcome) pair's
// annotated template, failing on the first error (design section 6.10):
// a template exists for exactly the pairs the parser accepts.
// response.RegisteredPairs is internal/response's own single source of
// truth for that enumeration, so this can never drift from what Parse
// actually accepts.
func checkResponseTemplates() error {
	for _, p := range response.RegisteredPairs() {
		if _, err := response.RenderTemplate(p.Job, p.Outcome); err != nil {
			return fmt.Errorf("render %s/%s: %w", p.Job, p.Outcome, err)
		}
	}
	return nil
}

// checkResponseExamples parses and validates every example under fsys's
// examples/ directory (feature kind, no FS), design section 6.10. It reads
// through fsys, rather than response.ExampleFS directly, so a test can
// substitute a tampered filesystem without touching the real embedded
// files.
func checkResponseExamples(fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, "examples")
	if err != nil {
		return fmt.Errorf("list examples: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := "examples/" + e.Name()
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		doc, err := response.Parse(data)
		if err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		if errs := response.Validate(doc, response.ValidateContext{Kind: response.KindFeature}); len(errs) > 0 {
			return fmt.Errorf("validate %s: %w", name, errs[0])
		}
	}
	return nil
}
