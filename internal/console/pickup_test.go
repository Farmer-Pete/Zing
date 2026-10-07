package console_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"zing/internal/bus"
	"zing/internal/store"
	"zing/internal/tracker"
)

// pickupComment is one Comment call pickupTestTracker recorded.
type pickupComment struct {
	project, ref, body string
}

// pickupTestTracker is a minimal Tracker test double for POST
// /projects/{id}/pickup (PKG9-PLAN.md D29): Issue answers from issues or
// issueErr, keyed by ref, and Comment records every call so a test can
// assert exactly one pickup comment was posted. Every other Tracker method
// panics: this handler never calls them.
type pickupTestTracker struct {
	mu       sync.Mutex
	issues   map[string]tracker.Ticket
	issueErr map[string]error
	comments []pickupComment
}

func newPickupTestTracker() *pickupTestTracker {
	return &pickupTestTracker{issues: map[string]tracker.Ticket{}, issueErr: map[string]error{}}
}

func (p *pickupTestTracker) Issue(_ context.Context, _, ref string) (tracker.Ticket, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err, ok := p.issueErr[ref]; ok {
		return tracker.Ticket{}, err
	}
	if tk, ok := p.issues[ref]; ok {
		return tk, nil
	}
	return tracker.Ticket{}, tracker.ErrIssueNotFound
}

func (p *pickupTestTracker) Comment(_ context.Context, project, ref, body string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.comments = append(p.comments, pickupComment{project: project, ref: ref, body: body})
	return nil
}

func (p *pickupTestTracker) recordedComments() []pickupComment {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]pickupComment(nil), p.comments...)
}

func (p *pickupTestTracker) Intake(context.Context, string, tracker.IntakeRule) ([]tracker.Ticket, error) {
	panic("pickupTestTracker: Intake is unused by POST /projects/{id}/pickup")
}

func (p *pickupTestTracker) Fetch(context.Context, string, string) (tracker.Ticket, error) {
	panic("pickupTestTracker: Fetch is unused by POST /projects/{id}/pickup")
}

func (p *pickupTestTracker) FileTicket(context.Context, string, tracker.NewTicket) (string, error) {
	panic("pickupTestTracker: FileTicket is unused by POST /projects/{id}/pickup")
}

func (p *pickupTestTracker) Collaborators(context.Context, string) ([]string, error) {
	panic("pickupTestTracker: Collaborators is unused by POST /projects/{id}/pickup")
}

func (p *pickupTestTracker) Close(context.Context, string, string) error {
	panic("pickupTestTracker: Close is unused by POST /projects/{id}/pickup")
}

func (p *pickupTestTracker) CommentContains(context.Context, string, string, string) (bool, error) {
	panic("pickupTestTracker: CommentContains is unused by POST /projects/{id}/pickup")
}

func (p *pickupTestTracker) Comments(context.Context, string, string) ([]tracker.Comment, error) {
	panic("pickupTestTracker: Comments is unused by POST /projects/{id}/pickup")
}

var _ tracker.Tracker = (*pickupTestTracker)(nil)

// seedPickupProject ensures testProject and returns its store id.
func seedPickupProject(t *testing.T, s *store.Store) int64 {
	t.Helper()
	id, err := s.EnsureProject(t.Context(), testProject)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return id
}

// pickupPath builds POST /projects/{id}/pickup's path.
func pickupPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/pickup"
}

// readBody reads and closes resp.Body, failing the test on a read error.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// TestPickup_RefusesAClosedIssue proves D29's exact closed-issue refusal
// (409, "issue #<n> is closed") and that no ticket is inserted.
func TestPickup_RefusesAClosedIssue(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	projectID := seedPickupProject(t, s)

	tr := newPickupTestTracker()
	tr.issueErr["5"] = tracker.ErrIssueClosed

	srv := newTestServerPickup(t, s, bus.New(), newTestLogHandler(t), tr)
	resp := doRequest(t, mutationRequest(t, srv, pickupPath(projectID), `{"n":5}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got, want := readBody(t, resp), "issue #5 is closed"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	assertNoTicketInserted(t, s, projectID)
}

// TestPickup_RefusesAPullRequest proves D29's exact pull-request refusal
// (409, "#<n> is a pull request, not an issue").
func TestPickup_RefusesAPullRequest(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	projectID := seedPickupProject(t, s)

	tr := newPickupTestTracker()
	tr.issueErr["5"] = tracker.ErrIssueIsPullRequest

	srv := newTestServerPickup(t, s, bus.New(), newTestLogHandler(t), tr)
	resp := doRequest(t, mutationRequest(t, srv, pickupPath(projectID), `{"n":5}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got, want := readBody(t, resp), "#5 is a pull request, not an issue"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	assertNoTicketInserted(t, s, projectID)
}

// TestPickup_RefusesAMissingIssue proves D29's exact not-found refusal
// (409, "issue #<n> not found").
func TestPickup_RefusesAMissingIssue(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	projectID := seedPickupProject(t, s)

	tr := newPickupTestTracker() // "5" is in neither map, so Issue returns ErrIssueNotFound

	srv := newTestServerPickup(t, s, bus.New(), newTestLogHandler(t), tr)
	resp := doRequest(t, mutationRequest(t, srv, pickupPath(projectID), `{"n":5}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got, want := readBody(t, resp), "issue #5 not found"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	assertNoTicketInserted(t, s, projectID)
}

// TestPickup_RefusesAnAlreadyTicketedIssue proves D29's exact already-ticket
// refusal (409, "issue #<n> is already ticket <id>"), checked before any
// tracker call (the tracker double here would panic on an Issue call it
// never expects for this ref, proving the dedup check ran first).
func TestPickup_RefusesAnAlreadyTicketedIssue(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	projectID := seedPickupProject(t, s)
	existingID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: "5", Title: "Already here", State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}

	tr := newPickupTestTracker()

	srv := newTestServerPickup(t, s, bus.New(), newTestLogHandler(t), tr)
	resp := doRequest(t, mutationRequest(t, srv, pickupPath(projectID), `{"n":5}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	want := fmt.Sprintf("issue #5 is already ticket %d", existingID)
	if got := readBody(t, resp); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestPickup_SucceedsAndReturnsTheTicketID proves the success path
// (PKG9-PLAN.md D29): 200 with a JSON body naming the issue and the new
// ticket's id, a new queued ticket lands in the store with the issue's
// fields, and exactly one pickup comment is posted through the same shared
// step intake itself uses (dispatch.InsertAndAnnounce).
func TestPickup_SucceedsAndReturnsTheTicketID(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	projectID := seedPickupProject(t, s)

	tr := newPickupTestTracker()
	tr.issues["5"] = tracker.Ticket{Ref: "5", Title: "A real issue", Body: "do the thing"}

	srv := newTestServerPickup(t, s, bus.New(), newTestLogHandler(t), tr)
	resp := doRequest(t, mutationRequest(t, srv, pickupPath(projectID), `{"n":5}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != contentTypeJSONForTest {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeJSONForTest)
	}

	tickets, err := s.TicketsByProject(t.Context(), projectID)
	if err != nil {
		t.Fatalf("TicketsByProject: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("tickets after pickup = %d, want 1", len(tickets))
	}
	got := tickets[0]
	if got.TrackerRef != "5" || got.Title != "A real issue" || got.Body != "do the thing" || got.State != testStateQueued {
		t.Errorf("inserted ticket = %+v, want ref=5 title=%q body=%q state=%s", got, "A real issue", "do the thing", testStateQueued)
	}

	var body struct {
		N        int   `json:"n"`
		TicketID int64 `json:"ticket_id"`
	}
	dec := json.NewDecoder(resp.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if body.N != 5 {
		t.Errorf("response n = %d, want 5", body.N)
	}
	if body.TicketID != got.ID {
		t.Errorf("response ticket_id = %d, want %d", body.TicketID, got.ID)
	}

	comments := tr.recordedComments()
	if len(comments) != 1 {
		t.Fatalf("pickup comments posted = %d, want exactly 1: %+v", len(comments), comments)
	}
	if comments[0].project != testProject.Name || comments[0].ref != "5" {
		t.Errorf("pickup comment = %+v, want project %q ref 5", comments[0], testProject.Name)
	}
}

// TestPickup_AcceptsAnIssueWhoseTicketIsAbandoned proves pickup accepts an
// issue whose only ticket is abandoned (#65): it retires the old ticket's
// ref to "5-abandoned-1" and inserts a fresh queued ticket at "5", exactly
// as a restart does.
func TestPickup_AcceptsAnIssueWhoseTicketIsAbandoned(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	projectID := seedPickupProject(t, s)

	oldID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: "5", Title: "first attempt", State: testStateQueued,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	if err = s.AbandonTicket(t.Context(), oldID, "test setup"); err != nil {
		t.Fatalf("AbandonTicket: %v", err)
	}

	tr := newPickupTestTracker()
	tr.issues["5"] = tracker.Ticket{Ref: "5", Title: "a fresh attempt", Body: "do it again"}

	srv := newTestServerPickup(t, s, bus.New(), newTestLogHandler(t), tr)
	resp := doRequest(t, mutationRequest(t, srv, pickupPath(projectID), `{"n":5}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	tickets, err := s.TicketsByProject(t.Context(), projectID)
	if err != nil {
		t.Fatalf("TicketsByProject: %v", err)
	}
	if len(tickets) != 2 {
		t.Fatalf("tickets after pickup = %d, want 2: %+v", len(tickets), tickets)
	}

	var newTicket, oldTicket *store.Ticket
	for i := range tickets {
		switch tickets[i].ID {
		case oldID:
			oldTicket = &tickets[i]
		default:
			newTicket = &tickets[i]
		}
	}
	if newTicket == nil || newTicket.TrackerRef != "5" || newTicket.State != testStateQueued {
		t.Errorf("new ticket = %+v, want ref=5 state=%s", newTicket, testStateQueued)
	}
	if oldTicket == nil || oldTicket.TrackerRef != testRetiredRef5First || oldTicket.State != "abandoned" {
		t.Errorf("old ticket = %+v, want ref=5-abandoned-1 state=abandoned", oldTicket)
	}

	comments := tr.recordedComments()
	if len(comments) != 1 {
		t.Fatalf("pickup comments posted = %d, want exactly 1: %+v", len(comments), comments)
	}
	if comments[0].ref != "5" {
		t.Errorf("pickup comment ref = %q, want 5", comments[0].ref)
	}
}

// TestPickup_RejectsNonPositiveN proves n must be a positive integer (400),
// before any tracker or store call.
func TestPickup_RejectsNonPositiveN(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	projectID := seedPickupProject(t, s)

	tr := newPickupTestTracker()
	srv := newTestServerPickup(t, s, bus.New(), newTestLogHandler(t), tr)

	for _, body := range []string{`{"n":0}`, `{"n":-1}`} {
		resp := doRequest(t, mutationRequest(t, srv, pickupPath(projectID), body))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

// TestPickup_CrossOriginRequestRefused proves POST /projects/{id}/pickup
// sits behind the same same-origin guard as every other console POST
// (mw.go, design section 6.14): a cross-site Origin is rejected with 403
// before the handler ever runs.
func TestPickup_CrossOriginRequestRefused(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	projectID := seedPickupProject(t, s)

	tr := newPickupTestTracker()
	srv := newTestServerPickup(t, s, bus.New(), newTestLogHandler(t), tr)

	req := mutationRequest(t, srv, pickupPath(projectID), `{"n":5}`)
	req.Header.Set("Origin", "http://evil.example")
	resp := doRequest(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}

	assertNoTicketInserted(t, s, projectID)
}

// assertNoTicketInserted fails the test if projectID carries any ticket.
func assertNoTicketInserted(t *testing.T, s *store.Store, projectID int64) {
	t.Helper()
	tickets, err := s.TicketsByProject(t.Context(), projectID)
	if err != nil {
		t.Fatalf("TicketsByProject: %v", err)
	}
	if len(tickets) != 0 {
		t.Errorf("tickets after refusal = %d, want 0: %+v", len(tickets), tickets)
	}
}
