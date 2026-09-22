// Package runtime runs one Zing job turn against a model and returns its
// parsed response. Package 2 ships one working implementation, the
// scripted Fake (fake.go), and two stubs, Claude and Codex (claude.go,
// codex.go), whose real runs land in a later package.
package runtime

import (
	"context"
	"time"

	"zing/internal/response"
)

// Runtime runs one turn of a job and returns its parsed response.
type Runtime interface {
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// RunRequest is what a caller asks a Runtime to run (design section 11).
type RunRequest struct {
	// Job is the job to run.
	Job response.Job
	// Label distinguishes a split ticket's sibling runs sharing one
	// parent ticket; empty for a ticket that was never split (design
	// section 11, decision Q-c).
	Label string
	// SessionID resumes a prior run's session; empty starts a new one.
	SessionID string
	// Prompt is the rendered prompt text for this turn.
	Prompt string
}

// RunResult is what a Runtime returns for one turn (design section 6.9).
type RunResult struct {
	Response  response.Response
	SessionID string
	Log       string
	AgentTime time.Duration
	ExitCode  int
}
