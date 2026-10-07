package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"zing/internal/gitbin"
)

// TestGitGoSteps_RejectsInvalidSHA proves Build checks sha's shape before
// touching git at all: a repoGit that does not even exist would make any
// real git command fail with git's own "not a git repository" message
// instead of the exact invalid-sha text this test asserts, and tmpRoot and
// out are both left untouched.
func TestGitGoSteps_RejectsInvalidSHA(t *testing.T) {
	tmp := t.TempDir()
	repoGit := filepath.Join(tmp, "missing-repo", ".git")
	tmpRoot := filepath.Join(tmp, "tmproot")
	g := gitGoSteps{repoGit: repoGit, defaultBranch: "main", tmpRoot: tmpRoot}

	upper40 := strings.ToUpper(strings.Repeat("0123456789abcdef", 3)[:40])
	cases := []string{
		"--help",
		"0123456789ab",
		upper40,
	}
	for _, sha := range cases {
		t.Run(sha, func(t *testing.T) {
			out := filepath.Join(tmp, "out")
			_, err := g.Build(t.Context(), sha, out)
			want := "build: invalid sha " + sha
			if err == nil || err.Error() != want {
				t.Fatalf("Build(%q) error = %v, want %q", sha, err, want)
			}
			if _, statErr := os.Stat(tmpRoot); !os.IsNotExist(statErr) {
				t.Fatalf("tmpRoot was created: %v", statErr)
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Fatalf("out was written: %v", statErr)
			}
		})
	}
}

// fixtureMainGo is the fixture binary's source: it prints the vcs.revision
// go build stamps in, and nothing else, so TestGitGoSteps_BuildsAtSHA can
// prove Build checked out and built the exact commit it resolved.
const fixtureMainGo = `package main

import (
	"fmt"
	"runtime/debug"
)

func main() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			fmt.Println(s.Value)
		}
	}
}
`

// gitFixtureRun runs one git command against dir and fails the test on
// error.
func gitFixtureRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), gitbin.Path(), args...) //nolint:gosec // G204: gitbin.Path() resolves git itself; args are test-fixed
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// gitFixtureWrite writes relPath under dir, creating parent directories as
// needed.
func gitFixtureWrite(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", relPath, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}
}

// gitFixtureCommit adds every change under dir and commits it with a fixed
// test identity, so the fixture needs no git identity configured on the
// host.
func gitFixtureCommit(t *testing.T, dir, msg string) {
	t.Helper()
	gitFixtureRun(t, dir, "add", "-A")
	gitFixtureRun(t, dir, "-c", "user.name=zing", "-c", "user.email=zing@example.invalid", "commit", "-q", "-m", msg)
}

// TestGitGoSteps_BuildsAtSHA proves Build resolves the sha it is given
// (even when that sha is not origin's current tip), checks it out in a
// detached worktree, and builds exactly that commit: the fixture binary
// reports its own vcs.revision, which must equal the sha Build resolved.
func TestGitGoSteps_BuildsAtSHA(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns git and go build; runs in the full suite")
	}
	ctx := t.Context()
	tmp := t.TempDir()

	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	gitFixtureRun(t, src, "init", "-q", "-b", "main")
	gitFixtureWrite(t, src, "go.mod", "module fixture\n\ngo 1.21\n")
	gitFixtureWrite(t, src, "cmd/zing/main.go", fixtureMainGo)
	gitFixtureCommit(t, src, "first")
	firstSHA := strings.TrimSpace(gitFixtureRun(t, src, "rev-parse", "HEAD"))

	gitFixtureWrite(t, src, "README.md", "second\n")
	gitFixtureCommit(t, src, "second")

	bare := filepath.Join(tmp, "origin.git")
	gitFixtureRun(t, tmp, "clone", "-q", "--bare", src, bare)

	clone := filepath.Join(tmp, "clone")
	gitFixtureRun(t, tmp, "clone", "-q", bare, clone)
	repoGit := filepath.Join(clone, ".git")

	tmpRoot := filepath.Join(tmp, "tmproot")
	g := gitGoSteps{repoGit: repoGit, defaultBranch: "main", tmpRoot: tmpRoot}

	out := filepath.Join(tmp, "candidate")
	resolved, err := g.Build(ctx, firstSHA, out)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if resolved != firstSHA {
		t.Fatalf("resolved sha = %s, want %s (origin's tip, not the one requested)", resolved, firstSHA)
	}

	built, err := exec.CommandContext(ctx, out).Output() //nolint:gosec // G204: out is the binary this test just built
	if err != nil {
		t.Fatalf("run built binary: %v", err)
	}
	if got := strings.TrimSpace(string(built)); got != firstSHA {
		t.Fatalf("built binary reports vcs.revision %s, want %s", got, firstSHA)
	}

	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatalf("read tmpRoot: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "upgrade-") {
			t.Fatalf("leftover worktree directory %s under tmpRoot", e.Name())
		}
	}
}

// TestGitGoSteps_SelftestReportsFailure proves Selftest surfaces a failing
// selftest's output, redacted, and that a successful run's reported
// version survives the "zing " prefix trim unchanged.
func TestGitGoSteps_SelftestReportsFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a process; runs in the full suite")
	}
	ctx := t.Context()
	tmp := t.TempDir()
	var g gitGoSteps

	failing := filepath.Join(tmp, "failing.sh")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho boom https://u:p@h/x\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write failing.sh: %v", err)
	}
	if _, _, err := g.Selftest(ctx, failing); err == nil {
		t.Fatalf("Selftest: want an error from the failing script")
	} else if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "https://REDACTED@h/x") {
		t.Fatalf("Selftest error = %v, want it to contain boom and the redacted url", err)
	}

	ok := filepath.Join(tmp, "ok.sh")
	okScript := "#!/bin/sh\nif [ \"$1\" = version ]; then echo 'zing abc1234def56'; else echo 'selftest ok'; fi\nexit 0\n"
	if err := os.WriteFile(ok, []byte(okScript), 0o755); err != nil {
		t.Fatalf("write ok.sh: %v", err)
	}
	version, output, err := g.Selftest(ctx, ok)
	if err != nil {
		t.Fatalf("Selftest: %v", err)
	}
	if version != "abc1234def56" {
		t.Fatalf("version = %q, want abc1234def56", version)
	}
	if !strings.Contains(output, "selftest ok") {
		t.Fatalf("output = %q, want it to contain the selftest command's own output", output)
	}
}
