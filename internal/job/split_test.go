// split_test.go tests the planner's split gate (split.go, task 3): a
// children outcome stores the children artifact and posts the split
// question. It reuses planning_test.go's own helpers (newJobTestStore,
// seedQueuedTicket, answeredRoundReadyForResume, claimWithRuntimes,
// readyScriptedRuntime, readyStep, runPlanning, apply, getTicket).
package job_test

import (
	"encoding/json"
	"strings"
	"testing"

	"zing/internal/response"
	"zing/internal/store"
)

// testSplitSharedNotes is the children response's own shared notes text,
// reused across this file's subtests (goconst: three or more call sites
// across this package compared the literal).
const testSplitSharedNotes = "the two halves share no code"

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
				{Key: "c1", Title: "Part one", Body: "build the read path"},
				{Key: "c2", Title: "Part two", Body: "build the write path", DependsOn: []string{"c1"}},
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
		if len(qp.Options) != 2 || qp.Options[0].Key != "a" || qp.Options[0].Text != testApproveOptionText ||
			qp.Options[1].Key != "b" || qp.Options[1].Text != testRejectOptionText {
			t.Errorf("question options = %+v, want a=Approve, b=Reject", qp.Options)
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
				{Key: "c1", Title: "Part one", Body: "build the read path"},
				{Key: "c2", Title: "Part two", Body: "build the write path", DependsOn: []string{"c1", "c1"}},
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
