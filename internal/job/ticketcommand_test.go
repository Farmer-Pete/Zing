package job_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	zing "zing"
	"zing/internal/gitfixture"
	"zing/internal/job"
	"zing/internal/orchestrator"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// newTicketCommandFixture builds a store with one queued ticket on a real,
// signed git repository (gitfixture.NewSigningRepo), with its worktree
// already created (orchestrator.EnsureWorktree, never TicketCommands'
// job), and a job.TicketCommands wired to cmds, mirroring
// resume_e2e_test.go's own store/orchestrator/worktree setup.
func newTicketCommandFixture(t *testing.T, cmds job.CommandRunner) (s *store.Store, ticketID int64, repoDir string, tc job.TicketCommands) {
	t.Helper()
	s = newJobTestStore(t)
	repoDir = t.TempDir()
	if err := gitfixture.NewSigningRepo(t.Context(), repoDir); err != nil {
		t.Fatalf("gitfixture.NewSigningRepo: %v", err)
	}

	proj := testProject
	proj.LocalPath = repoDir
	projectID, err := s.EnsureProject(t.Context(), proj)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err = s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: testRefFake1, Title: testTicketTitle, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	orch, orchErr := orchestrator.New(
		orchestrator.Project{Owner: testFixtureGitHubOwner, Repo: testFixtureGitHubOwner, LocalPath: repoDir, DefaultBranch: testFixtureDefaultBranch},
		jobTestGitHub{}, orchestrator.NewRunner(), nil)
	if orchErr != nil {
		t.Fatalf("orchestrator.New: %v", orchErr)
	}
	repoGit, gitErr := orch.GitCommonDir(t.Context())
	if gitErr != nil {
		t.Fatalf("GitCommonDir: %v", gitErr)
	}
	if _, _, err := orch.EnsureWorktree(t.Context(), ticketID, "ticket command"); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	tc = job.TicketCommands{
		Store: s, Machine: testMachine(t),
		Projects: map[int64]job.Project{projectID: {Orch: orch, RepoGit: repoGit}},
		Commands: cmds,
	}
	return s, ticketID, repoDir, tc
}

// ticketCommandWorktreeDir is the worktree directory EnsureWorktree creates
// for ticketID under repoDir, resolved: pwd (through /bin/sh) reports the
// kernel's own physical cwd, which has every symlink -- including macOS's
// default TMPDIR -- already resolved.
func ticketCommandWorktreeDir(t *testing.T, repoDir string, ticketID int64) string {
	t.Helper()
	dir := filepath.Join(repoDir, ".zing", "wt", strconv.FormatInt(ticketID, 10))
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	return resolved
}

// TestTicketCommandRunsInWorktree proves Run resolves the ticket's project
// and existing worktree and runs the command there, through the real
// unsandboxed CommandRunner, and that its three lookup failures each wrap
// the error the plan names.
func TestTicketCommandRunsInWorktree(t *testing.T) {
	t.Parallel()
	cmds := job.NewCommandRunner(sandbox.Off(), false)
	s, ticketID, repoDir, tc := newTicketCommandFixture(t, cmds)
	wantDir := ticketCommandWorktreeDir(t, repoDir, ticketID)

	t.Run("pwd", func(t *testing.T) {
		t.Parallel()
		res, err := tc.Run(t.Context(), ticketID, "pwd")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Exit != 0 {
			t.Errorf("Exit = %d, want 0", res.Exit)
		}
		if res.Output != wantDir+"\n" {
			t.Errorf("Output = %q, want %q", res.Output, wantDir+"\n")
		}
	})

	t.Run("exit 3", func(t *testing.T) {
		t.Parallel()
		res, err := tc.Run(t.Context(), ticketID, "exit 3")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Exit != 3 {
			t.Errorf("Exit = %d, want 3", res.Exit)
		}
	})

	t.Run("unknown ticket", func(t *testing.T) {
		t.Parallel()
		_, err := tc.Run(t.Context(), ticketID+1_000_000, "true")
		if !errors.Is(err, job.ErrNoTicket) {
			t.Fatalf("err = %v, want errors.Is(err, job.ErrNoTicket)", err)
		}
	})

	t.Run("project not configured", func(t *testing.T) {
		t.Parallel()
		noProjectTC := tc
		noProjectTC.Projects = map[int64]job.Project{}
		_, err := noProjectTC.Run(t.Context(), ticketID, "true")
		if !errors.Is(err, job.ErrNoProject) {
			t.Fatalf("err = %v, want errors.Is(err, job.ErrNoProject)", err)
		}
	})

	t.Run("no worktree", func(t *testing.T) {
		t.Parallel()
		noWorktreeTicketID, err := s.InsertTicket(t.Context(), store.Ticket{
			ProjectID: projectIDFor(t, s, ticketID), TrackerRef: "fake#2", Title: testTicketTitle, State: testStateQueued,
		})
		if err != nil {
			t.Fatalf("InsertTicket: %v", err)
		}
		_, err = tc.Run(t.Context(), noWorktreeTicketID, "true")
		if !errors.Is(err, orchestrator.ErrNoWorktree) {
			t.Fatalf("err = %v, want errors.Is(err, orchestrator.ErrNoWorktree)", err)
		}
	})
}

// projectIDFor reads ticketID's own project id back out of the store, so
// a second ticket inserted in a subtest shares the same project.
func projectIDFor(t *testing.T, s *store.Store, ticketID int64) int64 {
	t.Helper()
	ticket, err := s.GetTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	return ticket.ProjectID
}

// timeoutRecordingCommands is a job.CommandRunner that always reports
// job.ErrCommandTimeout, recording the timeout Run passed it.
type timeoutRecordingCommands struct {
	timeout time.Duration
}

func (c *timeoutRecordingCommands) Run(_ context.Context, _, _, _ string, timeout time.Duration, _ job.CommandIO) (int, error) {
	c.timeout = timeout
	return -1, job.ErrCommandTimeout
}

// TestTicketCommandTimeoutReported proves a timed-out run reports Exit -1,
// TimedOut true, and a nil error, and that the runner was given
// buildTimeout(Machine) -- Machine.Jobs["build"].TimeoutMinutes as a
// Duration -- as its timeout.
func TestTicketCommandTimeoutReported(t *testing.T) {
	t.Parallel()
	fake := &timeoutRecordingCommands{}
	_, ticketID, _, tc := newTicketCommandFixture(t, fake)

	res, err := tc.Run(t.Context(), ticketID, "sleep 4000")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Exit != -1 || !res.TimedOut {
		t.Fatalf("result = %+v, want Exit -1, TimedOut true", res)
	}

	wantTimeout := time.Duration(tc.Machine.Jobs["build"].TimeoutMinutes) * time.Minute
	if fake.timeout != wantTimeout {
		t.Errorf("recorded timeout = %v, want %v", fake.timeout, wantTimeout)
	}
}

// TestTicketCommandLogsExitNotOutput proves the one INFO log line names the
// ticket id and the exit code but never the command text or its output
// (owner requirement: never log the output). Not parallel: it points
// slog's default logger at a buffer (judging_test.go's own
// TestJudgeWorktreeRemoveFailureLogged is the pattern this mirrors).
func TestTicketCommandLogsExitNotOutput(t *testing.T) {
	cmds := job.NewCommandRunner(sandbox.Off(), false)
	_, ticketID, _, tc := newTicketCommandFixture(t, cmds)

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	const secretMarker = "zing-secret-marker"
	if _, err := tc.Run(t.Context(), ticketID, "echo "+secretMarker); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "sandbox command run") {
		t.Fatalf("log missing \"sandbox command run\"; got:\n%s", logged)
	}
	if !strings.Contains(logged, "ticket_id="+strconv.FormatInt(ticketID, 10)) {
		t.Errorf("log missing ticket_id=%d; got:\n%s", ticketID, logged)
	}
	if !strings.Contains(logged, "exit_code=0") {
		t.Errorf("log missing exit_code=0; got:\n%s", logged)
	}
	if strings.Contains(logged, secretMarker) {
		t.Errorf("log contains the command's output %q; it must never be logged:\n%s", secretMarker, logged)
	}
	if strings.Contains(logged, "echo") {
		t.Errorf("log contains the command text; it must never be logged:\n%s", logged)
	}
}

// ticketCommandSandboxPort is the arbitrary, valid port
// TestTicketCommandSandboxDeniesWriteOutsideWorktree's own sandbox.Load
// renders into the profile's console-deny rule: never dialed, so any value
// in 1-65535 would do.
const ticketCommandSandboxPort = 7420

// TestTicketCommandSandboxDeniesWriteOutsideWorktree proves a command run
// under the real build sandbox profile fails exactly as it would under
// CHECK when it writes outside the worktree, and succeeds when it writes
// inside it. It skips when this machine cannot load the sandbox at all
// (non-macOS, or already running inside Zing's own build sandbox).
func TestTicketCommandSandboxDeniesWriteOutsideWorktree(t *testing.T) {
	t.Parallel()
	profile, err := zing.Assets.ReadFile("sandbox/build.sb")
	if err != nil {
		t.Fatalf("read embedded sandbox profile: %v", err)
	}
	sb := sandbox.Load(profile, t.TempDir(), nil, ticketCommandSandboxPort)
	if !sb.Available() {
		t.Skipf("sandbox unavailable on this machine: %s", sb.Reason())
	}

	cmds := job.NewCommandRunner(sb, true)
	_, ticketID, repoDir, tc := newTicketCommandFixture(t, cmds)
	worktreeDir := ticketCommandWorktreeDir(t, repoDir, ticketID)

	outsideProbe := filepath.Join(repoDir, "outside-probe")
	res, err := tc.Run(t.Context(), ticketID, "touch "+outsideProbe)
	if err != nil {
		t.Fatalf("Run(outside probe): %v", err)
	}
	if res.Exit == 0 {
		t.Errorf("Exit = 0, want nonzero: the sandbox should deny a write outside the worktree")
	}
	if _, statErr := os.Stat(outsideProbe); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("os.Stat(%s) = %v, want os.ErrNotExist", outsideProbe, statErr)
	}

	res, err = tc.Run(t.Context(), ticketID, "touch inside-probe")
	if err != nil {
		t.Fatalf("Run(inside probe): %v", err)
	}
	if res.Exit != 0 {
		t.Fatalf("Exit = %d, want 0: a write inside the worktree should be allowed", res.Exit)
	}
	insideProbe := filepath.Join(worktreeDir, "inside-probe")
	if _, statErr := os.Stat(insideProbe); statErr != nil {
		t.Errorf("os.Stat(%s): %v, want the file to exist", insideProbe, statErr)
	}
}
