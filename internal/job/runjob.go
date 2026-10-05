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

	"zing/internal/proc"
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
// nil for both. throughBatch becomes store.RunSeed.ThroughBatch (D31,
// design section 22.3): the largest owner-message batch this reserved run
// carries, above 0 only for a planning resume or first turn that found
// PlanningConversation.Undelivered() non-empty; every other caller passes 0.
func runJob(
	ctx context.Context, d Deps, t store.Ticket, jobName string,
	su store.SessionUpsert, req runtime.RunRequest, taskN *int, lens *string, throughBatch int64,
) (runResult, error) {
	return runJobWith(ctx, d, t, jobName, su, req, taskN, lens, throughBatch, nil)
}

// afterReserve runs after Reserve and before rt.Run (PKG9-PLAN.md section
// 7.3, D19): it may add to req and returns the path the judge profile's
// SCENARIOS_FILE parameter takes (or "" for a job that needs none) and a
// cleanup runJobWith defers so it runs after rt.Run returns, ahead of the
// sandbox run directory's own cleanup (LIFO: the hook's own cleanup first,
// then the run directory it was written alongside). A cleanup error is
// logged at WARN as "run cleanup failed" with ticket_id and run_id; it
// never replaces the run's own result. hook runs only for a sandboxed job
// (one whose sandbox profile is available); runJobWith never calls it for
// an unsandboxed job.
type afterReserve func(ctx context.Context, rsv store.Reserved, req *runtime.RunRequest) (scenariosFile string, cleanup func() error, err error)

// budgetExhausted is the comparison runJobWith's refusal check and
// retryCapBudget's retry check both make, kept in one place so the two
// cannot drift (design section 6.7).
func budgetExhausted(agentSeconds int64, budget time.Duration) (exhausted bool, capSeconds int64) {
	capSeconds = int64(budget / time.Second)
	exhausted = time.Duration(agentSeconds)*time.Second >= budget
	return
}

// runJobWith is runJob with an afterReserve hook (PKG9-PLAN.md section
// 7.3): runJob itself calls this with a nil hook, which reproduces its
// exact former behavior byte for byte. For a job whose profile is
// sandboxed, the sandbox's availability is still checked before Reserve (as
// a hookless run always has), but with a non-nil hook, the sandbox's
// command prefix is built only after Reserve and the hook both return,
// because the prefix may need to name a file the hook just wrote under the
// run's own id (judging.go's writeScenariosFile, M2 task 7). A hook error
// surfaces after Reserve has already fixed this call's turn, so runJobWith
// still returns the reserved run (Reserved.RunID set) alongside it, the
// same shape a runtime failure returns, letting a caller like runAndRoute
// terminalize the run (postRunFailure) rather than orphaning it.
func runJobWith(
	ctx context.Context, d Deps, t store.Ticket, jobName string,
	su store.SessionUpsert, req runtime.RunRequest, taskN *int, lens *string, throughBatch int64,
	hook afterReserve,
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
	exhausted, capSeconds := budgetExhausted(agentSeconds, d.Budget)
	if exhausted {
		slog.Warn("budget refused a call", "ticket_id", t.ID, "job", jobName, "agent_seconds", agentSeconds, "cap_seconds", capSeconds)
		return runResult{}, ErrBudget
	}

	storeProj, err := d.Store.ProjectForTicket(ctx, t.ID)
	if err != nil {
		return runResult{}, fmt.Errorf("job: %s: project for ticket %d: %w", jobName, t.ID, err)
	}
	// Building passes the worktree directory in req.WorkDir already; every
	// other caller leaves it empty, and falls back to the ticket's project
	// checkout (design section 5.5).
	if req.WorkDir == "" {
		req.WorkDir = storeProj.LocalPath
	}

	req.Tools = jobCfg.Tools
	req.Timeout = time.Duration(jobCfg.TimeoutMinutes) * time.Minute

	// rsv is declared here, ahead of the sandbox/temp-root step's own defer,
	// so a cleanup closure (below) can log the run id Reserve fixes further
	// down: a deferred closure sees a captured variable's value as of when
	// it runs, not when it was deferred, as long as the variable is already
	// in scope (design section 7.3: a cleanup error is logged at WARN with
	// ticket_id and run_id).
	var rsv store.Reserved

	// sb and params are filled by the sandboxed branch below and read again,
	// after Reserve, only when a hook defers the sandbox prefix (the judge
	// profile, PKG9-PLAN.md section 7.3).
	var sb sandbox.Sandbox
	var params sandbox.Params
	sandboxed := false

	if jobCfg.Sandbox != "" {
		var sandboxOK bool
		sb, sandboxOK = d.Sandboxes.For(jobCfg.Sandbox)
		if !sandboxOK {
			return runResult{}, fmt.Errorf("%w: job %s: unknown sandbox profile %q", ErrConfig, jobName, jobCfg.Sandbox)
		}
		switch {
		case sb.Available():
			sandboxed = true
			jobProj, projOK := d.Projects[t.ProjectID]
			if !projOK {
				return runResult{}, fmt.Errorf("%w: no sandbox project for ticket %d (project %d)", ErrConfig, t.ID, t.ProjectID)
			}

			runDir, runDirCleanup, dirErr := sb.NewRunDir()
			if dirErr != nil {
				return runResult{}, fmt.Errorf("%w: sandbox run dir: %v", ErrConfig, dirErr) //nolint:errorlint // ErrConfig is the sentinel this wraps; err's own type carries nothing a caller matches on
			}
			// Deferred unconditionally, even on a later error: it must stay
			// in place for the whole life of the sandboxed process (design
			// section 5.5), and os.RemoveAll is safe to call more than once.
			defer runDirCleanup()

			params, err = sb.ParamsFor(req.WorkDir, jobProj.RepoGit, runDir)
			if err != nil {
				return runResult{}, fmt.Errorf("%w: sandbox params: %v", ErrConfig, err) //nolint:errorlint // see above
			}

			if jobCfg.Sandbox == sandboxProfileJudge {
				if d.JudgeCodexHome == "" {
					return runResult{}, fmt.Errorf("%w: judge codex home is not configured", ErrConfig)
				}
				params.CodexHome = d.JudgeCodexHome
				// Codex's own TLS stack needs com.apple.SecurityServer
				// unless told to use rustls and a CA bundle instead; D26
				// denies that Mach service, so CODEX_CA_CERTIFICATE must
				// point codex at one (M1 task 7's host probes: without it,
				// Codex under the judge profile fails TLS with
				// "SecurityServer" denied).
				req.Env = append(req.Env, "CODEX_CA_CERTIFICATE=/etc/ssl/cert.pem")
			}

			// With no hook, the prefix is built now, exactly as every
			// sandboxed job has always had it: Reserve below only fixes
			// the run id, which nothing here needs yet.
			if hook == nil {
				if finishErr := finishSandboxRequest(sb, params, &req); finishErr != nil {
					return runResult{}, finishErr
				}
			}
		case d.RequireSandbox:
			return runResult{}, ErrSandbox
		}
		// Unavailable and not required: the fake-runtime-suite case (design
		// D5). The job runs unwrapped, exactly like today, with no private
		// temp root either (a job that names a sandbox profile never falls
		// back to one, whether or not that profile loaded).
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
	rsv, reserveErr = d.Reserve(runCtx, t.ID, su, store.RunSeed{Model: req.Model, TaskN: taskN, Lens: lens, ThroughBatch: throughBatch})
	if reserveErr != nil {
		return runResult{}, fmt.Errorf("job: %s: reserve: %w", jobName, reserveErr)
	}
	req.RunToken = strconv.FormatInt(rsv.RunID, 10)
	req.OnStart = func(info runtime.StartInfo) {
		recordRunStart(ctx, d, t.ID, rsv.RunID, info)
	}

	if sandboxed && hook != nil {
		scenariosFile, hookCleanup, hookErr := hook(runCtx, rsv, &req)
		if hookCleanup != nil {
			// Deferred ahead of rt.Run below, so it runs after rt.Run
			// returns but before the run directory's own cleanup above
			// (LIFO): the scenarios file lives inside the run's own
			// process tree reach, not the run directory, so the order
			// between the two never matters in practice, but this is the
			// one that matches 7.3's own step-by-step removal story.
			defer func() {
				if rmErr := hookCleanup(); rmErr != nil {
					slog.Warn("run cleanup failed", "ticket_id", t.ID, "run_id", rsv.RunID, "error", rmErr)
				}
			}()
		}
		if hookErr != nil {
			return runResult{Reserved: rsv}, hookErr
		}
		params.ScenariosFile = scenariosFile
		if finishErr := finishSandboxRequest(sb, params, &req); finishErr != nil {
			return runResult{Reserved: rsv}, finishErr
		}
	}

	started := time.Now()
	res, runErr := rt.Run(runCtx, req)
	res, runErr = retryTransient(runCtx, rt, req, t.ID, rsv.RunID, jobName, res, runErr)

	stderrFile := ""
	if len(res.Stderr) > 0 && d.DataDir != "" {
		path, writeErr := writeStderrFile(d.DataDir, rsv.RunID, res.Stderr)
		if writeErr != nil {
			slog.Warn("stderr file not written", "ticket_id", t.ID, "run_id", rsv.RunID, "error", writeErr)
		}
		stderrFile = path
	}

	// Codex never sets TranscriptPath itself (unlike Claude, whose own
	// runtime does): its stdout, the codex exec --json event stream, is
	// where an early error that kills the run within seconds is actually
	// reported, so it is the transcript worth keeping (design: "Codex runs
	// that exit 1 within seconds leave no stderr, transcript, or cause").
	keepStdoutTranscript := len(res.Stdout) > 0 && d.DataDir != "" && res.TranscriptPath == ""
	if keepStdoutTranscript {
		path, writeErr := writeTranscriptFile(d.DataDir, rsv.RunID, res.Stdout)
		if writeErr != nil {
			slog.Warn("transcript file not written", "ticket_id", t.ID, "run_id", rsv.RunID, "error", writeErr)
		}
		res.TranscriptPath = path
	}

	recordRunEvidence(ctx, d, t.ID, rsv.RunID, res, stderrFile)

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
		"stderr_file", stderrFile,
		"final_message_len", len(res.FinalMessage),
		"transcript_path", res.TranscriptPath,
		"validate_denied", res.ValidateDenied,
		"stop_hook_events", res.StopHookEvents,
		"stop_hook_blocks", res.StopHookBlocks,
		"stop_hook_unread", res.StopHookUnread,
	)

	return runResult{Res: res, Reserved: rsv, Started: started}, runErr
}

// transientRetryDelay is retryTransient's own wait before its one retry
// (design goals: "retried once ... after 2 s", nongoals: "No ... backoff
// beyond the single 2 s wait").
const transientRetryDelay = 2 * time.Second

// maxTranscriptBytes caps the stdout retryTransient joins from both
// attempts before runJobWith ever writes it to disk (design goals: "After a
// retry, the file holds the last 64 KiB of both attempts' stdout combined,
// so it never passes 65536 bytes"), the same bound runtime.Codex itself
// applies to one attempt's own tailWriter.
const maxTranscriptBytes = 64 << 10

// retryTransient is runJobWith's own single automatic retry (design goals:
// a Codex ExecError whose Transient names a matched pattern -- 429, rate
// limit, 500, 502, 503, 504, connection reset, or stream disconnected -- is
// retried once, on the same reserved run, after a 2s wait; every other
// result or error passes through res and runErr unchanged. When runCtx ends
// during the wait, there is no second attempt: it keeps the first result
// (stdout, FailureDetail, AgentTime), sets ExitCode to -1, and returns
// runtime.ErrTimeout for the job deadline or runtime.ErrCanceled for a
// parent cancel, so routeFailure handles the retry exactly as it would an
// un-retried run that hit the same wall. When the wait completes, it calls
// rt.Run once more on the same req, adds the first attempt's AgentTime to
// the retry's, and joins both attempts' stdout, kept to the last
// maxTranscriptBytes (capTranscriptTail) so the eventual transcript file
// never passes that bound. A retry that still fails rewrites FailureDetail
// into a note naming the matched pattern and quoting the retry's own
// detail, so execFailureCommit's Tried (planning.go) tells the owner a
// retry was already tried, not just that the run failed twice silently.
func retryTransient(
	ctx context.Context, rt runtime.Runtime, req runtime.RunRequest,
	ticketID, runID int64, jobName string, res runtime.RunResult, runErr error,
) (runtime.RunResult, error) {
	var execErr *runtime.ExecError
	// errors.As, not the modernize-suggested errors.AsType: errcheck's
	// check-blank (this repo's config) flags AsType's (E, bool) result
	// since its discarded E is itself error-shaped, same as errKind's own
	// comment.
	if !errors.As(runErr, &execErr) || execErr.Transient == "" { //nolint:modernize // see comment above
		return res, runErr
	}

	slog.Info("runtime transient retry", "ticket_id", ticketID, "run_id", runID, "job", jobName, "match", execErr.Transient)

	select {
	case <-ctx.Done():
		res.ExitCode = -1
		skipErr := runtime.ErrCanceled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			skipErr = runtime.ErrTimeout
		}
		slog.Warn("runtime transient retry skipped", "ticket_id", ticketID, "run_id", runID, "job", jobName, "match", execErr.Transient, "err_kind", errKind(skipErr))
		return res, skipErr
	case <-time.After(transientRetryDelay):
	}

	firstAgentTime, firstStdout, firstDetail := res.AgentTime, res.Stdout, res.FailureDetail
	retryRes, retryErr := rt.Run(ctx, req)
	retryRes.AgentTime += firstAgentTime
	joinedStdout := append(append([]byte(nil), firstStdout...), retryRes.Stdout...)
	if len(joinedStdout) > maxTranscriptBytes {
		joinedStdout = joinedStdout[len(joinedStdout)-maxTranscriptBytes:]
	}
	retryRes.Stdout = joinedStdout

	if retryErr == nil {
		slog.Info("runtime transient retry succeeded", "ticket_id", ticketID, "run_id", runID, "job", jobName, "match", execErr.Transient)
		return retryRes, nil
	}

	slog.Warn("runtime transient retry failed", "ticket_id", ticketID, "run_id", runID, "job", jobName, "match", execErr.Transient, "err_kind", errKind(retryErr), "exit_code", retryRes.ExitCode)
	// retryRes.FailureDetail is "" whenever the retry's own failure is not a
	// Codex ExecError with no final message (an ErrTimeout from the job
	// deadline, ErrStart, ErrOutputTooLarge, or a -o file with content):
	// quoting retryErr.Error() and the first attempt's own detail there
	// keeps execFailureCommit's Tried from ending in "the retry failed
	// with: " with nothing after the colon.
	quoted := retryRes.FailureDetail
	if quoted == "" {
		quoted = fmt.Sprintf("(first attempt: %s); the retry failed with: %s", firstDetail, retryErr.Error())
	} else {
		quoted = "the retry failed with: " + quoted
	}
	retryRes.FailureDetail = runtime.CapFailureDetail(fmt.Sprintf("retried once after a transient failure matching %q; %s", execErr.Transient, quoted))
	return retryRes, retryErr
}

// writeRunsFile saves data to <dataDir>/runs/name, mode 0600 in a 0700
// directory, and returns the path. writeStderrFile and writeTranscriptFile
// both hold this one body: the directory and file modes, and the "never
// retained past this write" shape, are the same for either file.
func writeRunsFile(dataDir, name string, data []byte) (string, error) {
	dir := filepath.Join(dataDir, "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("runs file: %w", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("runs file: %w", err)
	}
	return path, nil
}

// writeStderrFile saves one run's captured stderr to
// <dataDir>/runs/run-<runID>-stderr.log. The log names the path, never the
// text, since stderr can echo anything the child saw.
func writeStderrFile(dataDir string, runID int64, data []byte) (string, error) {
	return writeRunsFile(dataDir, store.StderrFileName(runID), data)
}

// writeTranscriptFile saves a Codex run's captured stdout to
// <dataDir>/runs/run-<runID>-stdout.jsonl, runJobWith's own transcript for
// a runtime that never sets RunResult.TranscriptPath itself. The log names
// the path, never the text, since stdout is Codex's raw event stream.
func writeTranscriptFile(dataDir string, runID int64, data []byte) (string, error) {
	return writeRunsFile(dataDir, store.StdoutFileName(runID), data)
}

// recordRunEvidence stores what runID left behind (#43 split): the final
// message, the stderr file path, and the transcript path, each NULL when
// empty. Like recordRunStart it detaches from ctx and bounds the write to
// 10s, and a failure is logged at WARN, never returned: losing evidence
// must not fail a run that otherwise finished. The log names the run, never
// the text it would have stored.
func recordRunEvidence(ctx context.Context, d Deps, ticketID, runID int64, res runtime.RunResult, stderrFile string) {
	var ev store.RunEvidence
	if res.FinalMessage != "" {
		ev.FinalMessage = &res.FinalMessage
	}
	if stderrFile != "" {
		ev.StderrPath = &stderrFile
	}
	if res.TranscriptPath != "" {
		ev.TranscriptPath = &res.TranscriptPath
	}
	evCtx, cancel := onStartContext(ctx)
	defer cancel()
	if err := d.Store.RecordRunEvidence(evCtx, runID, ev); err != nil {
		slog.Warn("run evidence not recorded", "ticket_id", ticketID, "run_id", runID, "error", err)
	}
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

// sandboxProfileJudge is the one machine.toml job.sandbox value that takes
// Deps.JudgeCodexHome (PKG9-PLAN.md section 4.3, 4.7, 7.3, D27):
// machine.go's own validateJob already refuses any job.sandbox value but
// "", "build", "readonly", or "judge", so this is the one of those four
// runJobWith ever treats specially.
const sandboxProfileJudge = "judge"

// finishSandboxRequest is design section 5.5's own last sandbox step: build
// the sandbox-exec prefix from p and append it to req, append the
// sandbox's own Env, and overwrite req.WorkDir with the sandbox's resolved
// worktree (ParamsFor's Worktree), so the CLI runs with its working
// directory equal to the path the profile's WORKTREE rule and TRANSCRIPTS
// folder actually name, not a path that reaches the same directory through
// a symlink (task 16a: a worktree under macOS's own /var -> /private/var
// symlink otherwise fails every write with EPERM, since seatbelt matches
// subpath against the resolved path). runJobWith calls this before Reserve
// for every sandboxed job but a hooked judge run (PKG9-PLAN.md section
// 7.3), where it waits until the hook has filled p.ScenariosFile from the
// run id Reserve just returned.
func finishSandboxRequest(sb sandbox.Sandbox, p sandbox.Params, req *runtime.RunRequest) error {
	prefix, err := sb.Prefix(p)
	if err != nil {
		return fmt.Errorf("%w: sandbox prefix: %v", ErrConfig, err) //nolint:errorlint // ErrConfig is the sentinel this wraps; err's own type carries nothing a caller matches on
	}
	req.ExecPrefix = prefix
	req.Env = append(req.Env, sb.Env(p, os.Getenv("PATH"))...)
	req.WorkDir = p.Worktree
	return nil
}

// privateTempRootIDBytes is the number of random bytes newPrivateTempRoot
// reads to build a run id: 8 bytes hex-encode to 16 lowercase hex
// characters, matching sandbox.NewRunDir's own run-id shape.
const privateTempRootIDBytes = 8

// noopCleanupErr is applyPrivateTempRoot's own "nothing to clean up"
// return: a real closure rather than a nil func, so its (cleanup, error)
// result is never the (nil, nil) shape (nilnil), and runJobWith can defer
// it unconditionally.
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

// onStartContext detaches from ctx -- the outer context runJobWith was
// called with, not runCtx, the job-timeout-bound child it derives -- so the
// store write recordRunStart makes below can still land even if the job's
// own context is canceled around the same moment (a shutdown racing the
// process's own start), and bounds it to 10s so a stuck write can never
// block the run (design section 7.1).
func onStartContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// recordRunStart is runJobWith's own OnStart closure body (design section
// 7.1, #45): it resolves info.PID's start token (skipped for the fake
// runtime's PID 0, which proc.StartToken cannot look up) and records the
// process identity through RecordRunStart, logging any failure at WARN
// rather than failing the run -- a run that reaches this point has already
// been reserved, and a start-identity write failing it would orphan that
// reservation for no benefit. This is a plan-accepted risk (design section
// 11's "RecordRunStart fails" row): the run's pgid stays NULL, so reclaim
// treats it as not live (section 6.3's own "PGID NULL: not live" rule) and
// reclaims its claim at once, rather than waiting on it as unverified.
func recordRunStart(ctx context.Context, d Deps, ticketID, runID int64, info runtime.StartInfo) {
	var token string
	if info.PID > 0 {
		var tokErr error
		token, tokErr = proc.StartToken(info.PID)
		if tokErr != nil {
			slog.Warn("start token unavailable", "ticket_id", ticketID, "run_id", runID, "pid", info.PID, "error", tokErr)
			token = ""
		}
	}

	startCtx, cancel := onStartContext(ctx)
	defer cancel()
	if err := d.Store.RecordRunStart(startCtx, runID, info.PID, token, time.Now(), info.SessionID); err != nil {
		slog.Warn("record run start failed", "ticket_id", ticketID, "run_id", runID, "error", err)
	}
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
