// judging_amend_test.go tests #57's own amendment flow end to end (design
// section 7.1, 5.6): a judge cannot_run with an amendment escalates with
// Accept/Edit it/Abandon instead of Retry/Abandon, Accept applies the
// amendment through HandlerCommit.ScenarioEdit and starts a fresh judge
// round, a refused amendment (an unknown scenario id, or a check
// checkScenarioRules refuses) drops to a plain cannot_run escalation, and
// Abandon keeps today's abandon path. It reuses judging_test.go's own
// harness (judgeTicketReady, judgeAdvanceStart, judgeScriptsFS) and
// postbuild_test.go's own (pbClaim, pbApply, pbGetTicket, pbRunPrelude,
// pbAnswerEscalation), package job (unreachable from job_test).
package job

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// judgeAmendmentS2Reason and judgeAmendmentS2Check are the reason and check
// text TestJudgeAmendmentAccept's own script proposes for judgeTicketReady's
// s2 (kind negative, no check of its own). judgeAmendmentS2OldCheck is the
// check TestJudgeAmendmentAccept gives s2 before running the judge, through
// Store.OwnerEdit, so the escalation's Now/Amended diff and the owner_edit
// event's old text both have a real old check to carry (#57, r2f5 triage).
const (
	judgeAmendmentS2Reason   = "s2 had no check at all; a non-2xx POST response proves the then"
	judgeAmendmentS2Check    = "! curl -sf -X POST localhost:8080/hello"
	judgeAmendmentS2OldCheck = "! curl -sf localhost:8080/hello"
)

// judgeAmendmentNowLabel and judgeAmendmentAmendedLabel are
// judgeAmendmentDiff's own "Now:" and "Amended:" headings (judging.go),
// named once so goconst has nothing to flag across this file's and
// judging_amend_internal_test.go's own assertions against them.
const (
	judgeAmendmentNowLabel     = "Now:"
	judgeAmendmentAmendedLabel = "Amended:"
)

// judgeAmendmentS2ErrorScript is a judge cannot_run document proposing a
// usable amendment for judgeTicketReady's own s2: s2 never got a check of
// its own, so the judge proposes one and a reason, with no kind element
// (resolved to s2's current kind, negative).
const judgeAmendmentS2ErrorScript = `<zing job="judge" outcome="error">
  <error code="cannot_run">
    <what>s2 has no check to run</what>
    <why>the sealed scenario never got a check, so the judge cannot verify the then</why>
    <tried>looked for a check field on s2</tried>
    <amendment scenario="s2">
      <given>the server is running</given>
      <when>a client sends POST /hello</when>
      <then>the response is 405, since only GET is registered for the route</then>
      <check>` + judgeAmendmentS2Check + `</check>
      <reason>` + judgeAmendmentS2Reason + `</reason>
    </amendment>
  </error>
</zing>`

// judgeAmendmentRefusedScript amends s2 to kind behavior with a check that
// starts a nested sandbox (#78): checkScenarioRules refuses it, so
// judgeErrorCommit must drop the amendment rather than offer it.
const judgeAmendmentRefusedScript = `<zing job="judge" outcome="error">
  <error code="cannot_run">
    <what>s2's check cannot run as written</what>
    <why>the sandbox refuses to start another sandbox</why>
    <tried>ran the check as written</tried>
    <amendment scenario="s2" kind="behavior">
      <given>the server is running</given>
      <when>a client sends POST /hello</when>
      <then>the response is 405, since only GET is registered for the route</then>
      <check>sandbox-exec -p x true</check>
      <reason>the amended check starts another sandbox</reason>
    </amendment>
  </error>
</zing>`

// judgeAmendmentUnknownScenarioScript amends a scenario id (s12) that
// judgeTicketReady's own two-scenario cohort (s1, s2) does not have.
const judgeAmendmentUnknownScenarioScript = `<zing job="judge" outcome="error">
  <error code="cannot_run">
    <what>s12's check cannot run</what>
    <why>s12 does not exist in this cohort</why>
    <tried></tried>
    <amendment scenario="s12">
      <given>g</given>
      <when>w</when>
      <then>th</then>
      <check>true</check>
      <reason>testing the unknown-scenario refusal</reason>
    </amendment>
  </error>
</zing>`

// judgeAmendmentHostCheck is the host command TestJudgeAmendmentAcceptHostKind
// amends s2's check to.
const judgeAmendmentHostCheck = "echo host-check-ok"

// judgeAmendmentHostScript amends s2 to kind host.
const judgeAmendmentHostScript = `<zing job="judge" outcome="error">
  <error code="cannot_run">
    <what>s2 needs to run on the owner's machine</what>
    <why>s2 has no check, and the behavior it tests only runs outside any sandbox</why>
    <tried>ran s2 with no check</tried>
    <amendment scenario="s2" kind="host">
      <given>the server is running</given>
      <when>a client sends POST /hello</when>
      <then>the response is 405, since only GET is registered for the route</then>
      <check>` + judgeAmendmentHostCheck + `</check>
      <reason>s2 needs the owner's own machine, not the build sandbox</reason>
    </amendment>
  </error>
</zing>`

// judgeOpenQuestionPayload returns ticketID's one open question's id and its
// decoded payload, failing unless there is exactly one.
func judgeOpenQuestionPayload(t *testing.T, s *store.Store, ticketID int64) (int64, response.QuestionPayload) {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %d questions, want exactly 1", len(open))
	}
	var qp response.QuestionPayload
	if err := json.Unmarshal(open[0].Payload, &qp); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	return open[0].ID, qp
}

// judgeOpenAmendedQuestion is judgeOpenQuestionPayload, failing unless the
// open question's payload carries a non-nil Amendment.
func judgeOpenAmendedQuestion(t *testing.T, s *store.Store, ticketID int64) (int64, response.QuestionPayload) {
	t.Helper()
	qID, qp := judgeOpenQuestionPayload(t, s, ticketID)
	if qp.Amendment == nil {
		t.Fatalf("question %d payload has no amendment", qID)
	}
	return qID, qp
}

// judgeAssertAmendedOptions fails unless qp offers exactly "Accept the
// amended check" (a), "Edit it" (b) and "Abandon" (c), recommending a.
func judgeAssertAmendedOptions(t *testing.T, qp response.QuestionPayload) {
	t.Helper()
	wantOptions := []response.Option{
		{Key: "a", Text: "Accept the amended check"},
		{Key: "b", Text: "Edit it"},
		{Key: "c", Text: "Abandon"},
	}
	if !slices.Equal(qp.Options, wantOptions) {
		t.Errorf("options = %+v, want %+v", qp.Options, wantOptions)
	}
	if qp.Recommended != "a" {
		t.Errorf("recommended = %q, want %q", qp.Recommended, "a")
	}
}

// judgeAssertPlainOptions fails unless qp offers exactly "Retry" (a) and
// "Abandon" (c), with no amendment (judgeTicketReady's own ticket state,
// "judging", is post-seal, so a plain cannot_run never offers "Back to
// planning" either).
func judgeAssertPlainOptions(t *testing.T, qp response.QuestionPayload) {
	t.Helper()
	wantOptions := []response.Option{{Key: "a", Text: "Retry"}, {Key: "c", Text: "Abandon"}}
	if !slices.Equal(qp.Options, wantOptions) {
		t.Errorf("options = %+v, want %+v", qp.Options, wantOptions)
	}
	if qp.Amendment != nil {
		t.Errorf("question amendment = %+v, want nil", qp.Amendment)
	}
}

// judgeAnsweredOption returns the option key of the one sent "answer"
// message parented to qID, failing unless there is exactly one.
func judgeAnsweredOption(t *testing.T, s *store.Store, ticketID, qID int64) string {
	t.Helper()
	all, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var found *response.AnswerPayload
	for i := range all {
		m := &all[i]
		if m.Type != msgTypeAnswer || m.ParentID == nil || *m.ParentID != qID {
			continue
		}
		var ap response.AnswerPayload
		if err := json.Unmarshal(m.Payload, &ap); err != nil {
			t.Fatalf("unmarshal answer payload for question %d: %v", qID, err)
		}
		if found != nil {
			t.Fatalf("question %d has more than one answer message", qID)
		}
		found = &ap
	}
	if found == nil || found.Option == nil {
		t.Fatalf("question %d has no option answer", qID)
	}
	return *found.Option
}

// judgeOwnerEditEvents returns ticketID's owner_edit events, decoded.
func judgeOwnerEditEvents(t *testing.T, s *store.Store, ticketID int64) []response.OwnerEditEvent {
	t.Helper()
	rows, err := s.Events(t.Context(), ticketID, store.EventKindOwnerEdit, store.EventFilter{})
	if err != nil {
		t.Fatalf("Events(owner_edit): %v", err)
	}
	out := make([]response.OwnerEditEvent, len(rows))
	for i := range rows {
		if err := json.Unmarshal(rows[i].Payload, &out[i]); err != nil {
			t.Fatalf("unmarshal owner_edit event %d: %v", i, err)
		}
	}
	return out
}

// judgeScenarioS2 returns ticketID's current sealed s2, failing unless the
// cohort has one: every test in this file amends s2, never s1.
func judgeScenarioS2(t *testing.T, deps Deps, ticket store.Ticket) response.Scenario {
	t.Helper()
	scenarios, err := judgeScenariosFor(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("judgeScenariosFor: %v", err)
	}
	for _, sc := range scenarios {
		if sc.ID == "s2" {
			return sc
		}
	}
	t.Fatalf("scenario s2 not found in %+v", scenarios)
	return response.Scenario{}
}

// ---- TestJudgeAmendmentAccept ----------------------------------------------

// TestJudgeAmendmentAccept proves the whole amendment flow end to end
// (#57): a judge cannot_run with an amendment escalates with options
// "Accept the amended check", "Edit it" and "Abandon", recommending accept,
// and the escalation body shows the reason and the old and new check side
// by side; accepting it edits the sealed scenario, writes one owner_edit
// event carrying the judge's reason, and starts judge round 2.
func TestJudgeAmendmentAccept(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	if err := s.OwnerEdit(t.Context(), store.OwnerEditRequest{
		TicketID: ticket.ID, Target: store.OwnerEditScenario, Ref: "s2", Action: store.OwnerEditActionEdit,
		Check: new(judgeAmendmentS2OldCheck),
	}); err != nil {
		t.Fatalf("OwnerEdit (give s2 an old check): %v", err)
	}
	rt := runtime.NewFake(judgeScriptsFS(judgeAmendmentS2ErrorScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	oldS2 := judgeScenarioS2(t, deps, ticket)
	if oldS2.Check != judgeAmendmentS2OldCheck {
		t.Fatalf("s2 check = %q before the run, want %q", oldS2.Check, judgeAmendmentS2OldCheck)
	}
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want one")
	}
	if commit.Escalation.Payload.Amendment == nil {
		t.Fatal("commit.Escalation.Payload.Amendment is nil, want the resolved amendment")
	}
	body := commit.Escalation.Body
	for _, want := range []string{
		"Reason: " + judgeAmendmentS2Reason,
		judgeAmendmentNowLabel, judgeAmendmentAmendedLabel, judgeAmendmentS2OldCheck, judgeAmendmentS2Check,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("escalation body = %q, want it to contain %q", body, want)
		}
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	qID, qp := judgeOpenAmendedQuestion(t, s, ticket.ID)
	judgeAssertAmendedOptions(t, qp)

	open, openErr := s.QuestionsByState(t.Context(), ticket.ID, "open")
	if openErr != nil || len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %+v (err %v), want exactly 1", open, openErr)
	}
	for _, want := range []string{
		"Reason: " + judgeAmendmentS2Reason,
		judgeAmendmentNowLabel, judgeAmendmentAmendedLabel, judgeAmendmentS2OldCheck, judgeAmendmentS2Check,
	} {
		if !strings.Contains(open[0].Body, want) {
			t.Errorf("stored question body = %q, want it to contain %q", open[0].Body, want)
		}
	}

	pbAnswerEscalation(t, s, ticket.ID, qID, "a")

	deps2 := pbClaim(t, s, rt, ticket.ID)
	acceptCommit, handled := pbRunPrelude(t, s, deps2, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(acceptCommit.Runs) != 0 {
		t.Errorf("acceptCommit.Runs = %+v, want none (the next tick runs round 2's first turn)", acceptCommit.Runs)
	}
	ticket = pbGetTicket(t, s, ticket.ID)

	sc := judgeScenarioS2(t, deps2, ticket)
	if sc.Check != judgeAmendmentS2Check {
		t.Errorf("s2 check = %q, want %q", sc.Check, judgeAmendmentS2Check)
	}

	// Two events: the test's own setup edit that gave s2 its old check, and
	// the accept's own amendment edit, which is the one every assertion
	// below reads.
	events := judgeOwnerEditEvents(t, s, ticket.ID)
	if len(events) != 2 {
		t.Fatalf("owner_edit events = %d, want 2 (the setup edit and the accept)", len(events))
	}
	accept := events[1]
	if !strings.Contains(accept.New, judgeAmendmentS2Check) {
		t.Errorf("event new = %q, want it to contain %q", accept.New, judgeAmendmentS2Check)
	}
	if accept.Reason != judgeAmendmentS2Reason {
		t.Errorf("event reason = %q, want %q", accept.Reason, judgeAmendmentS2Reason)
	}
	if !strings.Contains(accept.Old, oldS2.Then) {
		t.Errorf("event old = %q, want it to contain the pre-accept s2 then %q", accept.Old, oldS2.Then)
	}
	if !strings.Contains(accept.Old, judgeAmendmentS2OldCheck) {
		t.Errorf("event old = %q, want it to contain the pre-accept s2 check %q", accept.Old, judgeAmendmentS2OldCheck)
	}
	if strings.Contains(accept.Old, judgeAmendmentS2Check) {
		t.Errorf("event old = %q, want it not to contain the amended check %q", accept.Old, judgeAmendmentS2Check)
	}

	markers, err := s.MarkersWithPrefix(t.Context(), ticket.ID, judgeRoundMarkerPrefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	if len(markers) == 0 {
		t.Fatal("no judge round markers, want at least one")
	}
	newest, _, _ := strings.Cut(markers[len(markers)-1].Body, "\n")
	if !strings.HasPrefix(newest, "judge round 2 started sha ") {
		t.Errorf("newest judge round marker = %q, want it to start with %q", newest, "judge round 2 started sha ")
	}

	if open, openErr := s.QuestionsByState(t.Context(), ticket.ID, "open"); openErr != nil || len(open) != 0 {
		t.Errorf("open questions = %+v (err %v), want none", open, openErr)
	}
}

// ---- TestJudgeAmendmentAcceptRefusedAtAccept -------------------------------

// TestJudgeAmendmentAcceptRefusedAtAccept proves acceptAmendment's own
// refusal branch (#57, r1f5): judgeAmendment runs again at accept time
// against the ticket's current sealed cohort, so an amendment that named a
// scenario at escalation time but no longer resolves (here, an escalation
// built by hand naming s12, which judgeTicketReady's cohort never had)
// re-escalates plain cannot_run with the refusal appended to Tried, rather
// than writing a ScenarioEdit or starting a new round.
func TestJudgeAmendmentAcceptRefusedAtAccept(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS())
	deps := pbClaim(t, s, rt, ticket.ID)

	commit := escalationCommit(ticket, deps, nil, nil, string(response.EscalationCodeCannotRun),
		"s12's check cannot run", "testing the accept-time refusal", "",
		response.EscalationOriginJudge)
	commit.Escalation.Payload.Amendment = &response.Amendment{
		Scenario: "s12", Given: "g", When: "w", Then: "th", Check: "true",
		Reason: "testing the accept-time refusal",
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	qID, _ := judgeOpenAmendedQuestion(t, s, ticket.ID)
	pbAnswerEscalation(t, s, ticket.ID, qID, "a")

	deps2 := pbClaim(t, s, rt, ticket.ID)
	reCommit, handled := pbRunPrelude(t, s, deps2, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if reCommit.Escalation == nil {
		t.Fatal("reCommit.Escalation is nil, want a plain cannot_run re-escalation")
	}
	if reCommit.Escalation.Payload.Amendment != nil {
		t.Errorf("reCommit.Escalation.Payload.Amendment = %+v, want nil", reCommit.Escalation.Payload.Amendment)
	}
	want := judgeAmendmentDroppedPrefix + judgeNoSealedScenarioRefusal + "s12"
	if reCommit.Escalation.Payload.Tried != want {
		t.Errorf("Tried = %q, want %q", reCommit.Escalation.Payload.Tried, want)
	}
	ticket = pbGetTicket(t, s, ticket.ID)

	_, qp := judgeOpenQuestionPayload(t, s, ticket.ID)
	judgeAssertPlainOptions(t, qp)

	if events := judgeOwnerEditEvents(t, s, ticket.ID); len(events) != 0 {
		t.Errorf("owner_edit events = %+v, want none", events)
	}
	markers, err := s.MarkersWithPrefix(t.Context(), ticket.ID, judgeRoundMarkerPrefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	if len(markers) != 0 {
		t.Errorf("judge round markers = %+v, want none (no new round started)", markers)
	}
}

// ---- TestJudgeAmendmentReplayIssue96S9 -------------------------------------

// TestJudgeAmendmentReplayIssue96S9 replays #96 s9, reconstructed from the
// ticket text, re-keyed to judgeTicketReady's own s2 (the cohort here has
// only s1 and s2): applying its given/when/then/check/kind to the sealed
// s2, then driving the same judge cannot_run and Accept flow, needs no
// database edit -- every write goes through Store.OwnerEdit (the seed) or a
// handler commit (the amendment itself).
func TestJudgeAmendmentReplayIssue96S9(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeTicketReady(t)

	seedBytes, err := os.ReadFile("testdata/amend/issue96-s9-scenario.json")
	if err != nil {
		t.Fatalf("read issue96-s9-scenario.json: %v", err)
	}
	var seed response.Scenario
	if unmarshalErr := json.Unmarshal(seedBytes, &seed); unmarshalErr != nil {
		t.Fatalf("unmarshal issue96-s9-scenario.json: %v", unmarshalErr)
	}
	kind := string(seed.Kind)
	if editErr := s.OwnerEdit(t.Context(), store.OwnerEditRequest{
		TicketID: ticket.ID, Target: store.OwnerEditScenario, Ref: "s2", Action: store.OwnerEditActionEdit,
		Given: &seed.Given, When: &seed.When, Then: &seed.Then, Check: &seed.Check, Kind: &kind,
	}); editErr != nil {
		t.Fatalf("OwnerEdit (seed #96 s9 onto s2): %v", editErr)
	}

	errorXML, err := os.ReadFile("testdata/amend/issue96-s9-judge-error.xml")
	if err != nil {
		t.Fatalf("read issue96-s9-judge-error.xml: %v", err)
	}

	rt := runtime.NewFake(judgeScriptsFS(string(errorXML)))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if commit.Escalation == nil || commit.Escalation.Payload.Amendment == nil {
		t.Fatalf("commit.Escalation = %+v, want an escalation carrying the amendment", commit.Escalation)
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	qID, _ := judgeOpenAmendedQuestion(t, s, ticket.ID)
	pbAnswerEscalation(t, s, ticket.ID, qID, "a")

	deps2 := pbClaim(t, s, rt, ticket.ID)
	if _, handled := pbRunPrelude(t, s, deps2, ticket.ID); !handled {
		t.Fatal("handled = false, want true")
	}
	ticket = pbGetTicket(t, s, ticket.ID)

	const wantCheck = `! git diff --name-only origin/main...HEAD -- internal/job | grep -v '_test[.]go$' | grep -q .`
	sc := judgeScenarioS2(t, deps2, ticket)
	if sc.Check != wantCheck {
		t.Errorf("s2 check = %q, want %q", sc.Check, wantCheck)
	}

	markers, err := s.MarkersWithPrefix(t.Context(), ticket.ID, judgeRoundMarkerPrefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	newest, _, _ := strings.Cut(markers[len(markers)-1].Body, "\n")
	if !strings.HasPrefix(newest, "judge round 2 started sha ") {
		t.Errorf("newest judge round marker = %q, want it to start with %q", newest, "judge round 2 started sha ")
	}
}

// ---- TestJudgeAmendmentRefusedByShape --------------------------------------

// TestJudgeAmendmentRefusedByShape proves an amendment checkScenarioRules
// refuses (a nested-sandbox check, #78) is dropped: the escalation carries
// no amendment, offers plain Retry/Abandon, and its own Tried ends with
// "amendment dropped: " plus checkScenarioRules' own message.
func TestJudgeAmendmentRefusedByShape(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeAmendmentRefusedScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want one")
	}
	if commit.Escalation.Payload.Amendment != nil {
		t.Errorf("commit.Escalation.Payload.Amendment = %+v, want nil (refused)", commit.Escalation.Payload.Amendment)
	}

	refusedScenario := response.Scenario{
		ID: "s2", Kind: response.ScenarioKindBehavior,
		Given: "the server is running", When: "a client sends POST /hello",
		Then:  "the response is 405, since only GET is registered for the route",
		Check: "sandbox-exec -p x true",
	}
	ruleErrs := checkScenarioRules(1, refusedScenario)
	if len(ruleErrs) == 0 {
		t.Fatal("checkScenarioRules returned no error for the test's own refused check")
	}
	wantTried := judgeAmendmentDroppedPrefix + ruleErrs[0].Msg
	if !strings.HasSuffix(commit.Escalation.Payload.Tried, wantTried) {
		t.Errorf("Tried = %q, want it to end with %q", commit.Escalation.Payload.Tried, wantTried)
	}

	pbApply(t, s, ticket, commit)
	_, qp := judgeOpenQuestionPayload(t, s, ticket.ID)
	judgeAssertPlainOptions(t, qp)
}

// ---- TestJudgeAmendmentUnknownScenario -------------------------------------

// TestJudgeAmendmentUnknownScenario proves an amendment naming a scenario
// id judgeTicketReady's own cohort does not have (s12) is dropped with the
// owner's own Q2 wording.
func TestJudgeAmendmentUnknownScenario(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeAmendmentUnknownScenarioScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want one")
	}
	if commit.Escalation.Payload.Amendment != nil {
		t.Errorf("commit.Escalation.Payload.Amendment = %+v, want nil (unknown scenario)", commit.Escalation.Payload.Amendment)
	}
	want := judgeAmendmentDroppedPrefix + judgeNoSealedScenarioRefusal + "s12"
	if commit.Escalation.Payload.Tried != want {
		t.Errorf("Tried = %q, want %q", commit.Escalation.Payload.Tried, want)
	}
	const literal = "amendment dropped: no sealed scenario s12"
	if commit.Escalation.Payload.Tried != literal {
		t.Errorf("Tried = %q, want the literal %q", commit.Escalation.Payload.Tried, literal)
	}

	pbApply(t, s, ticket, commit)
	_, qp := judgeOpenQuestionPayload(t, s, ticket.ID)
	judgeAssertPlainOptions(t, qp)
}

// ---- TestJudgeAmendmentAbandon ----------------------------------------------

// TestJudgeAmendmentAbandon proves answering "c" on an amended escalation
// abandons the ticket exactly as abandonCommit does, writing no owner_edit
// event and leaving s2's payload unchanged.
func TestJudgeAmendmentAbandon(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeAmendmentS2ErrorScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	before := judgeScenarioS2(t, deps, ticket)

	qID, _ := judgeOpenAmendedQuestion(t, s, ticket.ID)
	pbAnswerEscalation(t, s, ticket.ID, qID, "c")

	deps2 := pbClaim(t, s, rt, ticket.ID)
	if _, handled := pbRunPrelude(t, s, deps2, ticket.ID); !handled {
		t.Fatal("handled = false, want true")
	}

	final := pbGetTicket(t, s, ticket.ID)
	if final.State != stateAbandoned {
		t.Fatalf("ticket state = %q, want %q", final.State, stateAbandoned)
	}
	if events := judgeOwnerEditEvents(t, s, ticket.ID); len(events) != 0 {
		t.Errorf("owner_edit events = %+v, want none", events)
	}
	after := judgeScenarioS2(t, deps2, final)
	if after != before {
		t.Errorf("s2 = %+v, want unchanged %+v", after, before)
	}
}

// ---- TestJudgeAmendmentAcceptHostKind --------------------------------------

// TestJudgeAmendmentAcceptHostKind proves an amendment that sets kind host
// is accepted like any other: s2 ends up kind host with the amended check,
// and the next judging Run takes the pending host check path
// (runPendingHostCheck) at round 2's own sha, before any judge run.
func TestJudgeAmendmentAcceptHostKind(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeAmendmentHostScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if commit.Escalation == nil || commit.Escalation.Payload.Amendment == nil {
		t.Fatalf("commit.Escalation = %+v, want an escalation carrying the amendment", commit.Escalation)
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	qID, _ := judgeOpenAmendedQuestion(t, s, ticket.ID)
	pbAnswerEscalation(t, s, ticket.ID, qID, "a")

	deps2 := pbClaim(t, s, rt, ticket.ID)
	if _, handled := pbRunPrelude(t, s, deps2, ticket.ID); !handled {
		t.Fatal("handled = false, want true")
	}
	ticket = pbGetTicket(t, s, ticket.ID)

	sc := judgeScenarioS2(t, deps2, ticket)
	if sc.Kind != response.ScenarioKindHost || sc.Check != judgeAmendmentHostCheck {
		t.Fatalf("s2 = %+v, want kind host with check %q", sc, judgeAmendmentHostCheck)
	}

	hostCommands := &judgeScriptedHostCommands{}
	deps3 := pbClaim(t, s, rt, ticket.ID)
	deps3.HostCommands = hostCommands
	hostCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps3)
	if err != nil {
		t.Fatalf("judge run (host check): %v", err)
	}
	if len(hostCommit.Runs) != 0 {
		t.Errorf("hostCommit.Runs = %+v, want none (a host check, not an agent run)", hostCommit.Runs)
	}
	if hostCommands.callCount() != 1 {
		t.Errorf("host runner called %d times, want 1", hostCommands.callCount())
	}
	if got := hostCommands.lastCall().cmd; got != judgeAmendmentHostCheck {
		t.Errorf("host runner cmd = %q, want %q", got, judgeAmendmentHostCheck)
	}
}

// ---- TestJudgeAmendmentEditItStartsRound ------------------------------------

// judgeAmendmentOwnerCheck is the owner's own replacement check
// TestJudgeAmendmentEditItStartsRound saves through the edit box, in place
// of the judge's own proposed judgeAmendmentS2Check.
const judgeAmendmentOwnerCheck = "! curl -sf -X POST localhost:8080/hello -d 'owner wrote this'"

// TestJudgeAmendmentEditItStartsRound proves #57's "Edit it" path (Q3): the
// owner saves the amended-check box with their own text and
// Store.OwnerEdit's AnswerQuestion field, landing the edit and answering
// option b in one transaction; the next Run starts a fresh judge round
// through retryFreshRound, reserving a judge run, rather than the judge's
// own proposed check.
func TestJudgeAmendmentEditItStartsRound(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeAmendmentS2ErrorScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	qID, _ := judgeOpenAmendedQuestion(t, s, ticket.ID)

	if editErr := s.OwnerEdit(t.Context(), store.OwnerEditRequest{
		TicketID: ticket.ID, Target: store.OwnerEditScenario, Ref: "s2", Action: store.OwnerEditActionEdit,
		Check: new(judgeAmendmentOwnerCheck), AnswerQuestion: &qID,
	}); editErr != nil {
		t.Fatalf("OwnerEdit (edit it): %v", editErr)
	}

	deps2 := pbClaim(t, s, rt, ticket.ID)
	sc := judgeScenarioS2(t, deps2, ticket)
	if sc.Check != judgeAmendmentOwnerCheck {
		t.Fatalf("s2 check = %q, want the owner's own %q", sc.Check, judgeAmendmentOwnerCheck)
	}

	if open, openErr := s.QuestionsByState(t.Context(), ticket.ID, "open"); openErr != nil || len(open) != 0 {
		t.Fatalf("open questions = %+v (err %v), want none", open, openErr)
	}
	if got := judgeAnsweredOption(t, s, ticket.ID, qID); got != "b" {
		t.Errorf("answered option = %q, want %q", got, "b")
	}

	roundCommit, handled := pbRunPrelude(t, s, deps2, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(roundCommit.Runs) == 0 {
		t.Error("roundCommit.Runs is empty, want retryFreshRound to reserve a judge run")
	}
	ticket = pbGetTicket(t, s, ticket.ID)

	markers, err := s.MarkersWithPrefix(t.Context(), ticket.ID, judgeRoundMarkerPrefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	newest, _, _ := strings.Cut(markers[len(markers)-1].Body, "\n")
	if !strings.HasPrefix(newest, "judge round 2 started sha ") {
		t.Errorf("newest judge round marker = %q, want it to start with %q", newest, "judge round 2 started sha ")
	}

	if events := judgeOwnerEditEvents(t, s, ticket.ID); len(events) != 1 {
		t.Errorf("owner_edit events = %+v, want exactly 1 (the owner's own edit, not an amendment accept)", events)
	}
}

// TestJudgeAmendmentReplyOnlyNeverAccepts proves Q12's own rule (#57,
// r1f9 triage): a reply with no picked option must never accept an
// amendment. roundRecommendedOption's own qp.Amendment check (planning.go)
// falls an amended escalation's unanswered reply back to
// escalationChoiceBack ("Edit it"), not the stored Recommended "a", even
// though a plain escalation's own unanswered reply does read Recommended
// straight back.
func TestJudgeAmendmentReplyOnlyNeverAccepts(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeAmendmentS2ErrorScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	pbApply(t, s, ticket, commit)
	ticket = pbGetTicket(t, s, ticket.ID)

	before := judgeScenarioS2(t, deps, ticket)

	qID, _ := judgeOpenAmendedQuestion(t, s, ticket.ID)
	if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{
		TicketID: ticket.ID, QuestionID: &qID, Text: "I'm not sure about this one.",
	}); draftErr != nil {
		t.Fatalf("SaveDraft(reply, no option): %v", draftErr)
	}
	if _, sendErr := s.SendBatch(t.Context(), ticket.ID); sendErr != nil {
		t.Fatalf("SendBatch: %v", sendErr)
	}

	deps2 := pbClaim(t, s, rt, ticket.ID)
	roundCommit, handled := pbRunPrelude(t, s, deps2, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(roundCommit.Runs) == 0 {
		t.Error("roundCommit.Runs is empty, want retryFreshRound to reserve a judge run")
	}
	ticket = pbGetTicket(t, s, ticket.ID)

	after := judgeScenarioS2(t, deps2, ticket)
	if after != before {
		t.Errorf("s2 = %+v, want unchanged %+v (a reply with no option must never accept)", after, before)
	}
	if events := judgeOwnerEditEvents(t, s, ticket.ID); len(events) != 0 {
		t.Errorf("owner_edit events = %+v, want none", events)
	}

	markers, err := s.MarkersWithPrefix(t.Context(), ticket.ID, judgeRoundMarkerPrefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	newest, _, _ := strings.Cut(markers[len(markers)-1].Body, "\n")
	if !strings.HasPrefix(newest, "judge round 2 started sha ") {
		t.Errorf("newest judge round marker = %q, want it to start with %q (retryFreshRound, not accept)", newest, "judge round 2 started sha ")
	}
}
