package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"zing/internal/response"
)

// --- fixture helpers shared across this file's tests ------------------------

// insertPlanArtifact inserts a "plan" artifact at version, owned by runID
// (nil for a legacy plan with no run_id), the fixture CurrentCohort reads.
func insertPlanArtifact(t *testing.T, s *Store, ticketID int64, runID *int64, version int) {
	t.Helper()
	if _, err := s.InsertArtifact(t.Context(), Artifact{
		TicketID: ticketID, RunID: runID, Type: testTypePlan, Version: version, Payload: planPayload(),
	}); err != nil {
		t.Fatalf("insert plan artifact v%d: %v", version, err)
	}
}

// insertScenarioArtifact inserts one "scenario" artifact owned by runID (nil
// for a legacy, uncohorted scenario), sealed at sealedAt (nil for unsealed).
func insertScenarioArtifact(t *testing.T, s *Store, ticketID int64, runID *int64, id string, sealedAt *time.Time) {
	t.Helper()
	if _, err := s.InsertArtifact(t.Context(), Artifact{
		TicketID: ticketID, RunID: runID, Type: testTypeScenario, Payload: scenarioPayload(id), SealedAt: sealedAt,
	}); err != nil {
		t.Fatalf("insert scenario artifact %s: %v", id, err)
	}
}

// scenarioID is the one field this file's ScenariosForRun/AllScenarios tests
// need out of a scenario artifact's payload.
type scenarioID struct {
	ID string `json:"id"`
}

// insertSentAnswer inserts a sent "answer" message on questionID, the
// fixture AnsweredRounds' Answers bucket reads.
func insertSentAnswer(t *testing.T, s *Store, ticketID, questionID int64, option string) int64 {
	t.Helper()
	id, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, ParentID: &questionID, Type: msgTypeAnswer, Author: authorYou,
		State: new(answerStateSent), Payload: []byte(fmt.Sprintf(`{"option":%q}`, option)),
	})
	if err != nil {
		t.Fatalf("insert sent answer for question %d: %v", questionID, err)
	}
	return id
}

// insertSentReply inserts a sent "reply" message parented to questionID (nil
// for a bare thread reply), the fixture AnsweredRounds' Replies bucket reads.
func insertSentReply(t *testing.T, s *Store, ticketID int64, questionID *int64, body string) int64 {
	t.Helper()
	id, err := s.InsertMessage(t.Context(), Message{
		TicketID: ticketID, ParentID: questionID, Type: msgTypeReply, Author: authorYou,
		State: new(answerStateSent), Body: body,
	})
	if err != nil {
		t.Fatalf("insert sent reply: %v", err)
	}
	return id
}

// setRunAgentSeconds sets one run's agent_seconds directly, the fixture
// AgentSecondsForTicket sums over.
func setRunAgentSeconds(t *testing.T, s *Store, runID int64, seconds int) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `UPDATE runs SET agent_seconds = ? WHERE id = ?`, seconds, runID); err != nil {
		t.Fatalf("set run %d agent_seconds: %v", runID, err)
	}
}

// setRunOutcome sets one run's outcome directly, the fixture
// ConsecutiveInvalidOutputs walks.
func setRunOutcome(t *testing.T, s *Store, runID int64, outcome string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `UPDATE runs SET outcome = ? WHERE id = ?`, outcome, runID); err != nil {
		t.Fatalf("set run %d outcome: %v", runID, err)
	}
}

// insertUpdateMarker inserts a system "update" message with body, the shape
// every section 5.3 marker takes (pending/delivered, response invalid, seal
// mismatch).
func insertUpdateMarker(t *testing.T, s *Store, ticketID int64, body string) int64 {
	t.Helper()
	id, err := s.InsertMessage(t.Context(), Message{TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem, Body: body})
	if err != nil {
		t.Fatalf("insert update marker %q: %v", body, err)
	}
	return id
}

// insertInvalidMarker inserts the "response invalid run <id>" marker D14
// writes on an invalid-output commit (design section 5.3), naming runID and
// carrying reason on the line after.
func insertInvalidMarker(t *testing.T, s *Store, ticketID, runID int64, reason string) {
	t.Helper()
	insertUpdateMarker(t, s, ticketID, fmt.Sprintf("response invalid run %d\n%s", runID, reason))
}

// testEscalationBodyCapResumes is the escalation body HasEscalation's
// fixtures share across three tests.
const testEscalationBodyCapResumes = "resumes_exhausted: at cap"

// mustMarshalEscalation marshals p, the shape every direct escalation-message
// fixture in this file needs for InsertMessage's payload argument.
func mustMarshalEscalation(t *testing.T, p response.EscalationPayload) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal escalation payload: %v", err)
	}
	return b
}

// --- CurrentCohort (design section 4.5, 6.6 step 1) -------------------------

func TestCurrentCohort_NoPlanReturnsFalse(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	_, ok, err := s.CurrentCohort(ctx, ticketID)
	if err != nil {
		t.Fatalf("CurrentCohort: %v", err)
	}
	if ok {
		t.Error("CurrentCohort with no plan: ok = true, want false")
	}
}

func TestCurrentCohort_MaxVersionWins(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runA := insertRun(t, s, sess)
	runB := insertRun(t, s, sess)
	insertPlanArtifact(t, s, ticketID, &runA, 1)
	insertPlanArtifact(t, s, ticketID, &runB, 2)

	got, ok, err := s.CurrentCohort(ctx, ticketID)
	if err != nil {
		t.Fatalf("CurrentCohort: %v", err)
	}
	if !ok {
		t.Fatal("CurrentCohort: ok = false, want true")
	}
	if got.PlanVersion != 2 {
		t.Errorf("PlanVersion = %d, want 2 (the max version)", got.PlanVersion)
	}
	if got.RunID == nil || *got.RunID != runB {
		t.Errorf("RunID = %v, want %d", got.RunID, runB)
	}
}

func TestCurrentCohort_LegacyPlanNullRunIDReturnsNilRunID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	insertPlanArtifact(t, s, ticketID, nil, 1)

	got, ok, err := s.CurrentCohort(ctx, ticketID)
	if err != nil {
		t.Fatalf("CurrentCohort: %v", err)
	}
	if !ok {
		t.Fatal("CurrentCohort: ok = false, want true")
	}
	if got.RunID != nil {
		t.Errorf("RunID = %v, want nil (legacy plan)", got.RunID)
	}
}

// --- CohortSealState (design D16, section 4.5, 6.6 step 3) ------------------

func TestCohortSealState_NoneSealedCommonAtNil(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)
	insertScenarioArtifact(t, s, ticketID, &runID, "s1", nil)
	insertScenarioArtifact(t, s, ticketID, &runID, "s2", nil)

	total, sealed, commonAt, err := s.CohortSealState(ctx, ticketID, runID)
	if err != nil {
		t.Fatalf("CohortSealState: %v", err)
	}
	if total != 2 || sealed != 0 {
		t.Errorf("total,sealed = %d,%d, want 2,0", total, sealed)
	}
	if commonAt != nil {
		t.Errorf("commonAt = %v, want nil", commonAt)
	}
}

func TestCohortSealState_AllSealedSameInstantReturnsCommonAt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)
	at := time.Now().UTC().Truncate(time.Second)
	insertScenarioArtifact(t, s, ticketID, &runID, "s1", &at)
	insertScenarioArtifact(t, s, ticketID, &runID, "s2", &at)

	total, sealed, commonAt, err := s.CohortSealState(ctx, ticketID, runID)
	if err != nil {
		t.Fatalf("CohortSealState: %v", err)
	}
	if total != 2 || sealed != 2 {
		t.Errorf("total,sealed = %d,%d, want 2,2", total, sealed)
	}
	if commonAt == nil || !commonAt.Equal(at) {
		t.Errorf("commonAt = %v, want %v", commonAt, at)
	}
}

func TestCohortSealState_SealedAtTwoInstantsReturnsNilCommonAt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)
	at1 := time.Now().UTC().Truncate(time.Second)
	at2 := at1.Add(time.Minute)
	insertScenarioArtifact(t, s, ticketID, &runID, "s1", &at1)
	insertScenarioArtifact(t, s, ticketID, &runID, "s2", &at2)

	_, sealed, commonAt, err := s.CohortSealState(ctx, ticketID, runID)
	if err != nil {
		t.Fatalf("CohortSealState: %v", err)
	}
	if sealed != 2 {
		t.Errorf("sealed = %d, want 2", sealed)
	}
	if commonAt != nil {
		t.Errorf("commonAt = %v, want nil (two distinct instants disagree)", commonAt)
	}
}

func TestCohortSealState_DifferentCohortNotCounted(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runA := insertRun(t, s, sess)
	runB := insertRun(t, s, sess)
	insertScenarioArtifact(t, s, ticketID, &runA, "s1", nil)
	at := time.Now().UTC().Truncate(time.Second)
	insertScenarioArtifact(t, s, ticketID, &runB, "s2", &at)

	total, sealed, commonAt, err := s.CohortSealState(ctx, ticketID, runA)
	if err != nil {
		t.Fatalf("CohortSealState: %v", err)
	}
	if total != 1 || sealed != 0 || commonAt != nil {
		t.Errorf("CohortSealState(runA) = %d,%d,%v, want 1,0,nil (runB's sealed row must not count)", total, sealed, commonAt)
	}
}

// --- ScenariosForRun / AllScenarios (design section 7, 8) -------------------

func TestScenariosForRun_OrderedByIDAndSealedOnlyFilters(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)
	at := time.Now().UTC().Truncate(time.Second)
	insertScenarioArtifact(t, s, ticketID, &runID, "s1", &at)
	insertScenarioArtifact(t, s, ticketID, &runID, "s2", nil)
	insertScenarioArtifact(t, s, ticketID, &runID, "s3", &at)

	all, err := s.ScenariosForRun(ctx, ticketID, runID, false)
	if err != nil {
		t.Fatalf("ScenariosForRun(false): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ScenariosForRun(false) = %d rows, want 3", len(all))
	}
	for i, want := range []string{"s1", "s2", "s3"} {
		var got scenarioID
		if uErr := json.Unmarshal(all[i].Payload, &got); uErr != nil {
			t.Fatalf("unmarshal scenario %d: %v", i, uErr)
		}
		if got.ID != want {
			t.Errorf("ScenariosForRun(false)[%d].ID = %q, want %q (insertion order)", i, got.ID, want)
		}
	}

	sealedOnly, err := s.ScenariosForRun(ctx, ticketID, runID, true)
	if err != nil {
		t.Fatalf("ScenariosForRun(true): %v", err)
	}
	if len(sealedOnly) != 2 {
		t.Fatalf("ScenariosForRun(true) = %d rows, want 2 (sealed only)", len(sealedOnly))
	}
}

func TestAllScenarios_OrderedByIDAcrossRunsAndLegacyRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runA := insertRun(t, s, sess)
	runB := insertRun(t, s, sess)
	insertScenarioArtifact(t, s, ticketID, &runA, "s1", nil)
	insertScenarioArtifact(t, s, ticketID, &runB, "s2", nil)
	insertScenarioArtifact(t, s, ticketID, nil, "s3", nil) // legacy: no run_id

	got, err := s.AllScenarios(ctx, ticketID)
	if err != nil {
		t.Fatalf("AllScenarios: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("AllScenarios = %d rows, want 3", len(got))
	}
	for i, want := range []string{"s1", "s2", "s3"} {
		var s2 scenarioID
		if uErr := json.Unmarshal(got[i].Payload, &s2); uErr != nil {
			t.Fatalf("unmarshal scenario %d: %v", i, uErr)
		}
		if s2.ID != want {
			t.Errorf("AllScenarios[%d].ID = %q, want %q (insertion order)", i, s2.ID, want)
		}
	}
}

// --- AnsweredRounds (design section 4.5, 5.1 step 1) ------------------------

func TestAnsweredRounds_TwoRoundsNewestFirst(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	// The round-grouping mechanics under test here are generic to any job;
	// "classify" stands in rather than "planning" because a planning-job
	// session's own questions are excluded from AnsweredRounds entirely
	// (D31, design section 22.1, 22.3).
	const roundJob = "classify"
	sessA := insertSession(t, s, ticketID, roundJob)
	sessB := insertSession(t, s, ticketID, roundJob)
	runA := insertQuestionRun(t, s, sessA)
	runB := insertQuestionRun(t, s, sessB)

	qA := insertOpenQuestion(t, s, ticketID, runA, "Q1")
	markAnswered(t, s, qA)
	insertSentAnswer(t, s, ticketID, qA, "a")

	qB := insertOpenQuestion(t, s, ticketID, runB, "Q2")
	markAnswered(t, s, qB)
	insertSentAnswer(t, s, ticketID, qB, "b")

	rounds, err := s.AnsweredRounds(ctx, ticketID)
	if err != nil {
		t.Fatalf("AnsweredRounds: %v", err)
	}
	if len(rounds) != 2 {
		t.Fatalf("AnsweredRounds = %d rounds, want 2", len(rounds))
	}
	if rounds[0].RunID == nil || *rounds[0].RunID != runB {
		t.Errorf("rounds[0].RunID = %v, want %d (the round with the newest question)", rounds[0].RunID, runB)
	}
	if rounds[1].RunID == nil || *rounds[1].RunID != runA {
		t.Errorf("rounds[1].RunID = %v, want %d", rounds[1].RunID, runA)
	}
	if rounds[0].Job != roundJob || rounds[1].Job != roundJob {
		t.Errorf("rounds Job = [%q, %q], want both %q", rounds[0].Job, rounds[1].Job, roundJob)
	}
	if rounds[0].SessionID == nil || *rounds[0].SessionID != sessB {
		t.Errorf("rounds[0].SessionID = %v, want %d", rounds[0].SessionID, sessB)
	}
	if len(rounds[0].Answers) != 1 || len(rounds[1].Answers) != 1 {
		t.Errorf("each round should have exactly one answer: got %d and %d", len(rounds[0].Answers), len(rounds[1].Answers))
	}
}

func TestAnsweredRounds_RepliesWithNoAnswer(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	// "classify" stands in for the generic mechanics under test; see
	// TestAnsweredRounds_TwoRoundsNewestFirst's own comment.
	sess := insertSession(t, s, ticketID, "classify")
	runID := insertQuestionRun(t, s, sess)

	qID := insertOpenQuestion(t, s, ticketID, runID, "Q1")
	markAnswered(t, s, qID)
	insertSentReply(t, s, ticketID, &qID, "why though")

	rounds, err := s.AnsweredRounds(ctx, ticketID)
	if err != nil {
		t.Fatalf("AnsweredRounds: %v", err)
	}
	if len(rounds) != 1 {
		t.Fatalf("AnsweredRounds = %d rounds, want 1", len(rounds))
	}
	if len(rounds[0].Answers) != 0 {
		t.Errorf("Answers = %+v, want none", rounds[0].Answers)
	}
	if len(rounds[0].Replies) != 1 || rounds[0].Replies[0].Body != "why though" {
		t.Errorf("Replies = %+v, want exactly one reply %q", rounds[0].Replies, "why though")
	}
}

func TestAnsweredRounds_UnrelatedReplyToAnotherParentExcluded(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	// "classify" stands in for the generic mechanics under test; see
	// TestAnsweredRounds_TwoRoundsNewestFirst's own comment.
	sess := insertSession(t, s, ticketID, "classify")
	runID := insertQuestionRun(t, s, sess)

	qID := insertOpenQuestion(t, s, ticketID, runID, "Q1")
	markAnswered(t, s, qID)
	insertSentAnswer(t, s, ticketID, qID, "a")

	otherQID := insertOpenQuestion(t, s, ticketID, runID, "Q2") // a sibling, left open
	insertSentReply(t, s, ticketID, &otherQID, "unrelated")

	rounds, err := s.AnsweredRounds(ctx, ticketID)
	if err != nil {
		t.Fatalf("AnsweredRounds: %v", err)
	}
	if len(rounds) != 1 {
		t.Fatalf("AnsweredRounds = %d rounds, want 1", len(rounds))
	}
	if len(rounds[0].Replies) != 0 {
		t.Errorf("Replies = %+v, want none (the reply's parent is a different, still-open question)", rounds[0].Replies)
	}
}

func TestAnsweredRounds_ParentKeyedRoundFromEscalation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)

	owner, expires := claimForCommit(t, s, ticketID)
	if _, err := s.CommitHandlerResult(ctx, HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Waiting: new(testWaitingQuestions),
		Escalation: &EscalationCommit{
			RunID: nil, Body: testEscalationBodyWallClock,
			Payload: escalationTestPayload(response.EscalationCodeWallClock, response.EscalationOriginCapBudget),
		},
	}); err != nil {
		t.Fatalf("CommitHandlerResult (escalation): %v", err)
	}

	msgs, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var escID, qID int64
	for _, m := range msgs {
		switch m.Type {
		case msgTypeEscalation:
			escID = m.ID
		case msgTypeQuestion:
			qID = m.ID
		}
	}
	if escID == 0 || qID == 0 {
		t.Fatalf("escalation/question not both found in %+v", msgs)
	}

	if _, ansErr := s.AnswerQuestion(ctx, AnswerInput{TicketID: ticketID, QuestionID: qID, Option: "b"}); ansErr != nil {
		t.Fatalf("AnswerQuestion: %v", ansErr)
	}

	rounds, err := s.AnsweredRounds(ctx, ticketID)
	if err != nil {
		t.Fatalf("AnsweredRounds: %v", err)
	}
	if len(rounds) != 1 {
		t.Fatalf("AnsweredRounds = %d rounds, want 1", len(rounds))
	}
	r := rounds[0]
	if r.RunID != nil {
		t.Errorf("RunID = %v, want nil (a cap escalation no run caused)", r.RunID)
	}
	if r.SessionID != nil {
		t.Errorf("SessionID = %v, want nil", r.SessionID)
	}
	if r.Job != "" {
		t.Errorf("Job = %q, want empty", r.Job)
	}
	if r.ParentID == nil || *r.ParentID != escID {
		t.Errorf("ParentID = %v, want %d (the escalation's own id)", r.ParentID, escID)
	}
	if len(r.Answers) != 1 {
		t.Fatalf("Answers = %d, want 1", len(r.Answers))
	}
}

func TestAnsweredRounds_ResolvedQuestionExcluded(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	setTicketState(t, s, ticketID, testStatePlanning)
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertQuestionRun(t, s, sess)

	qID := insertOpenQuestion(t, s, ticketID, runID, "Q1")
	markAnswered(t, s, qID)
	insertSentAnswer(t, s, ticketID, qID, "a")
	if _, err := s.db.ExecContext(ctx, `UPDATE messages SET state = ? WHERE id = ?`, questionStateResolved, qID); err != nil {
		t.Fatalf("resolve question: %v", err)
	}

	rounds, err := s.AnsweredRounds(ctx, ticketID)
	if err != nil {
		t.Fatalf("AnsweredRounds: %v", err)
	}
	if len(rounds) != 0 {
		t.Errorf("AnsweredRounds = %+v, want none (the question is resolved)", rounds)
	}
}

// --- RunContext (design section 4.5) ----------------------------------------

func TestRunContext_ReturnsJobAndTicket(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)

	job, gotTicketID, err := s.RunContext(ctx, runID)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	if job != testStatePlanning {
		t.Errorf("job = %q, want %q", job, testStatePlanning)
	}
	if gotTicketID != ticketID {
		t.Errorf("ticketID = %d, want %d", gotTicketID, ticketID)
	}
}

func TestRunContext_UnknownRunWrapsErrNoRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	_, _, err := s.RunContext(ctx, 999)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("RunContext(unknown run): err = %v, want it to wrap sql.ErrNoRows", err)
	}
}

// --- ProjectForTicket --------------------------------------------------------

func TestProjectForTicket_ReturnsTheTicketsProject(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	projectID, ticketID := seedQueuedTicket(t, s, "1")

	got, err := s.ProjectForTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	if got.ID != projectID {
		t.Errorf("ProjectForTicket.ID = %d, want %d", got.ID, projectID)
	}
	if got.Name != testProject.Name || got.LocalPath != testProject.LocalPath {
		t.Errorf("ProjectForTicket = %+v, want it to match the seeded project", got)
	}
}

// --- AgentSecondsForTicket (design section 4.6's budget check) --------------

func TestAgentSecondsForTicket_SumsAcrossSessionsAndExcludesOtherTickets(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketA := seedQueuedTicket(t, s, "1")
	_, ticketB := seedQueuedTicket(t, s, "2")
	sessA1 := insertSession(t, s, ticketA, testStatePlanning)
	sessA2 := insertSession(t, s, ticketA, "classify")
	sessB := insertSession(t, s, ticketB, testStatePlanning)

	setRunAgentSeconds(t, s, insertRun(t, s, sessA1), 30)
	setRunAgentSeconds(t, s, insertRun(t, s, sessA2), 12)
	setRunAgentSeconds(t, s, insertRun(t, s, sessB), 99)

	got, err := s.AgentSecondsForTicket(ctx, ticketA)
	if err != nil {
		t.Fatalf("AgentSecondsForTicket: %v", err)
	}
	if got != 42 {
		t.Errorf("AgentSecondsForTicket(ticketA) = %d, want 42 (30+12, ticket B excluded)", got)
	}
}

func TestAgentSecondsForTicket_ZeroWhenNone(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	got, err := s.AgentSecondsForTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("AgentSecondsForTicket: %v", err)
	}
	if got != 0 {
		t.Errorf("AgentSecondsForTicket(no runs) = %d, want 0", got)
	}
}

// --- CountDeliveredReviews (design section 5.1 step 7, 5.3) -----------------

func TestCountDeliveredReviews_CountsOnlyDeliveredMarkers(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	insertUpdateMarker(t, s, ticketID, "planreview v1 pending")
	insertUpdateMarker(t, s, ticketID, "planreview v1 delivered")
	insertUpdateMarker(t, s, ticketID, "planreview v2 pending")
	insertUpdateMarker(t, s, ticketID, "planreview v2 delivered")
	insertUpdateMarker(t, s, ticketID, "validation errors delivered run 9") // must not count

	got, err := s.CountDeliveredReviews(ctx, ticketID)
	if err != nil {
		t.Fatalf("CountDeliveredReviews: %v", err)
	}
	if got != 2 {
		t.Errorf("CountDeliveredReviews = %d, want 2", got)
	}
}

// --- ConsecutiveInvalidOutputs (design D14, section 4.5, 5.4) ---------------

func TestConsecutiveInvalidOutputs_ZeroWhenNoErrorRuns(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)
	setRunOutcome(t, s, runID, "ok")

	n, reason, err := s.ConsecutiveInvalidOutputs(ctx, ticketID, testStatePlanning, nil)
	if err != nil {
		t.Fatalf("ConsecutiveInvalidOutputs: %v", err)
	}
	if n != 0 || reason != "" {
		t.Errorf("n,reason = %d,%q, want 0,\"\"", n, reason)
	}
}

func TestConsecutiveInvalidOutputs_OneWithReason(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	runID := insertRun(t, s, sess)
	setRunOutcome(t, s, runID, "error")
	insertInvalidMarker(t, s, ticketID, runID, "zing document failed validation")

	n, reason, err := s.ConsecutiveInvalidOutputs(ctx, ticketID, testStatePlanning, nil)
	if err != nil {
		t.Fatalf("ConsecutiveInvalidOutputs: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	if reason != "zing document failed validation" {
		t.Errorf("reason = %q, want %q", reason, "zing document failed validation")
	}
}

func TestConsecutiveInvalidOutputs_Two(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	run1 := insertRun(t, s, sess)
	run2 := insertRun(t, s, sess)
	setRunOutcome(t, s, run1, "error")
	insertInvalidMarker(t, s, ticketID, run1, "reason one")
	setRunOutcome(t, s, run2, "error")
	insertInvalidMarker(t, s, ticketID, run2, "reason two")

	n, reason, err := s.ConsecutiveInvalidOutputs(ctx, ticketID, testStatePlanning, nil)
	if err != nil {
		t.Fatalf("ConsecutiveInvalidOutputs: %v", err)
	}
	if n != 2 {
		t.Errorf("n = %d, want 2", n)
	}
	if reason != "reason two" {
		t.Errorf("reason = %q, want the newest marker's reason %q", reason, "reason two")
	}
}

func TestConsecutiveInvalidOutputs_InvalidValidInvalidGivesOne(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	run1 := insertRun(t, s, sess)
	run2 := insertRun(t, s, sess)
	run3 := insertRun(t, s, sess)
	setRunOutcome(t, s, run1, "error")
	insertInvalidMarker(t, s, ticketID, run1, "reason one")
	setRunOutcome(t, s, run2, "questions") // a valid terminalized run: resets the chain
	setRunOutcome(t, s, run3, "error")
	insertInvalidMarker(t, s, ticketID, run3, "reason three")

	n, reason, err := s.ConsecutiveInvalidOutputs(ctx, ticketID, testStatePlanning, nil)
	if err != nil {
		t.Fatalf("ConsecutiveInvalidOutputs: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1 (the valid run in between resets the chain)", n)
	}
	if reason != "reason three" {
		t.Errorf("reason = %q, want %q", reason, "reason three")
	}
}

func TestConsecutiveInvalidOutputs_EscalatedRunStopsTheWalk(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	run1 := insertRun(t, s, sess)
	run2 := insertRun(t, s, sess)
	setRunOutcome(t, s, run1, "error")
	insertInvalidMarker(t, s, ticketID, run1, "reason one")
	setRunOutcome(t, s, run2, "error")
	insertInvalidMarker(t, s, ticketID, run2, "reason two")
	if _, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, RunID: &run2, Type: msgTypeEscalation, Author: authorZing,
		Body:    "response_invalid: invalid output twice",
		Payload: mustMarshalEscalation(t, escalationTestPayload(response.EscalationCodeResponseInvalid, response.EscalationOriginPlanningResume)),
	}); err != nil {
		t.Fatalf("insert escalation on run2: %v", err)
	}

	n, reason, err := s.ConsecutiveInvalidOutputs(ctx, ticketID, testStatePlanning, nil)
	if err != nil {
		t.Fatalf("ConsecutiveInvalidOutputs: %v", err)
	}
	if n != 0 || reason != "" {
		t.Errorf("n,reason = %d,%q, want 0,\"\" (the walk stops at the escalated run without counting it)", n, reason)
	}
}

func TestConsecutiveInvalidOutputs_SessionFilterExcludesOtherSessionsRuns(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sessA := insertSession(t, s, ticketID, testStatePlanning)
	sessB := insertSession(t, s, ticketID, testStatePlanning)
	runA := insertRun(t, s, sessA)
	runB := insertRun(t, s, sessB)
	setRunOutcome(t, s, runA, "error")
	insertInvalidMarker(t, s, ticketID, runA, "reason a")
	setRunOutcome(t, s, runB, "error")
	insertInvalidMarker(t, s, ticketID, runB, "reason b")

	n, reason, err := s.ConsecutiveInvalidOutputs(ctx, ticketID, testStatePlanning, &sessA)
	if err != nil {
		t.Fatalf("ConsecutiveInvalidOutputs: %v", err)
	}
	if n != 1 || reason != "reason a" {
		t.Errorf("n,reason = %d,%q, want 1,%q (scoped to session A only)", n, reason, "reason a")
	}
}

func TestConsecutiveInvalidOutputs_NullOutcomeRunsAreSkipped(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)
	run1 := insertRun(t, s, sess)
	insertRun(t, s, sess) // a second, newer run still reserved: outcome NULL
	setRunOutcome(t, s, run1, "error")
	insertInvalidMarker(t, s, ticketID, run1, "reason one")

	n, reason, err := s.ConsecutiveInvalidOutputs(ctx, ticketID, testStatePlanning, nil)
	if err != nil {
		t.Fatalf("ConsecutiveInvalidOutputs: %v", err)
	}
	if n != 1 || reason != "reason one" {
		t.Errorf("n,reason = %d,%q, want 1,%q (the null-outcome run is skipped, not a stop)", n, reason, "reason one")
	}
}

// --- HasEscalation (design D17, section 5.1 step 3) -------------------------

func TestHasEscalation_TrueForMatchingOriginAndSession(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)

	payload := escalationTestPayload(response.EscalationCodeResumesExhausted, response.EscalationOriginCapResumes)
	payload.SessionID = &sess
	if _, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, Type: msgTypeEscalation, Author: authorZing, Body: testEscalationBodyCapResumes,
		Payload: mustMarshalEscalation(t, payload),
	}); err != nil {
		t.Fatalf("insert escalation: %v", err)
	}

	got, err := s.HasEscalation(ctx, ticketID, string(response.EscalationOriginCapResumes), sess)
	if err != nil {
		t.Fatalf("HasEscalation: %v", err)
	}
	if !got {
		t.Error("HasEscalation = false, want true")
	}
}

func TestHasEscalation_FalseByOrigin(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sess := insertSession(t, s, ticketID, testStatePlanning)

	payload := escalationTestPayload(response.EscalationCodeResumesExhausted, response.EscalationOriginCapResumes)
	payload.SessionID = &sess
	if _, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, Type: msgTypeEscalation, Author: authorZing, Body: testEscalationBodyCapResumes,
		Payload: mustMarshalEscalation(t, payload),
	}); err != nil {
		t.Fatalf("insert escalation: %v", err)
	}

	got, err := s.HasEscalation(ctx, ticketID, string(response.EscalationOriginCapLoops), sess)
	if err != nil {
		t.Fatalf("HasEscalation: %v", err)
	}
	if got {
		t.Error("HasEscalation with the wrong origin = true, want false")
	}
}

func TestHasEscalation_FalseBySession(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	sessA := insertSession(t, s, ticketID, testStatePlanning)
	sessB := insertSession(t, s, ticketID, testStatePlanning)

	payload := escalationTestPayload(response.EscalationCodeResumesExhausted, response.EscalationOriginCapResumes)
	payload.SessionID = &sessA
	if _, err := s.InsertMessage(ctx, Message{
		TicketID: ticketID, Type: msgTypeEscalation, Author: authorZing, Body: testEscalationBodyCapResumes,
		Payload: mustMarshalEscalation(t, payload),
	}); err != nil {
		t.Fatalf("insert escalation: %v", err)
	}

	got, err := s.HasEscalation(ctx, ticketID, string(response.EscalationOriginCapResumes), sessB)
	if err != nil {
		t.Fatalf("HasEscalation: %v", err)
	}
	if got {
		t.Error("HasEscalation with the wrong session = true, want false")
	}
}

// --- LiveMarker (design section 5.1 steps 5 and 7) --------------------------

func TestLiveMarker_PendingOnlyIsLive(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	insertUpdateMarker(t, s, ticketID, "planreview v1 pending")

	m, ok, err := s.LiveMarker(ctx, ticketID, "planreview v1 pending", "planreview v1 delivered")
	if err != nil {
		t.Fatalf("LiveMarker: %v", err)
	}
	if !ok {
		t.Fatal("LiveMarker: ok = false, want true")
	}
	if m.Body != "planreview v1 pending" {
		t.Errorf("Body = %q, want %q", m.Body, "planreview v1 pending")
	}
}

func TestLiveMarker_PendingThenDeliveredIsNotLive(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	insertUpdateMarker(t, s, ticketID, "planreview v1 pending")
	insertUpdateMarker(t, s, ticketID, "planreview v1 delivered")

	_, ok, err := s.LiveMarker(ctx, ticketID, "planreview v1 pending", "planreview v1 delivered")
	if err != nil {
		t.Fatalf("LiveMarker: %v", err)
	}
	if ok {
		t.Error("LiveMarker after delivered: ok = true, want false")
	}
}

func TestLiveMarker_DeliveredThenNewerPendingIsLive(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	insertUpdateMarker(t, s, ticketID, "planreview v1 delivered")
	insertUpdateMarker(t, s, ticketID, "planreview v1 pending")

	_, ok, err := s.LiveMarker(ctx, ticketID, "planreview v1 pending", "planreview v1 delivered")
	if err != nil {
		t.Fatalf("LiveMarker: %v", err)
	}
	if !ok {
		t.Error("LiveMarker with a newer pending after an older delivered: ok = false, want true")
	}
}

func TestLiveMarker_OlderDeliveredDoesNotCloseNewerPendingOfDifferentRun(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	insertUpdateMarker(t, s, ticketID, "validation errors pending run 1\nerr")
	insertUpdateMarker(t, s, ticketID, "validation errors pending run 2\nerr")
	insertUpdateMarker(t, s, ticketID, "validation errors delivered run 1")

	m, ok, err := s.LiveMarker(ctx, ticketID, "validation errors pending", "validation errors delivered")
	if err != nil {
		t.Fatalf("LiveMarker: %v", err)
	}
	if !ok {
		t.Fatal("LiveMarker: ok = false, want true")
	}
	if !strings.HasPrefix(m.Body, "validation errors pending run 2") {
		t.Errorf("Body = %q, want prefix %q", m.Body, "validation errors pending run 2")
	}
}

func TestLiveMarker_DeliveredForSameRunClosesPending(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")
	insertUpdateMarker(t, s, ticketID, "validation errors pending run 1\nerr")
	insertUpdateMarker(t, s, ticketID, "validation errors delivered run 1")

	_, ok, err := s.LiveMarker(ctx, ticketID, "validation errors pending", "validation errors delivered")
	if err != nil {
		t.Fatalf("LiveMarker: %v", err)
	}
	if ok {
		t.Error("LiveMarker after delivered for the same run: ok = true, want false")
	}
}

// TestAnsweredRoundsExcludesPlanningQuestions proves AnsweredRounds' own
// D31 exclusion (design section 22.3): a planning question left "answered"
// (the legacy upgrade case, section 22.8, since SendBatch never writes that
// state for one any more) never becomes a round -- it is delivered through
// PlanningConversation instead.
func TestAnsweredRoundsExcludesPlanningQuestions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	planningSess := insertSession(t, s, ticketID, testStatePlanning)
	planningRun := insertQuestionRun(t, s, planningSess)
	planningQID := insertOpenQuestion(t, s, ticketID, planningRun, "Q1")
	markAnswered(t, s, planningQID)
	insertSentAnswer(t, s, ticketID, planningQID, "a")

	classifySess := insertSession(t, s, ticketID, "classify")
	classifyRun := insertQuestionRun(t, s, classifySess)
	classifyQID := insertOpenQuestion(t, s, ticketID, classifyRun, "Q1")
	markAnswered(t, s, classifyQID)
	insertSentAnswer(t, s, ticketID, classifyQID, "a")

	rounds, err := s.AnsweredRounds(ctx, ticketID)
	if err != nil {
		t.Fatalf("AnsweredRounds: %v", err)
	}
	if len(rounds) != 1 {
		t.Fatalf("AnsweredRounds = %d rounds, want 1 (only the classify round)", len(rounds))
	}
	if rounds[0].RunID == nil || *rounds[0].RunID != classifyRun {
		t.Errorf("rounds[0].RunID = %v, want the classify run %d, not the planning one", rounds[0].RunID, classifyRun)
	}
}

// TestAnsweredRoundsRepliesAreOwnersOnly proves messagesByParent's own
// author filter (design section 22.3): a zing-authored reply parented to
// an answered, non-planning question (an agent reply D31's own
// conversation model can leave behind on a thread the owner never settles
// through a round) never reaches Round.Replies, which must hold only what
// the owner wrote.
func TestAnsweredRoundsRepliesAreOwnersOnly(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, ticketID := seedQueuedTicket(t, s, "1")

	sess := insertSession(t, s, ticketID, "classify")
	runID := insertQuestionRun(t, s, sess)
	qID := insertOpenQuestion(t, s, ticketID, runID, "Q1")
	markAnswered(t, s, qID)
	ownerReplyID := insertSentReply(t, s, ticketID, &qID, "why though")
	insertAgentReply(t, s, ticketID, qID, runID, "because the fixture says so")

	rounds, err := s.AnsweredRounds(ctx, ticketID)
	if err != nil {
		t.Fatalf("AnsweredRounds: %v", err)
	}
	if len(rounds) != 1 {
		t.Fatalf("AnsweredRounds = %d rounds, want 1", len(rounds))
	}
	if len(rounds[0].Replies) != 1 || rounds[0].Replies[0].ID != ownerReplyID {
		t.Errorf("Replies = %+v, want exactly the owner's one reply (id %d)", rounds[0].Replies, ownerReplyID)
	}
}
