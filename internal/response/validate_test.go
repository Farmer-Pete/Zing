package response

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func mustParse(t *testing.T, xmlDoc string) *Document {
	t.Helper()
	doc, err := Parse([]byte(xmlDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

// firstErrString renders errs[0] as Path + ": " + Msg, the form every
// "exactly one error, and it is this one" assertion in this file checks
// against.
func firstErrString(errs []*PathError) string {
	if len(errs) == 0 {
		return "<missing>"
	}
	return errs[0].Path + ": " + errs[0].Msg
}

const classifyOK = `<zing job="classify" outcome="bug"><reason>it crashes</reason></zing>`

func TestValidate_ValidDocumentReturnsNil(t *testing.T) {
	t.Parallel()

	doc := mustParse(t, classifyOK)
	errs := Validate(doc, ValidateContext{})
	if len(errs) != 0 {
		t.Fatalf("Validate = %v, want no errors", errs)
	}
}

func TestValidate_MissingRequiredElement_NoCascade(t *testing.T) {
	t.Parallel()

	// BuildResponse.Claims is required (no omitempty). Omit the element
	// entirely: it must fail exactly once, not also emit a nested error.
	xmlDoc := `<zing job="build" outcome="ok">` +
		`<report>did stuff</report><notes></notes></zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	if len(errs) != 1 {
		t.Fatalf("Validate = %v, want exactly 1 error", dumpErrs(errs))
	}
	want := "claims: missing required element"
	if got := firstErrString(errs); got != want {
		t.Errorf("errs[0] = %q, want %q", got, want)
	}
}

// TestValidateBuildIgnoresLegacyExitElements proves a build document that
// still carries the retired test_exit and lint_exit claims (#55) parses
// and validates clean: encoding/xml ignores the unknown elements.
func TestValidateBuildIgnoresLegacyExitElements(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="build" outcome="ok">` +
		`<claims><files_changed><path>a.go</path></files_changed><test_exit>0</test_exit><lint_exit>0</lint_exit></claims>` +
		`<report>did stuff</report><notes></notes></zing>`
	doc := mustParse(t, xmlDoc)
	if errs := Validate(doc, ValidateContext{}); len(errs) != 0 {
		t.Fatalf("Validate = %v, want no errors", dumpErrs(errs))
	}
	if _, ok := doc.Response.(*BuildResponse); !ok {
		t.Fatalf("Response is %T, want *BuildResponse", doc.Response)
	}
}

func TestValidate_BadEnum(t *testing.T) {
	t.Parallel()

	// Parse itself dispatches by (job, outcome), so an unregistered
	// outcome never reaches Validate as a *Document at all; it fails at
	// Parse with the usual catch-all string.
	xmlDoc := `<zing job="classify" outcome="not-a-real-outcome"><reason>x</reason></zing>`
	_, err := Parse([]byte(xmlDoc))
	if err == nil {
		t.Fatal("Parse succeeded, want an error: outcome is not registered for classify")
	}
}

func TestValidate_BadEnumOnField(t *testing.T) {
	t.Parallel()

	// Claim.Kind is an enum (code|env). Build a document with a bad value
	// by hand since Parse dispatch itself does not check field enums.
	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="bogus" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXML() +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	want := "claims/claim[0]/kind: must be one of code|env"
	if !containsErr(errs, want) {
		t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_MinItems(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + `</scenarios>` +
		planXML() +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	want := "scenarios/scenario: need at least 2"
	if !containsErr(errs, want) {
		t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_MinLength(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="classify" outcome="bug"><reason></reason></zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	want := "reason: must not be empty"
	if !containsErr(errs, want) {
		t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_Maximum(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXMLWithHypothesisRank(9) +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	want := "plan/overview/problem/hypotheses/hypothesis[0]/rank: must be <= 5"
	if !containsErr(errs, want) {
		t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_Pattern(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` +
		`<scenario id="bad-id" kind="behavior"><given>g</given><when>w</when><then>t</then></scenario>` +
		scenarioXML("s2") +
		`</scenarios>` +
		planXML() +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	want := `scenarios/scenario[0]/id: must match ^s[0-9]+$`
	if !containsErr(errs, want) {
		t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_FencePatternMessage(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXMLWithBadFence() +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	want := `plan/delivery/deletions/fence[0]: must contain "existed because"`
	if !containsErr(errs, want) {
		t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_RunningJobMismatch(t *testing.T) {
	t.Parallel()

	doc := mustParse(t, classifyOK)
	errs := Validate(doc, ValidateContext{Job: JobBuild})
	want := "job: document says classify, run is build"
	if !containsErr(errs, want) {
		t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_QuestionOutcomeLegalForEveryJob(t *testing.T) {
	t.Parallel()

	for _, j := range Job("").Values() {
		job := Job(j)
		t.Run(string(job), func(t *testing.T) {
			t.Parallel()
			doc := mustParse(t, `<zing job="`+string(job)+`" outcome="question"><question key="q1"><title>t</title><body>b</body><recommended>r</recommended></question><progress>p</progress></zing>`)
			errs := Validate(doc, ValidateContext{})
			if len(errs) != 0 {
				t.Fatalf("Validate = %v, want no errors: question is legal for job %s", dumpErrs(errs), job)
			}
		})
	}
}

// TestValidate_OutcomeIllegalForJob exercises checkHeader's pair-legality
// check directly. Parse itself dispatches by (job, outcome), so it can
// never hand Validate a Document whose pair is unregistered; a hand-built
// Document (never produced by Parse, but Validate must still be correct
// called this way) is the only way to reach this branch in a unit test.
func TestValidate_OutcomeIllegalForJob(t *testing.T) {
	t.Parallel()

	doc := &Document{
		Response: &ClassifyResponse{
			Job: JobClassify, Outcome: OutcomeReady,
			Reason: "x",
		},
		Elem: []byte(`<zing job="classify" outcome="ready"><reason>x</reason></zing>`),
	}
	errs := Validate(doc, ValidateContext{})
	want := "outcome: ready is not an outcome of classify"
	if !containsErr(errs, want) {
		t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_OrderIsStable(t *testing.T) {
	t.Parallel()

	// An empty ready document is missing several required members at once
	// (claims, scenarios, plan), so it yields errors at multiple distinct
	// paths; that is what lets the cross-run comparison below actually
	// exercise ordering rather than trivially match a one-element slice.
	xmlDoc := `<zing job="planning" outcome="ready"></zing>`
	doc := mustParse(t, xmlDoc)
	a := Validate(doc, ValidateContext{})
	b := Validate(doc, ValidateContext{})
	if len(a) < 2 {
		t.Fatalf("Validate returned %d error(s); need >= 2 distinct paths to test ordering: %v", len(a), dumpErrs(a))
	}
	if len(a) != len(b) {
		t.Fatalf("non-deterministic error count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Path != b[i].Path || a[i].Msg != b[i].Msg {
			t.Fatalf("non-deterministic order at %d: %v vs %v", i, a[i], b[i])
		}
	}
}

func TestPathError_Error(t *testing.T) {
	t.Parallel()

	e := &PathError{Path: "reason", Msg: "must not be empty"}
	want := "reason: must not be empty"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// judgeAmendmentXML builds a judge error document with the given code and,
// when reason is non-empty, a full amendment whose reason element is
// reason. An empty reason omits the amendment entirely.
func judgeAmendmentXML(code, reason string) string {
	amendment := ""
	if reason != "" {
		amendment = `<amendment scenario="s2">` +
			`<given>g</given><when>w</when><then>th</then><check>c</check>` +
			`<reason>` + reason + `</reason></amendment>`
	}
	return `<zing job="judge" outcome="error">` +
		`<error code="` + code + `">` +
		`<what>w</what><why>y</why><tried>t</tried>` +
		amendment +
		`</error></zing>`
}

func TestValidateJudgeAmendment(t *testing.T) {
	t.Parallel()

	t.Run("wrong code with amendment", func(t *testing.T) {
		t.Parallel()
		doc := mustParse(t, judgeAmendmentXML("environment", "the check is wrong"))
		errs := Validate(doc, ValidateContext{})
		want := "error/amendment: an amendment is allowed only with code cannot_run"
		if !containsErr(errs, want) {
			t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
		}
	})

	t.Run("cannot_run with amendment missing reason", func(t *testing.T) {
		t.Parallel()
		xmlDoc := `<zing job="judge" outcome="error">` +
			`<error code="cannot_run">` +
			`<what>w</what><why>y</why><tried>t</tried>` +
			`<amendment scenario="s2">` +
			`<given>g</given><when>w</when><then>th</then><check>c</check>` +
			`<reason></reason></amendment>` +
			`</error></zing>`
		doc := mustParse(t, xmlDoc)
		errs := Validate(doc, ValidateContext{})
		want := "error/amendment/reason: must not be empty"
		if !containsErr(errs, want) {
			t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
		}
	})

	t.Run("cannot_run with full amendment is clean", func(t *testing.T) {
		t.Parallel()
		doc := mustParse(t, judgeAmendmentXML("cannot_run", "the check is wrong as written"))
		errs := Validate(doc, ValidateContext{})
		if len(errs) != 0 {
			t.Fatalf("Validate = %v, want no errors", dumpErrs(errs))
		}
	})

	t.Run("cannot_run with no amendment is clean", func(t *testing.T) {
		t.Parallel()
		doc := mustParse(t, judgeAmendmentXML("cannot_run", ""))
		errs := Validate(doc, ValidateContext{})
		if len(errs) != 0 {
			t.Fatalf("Validate = %v, want no errors", dumpErrs(errs))
		}
	})

	t.Run("build job's error document still decodes as ErrorResponse", func(t *testing.T) {
		t.Parallel()
		xmlDoc := `<zing job="build" outcome="error">` +
			`<error code="other"><what>w</what><why>y</why><tried>t</tried></error></zing>`
		doc := mustParse(t, xmlDoc)
		if _, ok := doc.Response.(*ErrorResponse); !ok {
			t.Fatalf("Response is %T, want *ErrorResponse", doc.Response)
		}
	})
}

func containsErr(errs []*PathError, want string) bool {
	for _, e := range errs {
		if e.Path+": "+e.Msg == want {
			return true
		}
	}
	return false
}

func dumpErrs(errs []*PathError) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Path + ": " + e.Msg
	}
	return out
}

func TestValidate_ReadyResponse_CodeClaimEvidenceChecked(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXML() +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{FS: fstest.MapFS{}})
	want := "claims/claim[0]/evidence: no such file a.go"
	if !containsErr(errs, want) {
		t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_ReadyResponse_NoFSSkipsCodeClaimCheck(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXML() +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	if len(errs) != 0 {
		t.Fatalf("Validate = %v, want no errors: ctx.FS is nil, so the code-claim check is skipped", dumpErrs(errs))
	}
}

func TestValidate_ReadyResponse_PlanCheckerWired(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXMLWithShape("shape text TODO: fill in") +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	want := `plan/design/shape: placeholder "TODO" not allowed`
	if !containsErr(errs, want) {
		t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_ReadyResponse_NoneUnionWired(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXMLWithMigrationsNoneFalse() +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	want := `plan/design/migrations: none must be "true" when present`
	if !containsErr(errs, want) {
		t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_ReadyResponse_MissingPlanSkipsLayer2NoCrash(t *testing.T) {
	t.Parallel()

	// The whole <plan> element is absent: Layer 1 reports it missing and
	// suppresses its descendants, so Layer 2's plan-related checks (which
	// index into Delivery.Tests[0] under bug rules, among other things)
	// must not run over the zero-value Plan at all.
	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{Kind: KindBug})
	if len(errs) != 1 {
		t.Fatalf("Validate = %v, want exactly 1 error (plan missing), got %d", dumpErrs(errs), len(errs))
	}
	want := "plan: missing required element"
	if got := firstErrString(errs); got != want {
		t.Errorf("errs[0] = %q, want %q", got, want)
	}
}

// TestValidate_NothingToDoResponse_MissingVerdict_NoCascade proves a
// missing verdict attribute yields only Layer 1's "missing required
// element", not also Layer 2's CheckNothingToDoClaims error at the same
// path (design MAJOR finding 2).
func TestValidate_NothingToDoResponse_MissingVerdict_NoCascade(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="nothing_to_do">` +
		`<claims><claim kind="code" evidence="a.go:1">already exists</claim></claims>` +
		`<notes>nothing to do</notes>` +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	if len(errs) != 1 {
		t.Fatalf("Validate = %v, want exactly 1 error", dumpErrs(errs))
	}
	want := "claims/claim[0]/verdict: missing required element"
	if got := firstErrString(errs); got != want {
		t.Errorf("errs[0] = %q, want %q", got, want)
	}
}

// TestValidate_ReadyResponse_CodeClaimMissingEvidence_NoCascade proves a
// missing evidence attribute yields only Layer 1's "missing required
// element", not also Layer 2's CheckCodeClaims "no such file" error at the
// same path.
func TestValidate_ReadyResponse_CodeClaimMissingEvidence_NoCascade(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXML() +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{FS: fstest.MapFS{}})
	if len(errs) != 1 {
		t.Fatalf("Validate = %v, want exactly 1 error", dumpErrs(errs))
	}
	want := "claims/claim[0]/evidence: missing required element"
	if got := firstErrString(errs); got != want {
		t.Errorf("errs[0] = %q, want %q", got, want)
	}
}

// TestValidate_ReadyResponse_BugPlanMissingProblem_NoCascade proves that
// when the required <problem> element is entirely absent, a bug-kind run
// gets only Layer 1's "missing required element" for plan/overview/problem,
// not also Layer 2's loop/repro/hypotheses bug-shape errors read off the
// zero-value Problem Layer 1 already flagged as missing. The plan's first
// test is kind regression, so the (problem-independent) first-test-kind
// bug-shape check has nothing to add either.
func TestValidate_ReadyResponse_BugPlanMissingProblem_NoCascade(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXMLNoProblem() +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{Kind: KindBug})
	if len(errs) != 1 {
		t.Fatalf("Validate = %v, want exactly 1 error", dumpErrs(errs))
	}
	want := "plan/overview/problem: missing required element"
	if got := firstErrString(errs); got != want {
		t.Errorf("errs[0] = %q, want %q", got, want)
	}
}

// TestValidate_ReadyResponse_BugPlanFirstTestMissingKind_NoCascade proves
// that when the first test's required kind attribute is entirely absent,
// a bug-kind run gets only Layer 1's "missing required element" for
// plan/delivery/tests/test[0]/kind, not also Layer 2's "first test must be
// kind regression" read off the zero-value TestKind Layer 1 already
// flagged as missing. Problem is fully bug-shape-valid, so it has nothing
// to add.
func TestValidate_ReadyResponse_BugPlanFirstTestMissingKind_NoCascade(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXMLBugValidFirstTest("") +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{Kind: KindBug})
	if len(errs) != 1 {
		t.Fatalf("Validate = %v, want exactly 1 error", dumpErrs(errs))
	}
	want := "plan/delivery/tests/test[0]/kind: missing required element"
	if got := firstErrString(errs); got != want {
		t.Errorf("errs[0] = %q, want %q", got, want)
	}
}

// TestValidate_ReadyResponse_BugPlanFirstTestKindUnit_StillFlagged proves
// the presence gate only suppresses the check when kind is truly absent:
// a present kind="unit" still draws "first test must be kind regression".
func TestValidate_ReadyResponse_BugPlanFirstTestKindUnit_StillFlagged(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXMLBugValidFirstTest("unit") +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{Kind: KindBug})
	if len(errs) != 1 {
		t.Fatalf("Validate = %v, want exactly 1 error", dumpErrs(errs))
	}
	want := "plan/delivery/tests/test[0]/kind: first test must be kind regression"
	if got := firstErrString(errs); got != want {
		t.Errorf("errs[0] = %q, want %q", got, want)
	}
}

// TestValidate_ChildrenResponse_MissingKey_NoCascade proves a child with a
// missing key attribute (a zero-value Key, and a zero-value depends_on
// entry that would otherwise equal it) does not generate a spurious
// self-dependency error off those zero values, on top of Layer 1's own
// "missing required element".
func TestValidate_ChildrenResponse_MissingKey_NoCascade(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="children">` +
		`<child><title>t1</title><body>b1</body><depends_on></depends_on></child>` +
		`<child key="c2"><title>t2</title><body>b2</body></child>` +
		`<notes>shared shape</notes>` +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	if len(errs) != 1 {
		t.Fatalf("Validate = %v, want exactly 1 error", dumpErrs(errs))
	}
	want := "child[0]/key: missing required element"
	if got := firstErrString(errs); got != want {
		t.Errorf("errs[0] = %q, want %q", got, want)
	}
}

// TestValidate_ChildrenResponse_MissingKeyDependencyDoesNotCascadeIntoCycle
// proves a missing key does not additionally let findDependencyCycle treat
// its zero-value identity as a real graph node: child[0]'s key is absent
// and it depends on child[1] ("c2"); child[1] carries an empty
// <depends_on> entry, which legitimately targets no present key ("unknown
// key"), but must not also produce a phantom "dependency cycle" built
// from two absent keys colliding at "".
func TestValidate_ChildrenResponse_MissingKeyDependencyDoesNotCascadeIntoCycle(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="children">` +
		`<child><title>t1</title><body>b1</body><depends_on>c2</depends_on></child>` +
		`<child key="c2"><title>t2</title><body>b2</body><depends_on></depends_on></child>` +
		`<notes>shared shape</notes>` +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})

	if !containsErr(errs, "child[0]/key: missing required element") {
		t.Errorf("Validate = %v, want to contain the missing-key error", dumpErrs(errs))
	}
	for _, e := range errs {
		if strings.Contains(e.Msg, "dependency cycle") {
			t.Fatalf("Validate = %v, must not report a dependency cycle: child[0]'s key is absent, not a real graph node", dumpErrs(errs))
		}
	}
}

// TestValidate_ReadyResponse_ScenarioLeakSkippedWhenIDMissing_NoCascade
// proves a scenario missing its required id attribute yields only Layer
// 1's "missing required element" for scenarios/scenario[0]/id, not also
// the Layer 2 scenario-leak check firing with a blank id interpolated,
// even though that scenario's then-text is repeated verbatim in plan
// prose.
func TestValidate_ReadyResponse_ScenarioLeakSkippedWhenIDMissing_NoCascade(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` +
		`<scenario kind="behavior"><given>g</given><when>w</when><then>the response is 200 with body ok</then></scenario>` +
		scenarioXML("s2") +
		`</scenarios>` +
		planXMLWithShape("shape text: the response is 200 with body ok") +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	if len(errs) != 1 {
		t.Fatalf("Validate = %v, want exactly 1 error", dumpErrs(errs))
	}
	want := "scenarios/scenario[0]/id: missing required element"
	if got := firstErrString(errs); got != want {
		t.Errorf("errs[0] = %q, want %q", got, want)
	}
}

// TestValidate_ReadyResponse_ScenarioLeakStillFiresWhenIDPresent proves
// the presence gate only suppresses the check when id is truly absent: a
// present id still draws the scenario-leak error, with that id filled in.
func TestValidate_ReadyResponse_ScenarioLeakStillFiresWhenIDPresent(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` +
		scenarioXML("s1") +
		scenarioXML("s2") +
		`</scenarios>` +
		planXMLWithShape("shape text: s1 then") +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	if len(errs) != 1 {
		t.Fatalf("Validate = %v, want exactly 1 error", dumpErrs(errs))
	}
	want := "plan/design/shape: repeats scenario s1 then-text; the plan must not restate acceptance"
	if got := firstErrString(errs); got != want {
		t.Errorf("errs[0] = %q, want %q", got, want)
	}
}

func TestValidate_NothingToDoResponse_ClaimCheckWired(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="nothing_to_do">` +
		`<claims>` +
		`<claim kind="code" verdict="true" evidence="a.go:1">already exists</claim>` +
		`<claim kind="env" verdict="true" evidence="go is installed">checked</claim>` +
		`</claims>` +
		`<notes>nothing to do</notes>` +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	want := "claims/claim[0]/verdict: nothing_to_do needs every code claim false"
	if !containsErr(errs, want) {
		t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_ChildrenResponse_DAGCheckWired(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="children">` +
		`<child key="c1"><title>t1</title><body>b1</body></child>` +
		`<child key="c1"><title>t2</title><body>b2</body></child>` +
		`<notes>shared shape</notes>` +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	want := "child[1]/key: duplicate key c1"
	if !containsErr(errs, want) {
		t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
	}
}

func TestValidate_QuestionResponse_OptionCardinalityWired(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="questions">` +
		`<question key="q1"><title>t</title><body>b</body><option key="a">only one</option><recommended>a</recommended></question>` +
		`<progress>p</progress>` +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	want := "question[0]/options: give none, or two to four"
	if !containsErr(errs, want) {
		t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
	}
}

// TestValidate_QuestionResponse_FiveOptionsExactlyOneCardinalityError proves
// a count over four produces exactly one cardinality complaint: Layer 1's
// own maxItems=4 error at "question[0]/option" ("option", the XML child
// name), not also Layer 2's "give none, or two to four" at
// "question[0]/options" (the design section 7.2 path, one letter longer,
// for the same underlying problem).
func TestValidate_QuestionResponse_FiveOptionsExactlyOneCardinalityError(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="questions">` +
		`<question key="q1"><title>t</title><body>b</body>` +
		`<option key="a">1</option><option key="b">2</option><option key="c">3</option>` +
		`<option key="d">4</option><option key="e">5</option>` +
		`<recommended>a</recommended></question>` +
		`<progress>p</progress>` +
		`</zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})

	count := 0
	for _, e := range errs {
		if e.Msg == "at most 4 allowed" || e.Msg == "give none, or two to four" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("Validate = %v, want exactly 1 cardinality error, got %d", dumpErrs(errs), count)
	}
	if !containsErr(errs, "question[0]/option: at most 4 allowed") {
		t.Errorf("Validate = %v, want to contain the Layer 1 maxItems error", dumpErrs(errs))
	}
	if containsErr(errs, "question[0]/options: give none, or two to four") {
		t.Errorf("Validate = %v, must not also contain Layer 2's cardinality error for n>4", dumpErrs(errs))
	}
}

// planXMLWithOwnerSmoke returns planXML() with an owner_smoke element,
// one item per entry, inserted after the tasks element. A nil items
// leaves planXML() unchanged.
func planXMLWithOwnerSmoke(items []string) string {
	doc := planXML()
	if items == nil {
		return doc
	}
	var sb strings.Builder
	sb.WriteString("</tasks><owner_smoke>")
	for _, item := range items {
		sb.WriteString("<item>")
		sb.WriteString(item)
		sb.WriteString("</item>")
	}
	sb.WriteString("</owner_smoke></delivery>")
	return strings.Replace(doc, "</tasks></delivery>", sb.String(), 1)
}

func TestValidate_OwnerSmoke(t *testing.T) {
	t.Parallel()

	readyXML := func(plan string) string {
		return `<zing job="planning" outcome="ready">` +
			`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
			`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
			plan +
			`</zing>`
	}

	t.Run("absent", func(t *testing.T) {
		t.Parallel()

		doc := mustParse(t, readyXML(planXMLWithOwnerSmoke(nil)))
		errs := Validate(doc, ValidateContext{})
		for _, e := range errs {
			if strings.HasPrefix(e.Path, "plan/delivery/owner_smoke") {
				t.Errorf("Validate = %v, want no error under plan/delivery/owner_smoke", dumpErrs(errs))
			}
		}
	})

	t.Run("two_items", func(t *testing.T) {
		t.Parallel()

		items := []string{"Open the inbox and see the badge", "Click Abandon and see the confirm"}
		doc := mustParse(t, readyXML(planXMLWithOwnerSmoke(items)))
		errs := Validate(doc, ValidateContext{})
		if len(errs) != 0 {
			t.Fatalf("Validate = %v, want no errors", dumpErrs(errs))
		}
		ready, ok := doc.Response.(*ReadyResponse)
		if !ok {
			t.Fatalf("Response is %T, want *ReadyResponse", doc.Response)
		}
		if got := ready.Plan.Delivery.OwnerSmoke; !slices.Equal(got, items) {
			t.Errorf("Plan.Delivery.OwnerSmoke = %v, want %v", got, items)
		}
	})

	t.Run("eleven_items", func(t *testing.T) {
		t.Parallel()

		items := make([]string, 11)
		for i := range items {
			items[i] = "item " + strconv.Itoa(i)
		}
		doc := mustParse(t, readyXML(planXMLWithOwnerSmoke(items)))
		errs := Validate(doc, ValidateContext{})
		want := "plan/delivery/owner_smoke/item: at most 10 allowed"
		if !containsErr(errs, want) {
			t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
		}
	})

	t.Run("item_301_chars", func(t *testing.T) {
		t.Parallel()

		long := strings.Repeat("a", 301)
		doc := mustParse(t, readyXML(planXMLWithOwnerSmoke([]string{long})))
		errs := Validate(doc, ValidateContext{})
		want := "plan/delivery/owner_smoke/item[0]: must be at most 300 characters"
		if !containsErr(errs, want) {
			t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
		}

		ok := strings.Repeat("a", 300)
		doc2 := mustParse(t, readyXML(planXMLWithOwnerSmoke([]string{ok})))
		errs2 := Validate(doc2, ValidateContext{})
		for _, e := range errs2 {
			if strings.HasPrefix(e.Path, "plan/delivery/owner_smoke") {
				t.Errorf("Validate = %v, want no owner_smoke error for a 300-char item", dumpErrs(errs2))
			}
		}
	})

	t.Run("empty_item", func(t *testing.T) {
		t.Parallel()

		doc := mustParse(t, readyXML(planXMLWithOwnerSmoke([]string{""})))
		errs := Validate(doc, ValidateContext{})
		want := "plan/delivery/owner_smoke/item[0]: must not be empty"
		if !containsErr(errs, want) {
			t.Fatalf("errs = %v, want to contain %q", dumpErrs(errs), want)
		}
	})
}

func scenarioXML(id string) string {
	return `<scenario id="` + id + `" kind="behavior"><given>g</given><when>w</when><then>` + id + ` then</then></scenario>`
}

func planXML() string {
	return `<plan>` +
		`<overview>` +
		`<objective>o</objective><context>c</context>` +
		`<problem>problem text</problem>` +
		`<goals><goal>g1</goal></goals><nongoals><nongoal>ng1</nongoal></nongoals>` +
		`</overview>` +
		`<design>` +
		`<demo cmd="go run ./x">demo text</demo>` +
		`<shape>shape text</shape>` +
		`<migrations none="true"></migrations>` +
		`</design>` +
		`<delivery>` +
		`<files><file path="a.go" action="create" task="1">why</file></files>` +
		`<deletions none="true"></deletions>` +
		`<tests><test name="t1" seam="s" kind="unit" mocks="">asserts</test></tests>` +
		`<tasks><task n="1" test="t1" demo="true">do it</task></tasks>` +
		`</delivery>` +
		`<review>` +
		`<trust_root>none</trust_root>` +
		`<alternatives><alternative>alt</alternative></alternatives>` +
		`<risks><risk>risk</risk></risks>` +
		`</review>` +
		`</plan>`
}

func planXMLWithHypothesisRank(rank int) string {
	return `<plan>` +
		`<overview>` +
		`<objective>o</objective><context>c</context>` +
		`<problem>problem text<hypotheses><hypothesis rank="` +
		strconv.Itoa(rank) + `"><cause>c</cause><prediction>p</prediction></hypothesis></hypotheses></problem>` +
		`<goals><goal>g1</goal></goals><nongoals><nongoal>ng1</nongoal></nongoals>` +
		`</overview>` +
		`<design>` +
		`<demo cmd="go run ./x">demo text</demo>` +
		`<shape>shape text</shape>` +
		`<migrations none="true"></migrations>` +
		`</design>` +
		`<delivery>` +
		`<files><file path="a.go" action="create" task="1">why</file></files>` +
		`<deletions none="true"></deletions>` +
		`<tests><test name="t1" seam="s" kind="unit" mocks="">asserts</test></tests>` +
		`<tasks><task n="1" test="t1" demo="true">do it</task></tasks>` +
		`</delivery>` +
		`<review>` +
		`<trust_root>none</trust_root>` +
		`<alternatives><alternative>alt</alternative></alternatives>` +
		`<risks><risk>risk</risk></risks>` +
		`</review>` +
		`</plan>`
}

func planXMLWithShape(shape string) string {
	return `<plan>` +
		`<overview>` +
		`<objective>o</objective><context>c</context>` +
		`<problem>problem text</problem>` +
		`<goals><goal>g1</goal></goals><nongoals><nongoal>ng1</nongoal></nongoals>` +
		`</overview>` +
		`<design>` +
		`<demo cmd="go run ./x">demo text</demo>` +
		`<shape>` + shape + `</shape>` +
		`<migrations none="true"></migrations>` +
		`</design>` +
		`<delivery>` +
		`<files><file path="a.go" action="create" task="1">why</file></files>` +
		`<deletions none="true"></deletions>` +
		`<tests><test name="t1" seam="s" kind="unit" mocks="">asserts</test></tests>` +
		`<tasks><task n="1" test="t1" demo="true">do it</task></tasks>` +
		`</delivery>` +
		`<review>` +
		`<trust_root>none</trust_root>` +
		`<alternatives><alternative>alt</alternative></alternatives>` +
		`<risks><risk>risk</risk></risks>` +
		`</review>` +
		`</plan>`
}

func planXMLWithMigrationsNoneFalse() string {
	return `<plan>` +
		`<overview>` +
		`<objective>o</objective><context>c</context>` +
		`<problem>problem text</problem>` +
		`<goals><goal>g1</goal></goals><nongoals><nongoal>ng1</nongoal></nongoals>` +
		`</overview>` +
		`<design>` +
		`<demo cmd="go run ./x">demo text</demo>` +
		`<shape>shape text</shape>` +
		`<migrations none="false"></migrations>` +
		`</design>` +
		`<delivery>` +
		`<files><file path="a.go" action="create" task="1">why</file></files>` +
		`<deletions none="true"></deletions>` +
		`<tests><test name="t1" seam="s" kind="unit" mocks="">asserts</test></tests>` +
		`<tasks><task n="1" test="t1" demo="true">do it</task></tasks>` +
		`</delivery>` +
		`<review>` +
		`<trust_root>none</trust_root>` +
		`<alternatives><alternative>alt</alternative></alternatives>` +
		`<risks><risk>risk</risk></risks>` +
		`</review>` +
		`</plan>`
}

// planXMLNoProblem is planXML with the required <problem> element omitted
// entirely, and its first test kind regression, so under bug kind the only
// possible error is Layer 1's own "missing required element" for
// plan/overview/problem.
func planXMLNoProblem() string {
	return `<plan>` +
		`<overview>` +
		`<objective>o</objective><context>c</context>` +
		`<goals><goal>g1</goal></goals><nongoals><nongoal>ng1</nongoal></nongoals>` +
		`</overview>` +
		`<design>` +
		`<demo cmd="go run ./x">demo text</demo>` +
		`<shape>shape text</shape>` +
		`<migrations none="true"></migrations>` +
		`</design>` +
		`<delivery>` +
		`<files><file path="a.go" action="create" task="1">why</file></files>` +
		`<deletions none="true"></deletions>` +
		`<tests><test name="t1" seam="s" kind="regression" mocks="">asserts</test></tests>` +
		`<tasks><task n="1" test="t1" demo="true">do it</task></tasks>` +
		`</delivery>` +
		`<review>` +
		`<trust_root>none</trust_root>` +
		`<alternatives><alternative>alt</alternative></alternatives>` +
		`<risks><risk>risk</risk></risks>` +
		`</review>` +
		`</plan>`
}

// planXMLBugValidFirstTest is planXML with a full bug-shape-valid
// <problem> (a loop, a repro, three hypotheses), so a bug-kind Validate
// call has nothing to say about problem at all; the first test's kind
// attribute is set from testKindAttr verbatim, letting a caller omit it
// (pass "") or set it to something other than regression, to isolate the
// first-test-kind check.
func planXMLBugValidFirstTest(testKindAttr string) string {
	kindAttr := ""
	if testKindAttr != "" {
		kindAttr = ` kind="` + testKindAttr + `"`
	}
	return `<plan>` +
		`<overview>` +
		`<objective>o</objective><context>c</context>` +
		`<problem>problem text` +
		`<loop cmd="` + testBugLoopCmd + `">fails</loop>` +
		`<repro>steps</repro>` +
		`<hypotheses>` +
		`<hypothesis rank="1"><cause>c1</cause><prediction>p1</prediction></hypothesis>` +
		`<hypothesis rank="2"><cause>c2</cause><prediction>p2</prediction></hypothesis>` +
		`<hypothesis rank="3"><cause>c3</cause><prediction>p3</prediction></hypothesis>` +
		`</hypotheses>` +
		`</problem>` +
		`<goals><goal>g1</goal></goals><nongoals><nongoal>ng1</nongoal></nongoals>` +
		`</overview>` +
		`<design>` +
		`<demo cmd="go run ./x">demo text</demo>` +
		`<shape>shape text</shape>` +
		`<migrations none="true"></migrations>` +
		`</design>` +
		`<delivery>` +
		`<files><file path="a.go" action="create" task="1">why</file></files>` +
		`<deletions none="true"></deletions>` +
		`<tests><test name="t1" seam="s"` + kindAttr + ` mocks="">asserts</test></tests>` +
		`<tasks><task n="1" test="t1" demo="true">do it</task></tasks>` +
		`</delivery>` +
		`<review>` +
		`<trust_root>none</trust_root>` +
		`<alternatives><alternative>alt</alternative></alternatives>` +
		`<risks><risk>risk</risk></risks>` +
		`</review>` +
		`</plan>`
}

func planXMLWithBadFence() string {
	return `<plan>` +
		`<overview>` +
		`<objective>o</objective><context>c</context>` +
		`<problem>problem text</problem>` +
		`<goals><goal>g1</goal></goals><nongoals><nongoal>ng1</nongoal></nongoals>` +
		`</overview>` +
		`<design>` +
		`<demo cmd="go run ./x">demo text</demo>` +
		`<shape>shape text</shape>` +
		`<migrations none="true"></migrations>` +
		`</design>` +
		`<delivery>` +
		`<files><file path="a.go" action="create" task="1">why</file></files>` +
		`<deletions><fence path="a.go" symbol="Old">not the right words</fence></deletions>` +
		`<tests><test name="t1" seam="s" kind="unit" mocks="">asserts</test></tests>` +
		`<tasks><task n="1" test="t1" demo="true">do it</task></tasks>` +
		`</delivery>` +
		`<review>` +
		`<trust_root>none</trust_root>` +
		`<alternatives><alternative>alt</alternative></alternatives>` +
		`<risks><risk>risk</risk></risks>` +
		`</review>` +
		`</plan>`
}

// planXMLWithTasks is planXML with its <tasks> block replaced by tasksXML,
// so TestReadyTaskNumbering can exercise more than the one task planXML's
// own fixture carries.
func planXMLWithTasks(tasksXML string) string {
	return `<plan>` +
		`<overview>` +
		`<objective>o</objective><context>c</context>` +
		`<problem>problem text</problem>` +
		`<goals><goal>g1</goal></goals><nongoals><nongoal>ng1</nongoal></nongoals>` +
		`</overview>` +
		`<design>` +
		`<demo cmd="go run ./x">demo text</demo>` +
		`<shape>shape text</shape>` +
		`<migrations none="true"></migrations>` +
		`</design>` +
		`<delivery>` +
		`<files><file path="a.go" action="create" task="1 2">why</file></files>` +
		`<deletions none="true"></deletions>` +
		`<tests><test name="t1" seam="s" kind="unit" mocks="">asserts</test></tests>` +
		`<tasks>` + tasksXML + `</tasks>` +
		`</delivery>` +
		`<review>` +
		`<trust_root>none</trust_root>` +
		`<alternatives><alternative>alt</alternative></alternatives>` +
		`<risks><risk>risk</risk></risks>` +
		`</review>` +
		`</plan>`
}

// readyXMLWithTasks wraps planXMLWithTasks in the header, claims, and
// scenarios every ready document needs, so a Validate call sees a document
// whose only interesting content is its tasks.
func readyXMLWithTasks(tasksXML string) string {
	return `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXMLWithTasks(tasksXML) +
		`</zing>`
}

// TestReadyTaskNumbering proves layer2Ready's task numbering rule (design
// section 4.1): plan/delivery/tasks/task[i]/n must equal i+1, so a
// duplicate, a gap, and a wrong order are each caught as "want <i+1>" at
// the offending task's own index, and an in-order document draws no such
// error.
func TestReadyTaskNumbering(t *testing.T) {
	t.Parallel()

	t.Run("in order", func(t *testing.T) {
		t.Parallel()
		tasksXML := `<task n="1" test="t1" demo="true">do it</task>` +
			`<task n="2" test="t1" demo="false">then this</task>`
		doc := mustParse(t, readyXMLWithTasks(tasksXML))
		errs := Validate(doc, ValidateContext{})
		for _, e := range errs {
			if strings.Contains(e.Msg, "want ") {
				t.Errorf("Validate = %v, want no task numbering error", dumpErrs(errs))
			}
		}
	})

	t.Run("a duplicate", func(t *testing.T) {
		t.Parallel()
		tasksXML := `<task n="1" test="t1" demo="true">do it</task>` +
			`<task n="1" test="t1" demo="false">then this</task>`
		doc := mustParse(t, readyXMLWithTasks(tasksXML))
		errs := Validate(doc, ValidateContext{})
		if !containsErr(errs, wantTaskTwoNumber) {
			t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), wantTaskTwoNumber)
		}
	})

	t.Run("a gap", func(t *testing.T) {
		t.Parallel()
		tasksXML := `<task n="1" test="t1" demo="true">do it</task>` +
			`<task n="3" test="t1" demo="false">then this</task>`
		doc := mustParse(t, readyXMLWithTasks(tasksXML))
		errs := Validate(doc, ValidateContext{})
		if !containsErr(errs, wantTaskTwoNumber) {
			t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), wantTaskTwoNumber)
		}
	})

	t.Run("out of order", func(t *testing.T) {
		t.Parallel()
		tasksXML := `<task n="2" test="t1" demo="true">do it</task>` +
			`<task n="1" test="t1" demo="false">then this</task>`
		doc := mustParse(t, readyXMLWithTasks(tasksXML))
		errs := Validate(doc, ValidateContext{})
		for i, want := range []string{
			"plan/delivery/tasks/task[0]/n: want 1",
			wantTaskTwoNumber,
		} {
			if !containsErr(errs, want) {
				t.Errorf("task %d: Validate = %v, want to contain %q", i, dumpErrs(errs), want)
			}
		}
	})
}

// planXMLFileTask builds a minimal plan with one file, whose task
// attribute is the literal XML fragment fileTaskAttr (such as `task="1"`,
// or "" to omit the attribute entirely), and taskCount tasks numbered 1
// through taskCount, so TestValidateFileTasks can exercise checkFileTasks
// in isolation from the other Layer 2 plan checks.
func planXMLFileTask(fileTaskAttr string, taskCount int) string {
	var tasksXML strings.Builder
	for i := 1; i <= taskCount; i++ {
		tasksXML.WriteString(`<task n="` + strconv.Itoa(i) + `" test="t1" demo="true">do it</task>`)
	}
	return `<plan>` +
		`<overview>` +
		`<objective>o</objective><context>c</context>` +
		`<problem>problem text</problem>` +
		`<goals><goal>g1</goal></goals><nongoals><nongoal>ng1</nongoal></nongoals>` +
		`</overview>` +
		`<design>` +
		`<demo cmd="go run ./x">demo text</demo>` +
		`<shape>shape text</shape>` +
		`<migrations none="true"></migrations>` +
		`</design>` +
		`<delivery>` +
		`<files><file path="a.go" action="create" ` + fileTaskAttr + `>why</file></files>` +
		`<deletions none="true"></deletions>` +
		`<tests><test name="t1" seam="s" kind="unit" mocks="">asserts</test></tests>` +
		`<tasks>` + tasksXML.String() + `</tasks>` +
		`</delivery>` +
		`<review>` +
		`<trust_root>none</trust_root>` +
		`<alternatives><alternative>alt</alternative></alternatives>` +
		`<risks><risk>risk</risk></risks>` +
		`</review>` +
		`</plan>`
}

// readyXMLFileTask wraps planXMLFileTask in the header, claims, and
// scenarios every ready document needs.
func readyXMLFileTask(fileTaskAttr string, taskCount int) string {
	return `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		planXMLFileTask(fileTaskAttr, taskCount) +
		`</zing>`
}

// readyXMLNoFiles is readyXMLFileTask with the files list left empty, so
// TestValidateFileTasks/empty can drive checkFileTasks with files == nil
// without going through the file-element XML at all.
func readyXMLNoFiles(taskCount int) string {
	var tasksXML strings.Builder
	for i := 1; i <= taskCount; i++ {
		tasksXML.WriteString(`<task n="` + strconv.Itoa(i) + `" test="t1" demo="true">do it</task>`)
	}
	plan := `<plan>` +
		`<overview>` +
		`<objective>o</objective><context>c</context>` +
		`<problem>problem text</problem>` +
		`<goals><goal>g1</goal></goals><nongoals><nongoal>ng1</nongoal></nongoals>` +
		`</overview>` +
		`<design>` +
		`<demo cmd="go run ./x">demo text</demo>` +
		`<shape>shape text</shape>` +
		`<migrations none="true"></migrations>` +
		`</design>` +
		`<delivery>` +
		`<files></files>` +
		`<deletions none="true"></deletions>` +
		`<tests><test name="t1" seam="s" kind="unit" mocks="">asserts</test></tests>` +
		`<tasks>` + tasksXML.String() + `</tasks>` +
		`</delivery>` +
		`<review>` +
		`<trust_root>none</trust_root>` +
		`<alternatives><alternative>alt</alternative></alternatives>` +
		`<risks><risk>risk</risk></risks>` +
		`</review>` +
		`</plan>`
	return `<zing job="planning" outcome="ready">` +
		`<claims><claim kind="code" verdict="true" evidence="a.go:1">it works</claim></claims>` +
		`<scenarios>` + scenarioXML("s1") + scenarioXML("s2") + `</scenarios>` +
		plan +
		`</zing>`
}

// TestValidateFileTasks proves checkFileTasks' own rules (design section
// 4.1): every file names at least one task, in range, without repeats, and
// every task owns at least one file; a malformed task attribute draws only
// Layer 1's pattern error, since checkFileTasks skips the owns-no-file
// check entirely once any file's attribute failed to decode or match.
func TestValidateFileTasks(t *testing.T) {
	t.Parallel()

	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		doc := mustParse(t, readyXMLFileTask("", 2))
		errs := Validate(doc, ValidateContext{})
		want := "plan/delivery/files/file[0]/task: missing required element"
		if !containsErr(errs, want) {
			t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
		}
		for _, e := range errs {
			if strings.Contains(e.Msg, "owns no file") {
				t.Errorf("Validate = %v, want no owns-no-file error", dumpErrs(errs))
			}
		}
	})

	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		doc := mustParse(t, readyXMLFileTask(`task="3"`, 2))
		errs := Validate(doc, ValidateContext{})
		want := "plan/delivery/files/file[0]/task: names task 3, but the plan has 2 tasks"
		if !containsErr(errs, want) {
			t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
		}
	})

	t.Run("twice", func(t *testing.T) {
		t.Parallel()
		doc := mustParse(t, readyXMLFileTask(`task="1 1"`, 2))
		errs := Validate(doc, ValidateContext{})
		want := "plan/delivery/files/file[0]/task: lists task 1 twice"
		if !containsErr(errs, want) {
			t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
		}
	})

	t.Run("unowned", func(t *testing.T) {
		t.Parallel()
		doc := mustParse(t, readyXMLFileTask(`task="1"`, 2))
		errs := Validate(doc, ValidateContext{})
		want := "plan/delivery/tasks/task[1]: task 2 owns no file; fold it into a task that changes one"
		if !containsErr(errs, want) {
			t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
		}
	})

	t.Run("shared", func(t *testing.T) {
		t.Parallel()
		doc := mustParse(t, readyXMLFileTask(`task="1 2"`, 2))
		errs := Validate(doc, ValidateContext{})
		for _, e := range errs {
			if e.Path == "plan/delivery/files/file[0]/task" || strings.Contains(e.Msg, "owns no file") {
				t.Errorf("Validate = %v, want no file-task error, got %q", dumpErrs(errs), e.Error())
			}
		}
	})

	t.Run("malformed", func(t *testing.T) {
		t.Parallel()
		doc := mustParse(t, readyXMLFileTask(`task="0"`, 1))
		errs := Validate(doc, ValidateContext{})
		want := "plan/delivery/files/file[0]/task: must match ^[1-9][0-9]?( [1-9][0-9]?)*$"
		if len(errs) != 1 {
			t.Fatalf("Validate = %v, want exactly 1 error", dumpErrs(errs))
		}
		if got := firstErrString(errs); got != want {
			t.Errorf("errs[0] = %q, want %q", got, want)
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		doc := mustParse(t, readyXMLNoFiles(2))
		errs := Validate(doc, ValidateContext{})
		want := "plan/delivery/files/file: need at least 1"
		if !containsErr(errs, want) {
			t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
		}
		for _, e := range errs {
			if strings.Contains(e.Msg, "owns no file") {
				t.Errorf("Validate = %v, want no owns-no-file error", dumpErrs(errs))
			}
		}
	})
}

// repliesXML builds a minimal planning/replies document with one reply,
// attrs merged in verbatim (design section 22.2, D31).
func repliesXML(replyAttrs, replyText string) string {
	return `<zing job="planning" outcome="replies">` +
		`<replies><reply ` + replyAttrs + `>` + replyText + `</reply></replies>` +
		`<progress>progress</progress></zing>`
}

// TestDecisionMaxLength proves a reply's decision attribute accepts exactly
// 500 runes and refuses 501 (design section 22.2, D31).
func TestDecisionMaxLength(t *testing.T) {
	t.Parallel()

	t.Run("500 runes passes", func(t *testing.T) {
		t.Parallel()
		decision := strings.Repeat("d", 500)
		doc := mustParse(t, repliesXML(`question="Q1" settled="true" decision="`+decision+`"`, "ok"))
		errs := Validate(doc, ValidateContext{})
		const bad = "replies/reply[0]/decision: must be at most 500 characters"
		if containsErr(errs, bad) {
			t.Fatalf("Validate = %v, want no %q", dumpErrs(errs), bad)
		}
	})

	t.Run("501 runes fails", func(t *testing.T) {
		t.Parallel()
		decision := strings.Repeat("d", 501)
		doc := mustParse(t, repliesXML(`question="Q1" settled="true" decision="`+decision+`"`, "ok"))
		errs := Validate(doc, ValidateContext{})
		want := "replies/reply[0]/decision: must be at most 500 characters"
		if !containsErr(errs, want) {
			t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
		}
	})
}

// TestReplyQuestionPattern proves a reply's question attribute must match
// ^Q[0-9]+$: the model's own lowercase wire key fails (design section 22.2,
// D31, the key-confusion risk).
func TestReplyQuestionPattern(t *testing.T) {
	t.Parallel()

	doc := mustParse(t, repliesXML(`question="q1"`, "ok"))
	errs := Validate(doc, ValidateContext{})
	want := `replies/reply[0]/question: must match ^Q[0-9]+$`
	if !containsErr(errs, want) {
		t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
	}
}

// TestRepliesOutcomeNeedsOneReply proves the replies outcome refuses an
// empty <replies> block (design section 22.2, D31: "replies outcome with
// none").
func TestRepliesOutcomeNeedsOneReply(t *testing.T) {
	t.Parallel()

	xmlDoc := `<zing job="planning" outcome="replies"><progress>progress</progress></zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	want := "replies/reply: need at least 1"
	if !containsErr(errs, want) {
		t.Fatalf("Validate = %v, want to contain %q", dumpErrs(errs), want)
	}
}
