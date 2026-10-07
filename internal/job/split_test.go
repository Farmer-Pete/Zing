// split_test.go tests the planner's split gate (split.go, task 3): a
// children outcome stores the children artifact and posts the split
// question. It reuses planning_test.go's own helpers (newJobTestStore,
// seedQueuedTicket, answeredRoundReadyForResume, claimWithRuntimes,
// readyScriptedRuntime, readyStep, runPlanning, apply, getTicket).
package job_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"zing/internal/job"
	"zing/internal/response"
	"zing/internal/store"
)

// testSplitSharedNotes is the children response's own shared notes text,
// reused across this file's subtests (goconst: three or more call sites
// across this package compared the literal).
const testSplitSharedNotes = "the two halves share no code"

// testSplitPartOneTitle, testSplitPartOneBody, testSplitPartTwoTitle, and
// testSplitPartTwoBody are c1 and c2's own title and body, reused across
// this file's subtests (goconst).
const (
	testSplitPartOneTitle = "Part one"
	testSplitPartOneBody  = "build the read path"
	testSplitPartTwoTitle = "Part two"
	testSplitPartTwoBody  = "build the write path"
)

// seedSplitQuestion inserts one open split question directly (design
// section 6.6's split variant), mirroring gate_test.go's own
// seedGateQuestion: kind split, options a/b, recommended "a", key fixed
// since this bypasses CommitHandlerResult's own fillQuestionKeyTx
// allocation, which only the real split-posting commit (childrenCommit)
// goes through.
func seedSplitQuestion(t *testing.T, s *store.Store, ticketID int64) int64 {
	t.Helper()
	payload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindSplit, State: response.QuestionStateOpen,
		Recommended: "a",
		Options:     []response.Option{{Key: "a", Text: testApproveOptionText}, {Key: "b", Text: testRejectOptionText}},
	})
	if err != nil {
		t.Fatalf("marshal split question payload: %v", err)
	}
	id, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeQuestion, Author: testAuthorZing,
		State: new("open"), Body: "Split this ticket into 2 tickets?", Payload: payload,
	})
	if err != nil {
		t.Fatalf("InsertMessage(split question): %v", err)
	}
	return id
}

// seedChildrenArtifact inserts a children artifact directly
// (store.InsertArtifact), standing in for childrenCommit's own write: task
// 6's tests drive fileNextSplitChild straight from an answered split round,
// without replaying the planner's own first turn.
func seedChildrenArtifact(t *testing.T, s *store.Store, ticketID int64, children []response.Child, notes string) {
	t.Helper()
	payload, err := json.Marshal(response.ChildrenArtifact{Children: children, Notes: notes})
	if err != nil {
		t.Fatalf("marshal children artifact: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{TicketID: ticketID, Type: "children", Payload: payload}); err != nil {
		t.Fatalf("InsertArtifact(children): %v", err)
	}
}

// approveSplitQuestion seeds an open split question (seedSplitQuestion),
// then answers it with option a (Approve) and sends it, so the next
// planning tick's AnsweredRounds carries an approved split round. It
// returns the split question's own id, so a caller can confirm it is later
// resolved.
func approveSplitQuestion(t *testing.T, s *store.Store, ticketID int64) int64 {
	t.Helper()
	qID := seedSplitQuestion(t, s, ticketID)
	option := "a"
	if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &option}); err != nil {
		t.Fatalf("SaveDraft(option a): %v", err)
	}
	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	return qID
}

// fakeSplitTracker is a job.SplitTracker double (task 6): FileSplitChild
// returns refs in order from refs, recording every title and body it was
// called with, including a failed call; failAt, when non-zero, fails the
// failAt'th FileSplitChild call (1-based) once, with no ref consumed.
// CloseSplitParent records its own arguments on every call, and fails its
// first closeFailTimes calls (r2f6), succeeding every call after that.
type fakeSplitTracker struct {
	t    *testing.T
	refs []string

	filed  int
	titles []string
	failAt int

	closeFailTimes int
	closeCalls     int
	closeProjectID int64
	closeRef       string
	closeBody      string
}

func (f *fakeSplitTracker) FileSplitChild(_ context.Context, _ int64, title, _ string) (string, error) {
	f.t.Helper()
	f.titles = append(f.titles, title)
	if f.failAt != 0 && len(f.titles) == f.failAt {
		return "", errors.New("fake split tracker: file failed")
	}
	if f.filed >= len(f.refs) {
		f.t.Fatalf("fakeSplitTracker.FileSplitChild: call %d has no ref left (only %d)", len(f.titles), len(f.refs))
	}
	ref := f.refs[f.filed]
	f.filed++
	return ref, nil
}

func (f *fakeSplitTracker) CloseSplitParent(_ context.Context, projectID int64, ref, body string) error {
	f.closeCalls++
	f.closeProjectID = projectID
	f.closeRef = ref
	f.closeBody = body
	if f.closeCalls <= f.closeFailTimes {
		return errors.New("fake split tracker: close failed")
	}
	return nil
}

// TestPlanningHandler_Children_PostsSplitGate proves childrenCommit (design
// section 6.6's split variant, plan #74): a children outcome stores the
// children artifact, deduplicating a key repeated inside one child's own
// depends_on, and posts the split question with Approve/Reject, instead of
// escalating split_unsupported.
func TestPlanningHandler_Children_PostsSplitGate(t *testing.T) {
	t.Parallel()
	t.Run("basic", func(t *testing.T) {
		t.Parallel()
		s := newJobTestStore(t)
		ticketID := seedQueuedTicket(t, s)
		answeredRoundReadyForResume(t, s, ticketID)

		children := &response.ChildrenResponse{
			Job: response.JobPlanning, Outcome: response.OutcomeChildren,
			Children: []response.Child{
				{Key: "c1", Title: testSplitPartOneTitle, Body: testSplitPartOneBody},
				{Key: "c2", Title: testSplitPartTwoTitle, Body: testSplitPartTwoBody, DependsOn: []string{"c1"}},
			},
			Notes: testSplitSharedNotes,
			Replies: []response.Reply{
				{Question: "Q1", Settled: true, Decision: testQ1SettledDecision},
			},
		}
		resumeRT := readyScriptedRuntime(t, readyStep(children, "children-sess"))

		commit, err := runPlanning(t, s, claimWithRuntimes(t, s, resumeRT, ticketID), ticketID)
		if err != nil {
			t.Fatalf("planning resume (children) Run: %v", err)
		}
		if commit.Escalation != nil {
			t.Errorf("commit.Escalation = %+v, want nil", commit.Escalation)
		}
		if commit.Next != "" {
			t.Errorf("commit.Next = %q, want empty (stays in planning)", commit.Next)
		}
		if commit.Waiting == nil || *commit.Waiting != "split" {
			t.Errorf("commit.Waiting = %v, want split", commit.Waiting)
		}

		if len(commit.Artifacts) != 1 {
			t.Fatalf("commit.Artifacts = %+v, want exactly one children artifact", commit.Artifacts)
		}
		if commit.Artifacts[0].Type != "children" {
			t.Errorf("commit.Artifacts[0].Type = %q, want children", commit.Artifacts[0].Type)
		}
		var ca response.ChildrenArtifact
		if unmarshalErr := json.Unmarshal(commit.Artifacts[0].Payload, &ca); unmarshalErr != nil {
			t.Fatalf("unmarshal children artifact: %v", unmarshalErr)
		}
		if len(ca.Children) != 2 {
			t.Fatalf("stored children = %+v, want 2", ca.Children)
		}
		if ca.Notes != testSplitSharedNotes {
			t.Errorf("stored notes = %q, want %q", ca.Notes, testSplitSharedNotes)
		}
		if ca.Children[0].DependsOn == nil || len(ca.Children[0].DependsOn) != 0 {
			t.Errorf("c1 depends_on = %#v, want an empty array, not nil", ca.Children[0].DependsOn)
		}

		var splitMsg *store.Message
		for i := range commit.Messages {
			if commit.Messages[i].Type == "question" {
				splitMsg = &commit.Messages[i]
			}
		}
		if splitMsg == nil {
			t.Fatalf("commit.Messages = %+v, want exactly one split question among them", commit.Messages)
		}
		var qp response.QuestionPayload
		if unmarshalErr := json.Unmarshal(splitMsg.Payload, &qp); unmarshalErr != nil {
			t.Fatalf("unmarshal question payload: %v", unmarshalErr)
		}
		if qp.Kind != response.QuestionKindSplit {
			t.Errorf("question kind = %q, want split", qp.Kind)
		}
		if qp.Recommended != "a" {
			t.Errorf("question recommended = %q, want a", qp.Recommended)
		}
		wantOptions := []response.Option{{Key: "a", Text: testApproveOptionText}, {Key: "b", Text: testRejectOptionText}}
		if !slices.Equal(qp.Options, wantOptions) {
			t.Errorf("question options = %+v, want %+v", qp.Options, wantOptions)
		}
		if !strings.HasPrefix(splitMsg.Body, "Split this ticket into 2 tickets?") {
			t.Errorf("question body = %q, want it to start with %q", splitMsg.Body, "Split this ticket into 2 tickets?")
		}

		apply(t, s, getTicket(t, s, ticketID), commit)
	})

	t.Run("repeated dependency", func(t *testing.T) {
		t.Parallel()
		s := newJobTestStore(t)
		ticketID := seedQueuedTicket(t, s)
		answeredRoundReadyForResume(t, s, ticketID)

		children := &response.ChildrenResponse{
			Job: response.JobPlanning, Outcome: response.OutcomeChildren,
			Children: []response.Child{
				{Key: "c1", Title: testSplitPartOneTitle, Body: testSplitPartOneBody},
				{Key: "c2", Title: testSplitPartTwoTitle, Body: testSplitPartTwoBody, DependsOn: []string{"c1", "c1"}},
			},
			Notes: testSplitSharedNotes,
			Replies: []response.Reply{
				{Question: "Q1", Settled: true, Decision: testQ1SettledDecision},
			},
		}
		resumeRT := readyScriptedRuntime(t, readyStep(children, "children-sess"))

		commit, err := runPlanning(t, s, claimWithRuntimes(t, s, resumeRT, ticketID), ticketID)
		if err != nil {
			t.Fatalf("planning resume (children) Run: %v", err)
		}

		var ca response.ChildrenArtifact
		if unmarshalErr := json.Unmarshal(commit.Artifacts[0].Payload, &ca); unmarshalErr != nil {
			t.Fatalf("unmarshal children artifact: %v", unmarshalErr)
		}
		if len(ca.Children) != 2 {
			t.Fatalf("stored children = %+v, want 2", ca.Children)
		}
		if got := ca.Children[1].DependsOn; len(got) != 1 || got[0] != "c1" {
			t.Errorf("c2 depends_on = %#v, want exactly [c1]", got)
		}

		apply(t, s, getTicket(t, s, ticketID), commit)
	})
}

// TestPlanningHandler_SplitRejected_ResumesWithNotes proves
// enterFromSplitRound's reject branch (design section 6.6/6.7's split
// variant, mirroring the gate's own rejection): a split question answered
// with option b, or with a reply and no option picked at all, resumes the
// open planning session with the reply's body fenced as notes under
// splitRejectedNote, and resolves the split question. Either way,
// fileNextSplitChild must never run.
func TestPlanningHandler_SplitRejected_ResumesWithNotes(t *testing.T) {
	t.Parallel()
	t.Run("explicit reject", func(t *testing.T) {
		t.Parallel()
		runSplitRejectedResumesWithNotes(t, "b")
	})
	t.Run("no option picked", func(t *testing.T) {
		t.Parallel()
		runSplitRejectedResumesWithNotes(t, "")
	})
}

// runSplitRejectedResumesWithNotes is
// TestPlanningHandler_SplitRejected_ResumesWithNotes's shared body (r2f4):
// option is the owner's chosen option key, or "" to save only a text
// reply with no option picked at all, which enterFromSplitRound must also
// treat as a reject.
func runSplitRejectedResumesWithNotes(t *testing.T, option string) {
	t.Helper()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := fakeRuntime(t)
	advanceQueuedToPlanning(t, s, rt, ticketID)
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // classify
	apply(t, s, getTicket(t, s, ticketID), mustPlanning(t, s, claim(t, s, rt, ticketID), ticketID)) // first turn: posts Q1
	answerFixtureQuestion(t, s, ticketID)                                                           // Q1: opens the planning session, still open after

	qID := seedSplitQuestion(t, s, ticketID)
	const notes = "keep it as one ticket"
	if option != "" {
		opt := option
		if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &qID, Option: &opt}); draftErr != nil {
			t.Fatalf("SaveDraft(option): %v", draftErr)
		}
	}
	if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &qID, Text: notes}); draftErr != nil {
		t.Fatalf("SaveDraft(text): %v", draftErr)
	}
	if _, sendErr := s.SendBatch(t.Context(), ticketID); sendErr != nil {
		t.Fatalf("SendBatch: %v", sendErr)
	}

	openSess, _, err := s.LatestSession(t.Context(), ticketID, testStatePlanning, 12)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}

	resumeRT := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobPlanning, "split-reject-sess")}}
	rec := &recordingRuntime{rt: resumeRT}
	fake := &fakeSplitTracker{t: t}
	deps := claim(t, s, rec, ticketID)
	deps.Splitter = fake
	commit, err := runPlanning(t, s, deps, ticketID)
	if err != nil {
		t.Fatalf("split reject Run: %v", err)
	}
	if len(fake.titles) != 0 || fake.closeCalls != 0 {
		t.Errorf("fake split tracker calls = %d, closeCalls = %d, want 0 and 0 (reject must never file)", len(fake.titles), fake.closeCalls)
	}
	if commit.Session == nil || commit.Session.ID == nil || *commit.Session.ID != openSess.ID {
		t.Fatalf("commit.Session = %+v, want the already-open session %d (a resume, not fresh)", commit.Session, openSess.ID)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
	}
	if rec.lastReq.Prompt == "" {
		t.Fatal("recordingRuntime saw an empty resume prompt")
	}
	if !strings.Contains(rec.lastReq.Prompt, "rejected your proposed split") || !strings.Contains(rec.lastReq.Prompt, notes) {
		t.Errorf("resume prompt does not carry the rejection note and the owner's text:\n%s", rec.lastReq.Prompt)
	}
}

// TestPlanningHandler_SplitApproved_FilesOneChildPerTickThenClosesParent
// proves fileNextSplitChild and closeSplitParent (task 6, design section
// 6.6's split variant): an approved split files one child per tick, in
// dependency order, each child's body carrying its filed dependency's own
// human-readable ref, then closes the parent once every child is filed.
func TestPlanningHandler_SplitApproved_FilesOneChildPerTickThenClosesParent(t *testing.T) {
	t.Parallel()
	t.Run("basic", func(t *testing.T) {
		t.Parallel()
		s := newJobTestStore(t)
		ticketID := seedQueuedTicket(t, s)
		rt := &scriptedRuntime{t: t} // never called: approve files children without the agent
		advanceQueuedToPlanning(t, s, rt, ticketID)
		seedChildrenArtifact(t, s, ticketID, []response.Child{
			{Key: "c1", Title: "Detect the conflict", Body: "do c1", DependsOn: []string{}},
			{Key: "c2", Title: "The merge unit", Body: "do c2", DependsOn: []string{"c1"}},
		}, testSplitSharedNotes)
		qID := approveSplitQuestion(t, s, ticketID)

		fake := &fakeSplitTracker{t: t, refs: []string{"70", "71"}}

		deps := claimWithRuntimes(t, s, rt, ticketID)
		deps.Splitter = fake
		commit1, err := runPlanning(t, s, deps, ticketID)
		if err != nil {
			t.Fatalf("run 1: %v", err)
		}
		if commit1.SplitChild == nil || commit1.SplitChild.Key != "c1" || commit1.SplitChild.Ref != "70" {
			t.Fatalf("run 1: commit.SplitChild = %+v, want key c1, ref 70", commit1.SplitChild)
		}
		apply(t, s, getTicket(t, s, ticketID), commit1)

		deps = claimWithRuntimes(t, s, rt, ticketID)
		deps.Splitter = fake
		commit2, err := runPlanning(t, s, deps, ticketID)
		if err != nil {
			t.Fatalf("run 2: %v", err)
		}
		if commit2.SplitChild == nil || commit2.SplitChild.Key != "c2" || commit2.SplitChild.Ref != "71" {
			t.Fatalf("run 2: commit.SplitChild = %+v, want key c2, ref 71", commit2.SplitChild)
		}
		if len(commit2.SplitChild.DependsOn) != 1 || commit2.SplitChild.DependsOn[0] != "c1" {
			t.Errorf("run 2: commit.SplitChild.DependsOn = %v, want [c1]", commit2.SplitChild.DependsOn)
		}
		if !strings.Contains(commit2.SplitChild.Body, "Depends on #70") {
			t.Errorf("run 2: commit.SplitChild.Body = %q, want it to contain %q", commit2.SplitChild.Body, "Depends on #70")
		}
		apply(t, s, getTicket(t, s, ticketID), commit2)

		parent := getTicket(t, s, ticketID)
		deps = claimWithRuntimes(t, s, rt, ticketID)
		deps.Splitter = fake
		commit3, err := runPlanning(t, s, deps, ticketID)
		if err != nil {
			t.Fatalf("run 3: %v", err)
		}
		if fake.closeCalls != 1 {
			t.Fatalf("CloseSplitParent called %d times, want 1", fake.closeCalls)
		}
		if fake.closeRef != parent.TrackerRef {
			t.Errorf("CloseSplitParent ref = %q, want the parent's own ref %q", fake.closeRef, parent.TrackerRef)
		}
		if fake.closeProjectID != parent.ProjectID {
			t.Errorf("CloseSplitParent projectID = %d, want the parent's own project %d", fake.closeProjectID, parent.ProjectID)
		}
		if !strings.Contains(fake.closeBody, "#70, #71") {
			t.Errorf("CloseSplitParent body = %q, want it to name #70, #71", fake.closeBody)
		}
		if commit3.Next != "done" {
			t.Errorf("run 3: commit.Next = %q, want done", commit3.Next)
		}
		if commit3.Reason != "split into #70, #71" {
			t.Errorf("run 3: commit.Reason = %q, want %q", commit3.Reason, "split into #70, #71")
		}
		if len(commit3.ResolveQuestions) != 1 || commit3.ResolveQuestions[0] != qID {
			t.Errorf("commit3.ResolveQuestions = %v, want [%d]", commit3.ResolveQuestions, qID)
		}
		apply(t, s, getTicket(t, s, ticketID), commit3)

		if len(fake.titles) != 2 {
			t.Errorf("FileSplitChild called %d times, want exactly 2", len(fake.titles))
		}
		if got := getTicket(t, s, ticketID).State; got != "done" {
			t.Errorf("parent state = %q, want done", got)
		}
		answered, err := s.QuestionsByState(t.Context(), ticketID, "answered")
		if err != nil {
			t.Fatalf("QuestionsByState(answered): %v", err)
		}
		for _, q := range answered {
			if q.ID == qID {
				t.Errorf("QuestionsByState(answered) still lists the split question %d, want it resolved", qID)
			}
		}
	})

	t.Run("repeated dependency from planner", func(t *testing.T) {
		t.Parallel()
		s := newJobTestStore(t)
		ticketID := seedQueuedTicket(t, s)
		answeredRoundReadyForResume(t, s, ticketID)

		children := &response.ChildrenResponse{
			Job: response.JobPlanning, Outcome: response.OutcomeChildren,
			Children: []response.Child{
				{Key: "c1", Title: testSplitPartOneTitle, Body: testSplitPartOneBody},
				{Key: "c2", Title: testSplitPartTwoTitle, Body: testSplitPartTwoBody, DependsOn: []string{"c1", "c1"}},
			},
			Notes: testSplitSharedNotes,
			Replies: []response.Reply{
				{Question: "Q1", Settled: true, Decision: testQ1SettledDecision},
			},
		}
		resumeRT := readyScriptedRuntime(t, readyStep(children, "children-sess2"))
		postCommit, err := runPlanning(t, s, claimWithRuntimes(t, s, resumeRT, ticketID), ticketID)
		if err != nil {
			t.Fatalf("planning resume (children) Run: %v", err)
		}
		apply(t, s, getTicket(t, s, ticketID), postCommit)

		open, err := s.QuestionsByState(t.Context(), ticketID, "open")
		if err != nil {
			t.Fatalf("QuestionsByState(open): %v", err)
		}
		if len(open) != 1 {
			t.Fatalf("QuestionsByState(open) = %+v, want exactly one open split question", open)
		}
		if res, answerErr := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: "a"}); answerErr != nil {
			t.Fatalf("AnswerQuestion: %v", answerErr)
		} else if !res.Accepted {
			t.Fatalf("AnswerQuestion: Accepted = false, Conflict = %q, want accepted", res.Conflict)
		}

		rt := &scriptedRuntime{t: t}
		fake := &fakeSplitTracker{t: t, refs: []string{"70", "71"}}

		deps := claimWithRuntimes(t, s, rt, ticketID)
		deps.Splitter = fake
		commit1, err := runPlanning(t, s, deps, ticketID)
		if err != nil {
			t.Fatalf("file c1: %v", err)
		}
		if applied, applyErr := s.CommitHandlerResult(t.Context(), commit1); applyErr != nil || !applied {
			t.Fatalf("CommitHandlerResult(c1): applied=%v err=%v", applied, applyErr)
		}

		deps = claimWithRuntimes(t, s, rt, ticketID)
		deps.Splitter = fake
		commit2, err := runPlanning(t, s, deps, ticketID)
		if err != nil {
			t.Fatalf("file c2: %v", err)
		}
		if applied, applyErr := s.CommitHandlerResult(t.Context(), commit2); applyErr != nil || !applied {
			t.Fatalf("CommitHandlerResult(c2): applied=%v err=%v", applied, applyErr)
		}

		if commit2.SplitChild == nil {
			t.Fatalf("commit2.SplitChild = nil, want the c2 child")
		}
		var childTicketID, c1TicketID int64
		filed, err := s.SplitChildren(t.Context(), ticketID)
		if err != nil {
			t.Fatalf("SplitChildren: %v", err)
		}
		for _, f := range filed {
			switch f.Key {
			case "c2":
				childTicketID = f.TicketID
			case "c1":
				c1TicketID = f.TicketID
			}
		}
		if childTicketID == 0 || c1TicketID == 0 {
			t.Fatalf("SplitChildren = %+v, want filed c1 and c2", filed)
		}
		deps2, err := s.Dependencies(t.Context(), childTicketID)
		if err != nil {
			t.Fatalf("Dependencies: %v", err)
		}
		if len(deps2) != 1 {
			t.Fatalf("Dependencies(c2) = %+v, want exactly one row (c1)", deps2)
		}
		if deps2[0].TicketID != c1TicketID {
			t.Errorf("Dependencies(c2)[0].TicketID = %d, want c1's ticket id %d", deps2[0].TicketID, c1TicketID)
		}
	})
}

// TestPlanningHandler_SplitApproved_TrackerFailureRetriesWithoutDuplicate
// proves fileNextSplitChild's own retry rule (design section 6.6's split
// variant): a tracker failure logs a warning and returns ErrNoAction with
// no commit, and the next tick resumes from the store's filed children
// without filing the already-filed child again.
func TestPlanningHandler_SplitApproved_TrackerFailureRetriesWithoutDuplicate(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := &scriptedRuntime{t: t}
	advanceQueuedToPlanning(t, s, rt, ticketID)
	seedChildrenArtifact(t, s, ticketID, []response.Child{
		{Key: "c1", Title: "c1", Body: "do c1", DependsOn: []string{}},
		{Key: "c2", Title: "c2", Body: "do c2", DependsOn: []string{"c1"}},
	}, "")
	approveSplitQuestion(t, s, ticketID)

	fake := &fakeSplitTracker{t: t, refs: []string{"70", "71"}, failAt: 2}

	deps := claimWithRuntimes(t, s, rt, ticketID)
	deps.Splitter = fake
	commit1, err := runPlanning(t, s, deps, ticketID)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if commit1.SplitChild == nil || commit1.SplitChild.Key != "c1" {
		t.Fatalf("run 1: commit.SplitChild = %+v, want key c1", commit1.SplitChild)
	}
	apply(t, s, getTicket(t, s, ticketID), commit1)

	deps = claimWithRuntimes(t, s, rt, ticketID)
	deps.Splitter = fake
	_, err = runPlanning(t, s, deps, ticketID)
	if !errors.Is(err, job.ErrNoAction) {
		t.Fatalf("run 2: err = %v, want ErrNoAction", err)
	}

	// Run 2's failure applied no commit, so the claim from run 2 is still
	// held: reuse it rather than re-claiming, the same as a dispatcher tick
	// that retries within one lease.
	commit3, err := runPlanning(t, s, deps, ticketID)
	if err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if commit3.SplitChild == nil || commit3.SplitChild.Key != "c2" {
		t.Fatalf("run 3: commit.SplitChild = %+v, want key c2", commit3.SplitChild)
	}
	apply(t, s, getTicket(t, s, ticketID), commit3)

	wantTitles := []string{"c1", "c2", "c2"}
	if len(fake.titles) != len(wantTitles) {
		t.Fatalf("fake.titles = %v, want %v", fake.titles, wantTitles)
	}
	for i, want := range wantTitles {
		if fake.titles[i] != want {
			t.Errorf("fake.titles[%d] = %q, want %q", i, fake.titles[i], want)
		}
	}

	rows, err := s.SplitChildren(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SplitChildren: %v", err)
	}
	count := 0
	for _, r := range rows {
		if r.Key == "c1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("split children rows with key c1 = %d, want exactly 1", count)
	}
}

// TestPlanningHandler_SplitApproved_CloseParentRetriesWithoutDuplicateComment
// proves closeSplitParent's own retry rule (design section 6.6's split
// variant, r2f6): a CloseSplitParent failure logs a warning and returns
// ErrNoAction with no commit, leaving the parent in planning, and the next
// tick retries the close and reaches done. The real tracker's own
// once-only comment on a retried close is proved by
// internal/dispatch/split_test.go, against the fixture tracker.
func TestPlanningHandler_SplitApproved_CloseParentRetriesWithoutDuplicateComment(t *testing.T) {
	t.Parallel()
	s := newJobTestStore(t)
	ticketID := seedQueuedTicket(t, s)
	rt := &scriptedRuntime{t: t}
	advanceQueuedToPlanning(t, s, rt, ticketID)
	seedChildrenArtifact(t, s, ticketID, []response.Child{
		{Key: "c1", Title: "c1", Body: "do c1", DependsOn: []string{}},
		{Key: "c2", Title: "c2", Body: "do c2", DependsOn: []string{"c1"}},
	}, "")
	approveSplitQuestion(t, s, ticketID)

	fake := &fakeSplitTracker{t: t, refs: []string{"70", "71"}, closeFailTimes: 1}

	deps := claimWithRuntimes(t, s, rt, ticketID)
	deps.Splitter = fake
	commit1, err := runPlanning(t, s, deps, ticketID)
	if err != nil {
		t.Fatalf("run 1 (file c1): %v", err)
	}
	apply(t, s, getTicket(t, s, ticketID), commit1)

	deps = claimWithRuntimes(t, s, rt, ticketID)
	deps.Splitter = fake
	commit2, err := runPlanning(t, s, deps, ticketID)
	if err != nil {
		t.Fatalf("run 2 (file c2): %v", err)
	}
	apply(t, s, getTicket(t, s, ticketID), commit2)

	deps = claimWithRuntimes(t, s, rt, ticketID)
	deps.Splitter = fake
	_, err = runPlanning(t, s, deps, ticketID)
	if !errors.Is(err, job.ErrNoAction) {
		t.Fatalf("run 3 (close fails): err = %v, want ErrNoAction", err)
	}
	if got := getTicket(t, s, ticketID).State; got == "done" {
		t.Fatalf("parent state = %q after a failed close, want still planning", got)
	}

	// Run 3's failure applied no commit, so the claim from run 3 is still
	// held: reuse it rather than re-claiming, the same as
	// TestPlanningHandler_SplitApproved_TrackerFailureRetriesWithoutDuplicate.
	commit4, err := runPlanning(t, s, deps, ticketID)
	if err != nil {
		t.Fatalf("run 4 (close retries): %v", err)
	}
	if commit4.Next != "done" {
		t.Errorf("run 4: commit.Next = %q, want done", commit4.Next)
	}
	apply(t, s, getTicket(t, s, ticketID), commit4)

	if fake.closeCalls != 2 {
		t.Errorf("CloseSplitParent called %d times, want exactly 2 (one failure, one success)", fake.closeCalls)
	}
}
