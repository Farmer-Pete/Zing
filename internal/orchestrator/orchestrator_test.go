package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Path and slug literals for TestEndToEnd, pulled out as constants (matching
// worktree_test.go's testOwner/testRepo/mainBranch convention and
// perimeter_test.go's path-literal convention) so goconst does not flag the
// repeats: each of these is written, staged, and asserted on more than once
// across the three-task story.
const (
	e2eSlug         = "throwaway ticket"
	coneDir         = "app"
	outsideConeDir  = "docs"
	task1Path       = "app/task1.txt"
	task2Path       = "app/task2.txt"
	task2ExtraPath  = "app/extra.txt"
	task3Path       = "app/task3.txt"
	trustRootGlob   = "machine.toml"
	styleGuidePath  = "CLAUDE.md"
	e2eDraftPRURL   = "https://github.com/acme/widgets/pull/42"
	e2eDraftPRTitle = "Throwaway ticket end to end"
)

// prBodyHeadings is the fixed, ordered set of "## " headings push.go's
// PullRequest.Body renders (PKG5-PLAN.md section 13); TestEndToEnd asserts
// OpenDraftPR's body carries every one of them.
var prBodyHeadings = []string{
	"## What",
	"## Working demo",
	"## Scenarios",
	"## Declared files",
	"## Chesterton's fence",
	"## Plan",
}

// addConeDirsCommit adds a "app" directory (to include in the sparse cone)
// and a "docs" directory (to leave outside it) to repo and commits them, on
// top of newSigningTestRepo's single README.md commit, mirroring the shape
// worktree_test.go's newTestRepo builds for TestPrepareWorktree's own cone
// subtest. It does not touch repo-local git identity or signing config, so
// the local config newSigningTestRepo already set on repo still governs
// this commit.
func addConeDirsCommit(ctx context.Context, t *testing.T, repo string) {
	t.Helper()
	writeTestFile(t, filepath.Join(repo, coneDir, "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(repo, outsideConeDir, "extra.md"), "# extra\n")
	runGit(ctx, t, repo, "add", coneDir+"/main.go", outsideConeDir+"/extra.md")
	runGit(ctx, t, repo, "commit", "-q", "-m", "add app and docs")
}

// newE2EOrchestrator builds an Orchestrator directly through New, rather
// than reusing push_test.go's newTestOrchestratorWithGitHub: that helper's
// Runner parameter is called only with execRunner{} from its own file today,
// and adding a call site here that agrees would perturb golangci-lint's
// unparam finding for a helper this file does not own.
func newE2EOrchestrator(t *testing.T, localPath string, gh GitHub) *Orchestrator {
	t.Helper()
	proj := Project{Owner: testOwner, Repo: testRepo, LocalPath: localPath, DefaultBranch: mainBranch}
	log := slog.New(slog.DiscardHandler)
	o, err := New(proj, gh, execRunner{}, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o
}

// extrasToChanges maps Perimeter's []Extra result back to the []Change
// RevertPaths needs, by path, mirroring the per-task loop PKG5-PLAN.md
// section 8.6 describes: "RevertPaths(the extras' Changes)".
func extrasToChanges(changed []Change, extras []Extra) []Change {
	byPath := make(map[string]Change, len(changed))
	for _, c := range changed {
		byPath[c.Path] = c
	}
	out := make([]Change, 0, len(extras))
	for _, e := range extras {
		out = append(out, byPath[e.Path])
	}
	return out
}

// TestEndToEnd is the walking-skeleton proof for Package 5 (PKG5-PLAN.md
// section 11, the "Orchestrator end to end" row): a throwaway ticket, driven
// through the real orchestrator methods over a real git worktree, a bare
// temp remote, and a temp signing key, becomes a signed draft pull request
// with three commits.
func TestEndToEnd(t *testing.T) {
	t.Run("a throwaway ticket becomes a signed draft pull request", func(t *testing.T) {
		cases := []struct {
			name               string
			ticketID           int64
			withAllowedSigners bool
			wantVerified       bool
			wantGCode          string
		}{
			{
				name:               "with allowedSignersFile: %G? verifies G",
				ticketID:           100,
				withAllowedSigners: true,
				wantVerified:       true,
				wantGCode:          "G",
			},
			{
				name:               "without allowedSignersFile: %G? is N, presence fallback proves signed",
				ticketID:           101,
				withAllowedSigners: false,
				wantVerified:       false,
				wantGCode:          "N",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				fixture := newSigningFixture(t, tc.withAllowedSigners)
				repo := newSigningTestRepo(t, fixture)
				ctx := t.Context()
				addConeDirsCommit(ctx, t, repo)

				remote := newBareRemote(ctx, t)
				addOrigin(ctx, t, repo, remote)

				gh := &scriptedPushGitHub{createURL: e2eDraftPRURL, createNumber: 42}
				o := newE2EOrchestrator(t, repo, gh)

				// --- PrepareWorktree: the worktree dir, the zing/<id>-<slug>
				// branch, and a non-empty sparse cone ---
				wt, err := o.PrepareWorktree(ctx, tc.ticketID, e2eSlug, []string{coneDir})
				if err != nil {
					t.Fatalf("PrepareWorktree: %v", err)
				}

				wantDir := filepath.Join(repo, ".zing", "wt", strconv.FormatInt(tc.ticketID, 10))
				if wt.Dir() != wantDir {
					t.Errorf("Dir() = %q, want %q", wt.Dir(), wantDir)
				}
				if _, statErr := os.Stat(wt.Dir()); statErr != nil {
					t.Errorf("expected the worktree directory to exist: %v", statErr)
				}

				wantBranch := fmt.Sprintf("zing/%d-throwaway-ticket", tc.ticketID)
				if wt.Branch() != wantBranch {
					t.Errorf("Branch() = %q, want %q", wt.Branch(), wantBranch)
				}
				if branches := runGit(ctx, t, repo, "branch", "--list", wt.Branch()); strings.TrimSpace(branches) == "" {
					t.Errorf("expected branch %q to exist in %s", wt.Branch(), repo)
				}

				if _, statErr := os.Stat(filepath.Join(wt.Dir(), coneDir, "main.go")); statErr != nil {
					t.Errorf("expected %s/main.go inside the cone: %v", coneDir, statErr)
				}
				if _, statErr := os.Stat(filepath.Join(wt.Dir(), outsideConeDir, "extra.md")); statErr == nil {
					t.Errorf("expected %s/extra.md to be excluded by the sparse cone, but it exists", outsideConeDir)
				}

				trustRoot := []string{trustRootGlob}
				styleGuide := []string{styleGuidePath}

				// --- Task 1: a plain declared file, no extras, one signed
				// commit ---
				writeTestFile(t, filepath.Join(wt.Dir(), task1Path), "task one\n")
				changed1, err := o.ChangedPaths(ctx, wt)
				if err != nil {
					t.Fatalf("ChangedPaths (task 1): %v", err)
				}
				if extras1 := Perimeter(changed1, []string{task1Path}, trustRoot, styleGuide); len(extras1) != 0 {
					t.Fatalf("task 1: unexpected extras %+v", extras1)
				}
				msg1 := CommitMessage{Title: "Task 1: add task1.txt", FuncLines: []string{testFuncLine}}
				sha1, err := o.CommitTask(ctx, wt, []string{task1Path}, msg1)
				if err != nil {
					t.Fatalf("CommitTask (task 1): %v", err)
				}

				// --- Task 2: an undeclared extra file is caught, reverted,
				// and named in the notice; the declared file survives and is
				// committed, the extra is not ---
				writeTestFile(t, filepath.Join(wt.Dir(), task2Path), "task two, declared\n")
				writeTestFile(t, filepath.Join(wt.Dir(), task2ExtraPath), "an undeclared extra\n")

				changed2, err := o.ChangedPaths(ctx, wt)
				if err != nil {
					t.Fatalf("ChangedPaths (task 2): %v", err)
				}
				declared2 := []string{task2Path}
				extras2 := Perimeter(changed2, declared2, trustRoot, styleGuide)
				if len(extras2) != 1 || extras2[0].Path != task2ExtraPath {
					t.Fatalf("Perimeter extras = %+v, want exactly one extra at %q", extras2, task2ExtraPath)
				}

				if revertErr := o.RevertPaths(ctx, wt, extrasToChanges(changed2, extras2)); revertErr != nil {
					t.Fatalf("RevertPaths: %v", revertErr)
				}

				notice := PerimeterNotice(extras2)
				if !strings.Contains(notice, task2ExtraPath) {
					t.Errorf("PerimeterNotice = %q, want it to name %q", notice, task2ExtraPath)
				}

				if _, statErr := os.Stat(filepath.Join(wt.Dir(), task2ExtraPath)); !os.IsNotExist(statErr) {
					t.Errorf("expected %s to be removed by RevertPaths, stat err = %v", task2ExtraPath, statErr)
				}
				if _, statErr := os.Stat(filepath.Join(wt.Dir(), task2Path)); statErr != nil {
					t.Errorf("expected the declared %s to survive the revert: %v", task2Path, statErr)
				}

				msg2 := CommitMessage{Title: "Task 2: add task2.txt", FuncLines: []string{testFuncLine}}
				sha2, err := o.CommitTask(ctx, wt, declared2, msg2)
				if err != nil {
					t.Fatalf("CommitTask (task 2): %v", err)
				}

				committed2 := runGit(ctx, t, wt.Dir(), "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
				if strings.Contains(committed2, "extra.txt") {
					t.Errorf("commit for task 2 must not contain the reverted extra, committed paths = %q", committed2)
				}
				if !strings.Contains(committed2, "task2.txt") {
					t.Errorf("commit for task 2 must contain the declared file, committed paths = %q", committed2)
				}

				// --- Task 3: a second plain declared file, completing three
				// commits ---
				writeTestFile(t, filepath.Join(wt.Dir(), task3Path), "task three\n")
				changed3, err := o.ChangedPaths(ctx, wt)
				if err != nil {
					t.Fatalf("ChangedPaths (task 3): %v", err)
				}
				if extras3 := Perimeter(changed3, []string{task3Path}, trustRoot, styleGuide); len(extras3) != 0 {
					t.Fatalf("task 3: unexpected extras %+v", extras3)
				}
				msg3 := CommitMessage{Title: "Task 3: add task3.txt", FuncLines: []string{testFuncLine}}
				sha3, err := o.CommitTask(ctx, wt, []string{task3Path}, msg3)
				if err != nil {
					t.Fatalf("CommitTask (task 3): %v", err)
				}

				// --- Three commits on the branch, every one signed ---
				commitLog := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "log", "--oneline", mainBranch+".."+wt.Branch()))
				lines := strings.Split(commitLog, "\n")
				if len(lines) != 3 {
					t.Fatalf("branch carries %d commits ahead of %s, want 3:\n%s", len(lines), mainBranch, commitLog)
				}

				for _, sha := range []string{sha1, sha2, sha3} {
					signed, verified, statusErr := o.signedStatus(ctx, wt.Dir(), sha)
					if statusErr != nil {
						t.Fatalf("signedStatus(%s): %v", sha, statusErr)
					}
					if !signed {
						t.Errorf("signedStatus(%s).signed = false, want true", sha)
					}
					if verified != tc.wantVerified {
						t.Errorf("signedStatus(%s).verified = %v, want %v", sha, verified, tc.wantVerified)
					}

					code := strings.TrimSpace(runGitStdout(ctx, t, wt.Dir(), "show", "--no-patch", "--format=%G?", sha))
					if code != tc.wantGCode {
						t.Errorf("%%G? for %s = %q, want %q", sha, code, tc.wantGCode)
					}
				}

				// --- Push: the branch reaches the bare remote at the branch
				// head ---
				if pushErr := o.Push(ctx, wt); pushErr != nil {
					t.Fatalf("Push: %v", pushErr)
				}
				localHead := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))
				remoteShowRef := runGit(ctx, t, remote, "show-ref", "--verify", "refs/heads/"+wt.Branch())
				if fields := strings.Fields(remoteShowRef); len(fields) == 0 || fields[0] != localHead {
					t.Errorf("remote refs/heads/%s = %q, want it at the branch head %q", wt.Branch(), remoteShowRef, localHead)
				}

				// --- OpenDraftPR: the fake receives head, base, title, and a
				// body carrying all six sections ---
				pr := PullRequest{
					Title:            e2eDraftPRTitle,
					What:             "Drives a throwaway ticket from a fresh branch to a signed draft PR.",
					WorkingDemo:      "go test -race ./internal/orchestrator/...",
					Scenarios:        "- three tasks land as three signed commits\n- an undeclared extra is reverted",
					DeclaredFiles:    "| Path | Note |\n| --- | --- |\n| " + task1Path + " | new |",
					ChestertonsFence: "None removed.",
					PlanLink:         "https://example.com/pkg5-plan",
				}

				url, number, err := o.OpenDraftPR(ctx, wt, pr)
				if err != nil {
					t.Fatalf("OpenDraftPR: %v", err)
				}
				if url != gh.createURL {
					t.Errorf("url = %q, want %q", url, gh.createURL)
				}
				if number != gh.createNumber {
					t.Errorf("number = %d, want %d", number, gh.createNumber)
				}
				if gh.gotHead != wt.Branch() {
					t.Errorf("CreateDraftPR head = %q, want %q", gh.gotHead, wt.Branch())
				}
				if gh.gotBase != mainBranch {
					t.Errorf("CreateDraftPR base = %q, want %q", gh.gotBase, mainBranch)
				}
				if gh.gotTitle != pr.Title {
					t.Errorf("CreateDraftPR title = %q, want %q", gh.gotTitle, pr.Title)
				}
				for _, heading := range prBodyHeadings {
					if !strings.Contains(gh.gotBody, heading) {
						t.Errorf("CreateDraftPR body missing heading %q:\n%s", heading, gh.gotBody)
					}
				}
			})
		}
	})

	t.Run("push refuses the default branch and a forged branch", func(t *testing.T) {
		t.Run("default branch", func(t *testing.T) {
			o := newTestOrchestrator(t, absLocalPath, noCallRunner{t: t})
			wt := Worktree{dir: absLocalPath, branch: mainBranch}
			if err := o.Push(t.Context(), wt); err == nil {
				t.Fatal("Push: expected an error for the default branch, got nil")
			}
		})

		t.Run("forged non-zing branch", func(t *testing.T) {
			o := newTestOrchestrator(t, absLocalPath, noCallRunner{t: t})
			wt := Worktree{dir: absLocalPath, branch: "not-zing/anything"}
			if err := o.Push(t.Context(), wt); err == nil {
				t.Fatal("Push: expected an error for a forged non-zing branch, got nil")
			}
		})
	})

	t.Run("a genuinely unsigned commit resets HEAD and leaves no extra commit", func(t *testing.T) {
		repo := newUnsignedTestRepo(t)
		ctx := t.Context()
		o := newTestOrchestrator(t, repo, stripDashSRunner{inner: execRunner{}})

		wt, err := o.PrepareWorktree(ctx, 900, "unsigned", nil)
		if err != nil {
			t.Fatalf("PrepareWorktree: %v", err)
		}

		priorHead := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))
		priorCount := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-list", "--count", "HEAD"))

		writeTestFile(t, filepath.Join(wt.Dir(), approvedTestFile), "should never land\n")
		msg := CommitMessage{Title: "Should never land", FuncLines: []string{testFuncLine}}

		if _, err := o.CommitTask(ctx, wt, []string{approvedTestFile}, msg); err == nil {
			t.Fatal("CommitTask: expected an error for a genuinely unsigned commit, got nil")
		}

		afterHead := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-parse", "HEAD"))
		if afterHead != priorHead {
			t.Errorf("HEAD = %q after a failed signed commit, want it reset back to %q", afterHead, priorHead)
		}
		afterCount := strings.TrimSpace(runGit(ctx, t, wt.Dir(), "rev-list", "--count", "HEAD"))
		if afterCount != priorCount {
			t.Errorf("commit count = %s after a failed signed commit, want unchanged %s", afterCount, priorCount)
		}
	})
}
