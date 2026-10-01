package job

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// runResult is what runJob hands back to its caller (design section 4.6):
// the runtime's raw result, the run Reserve fixed this call's turn against,
// and when the call started, so a caller building a terminalizing commit
// never needs a second read to learn what it just ran.
type runResult struct {
	Res      runtime.RunResult
	Reserved store.Reserved
	Started  time.Time
}

// runJob is the one seam every runtime call passes through (design section
// 4.6): resolve the job and its runtime (in that order -- the runtime is
// resolved before a run is ever reserved, so an unknown runtime name spends
// no reservation), apply the model alias, check the ticket's spent
// agent-time budget, fill the run's workdir from the ticket's project and
// its tools and timeout from the job, reserve a run under the caller's claim
// (fixing this call's turn atomically), and finally call the resolved
// runtime under a context bounded by the job's own timeout. Every step
// before the reserve can fail with no side effect at all: nothing is
// written, and the caller commits no run. store.ErrClaimLost from step 7
// passes through wrapped but still satisfies errors.Is. Once Reserve has
// returned a run id, the call is logged ("runJob end") with that id
// regardless of how rt.Run comes back, so a caller can always terminalize
// it; the prompt and the raw output are never logged (design section 9,
// 10). taskN becomes RunSeed.TaskN on the reserved run: &n for a build or
// perimeter task unit, nil for every other job, including a fix unit.
// lens becomes RunSeed.Lens: the lens name for a review lens run (ROUND and
// CONTINUE, PKG9-PLAN.md section 6.2, 6.2a), nil for every other job.
// runAndRoute's own callers -- planning's four and building's own -- pass
// nil for both.
func runJob(
	ctx context.Context, d Deps, t store.Ticket, jobName string,
	su store.SessionUpsert, req runtime.RunRequest, taskN *int, lens *string,
) (runResult, error) {
	jobCfg, ok := d.Machine.Jobs[jobName]
	if !ok {
		return runResult{}, fmt.Errorf("%w: unknown job %q", ErrConfig, jobName)
	}

	rt, err := d.Runtimes.For(jobCfg.Runtime)
	if err != nil {
		return runResult{}, fmt.Errorf("%w: job %s: resolve runtime %q: %w", ErrConfig, jobName, jobCfg.Runtime, err)
	}

	model, ok := d.Models[jobCfg.Model]
	if !ok {
		return runResult{}, fmt.Errorf("%w: job %s: no model alias %q", ErrConfig, jobName, jobCfg.Model)
	}
	req.Model = model

	agentSeconds, err := d.Store.AgentSecondsForTicket(ctx, t.ID)
	if err != nil {
		return runResult{}, fmt.Errorf("job: %s: agent seconds for ticket %d: %w", jobName, t.ID, err)
	}
	capSeconds := int64(d.Budget / time.Second)
	if time.Duration(agentSeconds)*time.Second >= d.Budget {
		slog.Warn("budget refused a call", "ticket_id", t.ID, "job", jobName, "agent_seconds", agentSeconds, "cap_seconds", capSeconds)
		return runResult{}, ErrBudget
	}

	proj, err := d.Store.ProjectForTicket(ctx, t.ID)
	if err != nil {
		return runResult{}, fmt.Errorf("job: %s: project for ticket %d: %w", jobName, t.ID, err)
	}
	// Building passes the worktree directory in req.WorkDir already; every
	// other caller leaves it empty, and falls back to the ticket's project
	// checkout (design section 5.5).
	if req.WorkDir == "" {
		req.WorkDir = proj.LocalPath
	}

	req.Tools = jobCfg.Tools
	req.Timeout = time.Duration(jobCfg.TimeoutMinutes) * time.Minute

	// rsv is declared here, ahead of the sandbox/temp-root step's own defer,
	// so a private temp root's cleanup closure (below) can log the run id
	// Reserve fixes further down: a deferred closure sees a captured
	// variable's value as of when it runs, not when it was deferred, as
	// long as the variable is already in scope (design section 7.3: a
	// cleanup error is logged at WARN with ticket_id and run_id).
	var rsv store.Reserved

	if jobCfg.Sandbox != "" {
		sb, ok := d.Sandboxes.For(jobCfg.Sandbox)
		if !ok {
			return runResult{}, fmt.Errorf("%w: job %s: unknown sandbox profile %q", ErrConfig, jobName, jobCfg.Sandbox)
		}
		cleanup, sandboxErr := applySandbox(sb, d, t, jobCfg.Sandbox, req.WorkDir, &req)
		// Deferred unconditionally, even on a returned error: applySandbox
		// never returns a nil cleanup (noopCleanup stands in when there is
		// nothing to remove). Runs after rt.Run has returned, below: the run
		// directory must stay in place for the whole life of the sandboxed
		// process (design section 5.5).
		defer cleanup()
		if sandboxErr != nil {
			return runResult{}, sandboxErr
		}
	} else {
		// A job naming no sandbox (classify, planning, planreview) still
		// gets a private temp root under DATA_DIR (PKG9-PLAN.md section
		// 7.3, D27): build and readonly both deny DATA_DIR whole, so a
		// planning run's own temp files -- which can quote the sealed
		// scenarios -- are unreadable to a sandboxed run that could
		// otherwise see the host's shared TMPDIR.
		cleanup, tmpErr := applyPrivateTempRoot(d, &req)
		defer func() {
			if rmErr := cleanup(); rmErr != nil {
				slog.Warn("run cleanup failed", "ticket_id", t.ID, "run_id", rsv.RunID, "error", rmErr)
			}
		}()
		if tmpErr != nil {
			return runResult{}, tmpErr
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	var reserveErr error
	rsv, reserveErr = d.Reserve(runCtx, t.ID, su, store.RunSeed{Model: req.Model, TaskN: taskN, Lens: lens})
	if reserveErr != nil {
		return runResult{}, fmt.Errorf("job: %s: reserve: %w", jobName, reserveErr)
	}
	req.RunToken = strconv.FormatInt(rsv.RunID, 10)

	started := time.Now()
	res, runErr := rt.Run(runCtx, req)

	slog.Info("runJob end",
		"ticket_id", t.ID,
		"session_id", rsv.SessionID,
		"run_id", rsv.RunID,
		"job", jobName,
		"model", req.Model,
		"runtime", jobCfg.Runtime,
		"exit_code", res.ExitCode,
		"agent_seconds", runtime.Seconds(res.AgentTime),
		"err_kind", errKind(runErr),
		"stderr_len", res.StderrLen,
		"stderr_sha256", res.StderrSHA256,
	)

	return runResult{Res: res, Reserved: rsv, Started: started}, runErr
}

// seedTaskN is store.RunSeed.TaskN's own value for a build or perimeter
// unit run (review F003, F005): nil for a fix unit (task 0), so a fix's own
// runs never carry the task_n a real task 0 would otherwise be confused
// with; a pointer to taskN for every other unit.
func seedTaskN(taskN int) *int {
	if taskN == 0 {
		return nil
	}
	n := taskN
	return &n
}

// buildLabel is runtime.RunRequest.Label for a build run of taskN (design
// section 6.3's own table: "the decimal task number, or fix for a fix
// unit"): fixRunLabel for a fix unit (task 0), the decimal task number
// otherwise.
func buildLabel(taskN int) string {
	if taskN == 0 {
		return fixRunLabel
	}
	return strconv.Itoa(taskN)
}

// perimeterLabel is runtime.RunRequest.Label for a DESCRIBE run: the same
// fix-versus-task split as buildLabel, with i (the extra's own 1-based
// position among the unit's undescribed extras) appended: "fix-<i>" for a
// fix unit, "<n>-<i>" otherwise.
func perimeterLabel(taskN, i int) string {
	if taskN == 0 {
		return fmt.Sprintf("fix-%d", i)
	}
	return fmt.Sprintf("%d-%d", taskN, i)
}

// fixRunLabel is runtime.RunRequest's own Label for every fix run (design
// section 6.3's own table: "the decimal task number, or fix for a fix
// unit"), first turn and every resume alike (fix.go's own StartFix).
const fixRunLabel = "fix"

// noopCleanup is applySandbox's own "nothing to clean up" return: a real
// closure rather than a nil func, so its (cleanup, error) result is never
// the (nil, nil) shape (nilnil), and runJob can defer it unconditionally.
func noopCleanup() {}

// sandboxProfileJudge is the one machine.toml job.sandbox value that takes
// Deps.JudgeCodexHome (PKG9-PLAN.md section 4.3, 4.7, 7.3, D27):
// machine.go's own validateJob already refuses any job.sandbox value but
// "", "build", "readonly", or "judge", so this is the one of those four
// applySandbox ever treats specially.
const sandboxProfileJudge = "judge"

// applySandbox is design section 5.5's sandbox step, run after WorkDir,
// Tools, and Timeout are filled and before Reserve, for any job whose
// machine.toml entry names a sandbox (profileName, jobCfg.Sandbox). With
// the sandbox available, it reserves a fresh run directory, builds req's
// ExecPrefix and appends its Env, all from d.Projects[t.ProjectID]'s own
// RepoGit, and returns the run directory's cleanup for the caller to defer
// (nothing is reserved yet, so runJob's own defer chain, not this
// function, decides when it runs). It also overwrites req.WorkDir with
// the sandbox's own resolved worktree (ParamsFor's Worktree), so the CLI
// runs with its working directory equal to the path the profile's
// WORKTREE rule and TRANSCRIPTS folder actually name, not a path that
// reaches the same directory through a symlink (task 16a: a worktree
// under macOS's own /var -> /private/var symlink otherwise fails every
// write with EPERM, since seatbelt matches subpath against the resolved
// path). For profileName == "judge" (PKG9-PLAN.md section 7.3, D27), it
// also fills Params.CodexHome from d.JudgeCodexHome, itself ErrConfig
// ("job: judge codex home is not configured") when empty, checked before
// Prefix's own deeper, profile-agnostic "both or neither" rule
// (sandbox.errJudgeParamsIncomplete) so a misconfigured zing.toml is
// reported with this clearer text. With the sandbox unavailable, it
// returns ErrSandbox when d.RequireSandbox, or noopCleanup and no error
// for a suite on the fake runtime. Every failure short of an
// unavailable-and-required sandbox is a configuration error (design
// section 5.5): a job named a sandbox but this process has no Project row
// for the ticket, or the sandbox's own run-dir, param, or prefix calls
// failed.
func applySandbox(sb sandbox.Sandbox, d Deps, t store.Ticket, profileName, workDir string, req *runtime.RunRequest) (cleanup func(), err error) {
	if !sb.Available() {
		if d.RequireSandbox {
			return noopCleanup, ErrSandbox
		}
		return noopCleanup, nil
	}

	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return noopCleanup, fmt.Errorf("%w: no sandbox project for ticket %d (project %d)", ErrConfig, t.ID, t.ProjectID)
	}

	runDir, cleanup, err := sb.NewRunDir()
	if err != nil {
		return noopCleanup, fmt.Errorf("%w: sandbox run dir: %v", ErrConfig, err) //nolint:errorlint // ErrConfig is the sentinel this wraps; err's own type carries nothing a caller matches on
	}

	p, err := sb.ParamsFor(workDir, proj.RepoGit, runDir)
	if err != nil {
		cleanup()
		return noopCleanup, fmt.Errorf("%w: sandbox params: %v", ErrConfig, err) //nolint:errorlint // see above
	}

	if profileName == sandboxProfileJudge {
		if d.JudgeCodexHome == "" {
			cleanup()
			return noopCleanup, fmt.Errorf("%w: judge codex home is not configured", ErrConfig)
		}
		p.CodexHome = d.JudgeCodexHome
		// Codex's own TLS stack needs com.apple.SecurityServer unless told
		// to use rustls and a CA bundle instead; D26 denies that Mach
		// service, so CODEX_CA_CERTIFICATE must point codex at one (M1 task
		// 7's host probes: without it, Codex under the judge profile fails
		// TLS with "SecurityServer" denied).
		req.Env = append(req.Env, "CODEX_CA_CERTIFICATE=/etc/ssl/cert.pem")
	}

	prefix, err := sb.Prefix(p)
	if err != nil {
		cleanup()
		return noopCleanup, fmt.Errorf("%w: sandbox prefix: %v", ErrConfig, err) //nolint:errorlint // see above
	}
	req.ExecPrefix = prefix
	req.Env = append(req.Env, sb.Env(p, os.Getenv("PATH"))...)
	req.WorkDir = p.Worktree
	return cleanup, nil
}

// privateTempRootIDBytes is the number of random bytes newPrivateTempRoot
// reads to build a run id: 8 bytes hex-encode to 16 lowercase hex
// characters, matching sandbox.NewRunDir's own run-id shape.
const privateTempRootIDBytes = 8

// noopCleanupErr is applyPrivateTempRoot's own "nothing to clean up"
// return, the func() error twin of noopCleanup (applyPrivateTempRoot's own
// cleanup can fail to remove what it created, unlike applySandbox's, so its
// signature carries an error).
func noopCleanupErr() error { return nil }

// newPrivateTempRoot creates <dataDir>/tmp/run/<16 hex>/, with "tmp" and
// "claude-tmp" inside, all mode 0700 (PKG9-PLAN.md section 7.3): the
// private temp root an unsandboxed run's TMPDIR and CLAUDE_CODE_TMPDIR
// point at, mirroring sandbox.Sandbox.NewRunDir's own shape so both kinds
// of run carry the same two subfolders. cleanup removes the whole
// directory and is safe to call twice.
func newPrivateTempRoot(dataDir string) (dir string, cleanup func() error, err error) {
	var b [privateTempRootIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, fmt.Errorf("job: generate private temp root id: %w", err)
	}
	id := hex.EncodeToString(b[:])

	dir = filepath.Join(dataDir, "tmp", "run", id)
	for _, sub := range []string{"", "tmp", "claude-tmp"} {
		p := filepath.Join(dir, sub)
		if err := os.MkdirAll(p, 0o700); err != nil {
			return "", nil, fmt.Errorf("job: create private temp root %s: %w", p, err)
		}
	}
	cleanup = func() error { return os.RemoveAll(dir) }
	return dir, cleanup, nil
}

// applyPrivateTempRoot is design section 7.3's own step for a job naming no
// sandbox: it requires d.DataDir (ErrConfig when empty, before any
// reserve), creates a fresh private temp root under it, and appends TMPDIR
// and CLAUDE_CODE_TMPDIR pointing at its two subfolders to req.Env, after
// the allowlisted parent TMPDIR agentEnv already carries, so these win
// (os/exec keeps the last value for a duplicate name). The returned cleanup
// removes the root; the caller defers it so it runs once rt.Run has
// returned, logging any removal failure at WARN rather than losing it
// (design section 7.3).
func applyPrivateTempRoot(d Deps, req *runtime.RunRequest) (cleanup func() error, err error) {
	if d.DataDir == "" {
		return noopCleanupErr, fmt.Errorf("%w: data directory is not configured", ErrConfig)
	}
	dir, cleanup, err := newPrivateTempRoot(d.DataDir)
	if err != nil {
		return noopCleanupErr, fmt.Errorf("%w: private temp root: %v", ErrConfig, err) //nolint:errorlint // ErrConfig is the sentinel this wraps; err's own type carries nothing a caller matches on
	}
	req.Env = append(req.Env, "TMPDIR="+filepath.Join(dir, "tmp"), "CLAUDE_CODE_TMPDIR="+filepath.Join(dir, "claude-tmp"))
	return cleanup, nil
}

// errKind renders err for the "runJob end" observability event only (design
// section 9): never the error text itself, just which of the runtime's
// typed failures this run ended with, "" for a nil err.
func errKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, runtime.ErrStart):
		return "ErrStart"
	case errors.Is(err, runtime.ErrTimeout):
		return "ErrTimeout"
	case errors.Is(err, runtime.ErrCanceled):
		return "ErrCanceled"
	case errors.Is(err, runtime.ErrOutputTooLarge):
		return "ErrOutputTooLarge"
	}
	// errors.As, not the modernize-suggested errors.AsType: AsType's (E, bool)
	// result has E discarded via _, and errcheck's check-blank (this repo's
	// config) flags that discard since E is itself error-shaped.
	var execErr *runtime.ExecError
	if errors.As(err, &execErr) { //nolint:modernize // see comment above
		return "ExecError"
	}
	var invalidErr *runtime.InvalidOutputError
	if errors.As(err, &invalidErr) { //nolint:modernize // see comment above
		return "InvalidOutputError"
	}
	return "other"
}
