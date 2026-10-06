package job

// planning_internal_test.go tests planning.go's own unexported functions
// that no seam in planning_test.go (package job_test) can reach -- the same
// reason runjob_test.go lives in package job instead of alongside it (see
// that file's own comment).

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"zing/internal/response"
	"zing/internal/store"
)

// testObjectiveLocation is the plan element path this file's finding and
// disposition fixtures repeat (goconst): a plan review major at the plan's
// own objective, the shape every "needs a disposition" test below uses.
const testObjectiveLocation = "plan/overview/objective"

// requiredID is the plan-review finding id TestCheckDispositions and
// TestMarkReopened both use for their one required/previous major finding.
const requiredID = "p1-f2"

// TestGateQuestionMessage_StatesWhatApproveDoes proves F013: the gate
// question's body carries a plain-language paragraph explaining what
// approving does, not just the bare plan objective, while leaving the two
// chip options unchanged.
func TestGateQuestionMessage_StatesWhatApproveDoes(t *testing.T) {
	t.Parallel()
	const objective = "Add a hello endpoint so a caller can get a plain-text greeting back over HTTP."
	msg, err := gateQuestionMessage(1, objective, gateApproveExplains)
	if err != nil {
		t.Fatalf("gateQuestionMessage: %v", err)
	}
	if !strings.HasPrefix(msg.Body, objective+"\n\n") {
		t.Fatalf("message body = %q, want it to start with the objective, a blank line, then what Approve does", msg.Body)
	}
	if !strings.Contains(msg.Body, "seals") {
		t.Errorf("message body = %q, want it to say approve seals the scenario set", msg.Body)
	}
	if !strings.Contains(msg.Body, "cannot be undone") {
		t.Errorf("message body = %q, want it to say this cannot be undone", msg.Body)
	}

	var payload response.QuestionPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	if payload.Kind != response.QuestionKindGate {
		t.Errorf("payload.Kind = %q, want gate", payload.Kind)
	}
	wantOptions := []response.Option{{Key: gateOptionApprove, Text: "Approve"}, {Key: gateOptionReject, Text: "Reject"}}
	if len(payload.Options) != len(wantOptions) {
		t.Fatalf("payload.Options = %+v, want %+v", payload.Options, wantOptions)
	}
	for i, o := range wantOptions {
		if payload.Options[i] != o {
			t.Errorf("payload.Options[%d] = %+v, want %+v", i, payload.Options[i], o)
		}
	}
}

// TestGateQuestionMessage_LoopsExhaustedUsesDifferentExplainsText proves
// issue #48's and ticket 66's gate text split: the body always equals
// objective, a blank line, then the explains text passed in, and the three
// explains constants never share the phrases that mark the other two.
func TestGateQuestionMessage_LoopsExhaustedUsesDifferentExplainsText(t *testing.T) {
	t.Parallel()
	const objective = "Loop exhausted on floor-only findings."
	const alreadyFixedAutomaticallyPhrase = "already fixed automatically"

	tests := []struct {
		name         string
		explains     string
		wantContains string
		wantAbsent   string
	}{
		{"clean review", gateApproveExplains, alreadyFixedAutomaticallyPhrase, "max_loops"},
		{"loops exhausted", gateApproveExplainsLoopsExhausted, "max_loops", alreadyFixedAutomaticallyPhrase},
		{"owner chose", gateApproveExplainsOwnerChose, "chose", alreadyFixedAutomaticallyPhrase},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msg, err := gateQuestionMessage(1, objective, tt.explains)
			if err != nil {
				t.Fatalf("gateQuestionMessage: %v", err)
			}
			if msg.Body != objective+"\n\n"+tt.explains {
				t.Errorf("message body = %q, want %q", msg.Body, objective+"\n\n"+tt.explains)
			}
			if !strings.Contains(msg.Body, tt.wantContains) {
				t.Errorf("message body = %q, want it to contain %q", msg.Body, tt.wantContains)
			}
			if strings.Contains(msg.Body, tt.wantAbsent) {
				t.Errorf("message body = %q, want it to not contain %q", msg.Body, tt.wantAbsent)
			}
		})
	}
	if strings.Contains(gateApproveExplains, "chose") {
		t.Errorf("gateApproveExplains = %q, want it to not contain chose", gateApproveExplains)
	}
	if !strings.Contains(gateApproveExplainsLoopsExhausted, "max_loops") || strings.Contains(gateApproveExplainsLoopsExhausted, "chose") {
		t.Errorf("gateApproveExplainsLoopsExhausted = %q, want max_loops and not chose", gateApproveExplainsLoopsExhausted)
	}
	if !strings.Contains(gateApproveExplainsOwnerChose, "max_loops") {
		t.Errorf("gateApproveExplainsOwnerChose = %q, want it to mention max_loops too", gateApproveExplainsOwnerChose)
	}
}

// TestRenderGateFindings proves renderGateFindings' rendering rules for
// acceptPlanAtCap's gate list: one "- SEVERITY LOCATION TEXT" line per
// finding in input order, whitespace collapsed, and TEXT cut at
// gateFindingTextMaxRunes runes.
func TestRenderGateFindings(t *testing.T) {
	t.Parallel()

	const shapeLocation = "plan/design/shape"

	if got := renderGateFindings(nil); got != "" {
		t.Errorf("renderGateFindings(nil) = %q, want %q", got, "")
	}

	one := []response.Finding{{Severity: response.SeverityMajor, Location: "plan/design/other", Text: "worse\n  than   before"}}
	if got, want := renderGateFindings(one), "- major plan/design/other worse than before"; got != want {
		t.Errorf("renderGateFindings(one) = %q, want %q", got, want)
	}

	longText := strings.Repeat("x", 250) + "é"
	long := []response.Finding{{Severity: response.SeverityMinor, Location: shapeLocation, Text: longText}}
	gotLong := renderGateFindings(long)
	wantPrefix := "- minor " + shapeLocation + " " + strings.Repeat("x", gateFindingTextMaxRunes)
	if gotLong != wantPrefix {
		t.Errorf("renderGateFindings(long) = %q, want %q", gotLong, wantPrefix)
	}

	multibyteText := strings.Repeat("é", 250)
	multibyte := []response.Finding{{Severity: response.SeverityMinor, Location: shapeLocation, Text: multibyteText}}
	gotMultibyte := renderGateFindings(multibyte)
	wantMultibyteText := strings.Repeat("é", gateFindingTextMaxRunes)
	wantMultibyte := "- minor " + shapeLocation + " " + wantMultibyteText
	if gotMultibyte != wantMultibyte {
		t.Errorf("renderGateFindings(multibyte) = %q, want %q", gotMultibyte, wantMultibyte)
	}
	if !utf8.ValidString(gotMultibyte) {
		t.Errorf("renderGateFindings(multibyte) = %q, want valid UTF-8", gotMultibyte)
	}

	two := []response.Finding{
		{Severity: response.SeverityMinor, Location: shapeLocation, Text: "still wrong"},
		{Severity: response.SeverityMajor, Location: "plan/design/other", Text: "worse"},
	}
	gotTwo := renderGateFindings(two)
	wantTwo := "- minor " + shapeLocation + " still wrong\n- major plan/design/other worse"
	if gotTwo != wantTwo {
		t.Errorf("renderGateFindings(two) = %q, want %q", gotTwo, wantTwo)
	}
}

// TestFloorResumeInputs proves floorResumeInputs' split (ticket 72 task 1):
// a minor plus a major gives two inputs, "findings" and "needs_disposition",
// both fenced; only a minor gives one "findings" input; only a major gives
// one "needs_disposition" input; no findings gives nil.
func TestFloorResumeInputs(t *testing.T) {
	t.Parallel()

	minor := response.Finding{Severity: response.SeverityMinor, Location: "plan/design/shape", Text: "needs a name", Fix: "name it"}
	major := response.Finding{Severity: response.SeverityMajor, Location: testObjectiveLocation, Text: "wrong goal", Fix: "restate it"}

	t.Run("minor and major", func(t *testing.T) {
		t.Parallel()
		got := floorResumeInputs([]response.Finding{minor, major}, response.SeverityMinor)
		if len(got) != 2 {
			t.Fatalf("floorResumeInputs = %+v, want 2 inputs", got)
		}
		if got[0].Label != "findings" || !got[0].Untrusted || !strings.Contains(got[0].Text, minor.Text) {
			t.Errorf("got[0] = %+v, want the fenced findings input carrying the minor", got[0])
		}
		if got[1].Label != needsDispositionLabel || !got[1].Untrusted || !strings.Contains(got[1].Text, major.Text) {
			t.Errorf("got[1] = %+v, want the fenced needs_disposition input carrying the major", got[1])
		}
	})

	t.Run("only minor", func(t *testing.T) {
		t.Parallel()
		got := floorResumeInputs([]response.Finding{minor}, response.SeverityMinor)
		if len(got) != 1 || got[0].Label != "findings" {
			t.Fatalf("floorResumeInputs = %+v, want one findings input", got)
		}
	})

	t.Run("only major", func(t *testing.T) {
		t.Parallel()
		got := floorResumeInputs([]response.Finding{major}, response.SeverityMinor)
		if len(got) != 1 || got[0].Label != needsDispositionLabel {
			t.Fatalf("floorResumeInputs = %+v, want one needs_disposition input", got)
		}
	})

	t.Run("no findings", func(t *testing.T) {
		t.Parallel()
		if got := floorResumeInputs(nil, response.SeverityMinor); got != nil {
			t.Errorf("floorResumeInputs(nil) = %+v, want nil", got)
		}
	})
}

// checkDispositionsTestPlan builds a minimal valid response.Plan with one
// task, so planXMLFor's rendering gives checkDispositions a real plan/...
// element tree to resolve a fixed disposition's path against.
func checkDispositionsTestPlan() response.Plan {
	return response.Plan{
		Overview: response.Overview{
			Objective: "x", Context: "y",
			Problem:  response.Problem{Text: "z"},
			Goals:    []string{"g"},
			NonGoals: []string{"n"},
		},
		Design: response.Design{Demo: response.Demo{Cmd: "c", Text: "d"}, Shape: "s"},
		Delivery: response.Delivery{
			Files: []response.FileChange{{Path: "f", Action: response.FileActionModify, Reason: "r"}},
			Tests: []response.TestCase{{Name: "t", Seam: "s", Kind: response.TestKindIntegration, Asserts: "a"}},
			Tasks: []response.Task{{N: 1, Test: "t", Text: "do it"}},
		},
		Review: response.Review{TrustRoot: "none", Alternatives: []string{"alt"}, Risks: []string{"risk"}},
	}
}

// TestCheckDispositions proves checkDispositions' own table of rules
// (ticket 72 task 2): a valid fixed or disputed entry gives no errors; a
// required finding with no disposition gives one error at
// plan/dispositions naming it; an id not in the needs_disposition input
// gives an error at its own disposition[n] path, in addition to the
// missing-required error; a repeated id gives an error at the second
// disposition[n]; fixed with no path, or with a path that does not
// resolve in the plan, gives an error; disputed with a blank reason gives
// an error; an unknown kind -- the one branch Layer 1 already refuses in
// a real run -- gives an error too; and with nothing required and nothing
// delivered, it returns nil.
func TestCheckDispositions(t *testing.T) {
	t.Parallel()

	planXML, err := planXMLFor(checkDispositionsTestPlan())
	if err != nil {
		t.Fatalf("planXMLFor: %v", err)
	}

	const dispositionOne = "plan/dispositions/disposition[1]"
	required := []response.Finding{{
		ID: requiredID, Severity: response.SeverityMajor, Location: testObjectiveLocation,
		Text: "wrong goal", Fix: "restate it",
	}}

	tests := []struct {
		name      string
		required  []response.Finding
		ds        []response.Disposition
		wantPaths []string // one PathError per entry, Path values, in order
	}{
		{
			name:     "valid fixed",
			required: required,
			ds:       []response.Disposition{{Finding: requiredID, Kind: response.DispositionFixed, Path: testObjectiveLocation}},
		},
		{
			name:     "valid disputed",
			required: required,
			ds:       []response.Disposition{{Finding: requiredID, Kind: response.DispositionDisputed, Reason: "the objective already says this"}},
		},
		{
			name:      "missing disposition",
			required:  required,
			ds:        nil,
			wantPaths: []string{"plan/dispositions"},
		},
		{
			name:     "undelivered id",
			required: required,
			ds:       []response.Disposition{{Finding: "p9-f9", Kind: response.DispositionFixed, Path: testObjectiveLocation}},
			wantPaths: []string{
				dispositionOne,      // p9-f9 not in the input
				"plan/dispositions", // requiredID still missing
			},
		},
		{
			name:     "repeated id",
			required: required,
			ds: []response.Disposition{
				{Finding: requiredID, Kind: response.DispositionFixed, Path: testObjectiveLocation},
				{Finding: requiredID, Kind: response.DispositionFixed, Path: testObjectiveLocation},
			},
			wantPaths: []string{"plan/dispositions/disposition[2]"},
		},
		{
			name:      "fixed with no path",
			required:  required,
			ds:        []response.Disposition{{Finding: requiredID, Kind: response.DispositionFixed}},
			wantPaths: []string{dispositionOne},
		},
		{
			name:      "fixed path does not resolve",
			required:  required,
			ds:        []response.Disposition{{Finding: requiredID, Kind: response.DispositionFixed, Path: "plan/delivery/tasks/task[9]"}},
			wantPaths: []string{dispositionOne},
		},
		{
			name:      "disputed with blank reason",
			required:  required,
			ds:        []response.Disposition{{Finding: requiredID, Kind: response.DispositionDisputed}},
			wantPaths: []string{dispositionOne},
		},
		{
			name:      "kind ignored",
			required:  required,
			ds:        []response.Disposition{{Finding: requiredID, Kind: response.DispositionKind("ignored")}},
			wantPaths: []string{dispositionOne},
		},
		{
			name:     "nothing required, nothing delivered",
			required: nil,
			ds:       nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			errs := checkDispositions(tt.required, tt.ds, []byte(planXML))
			if len(errs) != len(tt.wantPaths) {
				t.Fatalf("checkDispositions = %+v, want %d error(s) at %v", errs, len(tt.wantPaths), tt.wantPaths)
			}
			for i, wantPath := range tt.wantPaths {
				if errs[i].Path != wantPath {
					t.Errorf("errs[%d].Path = %q, want %q (full: %+v)", i, errs[i].Path, wantPath, errs)
				}
			}
		})
	}
}

// TestMarkReopened proves markReopened's own rule (ticket 72 task 3, owner
// decision Q3): a new above-floor finding at the exact location of a
// previous above-floor finding marked fixed or left without a disposition
// gets Reopens and ReopensAfter set; a disputed match, a different
// location, an at-or-below-floor new finding, and a previous finding with
// no id set nothing. It also proves renderFindings' own reopens suffix on
// the fixed and no-disposition rows.
func TestMarkReopened(t *testing.T) {
	t.Parallel()

	// prevMajor is the one previous above-floor finding every row below
	// matches (or deliberately fails to match) against; prevNoID is the
	// same finding with its id cleared, for the "no id" row.
	prevMajor := response.Finding{
		ID: requiredID, Severity: response.SeverityMajor, Location: testObjectiveLocation,
		Text: "stale objective finding", Fix: "restate the objective",
	}
	prevNoID := prevMajor
	prevNoID.ID = ""

	// newMajor is the shape every row's own new finding takes, varied only
	// by Location or Severity where a row needs to; differentLocation
	// reuses TestFloorResumeInputs' own shape location rather than
	// introducing a second plan path literal here.
	newMajor := response.Finding{Severity: response.SeverityMajor, Location: testObjectiveLocation, Text: "still wrong", Fix: "fix it"}
	belowFloor := newMajor
	belowFloor.Severity = response.SeverityMinor
	differentLocation := newMajor
	differentLocation.Location = "plan/design/shape"

	fixedDisposition := []response.Disposition{{Finding: requiredID, Kind: response.DispositionFixed, Path: testObjectiveLocation}}

	tests := []struct {
		name         string
		findings     []response.Finding
		prev         []response.Finding
		ds           []response.Disposition
		wantReopens  string
		wantAfter    string
		wantRenderIn string // rendered line must contain this when wantReopens != ""
	}{
		{
			name:         "fixed match",
			findings:     []response.Finding{newMajor},
			prev:         []response.Finding{prevMajor},
			ds:           fixedDisposition,
			wantReopens:  requiredID,
			wantAfter:    reopensAfterFixed,
			wantRenderIn: "was marked fixed",
		},
		{
			name:         "no disposition match",
			findings:     []response.Finding{newMajor},
			prev:         []response.Finding{prevMajor},
			ds:           nil,
			wantReopens:  requiredID,
			wantAfter:    reopensAfterNoDisposition,
			wantRenderIn: "got no disposition",
		},
		{
			name:     "disputed match sets nothing",
			findings: []response.Finding{newMajor},
			prev:     []response.Finding{prevMajor},
			ds:       []response.Disposition{{Finding: requiredID, Kind: response.DispositionDisputed, Reason: "not a bug"}},
		},
		{
			name:     "different location sets nothing",
			findings: []response.Finding{differentLocation},
			prev:     []response.Finding{prevMajor},
			ds:       fixedDisposition,
		},
		{
			name:     "at-or-below-floor finding sets nothing",
			findings: []response.Finding{belowFloor},
			prev:     []response.Finding{prevMajor},
			ds:       fixedDisposition,
		},
		{
			name:     "previous finding with no id sets nothing",
			findings: []response.Finding{newMajor},
			prev:     []response.Finding{prevNoID},
			ds:       fixedDisposition,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			findings := make([]response.Finding, len(tt.findings))
			copy(findings, tt.findings)
			markReopened(findings, tt.prev, tt.ds, response.SeverityMinor)
			if findings[0].Reopens != tt.wantReopens {
				t.Errorf("Reopens = %q, want %q", findings[0].Reopens, tt.wantReopens)
			}
			if findings[0].ReopensAfter != tt.wantAfter {
				t.Errorf("ReopensAfter = %q, want %q", findings[0].ReopensAfter, tt.wantAfter)
			}
			if tt.wantReopens != "" {
				rendered := renderFindings(findings)
				if !strings.HasSuffix(rendered, tt.wantRenderIn+")") {
					t.Errorf("renderFindings = %q, want it to end with %q)", rendered, tt.wantRenderIn)
				}
			}
		})
	}
}

// TestPostRunFailedWhatFor covers postRunFailedWhatFor's own table (design
// section 4.1): the four origins runAndRoute already threads through
// postRunFailure, the three origins Package 8 adds ahead of their own
// callers, the three origins Package 9 adds the same way, and an unknown
// origin's fallback sentence.
func TestPostRunFailedWhatFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		origin response.EscalationOrigin
		want   string
	}{
		{response.EscalationOriginClassify, "classifying the ticket"},
		{response.EscalationOriginPlanningFirst, "storing or checking the plan"},
		{response.EscalationOriginPlanningResume, "storing or checking the plan"},
		{response.EscalationOriginPlanreview, "storing the plan review"},
		{response.EscalationOriginBuild, "storing or checking the build result"},
		{response.EscalationOriginFix, "storing or checking the build result"},
		{response.EscalationOriginPerimeter, "storing the perimeter description"},
		{response.EscalationOriginReview, "storing the review findings"},
		{response.EscalationOriginJudge, "storing or checking the verdicts"},
		{response.EscalationOriginRespond, "storing the thread actions"},
		{response.EscalationOriginSeal, "storing or checking the agent's result"},
	}
	for _, tt := range tests {
		t.Run(string(tt.origin), func(t *testing.T) {
			t.Parallel()
			if got := postRunFailedWhatFor(tt.origin); got != tt.want {
				t.Errorf("postRunFailedWhatFor(%s) = %q, want %q", tt.origin, got, tt.want)
			}
		})
	}
}

// TestRenderAnswerText_MarksARevisedAnswer proves D30: when a question
// carries more than one sent answer (the console now lets an
// already-answered question take a revised draft while the ticket still
// waits), the first reads as an ordinary pick and every later one is
// marked "(revised)", so the resume prompt reads it as superseding the
// pick(s) before it rather than a second, unrelated choice.
func TestRenderAnswerText_MarksARevisedAnswer(t *testing.T) {
	t.Parallel()
	qp := response.QuestionPayload{
		Key: "Q1", Options: []response.Option{{Key: "a", Text: "Plain hello"}, {Key: "b", Text: "hello, world"}},
	}
	q := store.MessageRow{Message: store.Message{Body: "Q1\n\nHow should the greeting read?"}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped

	firstPayload, err := json.Marshal(response.AnswerPayload{Option: new("a")})
	if err != nil {
		t.Fatalf("marshal first answer payload: %v", err)
	}
	revisedPayload, err := json.Marshal(response.AnswerPayload{Option: new("b")})
	if err != nil {
		t.Fatalf("marshal revised answer payload: %v", err)
	}
	answers := []store.MessageRow{
		{Message: store.Message{Payload: firstPayload}},   //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{Message: store.Message{Payload: revisedPayload}}, //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
	}

	got := renderAnswerText(q, qp, answers, nil)
	wantFirst := "chose a: Plain hello\n"
	wantRevised := "chose b: hello, world (revised)\n"
	if !strings.Contains(got, wantFirst) {
		t.Errorf("renderAnswerText missing the first, unmarked pick %q; got:\n%s", wantFirst, got)
	}
	if !strings.Contains(got, wantRevised) {
		t.Errorf("renderAnswerText missing the (revised) second pick %q; got:\n%s", wantRevised, got)
	}
}
