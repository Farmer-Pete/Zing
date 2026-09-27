package runtime

import (
	"context"
	"os/exec"
	"testing"
)

// TestClaude_Version runs the real claude binary's --version output when
// it is on PATH, and skips otherwise: this is a smoke test for the local
// environment, not a correctness check on package 2's own code.
func TestClaude_Version(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}

	var c Claude
	out, err := c.version(context.Background())
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if out == "" {
		t.Error("version returned empty output")
	}
}

// TestCodex_Version is TestClaude_Version's counterpart for the codex
// binary.
func TestCodex_Version(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not on PATH")
	}

	var c Codex
	out, err := c.version(context.Background())
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if out == "" {
		t.Error("version returned empty output")
	}
}
