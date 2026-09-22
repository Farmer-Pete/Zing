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
	// Job is the job this run serves; the fake runtime keys its script on it.
	Job response.Job
	// Label is the task number or lens name, empty for a whole-job run. It is
	// part of the fake runtime's script key (design section 11, decision Q-c).
	Label string
	// Model is the exact model id from zing.toml.
	Model string
	// Prompt is the assembled prompt for this turn, inputs already fenced.
	Prompt string
	// Tools is the allowed tool list for the run.
	Tools []string
	// WorkDir is the working directory for the run.
	WorkDir string
	// Env is the scrubbed environment for the run.
	Env []string
	// Timeout bounds the run.
	Timeout time.Duration
	// MaxTurns caps the turns in one run (default 20).
	MaxTurns int
	// SessionID resumes a prior run's session; empty starts a new one.
	SessionID string
	// RunToken rides in the run's env so a later zing scenarios finds this
	// ticket's scenarios.
	RunToken string
}

// RunResult is what a Runtime returns for one turn (design section 6.9).
type RunResult struct {
	Response  response.Response
	SessionID string
	Log       string
	AgentTime time.Duration
	ExitCode  int
}
