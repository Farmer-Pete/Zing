package runtime

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

var _ Runtime = Claude{}

// Claude runs a job through the claude CLI. Its real run lands in a later
// package; here it is a stub (design section 6.9).
type Claude struct{}

// Run always returns "not implemented in package 2".
func (Claude) Run(_ context.Context, _ RunRequest) (RunResult, error) {
	return RunResult{}, errors.New("not implemented in package 2")
}

// version runs `claude --version` and returns its trimmed output, for the
// smoke test that skips when the binary is not on PATH.
func (Claude) version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "claude", "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
