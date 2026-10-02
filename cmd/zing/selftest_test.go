package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"zing/internal/gitfixture"
	"zing/internal/response"
	"zing/internal/schemagen"
)

// TestRunSelftest_ReturnsZeroOnEmptyMachine proves the whole selftest suite
// passes end to end, schema and template checks through the design section
// 11 dispatcher-to-done e2e suite (selftestE2E) included: runSelftest
// prints "selftest: OK" and exits 0.
func TestRunSelftest_ReturnsZeroOnEmptyMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	if got := runSelftest(); got != 0 {
		t.Errorf("runSelftest() = %d, want 0", got)
	}
}

// TestSelftestE2E_TicketReachesDoneWithOneQuestionAnswered isolates the
// section 11 end-to-end suite from the rest of selftest, so a failure here
// names the e2e path specifically rather than surfacing only as
// runSelftest's generic non-zero exit. The path it drives runs all the way
// through shipping (PKG9-PLAN.md section 19.4 task 8): selftestShipGH
// scripts a CI failure, a landed ci_log fix, and the push that follows; and
// through respond (section 19.5 task 10): two seeded review threads, one
// answered with a disclosed reply and resolved by APPLY, the other
// collected into a fix request the shared fix driver lands and FIX-REPLIES
// then closes -- before row 8's ready flip, the merge question, "Merge
// now", and the real Merge GitHub reports once every check and thread
// reads clean, verified by verifySelftestShipCILogFixedThenMerged,
// verifySelftestRespondAnsweredThenReady, and
// verifySelftestRespondDisclosedReply.
func TestSelftestE2E_TicketReachesDoneWithOneQuestionAnswered(t *testing.T) {
	t.Parallel()
	if err := selftestE2E(t.Context()); err != nil {
		t.Errorf("selftestE2E() = %v, want nil", err)
	}
}

// TestSelftestShipThreadTIDsMatchFixture proves selftestShipReplyThreadID
// and selftestShipFixThreadID (cmd/zing/selftest.go) still hash, by
// threadrules.go's own tid function (sha256 of the raw id, "t" plus its
// first 16 hex characters, PKG9-PLAN.md section 9.1), to the two thread ids
// fixtures/scripts/respond/1/1.xml answers: a renamed raw id here would
// otherwise surface only as a cryptic RESPOND coverage failure deep inside
// the e2e, not at the seam that actually broke.
func TestSelftestShipThreadTIDsMatchFixture(t *testing.T) {
	t.Parallel()
	tid := func(rawID string) string {
		sum := sha256.Sum256([]byte(rawID))
		return "t" + hex.EncodeToString(sum[:])[:16]
	}
	tests := []struct {
		rawID, wantTID string
	}{
		{selftestShipReplyThreadID, "t9d07af1e65f013d5"},
		{selftestShipFixThreadID, "t121a2150523f6fa7"},
	}
	for _, tt := range tests {
		if got := tid(tt.rawID); got != tt.wantTID {
			t.Errorf("tid(%q) = %q, want %q (fixtures/scripts/respond/1/1.xml answers %q)", tt.rawID, got, tt.wantTID, tt.wantTID)
		}
	}
}

// TestSchemagenDiff_CatchesTamperedSchema proves the schema-drift check
// selftest runs (step 3 of section 6.9) actually detects a tampered
// committed schema, using an in-memory filesystem so the real committed
// files stay untouched.
func TestSchemagenDiff_CatchesTamperedSchema(t *testing.T) {
	t.Parallel()
	generated, err := schemagen.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	tampered := make(fstest.MapFS, len(generated))
	for relpath, b := range generated {
		tampered[relpath] = &fstest.MapFile{Data: b}
	}

	var tamperedPath string
	for relpath := range generated {
		tamperedPath = relpath
		break
	}
	tampered[tamperedPath] = &fstest.MapFile{Data: []byte("{}")}

	diffs, err := schemagen.Diff(tampered)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(diffs) != 1 || diffs[0] != tamperedPath {
		t.Errorf("Diff(tampered) = %v, want [%s]", diffs, tamperedPath)
	}
}

// TestSelftestChecksProfileOnDarwin proves checkSandboxProfile's two darwin
// behaviors Task 15 adds: it fails with the sandbox's own reason when the
// real profile does not load, and it skips the check entirely when
// ZING_SANDBOXED is set (design section 5.3). Skipped off darwin, where
// checkSandboxProfile is always a no-op by definition. t.Setenv("PATH", "")
// forces sandbox.Load's own exec.LookPath("sandbox-exec") to fail
// (reasonSandboxExecNotFound), the same closed reason a laptop missing
// Xcode's command line tools would hit for real, so this proves the failure
// path without touching the checked-in profile.
//
// Not parallel: both of its own subtests below call t.Setenv("PATH", ...),
// so neither the parent nor its subtests can call t.Parallel.
func TestSelftestChecksProfileOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("checkSandboxProfile is a no-op off darwin")
	}

	t.Run("fails with the sandbox's reason when the profile cannot load", func(t *testing.T) {
		t.Setenv("PATH", "")
		err := checkSandboxProfile(t.TempDir())
		if err == nil {
			t.Fatal("checkSandboxProfile() = nil, want an error with an empty PATH")
		}
		want := "selftest: sandbox profile did not load: sandbox-exec not found"
		if err.Error() != want {
			t.Errorf("checkSandboxProfile() = %q, want %q", err.Error(), want)
		}
	})

	t.Run("skips when ZING_SANDBOXED is set", func(t *testing.T) {
		t.Setenv("PATH", "")
		t.Setenv("ZING_SANDBOXED", "1")
		if err := checkSandboxProfile(t.TempDir()); err != nil {
			t.Errorf("checkSandboxProfile() = %v, want nil with ZING_SANDBOXED set", err)
		}
	})
}

// TestCheckResponseTemplates_RendersEveryRegisteredPair proves the pair
// list task 6 hardcodes (registeredPairs) actually renders end to end: an
// unregistered or misnamed pair here would make RenderTemplate error.
func TestCheckResponseTemplates_RendersEveryRegisteredPair(t *testing.T) {
	t.Parallel()
	if err := checkResponseTemplates(); err != nil {
		t.Errorf("checkResponseTemplates() = %v, want nil", err)
	}
}

// TestCheckResponseExamples_RealExamplesPass proves the real embedded
// examples (internal/response/examples/*.xml) all parse and validate, the
// same check selftest itself runs.
func TestCheckResponseExamples_RealExamplesPass(t *testing.T) {
	t.Parallel()
	if err := checkResponseExamples(response.ExampleFS); err != nil {
		t.Errorf("checkResponseExamples(response.ExampleFS) = %v, want nil", err)
	}
}

// TestCheckResponseExamples_CatchesTamperedExample proves the
// parse-and-validate step selftest runs over response.ExampleFS actually
// detects a broken example, using an in-memory copy so the real committed
// files stay untouched.
func TestCheckResponseExamples_CatchesTamperedExample(t *testing.T) {
	t.Parallel()
	files, err := response.ExampleFiles()
	if err != nil {
		t.Fatalf("ExampleFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("ExampleFiles returned none")
	}

	tampered := make(fstest.MapFS, len(files))
	for _, name := range files {
		data, err := response.ExampleFS.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		tampered[name] = &fstest.MapFile{Data: data}
	}

	// A well-formed <zing> element that fails Validate: classify/bug
	// requires a non-empty <reason>, which this omits.
	tampered[files[0]] = &fstest.MapFile{Data: []byte(`<zing job="classify" outcome="bug"></zing>`)}

	gotErr := checkResponseExamples(tampered)
	if gotErr == nil {
		t.Fatal("checkResponseExamples(tampered) = nil, want an error")
	}
	// Tie the failure to the planted file and to the mechanism under test
	// (Validate rejecting the missing <reason>), so a regression that fails
	// for some other reason cannot quietly satisfy this test.
	if !strings.Contains(gotErr.Error(), files[0]) {
		t.Errorf("error %q does not name the tampered example %s", gotErr, files[0])
	}
	if !strings.Contains(gotErr.Error(), "reason: missing required element") {
		t.Errorf("error %q is not the expected missing-reason validation error", gotErr)
	}
}

// TestSelftestShipGitHubHeadSHAIgnoresInheritedGitDir reproduces the
// pre-push failure of the selftest e2e: lefthook's pre-push exports
// GIT_DIR, which overrides "git -C <origin>", so headSHA read the branch
// off the repository being pushed, GetPR failed every poll, and the ticket
// never left shipping. headSHA must read the fixture origin.
//
// Not parallel: it calls t.Setenv, which t.Parallel forbids.
func TestSelftestShipGitHubHeadSHAIgnoresInheritedGitDir(t *testing.T) {
	ctx := t.Context()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := gitfixture.NewSigningRepo(ctx, repo); err != nil {
		t.Fatalf("NewSigningRepo: %v", err)
	}
	remoteDir, err := gitfixture.WithBareOrigin(ctx, repo)
	if err != nil {
		t.Fatalf("WithBareOrigin: %v", err)
	}
	if out, pushErr := gitfixture.Git(ctx, repo, "push", "-q", "origin", "main"); pushErr != nil {
		t.Fatalf("push: %v: %s", pushErr, out)
	}
	want, err := gitfixture.Git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	// A decoy repository with no "main" branch, exported as a git hook would.
	decoy := t.TempDir()
	if out, initErr := gitfixture.Git(ctx, decoy, "init", "-q", "-b", "other"); initErr != nil {
		t.Fatalf("init decoy: %v: %s", initErr, out)
	}
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))

	got, err := newSelftestShipGitHub(remoteDir).headSHA(ctx, "main")
	if err != nil {
		t.Fatalf("headSHA: %v", err)
	}
	if got != strings.TrimSpace(string(want)) {
		t.Errorf("headSHA = %q, want %q", got, strings.TrimSpace(string(want)))
	}
}
