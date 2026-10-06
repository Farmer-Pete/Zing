// split_test.go is this package's own end-to-end proof of the planner's
// split gate (design section 6.6's split variant, plan #74): a children
// outcome, approved by the owner, files every child as a real tracker
// issue, in dependency order, and the dispatcher never picks a child whose
// dependency is not yet done.
package dispatch_test

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"zing/fixtures"
	"zing/internal/bus"
	"zing/internal/dispatch"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
	"zing/internal/tracker"
)

// splitTestProject is the fixture tracker's own project name this file's
// one binding carries (distinct from testProject, so this test's tracker
// never serves the real fixtures/tickets.toml ticket).
const splitTestProject = "split-fixture"

// splitTestParentRef is the parent ticket's own tracker ref (never actually
// filed on the fixture tracker itself: it is seeded straight into the
// store, the same way seedQueuedTicket seeds testFixtureRef).
const splitTestParentRef = "fake#parent"

// splitTestChildrenXML copies fixtures/scripts/planning/1.xml's own
// document shape, except it returns children instead of a question, so a
// freshly queued ticket reaches the split gate in one classify tick (the
// real fixture's own classify/1.xml, splitTestRuntime below) and one
// planning tick, with no owner question in between.
const splitTestChildrenXML = `<zing job="planning" outcome="children">
  <child key="c1"><title>Detect the conflict</title><body>Add conflict detection for the merge step.</body></child>
  <child key="c2"><title>Build the merge unit</title><body>Build the merge unit that uses the detector.</body><depends_on>c1</depends_on></child>
  <notes>Both children touch the merge package.</notes>
</zing>
`

// splitOverlayFS layers over on top of base: Open serves over's own entry
// when it has one, and base's otherwise. splitTestRuntime uses it to keep
// the real, checked-in fixtures/scripts tree's own classify/1.xml (a
// "feature" outcome, exactly what this test needs) while substituting only
// planning's first turn.
type splitOverlayFS struct {
	over fs.FS
	base fs.FS
}

func (o splitOverlayFS) Open(name string) (fs.File, error) {
	if f, err := o.over.Open(name); err == nil {
		return f, nil
	}
	return o.base.Open(name)
}

// splitTestRuntime returns a *runtime.Fake serving the real, checked-in
// fixtures/scripts tree, except planning's first turn is overridden to
// splitTestChildrenXML (a children outcome) in place of the real fixture's
// own question.
func splitTestRuntime(t *testing.T) *runtime.Fake {
	t.Helper()
	base, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		t.Fatalf("fs.Sub(scripts): %v", err)
	}
	over := fstest.MapFS{"planning/1.xml": &fstest.MapFile{Data: []byte(splitTestChildrenXML)}}
	return runtime.NewFake(splitOverlayFS{over: over, base: base})
}

// newSplitFixtureTracker returns a *tracker.Fixture over an inline,
// ticketless tickets.toml naming splitTestProject: this test files its two
// children through FileTicket, and never needs Intake to serve a ticket of
// its own (the one binding's Mode is manual, below).
func newSplitFixtureTracker(t *testing.T) *tracker.Fixture {
	t.Helper()
	fsys := fstest.MapFS{"tickets.toml": &fstest.MapFile{Data: []byte(`project = "` + splitTestProject + `"
`)}}
	tr, err := tracker.NewFixture(fsys, "tickets.toml")
	if err != nil {
		t.Fatalf("tracker.NewFixture: %v", err)
	}
	return tr
}

// splitTestStateMessageReason returns the Reason the newest "state" message
// on ticketID's own thread carries (store.commit.go's own c.Next != ""
// write): the ticket row itself carries no reason column, so this is the
// only place a transition's reason can be read back.
func splitTestStateMessageReason(t *testing.T, s *store.Store, ticketID int64) string {
	t.Helper()
	msgs, err := s.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var reason string
	for i := range msgs {
		if msgs[i].Type != "state" {
			continue
		}
		var sp response.StatePayload
		if err := json.Unmarshal(msgs[i].Payload, &sp); err != nil {
			t.Fatalf("unmarshal state payload: %v", err)
		}
		reason = sp.Reason
	}
	return reason
}

// TestSplit_ApproveFilesBothQueuesFirstHoldsSecond is this package's own
// demo (design section 6.6's split variant, plan #74): a fake planner
// proposes two children, c2 depending on c1. The owner approves. The
// dispatcher files both issues on the fixture tracker, closes the parent,
// queues the first child, and holds the second until the first is done.
func TestSplit_ApproveFilesBothQueuesFirstHoldsSecond(t *testing.T) {
	t.Parallel()

	s := newDispatchTestStore(t)
	projectID := seedProject(t, s)
	parentID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: splitTestParentRef, Title: testTicketTitle, State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket(parent): %v", err)
	}

	fx := newSplitFixtureTracker(t)
	rt := splitTestRuntime(t)
	bindings := []dispatch.Binding{
		{StoreProjectID: projectID, TrackerProject: splitTestProject, User: testBindingUser, Mode: "manual"},
	}
	d := newDispatcher(t, s, fx, bus.New(), rt, nil, bindings, dispatch.Config{MaxParallel: 1, Owner: testOwner})

	// Step 1: tick until waiting_on is "split" (classify, then planning's
	// first turn posting the split question).
	waiting := false
	for range 5 {
		if err := d.Tick(t.Context()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if w := getTicket(t, s, parentID).WaitingOn; w != nil && *w == "split" {
			waiting = true
			break
		}
	}
	if !waiting {
		t.Fatalf("parent ticket %d never reached waiting_on = split", parentID)
	}
	if _, found, artErr := s.GetArtifact(t.Context(), parentID, "children"); artErr != nil || !found {
		t.Fatalf("GetArtifact(children): found=%v err=%v", found, artErr)
	}

	// Step 2: answer the split question with Approve, then send it, the
	// same console flow the owner's own click drives (design section 6.6/
	// 6.7: the existing answer-and-send route already handles split
	// questions).
	open, qErr := s.QuestionsByState(t.Context(), parentID, "open")
	if qErr != nil {
		t.Fatalf("QuestionsByState: %v", qErr)
	}
	if len(open) != 1 {
		t.Fatalf("open questions = %d, want exactly 1 (the split question)", len(open))
	}
	qID := open[0].ID
	option := "a"
	if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{TicketID: parentID, QuestionID: &qID, Option: &option}); draftErr != nil {
		t.Fatalf("SaveDraft(option a): %v", draftErr)
	}
	if _, sendErr := s.SendBatch(t.Context(), parentID); sendErr != nil {
		t.Fatalf("SendBatch: %v", sendErr)
	}

	// Step 3: tick until the parent's state is done (one tick per filed
	// child, then the close).
	done := false
	for range 5 {
		if err := d.Tick(t.Context()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if getTicket(t, s, parentID).State == testStateDone {
			done = true
			break
		}
	}
	if !done {
		t.Fatalf("parent ticket %d never reached done", parentID)
	}

	children, splitErr := s.SplitChildren(t.Context(), parentID)
	if splitErr != nil {
		t.Fatalf("SplitChildren: %v", splitErr)
	}
	if len(children) != 2 {
		t.Fatalf("SplitChildren = %+v, want 2", children)
	}
	refByKey := make(map[string]string, len(children))
	for _, c := range children {
		refByKey[c.Key] = c.Ref
	}
	c1Ref, c2Ref := refByKey["c1"], refByKey["c2"]
	if c1Ref == "" || c2Ref == "" {
		t.Fatalf("SplitChildren = %+v, want both c1 and c2 filed", children)
	}

	// Step 3 (fixture assertions): both issues exist on the fixture
	// tracker, and the second carries the first's ref.
	c2Issue, fetchErr := fx.Fetch(t.Context(), splitTestProject, c2Ref)
	if fetchErr != nil {
		t.Fatalf("Fetch(c2): %v", fetchErr)
	}
	if !strings.Contains(c2Issue.Body, "Split from") {
		t.Errorf("c2 body = %q, want it to carry \"Split from\"", c2Issue.Body)
	}
	if !strings.Contains(c2Issue.Body, "Depends on "+c1Ref) {
		t.Errorf("c2 body = %q, want it to carry \"Depends on %s\"", c2Issue.Body, c1Ref)
	}

	// Step 4: the child rows are queued, with parent_ticket_id and the
	// right split_key.
	for _, c := range children {
		child := getTicket(t, s, c.TicketID)
		if child.State != testStateQueued {
			t.Errorf("child %s state = %q, want queued", c.Key, child.State)
		}
		if child.ParentTicketID == nil || *child.ParentTicketID != parentID {
			t.Errorf("child %s parent_ticket_id = %v, want %d", c.Key, child.ParentTicketID, parentID)
		}
	}

	// Step 5: the parent's reason, its tracker close, and its comment.
	if reason := splitTestStateMessageReason(t, s, parentID); !strings.Contains(reason, "split into") {
		t.Errorf("parent reason = %q, want it to contain %q", reason, "split into")
	}
	if !fx.Closed(splitTestParentRef) {
		t.Errorf("parent issue %q was never closed", splitTestParentRef)
	}
	hasComment, commentErr := fx.CommentContains(t.Context(), splitTestProject, splitTestParentRef, "Zing split this ticket into")
	if commentErr != nil {
		t.Fatalf("CommentContains: %v", commentErr)
	}
	if !hasComment {
		t.Errorf("parent issue %q never got the split comment", splitTestParentRef)
	}

	// Step 6: c1 is a ready candidate, c2 is held on it.
	ready, readyErr := s.ListReadyCandidates(t.Context(), nil, time.Now())
	if readyErr != nil {
		t.Fatalf("ListReadyCandidates: %v", readyErr)
	}
	readyIDs := make(map[int64]bool, len(ready))
	for _, r := range ready {
		readyIDs[r.ID] = true
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
	if !readyIDs[c1ID] {
		t.Errorf("ListReadyCandidates = %v, want it to contain c1 (ticket %d)", readyIDs, c1ID)
	}
	if readyIDs[c2ID] {
		t.Errorf("ListReadyCandidates = %v, want it to exclude c2 (ticket %d), held on c1", readyIDs, c2ID)
	}

	// Step 7: once c1 is done, c2 becomes ready.
	seedTicketDirectlyToState(t, s, c1ID, testStateDone)
	ready, readyErr = s.ListReadyCandidates(t.Context(), nil, time.Now())
	if readyErr != nil {
		t.Fatalf("ListReadyCandidates (after c1 done): %v", readyErr)
	}
	readyIDs = make(map[int64]bool, len(ready))
	for _, r := range ready {
		readyIDs[r.ID] = true
	}
	if !readyIDs[c2ID] {
		t.Errorf("ListReadyCandidates (after c1 done) = %v, want it to contain c2 (ticket %d)", readyIDs, c2ID)
	}
}
