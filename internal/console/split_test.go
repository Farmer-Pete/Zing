// split_test.go is Task 8's test-first proof for the split gate's own
// context region (design section 7.2): the proposed children, each with its
// title, key, dependencies, and rendered body, and the planner's shared
// notes above them, rendered through the live GET /stream the same way
// gate_test.go proves gateContext.
package console_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/response"
	"zing/internal/store"
)

// splitQuestionTitle is the fixed heading this file's seeded split question
// carries, distinct from every other fixture title in this package, so
// findGroup never matches the wrong ticket's group.
const splitQuestionTitle = "Split this ticket into 2 tickets?"

// seedSplitQuestion inserts one open split-kind "question" message on
// ticketID, the same shape a real children outcome's commit writes
// (internal/job/split.go's splitQuestionMessage).
func seedSplitQuestion(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	payload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindSplit, State: response.QuestionStateOpen,
		Recommended: "a",
		Options:     []response.Option{{Key: "a", Text: "Approve"}, {Key: "b", Text: "Reject"}},
	})
	if err != nil {
		t.Fatalf("marshal split question payload: %v", err)
	}
	openState := testQuestionStateOpen
	if _, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeQuestion, Author: testAuthorZing,
		State:   &openState,
		Body:    splitQuestionTitle,
		Payload: payload,
	}); err != nil {
		t.Fatalf("InsertMessage(split question): %v", err)
	}
}

// seedChildrenArtifact inserts one "children" artifact on ticketID, the
// same shape a children outcome's commit writes (job/split.go's
// childrenCommit).
func seedChildrenArtifact(t *testing.T, s *store.Store, ticketID int64, children []response.Child, notes string) {
	t.Helper()
	payload, err := json.Marshal(response.ChildrenArtifact{Children: children, Notes: notes})
	if err != nil {
		t.Fatalf("marshal children artifact: %v", err)
	}
	if _, err := s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, Type: "children", Payload: payload,
	}); err != nil {
		t.Fatalf("InsertArtifact(children): %v", err)
	}
}

func TestSplitGate_RendersChildrenAndNotes(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	ticketID := seedTicket(t, s, "fake#1", "Split this big ticket")

	seedChildrenArtifact(t, s, ticketID, []response.Child{
		{Key: "c1", Title: "Detect the conflict", Body: "Write the detector.", DependsOn: []string{}},
		{Key: "c2", Title: "Merge the unit", Body: "Write the merge step.", DependsOn: []string{"c1"}},
	}, "Shared arch")
	seedSplitQuestion(t, s, ticketID)

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", ticketID, 0)
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	_, main, _, _ := readInitialFrames(t, r)
	assertExactSSEFraming(t, main)

	group := findGroup(t, splitQuestionGroups(t, main), splitQuestionTitle)

	for _, want := range []string{"Shared notes", "Shared arch", "Detect the conflict", "Merge the unit", "Depends on c1", "Write the merge step."} {
		if !strings.Contains(group, want) {
			t.Errorf("split group missing %q; got:\n%s", want, group)
		}
	}
	if strings.Contains(group, "once split has a producer") {
		t.Errorf("split group still shows the removed placeholder; got:\n%s", group)
	}

	for _, key := range []string{"c1", "c2"} {
		re := regexp.MustCompile(`class="split-key"[^>]*>` + key + `<`)
		if !re.MatchString(group) {
			t.Errorf("split group missing split-key span for %q; got:\n%s", key, group)
		}
	}
}
