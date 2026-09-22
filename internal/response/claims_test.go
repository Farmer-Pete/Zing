package response

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// Shared test-fixture literals, factored out so goconst does not flag
// their repetition across these tables. testMainGo is shared with
// plancheck_test.go.
const (
	testFileA      = "a.go"
	testFileB      = "b.go"
	testMainGo     = "cmd/zing/main.go"
	ranIt          = "ran it"
	evidenceAGoOne = "a.go:1"
)

func TestCheckBuildClaims_MatchingPasses(t *testing.T) {
	t.Parallel()

	c := BuildClaims{FilesChanged: []string{testFileA, testFileB}, TestExit: 0, LintExit: 0}
	o := BuildObservation{FilesChanged: []string{testFileA, testFileB}, TestExit: 0, LintExit: 0}

	errs := CheckBuildClaims(c, o)
	if len(errs) != 0 {
		t.Fatalf("CheckBuildClaims = %v, want no errors", dumpErrs(errs))
	}
}

func TestCheckBuildClaims_TestExitMismatch(t *testing.T) {
	t.Parallel()

	c := BuildClaims{FilesChanged: []string{testFileA}, TestExit: 0, LintExit: 0}
	o := BuildObservation{FilesChanged: []string{testFileA}, TestExit: 1, LintExit: 0}

	errs := CheckBuildClaims(c, o)
	want := "claims/test_exit: observed 1, claimed 0"
	if !containsErr(errs, want) {
		t.Fatalf("CheckBuildClaims = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckBuildClaims_LintExitMismatch(t *testing.T) {
	t.Parallel()

	c := BuildClaims{FilesChanged: []string{testFileA}, TestExit: 0, LintExit: 0}
	o := BuildObservation{FilesChanged: []string{testFileA}, TestExit: 0, LintExit: 2}

	errs := CheckBuildClaims(c, o)
	want := "claims/lint_exit: observed 2, claimed 0"
	if !containsErr(errs, want) {
		t.Fatalf("CheckBuildClaims = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckBuildClaims_FilesChangedMismatch(t *testing.T) {
	t.Parallel()

	c := BuildClaims{FilesChanged: []string{testFileA, "c.go"}, TestExit: 0, LintExit: 0}
	o := BuildObservation{FilesChanged: []string{testFileA, testFileB}, TestExit: 0, LintExit: 0}

	errs := CheckBuildClaims(c, o)
	want := "claims/files_changed: observed [a.go, b.go], claimed [a.go, c.go]"
	if !containsErr(errs, want) {
		t.Fatalf("CheckBuildClaims = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckCodeClaims_PathExists(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{testMainGo: {Data: []byte("package main")}}
	claims := []Claim{
		{Kind: ClaimKindCode, Verdict: ClaimVerdictTrue, Evidence: "cmd/zing/main.go:12", Text: "it exists"},
	}
	errs := CheckCodeClaims(claims, fsys)
	if len(errs) != 0 {
		t.Fatalf("CheckCodeClaims = %v, want no errors: the path exists", dumpErrs(errs))
	}
}

func TestCheckCodeClaims_MissingPathFails(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{testMainGo: {Data: []byte("package main")}}
	claims := []Claim{
		{Kind: ClaimKindCode, Verdict: ClaimVerdictTrue, Evidence: "cmd/zing/nope.go:1", Text: "it exists"},
	}
	errs := CheckCodeClaims(claims, fsys)
	want := "claims/claim[0]/evidence: no such file cmd/zing/nope.go"
	if !containsErr(errs, want) {
		t.Fatalf("CheckCodeClaims = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestCheckCodeClaims_IgnoresEnvClaims(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{}
	claims := []Claim{
		{Kind: ClaimKindEnv, Verdict: ClaimVerdictTrue, Evidence: "no such tool installed", Text: "checked the environment"},
	}
	errs := CheckCodeClaims(claims, fsys)
	if len(errs) != 0 {
		t.Fatalf("CheckCodeClaims = %v, want no errors: env claims are never path-checked", dumpErrs(errs))
	}
}

// permDeniedFS is an fs.FS whose Stat fails with a non-missing error, to
// exercise CheckCodeClaims' handling of an inspection failure that is not
// fs.ErrNotExist.
type permDeniedFS struct{}

func (permDeniedFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
}

func (permDeniedFS) Stat(name string) (fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrPermission}
}

func TestCheckCodeClaims_NonMissingStatErrorReported(t *testing.T) {
	t.Parallel()

	claims := []Claim{
		{Kind: ClaimKindCode, Verdict: ClaimVerdictTrue, Evidence: "internal/foo.go:10"},
	}
	errs := CheckCodeClaims(claims, permDeniedFS{})
	if len(errs) != 1 {
		t.Fatalf("CheckCodeClaims = %v, want exactly one error", dumpErrs(errs))
	}
	msg := errs[0].Error()
	if !strings.Contains(msg, "cannot inspect") || strings.Contains(msg, "no such file") {
		t.Errorf("error = %q, want a 'cannot inspect' message, not 'no such file'", msg)
	}
}

func TestCheckNothingToDoClaims_AllCodeFalsePasses(t *testing.T) {
	t.Parallel()

	claims := []Claim{
		{Kind: ClaimKindCode, Verdict: ClaimVerdictFalse, Evidence: evidenceAGoOne, Text: "already exists"},
		{Kind: ClaimKindEnv, Verdict: ClaimVerdictTrue, Evidence: "go is installed", Text: "checked"},
	}
	errs := CheckNothingToDoClaims(claims)
	if len(errs) != 0 {
		t.Fatalf("CheckNothingToDoClaims = %v, want no errors", dumpErrs(errs))
	}
}

func TestCheckNothingToDoClaims_CodeClaimTrueFails(t *testing.T) {
	t.Parallel()

	// A mixed code/env document: the env claim's verdict must not matter,
	// only the code claim's does.
	claims := []Claim{
		{Kind: ClaimKindEnv, Verdict: ClaimVerdictTrue, Evidence: "go is installed", Text: "checked"},
		{Kind: ClaimKindCode, Verdict: ClaimVerdictTrue, Evidence: evidenceAGoOne, Text: "already exists"},
	}
	errs := CheckNothingToDoClaims(claims)
	want := "claims/claim[1]/verdict: nothing_to_do needs every code claim false"
	if !containsErr(errs, want) {
		t.Fatalf("CheckNothingToDoClaims = %v, want to contain %q", dumpErrs(errs), want)
	}
	if len(errs) != 1 {
		t.Fatalf("CheckNothingToDoClaims = %v, want exactly 1 error: the env claim must not be checked", dumpErrs(errs))
	}
}

func TestCheckBuildClaims_FilesChangedIgnoresOrderAndDupes(t *testing.T) {
	t.Parallel()

	c := BuildClaims{FilesChanged: []string{testFileB, testFileA, testFileA}, TestExit: 0, LintExit: 0}
	o := BuildObservation{FilesChanged: []string{testFileA, testFileB}, TestExit: 0, LintExit: 0}

	errs := CheckBuildClaims(c, o)
	if len(errs) != 0 {
		t.Fatalf("CheckBuildClaims = %v, want no errors: same set once deduped and sorted", dumpErrs(errs))
	}
}

func msgs(errs []*PathError) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Msg
	}
	return out
}

func containsMsg(errs []*PathError, want string) bool {
	for _, e := range errs {
		if e.Msg == want {
			return true
		}
	}
	return false
}

func TestCheckCoverage_ExactCoveragePasses(t *testing.T) {
	t.Parallel()

	ids := []string{"s1", "s2"}
	verdicts := []Verdict{
		{Scenario: "s1", Result: ResultPass, Evidence: ranIt},
		{Scenario: "s2", Result: ResultFail, Evidence: ranIt},
	}
	errs := CheckCoverage(ids, verdicts)
	if len(errs) != 0 {
		t.Fatalf("CheckCoverage = %v, want no errors", msgs(errs))
	}
}

func TestCheckCoverage_MissingVerdict(t *testing.T) {
	t.Parallel()

	ids := []string{"s1", "s2"}
	verdicts := []Verdict{{Scenario: "s1", Result: ResultPass, Evidence: ranIt}}
	errs := CheckCoverage(ids, verdicts)
	want := "missing verdict for scenario s2"
	if !containsMsg(errs, want) {
		t.Fatalf("CheckCoverage = %v, want to contain %q", msgs(errs), want)
	}
}

func TestCheckCoverage_DuplicateVerdict(t *testing.T) {
	t.Parallel()

	ids := []string{"s1"}
	verdicts := []Verdict{
		{Scenario: "s1", Result: ResultPass, Evidence: ranIt},
		{Scenario: "s1", Result: ResultFail, Evidence: "ran it again"},
	}
	errs := CheckCoverage(ids, verdicts)
	want := "duplicate verdict for scenario s1"
	if !containsMsg(errs, want) {
		t.Fatalf("CheckCoverage = %v, want to contain %q", msgs(errs), want)
	}
}

func TestCheckCoverage_UnknownScenario(t *testing.T) {
	t.Parallel()

	ids := []string{"s1"}
	verdicts := []Verdict{
		{Scenario: "s1", Result: ResultPass, Evidence: ranIt},
		{Scenario: "s9", Result: ResultPass, Evidence: ranIt},
	}
	errs := CheckCoverage(ids, verdicts)
	want := "verdict for unknown scenario s9"
	if !containsMsg(errs, want) {
		t.Fatalf("CheckCoverage = %v, want to contain %q", msgs(errs), want)
	}
}
