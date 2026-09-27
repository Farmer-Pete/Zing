package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"zing/internal/runtime"
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
// 10).
func runJob(
	ctx context.Context, d Deps, t store.Ticket, jobName string,
	su store.SessionUpsert, req runtime.RunRequest,
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
	req.WorkDir = proj.LocalPath

	req.Tools = jobCfg.Tools
	req.Timeout = time.Duration(jobCfg.TimeoutMinutes) * time.Minute
	runCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	rsv, err := d.Reserve(runCtx, t.ID, su, req.Model)
	if err != nil {
		return runResult{}, fmt.Errorf("job: %s: reserve: %w", jobName, err)
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
