package response

import (
	"strconv"
	"testing"
)

func mustParse(t *testing.T, xmlDoc string) *Document {
	t.Helper()
	doc, err := Parse([]byte(xmlDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

func errString(errs []*PathError, i int) string {
	if i >= len(errs) {
		return "<missing>"
	}
	return errs[i].Path + ": " + errs[i].Msg
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

	// BuildClaims.TestExit is required (no omitempty). Omit the element
	// entirely: it must fail exactly once, not also emit a bounds error.
	xmlDoc := `<zing job="build" outcome="ok">` +
		`<claims><files_changed><path>a.go</path></files_changed><lint_exit>0</lint_exit></claims>` +
		`<report>did stuff</report><notes></notes></zing>`
	doc := mustParse(t, xmlDoc)
	errs := Validate(doc, ValidateContext{})
	if len(errs) != 1 {
		t.Fatalf("Validate = %v, want exactly 1 error", dumpErrs(errs))
	}
	want := "claims/test_exit: missing required element"
	if got := errString(errs, 0); got != want {
		t.Errorf("errs[0] = %q, want %q", got, want)
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

	doc := mustParse(t, `<zing job="build" outcome="question"><question key="q1"><title>t</title><body>b</body><recommended>r</recommended></question><progress>p</progress></zing>`)
	errs := Validate(doc, ValidateContext{})
	if len(errs) != 0 {
		t.Fatalf("Validate = %v, want no errors: question is legal for every job", dumpErrs(errs))
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

	xmlDoc := `<zing job="classify" outcome="bug"><reason></reason></zing>`
	doc := mustParse(t, xmlDoc)
	a := Validate(doc, ValidateContext{})
	b := Validate(doc, ValidateContext{})
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
		`<files><file path="a.go" action="create">why</file></files>` +
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
		`<files><file path="a.go" action="create">why</file></files>` +
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
		`<files><file path="a.go" action="create">why</file></files>` +
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
