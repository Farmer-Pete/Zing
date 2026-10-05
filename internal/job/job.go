// Package job is the dispatcher-to-job seam (design section 6.5): a
// Handler runs one pipeline state's work and returns a store.HandlerCommit
// describing what to persist and where the ticket goes next. A handler
// performs no write of its own; the dispatcher validates the commit with
// ValidateCommit and applies it through store.CommitHandlerResult under the
// claim fence.
package job

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"zing/internal/machine"
	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// The pipeline state names, spelled once so job.go and skeleton.go never
// repeat the literal (design section 7.1).
const (
	stateQueued    = "queued"
	statePlanning  = "planning"
	stateBuilding  = "building"
	stateReviewing = "reviewing"
	stateJudging   = "judging"
	stateShipping  = "shipping"
	stateDone      = "done"
	stateAbandoned = "abandoned"
)

// Deps is what a handler needs to do its work and build a commit: read-only
// store access, the runtime set a job resolves its runtime from by name, the
// process definition and the model alias table a handler reads a job's
// config out of, the per-ticket agent-time budget and the review floor, and
// the claim this run holds (Owner, Expires), which every commit must carry
// back unchanged as its fence (design section 4.4).
type Deps struct {
	Store    *store.Store // reads only inside a handler
	Runtimes runtime.Set  // resolves a job's machine.toml runtime name to a Runtime
	Machine  *machine.Machine
	Models   map[string]string // alias -> exact model id (config.Models)
	Budget   time.Duration     // time.Duration(cfg.Budget.AgentMinutesPerTicket) * time.Minute
	Floor    response.Severity
	Owner    string
	Expires  time.Time // the claim lease; the commit fence
	// Reserve is the one pre-commit write a handler may make under its claim
	// (design D13, section 4.4, 4.6): runJob is the only caller. The
	// dispatcher wires it to a closure over store.Reserve carrying this
	// tick's Owner and Expires, so a handler and its tests never see those
	// two arguments directly.
	Reserve ReserveFunc
	// Projects carries what building needs to know about each store
	// project, keyed by its id (PKG8-PLAN.md section 4.3): the orchestrator,
	// the repository's common git dir, and the project's test, lint and
	// fix commands. Wired by dispatch.Config.Projects.
	Projects map[int64]Project
	// Sandboxes holds the loaded profile set runJob wraps a sandboxed job's
	// run in, keyed by the name machine.toml's job.sandbox gives it: build,
	// readonly, or judge (PKG9-PLAN.md section 4.3, 4.7, replacing the
	// single Sandbox field). serve loads real profiles; selftest and most
	// test suites use sandbox.OffSet().
	Sandboxes sandbox.Set
	// RequireSandbox is true in serve (a real build run refuses to start
	// without a loaded sandbox, design N9) and false in selftest and every
	// suite that drives the fake runtime.
	RequireSandbox bool
	// Commands runs the test and lint re-runs a build unit's CHECK step
	// makes (task 9). Wired by dispatch.Config.Commands.
	Commands CommandRunner
	// DataDir is the resolved data directory (PKG9-PLAN.md section 4.3,
	// 7.3): the private temp root runJob gives every run whose job names no
	// sandbox lives under it. Empty is ErrConfig for a run that needs one.
	DataDir string
	// LensesParallel bounds how many of ROUND's seven lens runs are ever in
	// flight at once (PKG9-PLAN.md section 4.3, 6.2): config.Review's own
	// max_lenses_parallel, 1..7. Wired by dispatch.Config.LensesParallel.
	LensesParallel int
	// JudgeCodexHome is the resolved judge_codex_home (PKG9-PLAN.md section
	// 4.3, 4.5, D27): the judge's own persistent Codex home, copied into
	// the sandbox's CODEX_HOME parameter for any job whose profile is
	// "judge" (runjob.go). Empty for a judge run is ErrConfig. Wired by
	// dispatch.Config.JudgeCodexHome.
	JudgeCodexHome string
	// Tracker is what the shipping handler needs from the tracker
	// (PKG9-PLAN.md section 8.6, 17.1): PUBLISH's own PostPRLink, and
	// POLL's PostDone (task 7). The dispatcher implements ShipTracker over
	// its own tracker and bindings and passes itself here
	// (internal/dispatch/dispatch.go's runAndCommit).
	Tracker ShipTracker
	// MergeRule is config.Merge, resolved once at startup (PKG9-PLAN.md
	// section 4.3, 8.8): mergeDecision's own input for row 9's automatic
	// merge gate (shiprules.go). The zero value (Auto false) always asks.
	MergeRule MergeRule
	// ReviewBots is config.ReviewBots, resolved once at startup: pollIdle's
	// own input for nudging, then escalating, a required review-bot check
	// that has gone quiet (shipping.go). The zero value has no checks, so
	// pollIdle never nudges.
	ReviewBots ReviewBotRule
	// Now is runJobWith's own clock for the claude_hold_until gate (#45):
	// nil, its default, means time.Now. The dispatcher wires it to
	// Config.Now, so a test that drives the dispatcher's own clock also
	// drives the hold check.
	Now func() time.Time
}

// now is d.Now(), or time.Now when Now is nil.
func (d Deps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

// MergeRule is design section 8.8's own merge rule (zing.toml's [merge]
// table, config.Merge): Auto gates every automatic merge off by default
// (N5); ManualPaths and DependencyFiles each name path globs or exact/base
// names that block an automatic merge even when Auto is on, so
// mergeDecision (shiprules.go) can ask instead. Method is the GitHub merge
// method MERGE itself calls with (squash, merge, or rebase).
type MergeRule struct {
	Auto            bool
	Method          string
	ManualPaths     []string
	DependencyFiles []string
}

// ReviewBotRule is config.ReviewBots, resolved once at startup: pollIdle's
// own input for a required check that belongs to a review bot and has gone
// missing on a non-draft PR. Checks nil (the zero value) disables the
// nudge entirely.
type ReviewBotRule struct {
	// Wait is how long a configured check may sit missing before pollIdle
	// posts its Trigger comment, and again that long before it escalates.
	// Only read when Checks is non-empty.
	Wait time.Duration
	// Checks is config.ReviewBots.Checks, one entry per required check that
	// belongs to a review bot.
	Checks []ReviewBotCheck
}

// ReviewBotCheck names one required check that belongs to a review bot
// (Check, matched against a CIResult's Missing set by equality) and the
// comment pollIdle posts once to nudge it (Trigger, posted verbatim as a PR
// comment).
type ReviewBotCheck struct {
	Check   string
	Trigger string
}

// Project is what building needs to know about one store project (design
// section 4.3).
type Project struct {
	Orch    *orchestrator.Orchestrator
	RepoGit string // the repository's common git dir, absolute; Orch.GitCommonDir at startup
	TestCmd string // config projects[i].commands.test
	LintCmd string // config projects[i].commands.lint
	// FixCmd is config projects[i].commands.fix: owner-set, trusted like
	// test and lint, run in the build sandbox; empty runs no fix.
	FixCmd string
	Owner  string // the GitHub repository owner serve fills every project with (PKG9-PLAN.md section 10.3)
	Repo   string // the GitHub repository name serve fills every project with (PKG9-PLAN.md section 10.3)
	// PullRequests, Flips, Checks, and Threads are the shipping and respond
	// handlers' own window onto GitHub (PKG9-PLAN.md section 10.3): serve
	// fills all four from one shared *orchestrator.GitHubClient; a test fake
	// implements only the interface its test needs.
	PullRequests PullRequests
	Flips        DraftFlips
	Checks       Checks
	Threads      ReviewThreads
}

// CommandIO is what a CommandRunner caller can attach to one command. The
// zero value discards output and records nothing.
type CommandIO struct {
	Out     io.Writer      // stdout and stderr, one child-side stream (#55 plan D2); nil discards
	OnStart func(pgid int) // called once, synchronously, right after Start; pgid == pid (Setpgid)
}

// CommandRunner runs one shell command in dir, in its own process group, and
// returns its exit code. The whole group is killed when Run returns (design
// section 4.3, 5.5). cio attaches an output writer and a start callback;
// CommandIO{} attaches neither:
//
//	err == nil:                 the process ran and exited; exitCode is real
//	ErrCommandTimeout:          the timeout killed it; exitCode is -1
//	ErrSandbox:                 the sandbox is required and unavailable
//	context.Canceled (wrapped): the parent context ended; exitCode is -1
//	any other error:            the command could not start; exitCode is -1
type CommandRunner interface {
	Run(ctx context.Context, dir, repoGit, shellCmd string, timeout time.Duration, cio CommandIO) (exitCode int, err error)
}

// ErrSandbox and ErrCommandTimeout are the two new job-level errors this
// package's sandboxing adds (design section 4.3). ErrSandbox is runJob's own
// sandbox-unavailable failure (section 5.5) and sandboxedCommands' failure
// when RequireSandbox is true and the sandbox never loaded; routeFailure
// (planning.go) escalates it as sandbox_unavailable. ErrCommandTimeout is
// the real CommandRunner's own timeout failure (commands.go).
var (
	ErrSandbox        = errors.New("job: sandbox unavailable")
	ErrCommandTimeout = errors.New("job: command timed out")
)

// ReserveFunc reserves the next run for ticketID under the caller's claim
// and returns it (design section 4.4, 4.5): su creates or resumes a session,
// seed carries the run's exact model id and, for a build or perimeter task
// unit, its task number. It returns store.ErrClaimLost,
// unwrapped-but-wrappable, when the claim this call was reserved under has
// already moved on.
type ReserveFunc func(ctx context.Context, ticketID int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error)

// The typed job-level errors (design section 4.4). runJob (runjob.go) wires
// ErrBudget into its budget check and ErrConfig into the job/runtime/model
// lookups it performs before ever reserving a run; planningHandler.Run
// (planning.go) already wires ErrNoAction into the planning entry decision,
// returning it from three places: an exhausted session whose cap escalation
// is already recorded, an open session with no live step left to run, and
// any session state the switch does not otherwise recognize. ErrConfig must
// never reach a caller as a panic: a missing or misconfigured job, runtime,
// or model alias is a configuration mistake to report, not a programming
// invariant to crash on.
var (
	ErrNoAction = errors.New("job: no actionable state")
	ErrBudget   = errors.New("job: agent budget exhausted")
	ErrConfig   = errors.New("job: configuration error")
)

// Handler runs one pipeline state's job for ticket t and returns the commit
// the dispatcher should validate and apply. It writes nothing itself.
type Handler interface {
	Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error)
}

// Registry returns the six pipeline-state handlers (design section 6.5),
// keyed by the state each drives: queued is still the skeleton's code-only
// transition; planning (task 6), building (task 9), reviewing (task 10),
// judging (M2 task 8), and shipping (M3 task 6, PUBLISH; task 7, POLL) are
// the real handlers. done is terminal and carries no handler.
func Registry() map[string]Handler {
	return map[string]Handler{
		stateQueued:    queuedHandler{},
		statePlanning:  planningHandler{},
		stateBuilding:  buildingHandler{},
		stateReviewing: reviewingHandler{},
		stateJudging:   judgeHandler{},
		stateShipping:  shipHandler{},
	}
}

// interruptedResumeText is the fixed wording of the "interrupted" resume
// input (design section 6.3, 7.3): the raw, never-fenced sentence a resume
// carries when the session's latest run was cut off by a shutdown or a
// dead-serve reclaim (store.Run.Interrupted), shared by every job that
// resumes a session (building.go, fix.go, judging.go, respond.go).
const interruptedResumeText = "the previous run was interrupted; continue and return your document"

// labelInterrupted is the NamedInput.Label every "interrupted" resume input
// carries (building.go, judging.go, respond.go): a shared constant, not a
// literal repeated at each call site (goconst).
const labelInterrupted = "interrupted"

// resumeCharge says how a resume of a session is charged, from the newest
// run of that session (design D5, section 7.4): an interrupted latest run
// (store.Run.Interrupted) resumes free (bump is false, so the caller's own
// SessionUpsert carries BumpResumes: false) and ignores max_resumes (gate is
// false, so the caller skips its own resumeCapGate / respondCapGate check
// entirely, even on an already-exhausted session); anything else is charged
// and cap-gated exactly as before (bump and gate both true). Every resume
// branch of every job that can resume an interrupted run calls this with
// that session's newest run and obeys both return values.
func resumeCharge(latest store.Run) (bump, gate bool) {
	if latest.Interrupted {
		return false, false
	}
	return true, true
}

// priorNonInterruptedRun returns sessionID's own newest run strictly before
// beforeRunID whose outcome is not itself an unanswered interrupted error
// (design section 7.4, F009/7.2): it walks back past every run in the same
// "resumed, interrupted again" chain -- each one outcome "error" with
// Interrupted true -- so a claims or coverage resume that gets interrupted
// more than once in a row still lands on the same settled run every time,
// the one a pending-but-undelivered marker (claim errors, judge or respond
// coverage) is keyed to. found is false when sessionID has no such run
// (beforeRunID was its own first run, or every earlier run is itself part
// of that same interrupted chain).
func priorNonInterruptedRun(ctx context.Context, d Deps, ticketID, sessionID, beforeRunID int64) (store.Run, bool, error) {
	runs, err := d.Store.RunsForTicket(ctx, ticketID)
	if err != nil {
		return store.Run{}, false, fmt.Errorf("job: prior non-interrupted run: runs for ticket: %w", err)
	}
	var best store.Run
	found := false
	for _, r := range runs {
		if r.SessionID != sessionID || r.ID >= beforeRunID {
			continue
		}
		if r.Outcome != nil && *r.Outcome == string(response.OutcomeError) && r.Interrupted {
			continue // still inside the same interrupted-resume chain
		}
		if !found || r.ID > best.ID {
			best = r
			found = true
		}
	}
	return best, found, nil
}

// Validate confirms every non-terminal state m.States.Order names has a
// handler in reg, so a missing handler fails at startup, never at a nil map
// read mid-tick.
func Validate(m *machine.Machine, reg map[string]Handler) error {
	terminal := make(map[string]bool, len(m.States.Terminal))
	for _, s := range m.States.Terminal {
		terminal[s] = true
	}
	for _, s := range m.States.Order {
		if terminal[s] {
			continue
		}
		h, ok := reg[s]
		if !ok || h == nil {
			return fmt.Errorf("job: state %s has no handler", s)
		}
	}
	return nil
}

// legalEdges is the section 7.1 state table: for each From state, the To
// states a transition (a commit with Next set) may legally target.
var legalEdges = map[string][]string{
	stateQueued:    {statePlanning},
	statePlanning:  {statePlanning, stateBuilding, stateDone, stateAbandoned},
	stateBuilding:  {stateReviewing, stateAbandoned},
	stateReviewing: {stateJudging, stateAbandoned},
	stateJudging:   {stateShipping, stateAbandoned},
	stateShipping:  {stateDone, stateAbandoned},
}

// legalWaiting is the eight waiting_on flags migrations/0001_init.sql
// allows (design section 7.1 and 8).
var legalWaiting = map[string]bool{
	"questions": true, "split": true, "gate": true, "perimeter": true,
	"review": true, "merge": true, "children": true, "error": true,
}

// ValidateCommit checks c against the section 7.1 state table and the
// commit shape rules (design section 6.5): c.TicketID must name the ticket
// it was built against, the commit must do something (it is never wholly
// empty: at least one of Next, Waiting, Messages, Runs, ResolveQuestions,
// Session, Sessions, SetKind, SetBranch, Artifacts, ResolveAll, Seal,
// Escalation, TrackerEffect, SetPRURL, Poll, PollSchedule, ClearPoll, or
// Conversation must be set), when Next is set it names a legal successor of
// t.State and carries a non-empty Reason, and it does not also set a
// non-error Waiting; any set Waiting is one of the eight closed-set flags.
func ValidateCommit(t store.Ticket, c store.HandlerCommit) error {
	if c.TicketID != t.ID {
		return fmt.Errorf("job: commit is for ticket %d, not ticket %d", c.TicketID, t.ID)
	}
	if c.Next == "" && c.Waiting == nil && len(c.Messages) == 0 && len(c.Runs) == 0 &&
		len(c.ResolveQuestions) == 0 && c.Session == nil && len(c.Sessions) == 0 &&
		c.SetKind == nil && c.SetBranch == nil && len(c.Artifacts) == 0 && !c.ResolveAll &&
		c.Seal == nil && c.Escalation == nil && c.TrackerEffect == nil &&
		c.SetPRURL == nil && c.Poll == nil && c.PollSchedule == nil && !c.ClearPoll &&
		len(c.WithdrawQuestions) == 0 && c.Conversation == nil {
		return fmt.Errorf("job: commit for ticket %d carries no Next, Waiting, Messages, Runs, ResolveQuestions, Session, Sessions, SetKind, SetBranch, Artifacts, ResolveAll, Seal, Escalation, TrackerEffect, SetPRURL, Poll, PollSchedule, ClearPoll, WithdrawQuestions, or Conversation", t.ID)
	}
	if c.Next != "" {
		if c.Reason == "" {
			return fmt.Errorf("job: transition to %s carries no reason", c.Next)
		}
		if !legalEdge(t.State, c.Next) {
			return fmt.Errorf("job: %s -> %s is not a legal edge", t.State, c.Next)
		}
		if c.Waiting != nil && *c.Waiting != "error" {
			return fmt.Errorf("job: commit transitions to %s and also waits on %s", c.Next, *c.Waiting)
		}
	}
	if c.Waiting != nil && !legalWaiting[*c.Waiting] {
		return fmt.Errorf("job: waiting_on %q is not one of the eight waiting flags", *c.Waiting)
	}
	return nil
}

func legalEdge(from, to string) bool {
	return slices.Contains(legalEdges[from], to)
}

// OrderCandidates returns a new slice holding candidates ordered by reverse
// pipeline position (the state's index in order, largest first), then by
// the numeric external id parsed from TrackerRef (ascending), then by row
// id (ascending). A TrackerRef with no numeric suffix sorts after every
// candidate that has one (design section 6.2, 6.8).
func OrderCandidates(candidates []store.Ticket, order []string) []store.Ticket {
	pos := make(map[string]int, len(order))
	for i, s := range order {
		pos[s] = i
	}

	out := make([]store.Ticket, len(candidates))
	copy(out, candidates)
	slices.SortFunc(out, func(a, b store.Ticket) int {
		if c := cmp.Compare(pos[b.State], pos[a.State]); c != 0 { // reverse: larger index first
			return c
		}
		aNum, aOK := numericSuffix(a.TrackerRef)
		bNum, bOK := numericSuffix(b.TrackerRef)
		switch {
		case aOK && bOK:
			if c := cmp.Compare(aNum, bNum); c != 0 {
				return c
			}
		case aOK && !bOK:
			return -1
		case !aOK && bOK:
			return 1
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

// numericSuffix parses the trailing run of ASCII digits in ref (for
// example "fake#2" -> 2), reporting ok=false when ref carries no trailing
// digit at all.
func numericSuffix(ref string) (n int64, ok bool) {
	i := len(ref)
	for i > 0 && ref[i-1] >= '0' && ref[i-1] <= '9' {
		i--
	}
	if i == len(ref) {
		return 0, false
	}
	v, err := strconv.ParseInt(ref[i:], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
