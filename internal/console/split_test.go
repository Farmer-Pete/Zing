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
	"time"

	"zing/internal/bus"
	"zing/internal/response"
	"zing/internal/store"
)

// seedSplitChild commits one queued child ticket under parentID through
// store.CommitHandlerResult, the same SplitChild commit shape
// internal/job/split.go's fileNextSplitChild produces (#74), re-claiming
// the parent as the dispatcher's own retried tick does between children.
func seedSplitChild(t *testing.T, s *store.Store, parentID int64, sc store.SplitChild) {
	t.Helper()
	const owner = "test-owner"
	expires := time.Now().Add(10 * time.Minute)
	claimed, err := s.Claim(t.Context(), parentID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatalf("Claim(%d): got false, want true", parentID)
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: parentID, Owner: owner, Expires: expires,
		SplitChild: &sc,
	})
	if err != nil {
		t.Fatalf("CommitHandlerResult(SplitChild %s): %v", sc.Key, err)
	}
	if !applied {
		t.Fatalf("CommitHandlerResult(SplitChild %s): applied = false, want true", sc.Key)
	}
}

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

// TestThreadBanner_HeldTicketNamesAbandonedDependency proves holdBanner
// (#74 owner decision Q3): a child ticket held on a dependency that ends
// abandoned stays held, and its thread banner names the dependency and
// says it was abandoned, until that dependency reaches done.
func TestThreadBanner_HeldTicketNamesAbandonedDependency(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	parentID := seedTicket(t, s, "fake#65", "Split this big ticket")

	seedSplitChild(t, s, parentID, store.SplitChild{Key: "c1", Ref: "12", Title: "First child", Body: "do c1"})
	seedSplitChild(t, s, parentID, store.SplitChild{Key: "c2", Ref: "13", Title: "Second child", Body: "do c2", DependsOn: []string{"c1"}})

	children, err := s.SplitChildren(t.Context(), parentID)
	if err != nil {
		t.Fatalf("SplitChildren: %v", err)
	}
	var c1ID, c2ID int64
	for _, c := range children {
		switch c.Key {
		case "c1":
			c1ID = c.TicketID
		case "c2":
			c2ID = c.TicketID
		}
	}
	if c1ID == 0 || c2ID == 0 {
		t.Fatalf("missing child ticket ids: %+v", children)
	}

	transitionTicket(t, s, c1ID, response.TicketStateAbandoned)

	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))
	resp, r, cancel := openStream(t, srv.URL, "thread", c2ID, 0)
	_, main, _, _ := readInitialFrames(t, r)
	cancel()
	_ = resp.Body.Close()

	if !strings.Contains(main, "Waits on #12, which was abandoned.") {
		t.Errorf("expected held banner naming #12 as abandoned; got:\n%s", main)
	}

	transitionTicket(t, s, c1ID, response.TicketStateDone)

	resp2, r2, cancel2 := openStream(t, srv.URL, "thread", c2ID, 0)
	_, main2, _, _ := readInitialFrames(t, r2)
	cancel2()
	_ = resp2.Body.Close()

	if strings.Contains(main2, "Waits on") {
		t.Errorf("expected no held banner once the dependency is done; got:\n%s", main2)
	}
}
