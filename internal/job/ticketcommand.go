package job

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"zing/internal/machine"
	"zing/internal/store"
)

// TicketCommands runs one owner-typed shell command in a ticket's
// existing worktree exactly as CHECK runs its lint and test commands: the
// same CommandRunner, worktree, RepoGit, timeout, and 16 KiB output tail
// (runCheckCommand). Serve builds it with the same Commands value it hands
// the dispatcher.
type TicketCommands struct {
	Store    *store.Store
	Machine  *machine.Machine
	Projects map[int64]Project
	Commands CommandRunner
}

// ErrNoTicket and ErrNoProject are TicketCommands.Run's own lookup
// failures: no ticket with that id, or a ticket whose project is not in
// Projects.
var (
	ErrNoTicket  = errors.New("job: no such ticket")
	ErrNoProject = errors.New("job: ticket's project is not configured")
)

// TicketCommandResult is one TicketCommands.Run outcome.
type TicketCommandResult struct {
	Exit     int    // the command's exit code; -1 when TimedOut or when a signal killed it
	TimedOut bool   // true only when the runner returned ErrCommandTimeout
	Output   string // tailBuffer.Tail(): stdout and stderr interleaved, last 16384 bytes at most
	Total    int64  // every byte the command wrote, kept or not
	Cut      bool   // Total > 16384
}

// Run runs shellCmd in ticketID's existing worktree, through tc.Commands,
// with buildTimeout(tc.Machine) as the timeout and checkOutputCap as the
// output cap -- the same values runCheckCommand gives a CHECK command. It
// never creates, repairs, or reattaches a worktree: a ticket with none
// returns an error wrapping orchestrator.ErrNoWorktree. Each run logs one
// INFO line naming the ticket id and the exit, never shellCmd or the
// output.
func (tc TicketCommands) Run(ctx context.Context, ticketID int64, shellCmd string) (TicketCommandResult, error) {
	t, err := tc.Store.GetTicket(ctx, ticketID)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketCommandResult{}, fmt.Errorf("job: ticket command: ticket %d: %w", ticketID, ErrNoTicket)
	}
	if err != nil {
		return TicketCommandResult{}, fmt.Errorf("job: ticket command: %w", err)
	}
	proj, ok := tc.Projects[t.ProjectID]
	if !ok {
		return TicketCommandResult{}, fmt.Errorf("job: ticket command: project %d: %w", t.ProjectID, ErrNoProject)
	}
	wt, err := proj.Orch.ExistingWorktree(ctx, ticketID)
	if err != nil {
		return TicketCommandResult{}, fmt.Errorf("job: ticket command: %w", err)
	}
	out := newTailBuffer(checkOutputCap)
	started := time.Now()
	exit, runErr := tc.Commands.Run(ctx, wt.Dir(), proj.RepoGit, shellCmd, buildTimeout(tc.Machine), CommandIO{Out: out})
	r := TicketCommandResult{Exit: exit}
	switch {
	case runErr == nil:
	case errors.Is(runErr, ErrCommandTimeout):
		r.Exit, r.TimedOut = -1, true
	default:
		slog.Info("sandbox command not run", "ticket_id", ticketID, "error", runErr)
		return TicketCommandResult{}, runErr
	}
	r.Output, r.Total = out.Tail(), out.Total()
	r.Cut = r.Total > checkOutputCap
	slog.Info("sandbox command run", "ticket_id", ticketID, "exit_code", r.Exit, "timed_out", r.TimedOut,
		"seconds", int(time.Since(started).Seconds()), "output_bytes", r.Total)
	return r, nil
}
