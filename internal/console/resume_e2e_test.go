// resume_e2e_test.go is Task 7's verify-by (design section 6.7, 12 row 7,
// 14): drive the fixture ticket from queued through planning until it posts
// its Q1 question and waits, answer it through the console's real POST
// /draft then POST /send exactly as a browser's chip click and send chord
// would, and watch the dispatcher resume the planning run once and carry
// the ticket the rest of the way to done, over the console's own live
// GET /stream. It uses the real dispatcher, the fixture tracker, the fake
// runtime, the real store, the real bus, and the real console endpoints --
// nothing is mocked.
package console_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	zing "zing"
	"zing/fixtures"
	"zing/internal/bus"
	zdispatch "zing/internal/dispatch"
	"zing/internal/job"
	"zing/internal/machine"
	"zing/internal/response"
	zruntime "zing/internal/runtime"
	"zing/internal/store"
	"zing/internal/tracker"
)

// resumeE2EMaxTicks and resumeE2EOwner mirror cmd/zing/selftest.go's own
// e2eMaxTicks and e2eOwner: enough bounded ticks for intake plus one
// handler call per pipeline transition, with headroom, so a stuck
// dispatcher fails this test promptly instead of hanging it.
const (
	resumeE2EMaxTicks = 50
	resumeE2EOwner    = "resume-e2e"
)

// resumeE2EWantStates is the ordered "to" state of every state message the
// fixture ticket's full run posts (design section 7.1), the same sequence
// cmd/zing/selftest.go's e2eWantStates asserts.
var resumeE2EWantStates = []string{"planning", "building", "reviewing", "judging", "shipping", "done"}

// TestResumeE2E_AnswerViaConsoleAdvancesTicketToDoneWithNoLeak is the
// verify-by. See the file doc comment above.
func TestResumeE2E_AnswerViaConsoleAdvancesTicketToDoneWithNoLeak(t *testing.T) {
	ctx := t.Context()

	st, err := store.Open(ctx, t.TempDir()+"/zing.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	m, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("machine.Load: %v", err)
	}

	scripts, err := fs.Sub(fixtures.FS, "scripts")
	if err != nil {
		t.Fatalf("sub scripts fs: %v", err)
	}
	rt := zruntime.NewFake(scripts)

	tr, err := tracker.NewFixture(fixtures.FS, "tickets.toml")
	if err != nil {
		t.Fatalf("tracker.NewFixture: %v", err)
	}

	projectID, err := st.EnsureProject(ctx, store.Project{
		Name: testAuthorZing, RepoURL: "https://example.invalid/zing", LocalPath: t.TempDir(), Tracker: testTrackerGitHub,
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	b := bus.New()
	d, err := zdispatch.New(st, tr, b, m, job.Registry(),
		[]zdispatch.Binding{{StoreProjectID: projectID, TrackerProject: testAuthorZing}},
		zdispatch.Config{Interval: time.Millisecond, MaxParallel: 2, Owner: resumeE2EOwner}, rt)
	if err != nil {
		t.Fatalf("dispatch.New: %v", err)
	}

	srv, _ := newMutationTestServer(t, st, b)

	// Drive the dispatcher directly (as cmd/zing's own selftestE2E does),
	// bounded, until the fixture ticket appears and waits on its planning
	// question.
	var ticketID int64
	for i := range resumeE2EMaxTicks {
		if tickErr := d.Tick(ctx); tickErr != nil {
			t.Fatalf("tick %d: %v", i, tickErr)
		}
		tickets, listErr := st.ListAllTickets(ctx)
		if listErr != nil {
			t.Fatalf("ListAllTickets: %v", listErr)
		}
		if len(tickets) == 0 {
			continue
		}
		if len(tickets) != 1 {
			t.Fatalf("%d tickets after intake, want exactly 1", len(tickets))
		}
		ticketID = tickets[0].ID
		if tickets[0].WaitingOn != nil && *tickets[0].WaitingOn == testWaitingQuestions {
			break
		}
	}
	if ticketID == 0 {
		t.Fatal("the fixture ticket never appeared within the tick budget")
	}
	ticket, err := st.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if ticket.WaitingOn == nil || *ticket.WaitingOn != testWaitingQuestions {
		t.Fatalf("ticket.WaitingOn = %v, want \"questions\" (the ticket never reached its planning question)", ticket.WaitingOn)
	}

	// The baseline is taken right before the /stream connection opens, with
	// the server and every other steady-state goroutine already running, so
	// the leak check at the end isolates this one connection's own
	// resources rather than the server's constant overhead (design section
	// 11, "stream" test seam; stream_test.go's own
	// TestStreamDisconnectUnsubscribesWithNoGoroutineLeak uses the same
	// technique).
	baseline := runtime.NumGoroutine()

	// Open the thread's live stream now, before answering, so its frames
	// cover the whole resume (design section 12 row 7: "watch the resume").
	streamResp, streamReader, cancelStream := openThreadStream(t, srv.URL, ticketID)
	frames := newFrameCollector(streamReader)

	open, err := st.QuestionsByState(ctx, ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open questions on the fixture ticket = %d, want exactly 1 (Q1)", len(open))
	}
	var payload response.QuestionPayload
	if unmarshalErr := json.Unmarshal(open[0].Payload, &payload); unmarshalErr != nil {
		t.Fatalf("unmarshal question payload: %v", unmarshalErr)
	}
	if len(payload.Options) == 0 {
		t.Fatal("Q1 has no options")
	}
	chosen := payload.Options[0].Key // "a", the worked example's own pick (design section 6.7)

	// POST /draft with the chosen option, then POST /send: the same
	// keyboard-driven two-step the composer takes (design section 6.4, 6.7).
	// Both requests set Close so their connections do not linger in the
	// client's keep-alive pool, which would otherwise show up as apparent
	// growth in the goroutine-settle check below even though nothing leaked.
	draftBody := fmt.Sprintf(`{"ticket":%d,"question":%d,"option":%q}`, ticketID, open[0].ID, chosen)
	draftReq := mutationRequest(t, srv, "/draft", draftBody)
	draftReq.Close = true
	draftResp := doRequest(t, draftReq)
	_ = draftResp.Body.Close()
	if draftResp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /draft status = %d, want 204", draftResp.StatusCode)
	}

	sendBody := fmt.Sprintf(`{"ticket":%d}`, ticketID)
	sendReq := mutationRequest(t, srv, "/send", sendBody)
	sendReq.Close = true
	sendResp := doRequest(t, sendReq)
	_ = sendResp.Body.Close()
	if sendResp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /send status = %d, want 204", sendResp.StatusCode)
	}

	afterSend, err := st.GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("GetTicket after send: %v", err)
	}
	if afterSend.WaitingOn != nil {
		t.Fatalf("ticket.WaitingOn after send = %q, want nil (the batch cleared the wait)", *afterSend.WaitingOn)
	}

	// Keep ticking: the next tick picks the now-eligible ticket back up
	// (design section 14 reconciliation: "dispatch resume is emergent ...
	// for the verify-by ... the cleared ticket is picked on the next
	// tick"), resumes the planning run once, and carries the ticket the
	// rest of the way to done.
	reachedDone := false
	for i := range resumeE2EMaxTicks {
		if tickErr := d.Tick(ctx); tickErr != nil {
			t.Fatalf("post-send tick %d: %v", i, tickErr)
		}
		ticket, err = st.GetTicket(ctx, ticketID)
		if err != nil {
			t.Fatalf("GetTicket: %v", err)
		}
		if ticket.State == "done" {
			reachedDone = true
			break
		}
	}
	if !reachedDone {
		t.Fatalf("ticket did not reach done within the post-send tick budget; last state %q waiting_on %v", ticket.State, ticket.WaitingOn)
	}

	assertResumeE2EStateSequence(t, st, ticketID)
	assertResumeE2EResumedOnce(t, st, ticketID)

	// Poll (bounded, not a fixed sleep) until the live stream itself has
	// shown every state transition, including "done": the ticket reaching
	// done in the store (asserted above) and the SSE handler's own
	// subsequent wake-and-patch are two different goroutines, so a fixed
	// assertion right after the store read could race a frame that simply
	// has not been flushed yet.
	waitForFrames(t, frames, resumeE2EWantStates)
	assertResumeE2EFramesSawEveryState(t, frames.String())

	// Stop watching and prove the stream handler's goroutine, and its bus
	// subscription, are both really gone -- the same technique
	// stream_test.go's own disconnect test uses (design section 11:
	// "no goroutine or subscription leak").
	cancelStream()
	_ = streamResp.Body.Close()
	deadline := time.Now().Add(frameTimeout)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Errorf("goroutine count did not settle back to the pre-stream baseline: got %d, baseline %d",
				runtime.NumGoroutine(), baseline)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ch, unsub := b.Subscribe()
	defer unsub()
	b.Publish()
	select {
	case <-ch:
	case <-time.After(frameTimeout):
		t.Fatal("a fresh subscriber saw no publish after the resume stream disconnected")
	}
}

// openThreadStream opens GET /stream?view=thread&open=<ticketID> against
// base and returns the response, a buffered reader over its body, and its
// cancel func, matching stream_test.go's own openStream helper but scoped
// to this file so the E2E does not need to import a private detail from
// there.
func openThreadStream(t *testing.T, base string, ticketID int64) (*http.Response, *bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL(base, "thread", ticketID, 0), http.NoBody)
	if err != nil {
		cancel()
		t.Fatalf("build GET /stream request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /stream: %v", err)
	}
	return resp, bufio.NewReader(resp.Body), cancel
}

// frameCollector accumulates every line read from a live /stream connection
// behind a mutex, so the main goroutine can poll its running text
// (String()) while the background reader is still live, rather than only
// learning what arrived after the connection closes.
type frameCollector struct {
	mu   sync.Mutex
	text strings.Builder
}

// newFrameCollector spawns one goroutine reading every SSE frame from r
// until it hits an error (the stream closing, on the caller's cancel).
func newFrameCollector(r *bufio.Reader) *frameCollector {
	c := &frameCollector{}
	go func() {
		for {
			line, err := r.ReadString('\n')
			c.mu.Lock()
			c.text.WriteString(line)
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return c
}

// String returns everything read so far, safe to call while the background
// reader is still running.
func (c *frameCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String()
}

// waitForFrames polls (bounded by frameTimeout, not a fixed sleep) until
// frames has shown a transition to every one of wantStates, matching this
// package's own bounded-poll style for synchronizing on the SSE stream
// (stream_test.go's TestStreamDisconnectUnsubscribesWithNoGoroutineLeak).
func waitForFrames(t *testing.T, frames *frameCollector, wantStates []string) {
	t.Helper()
	deadline := time.Now().Add(frameTimeout)
	for {
		if framesShowEveryState(frames.String(), wantStates) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("live stream frames never showed every transition within %s; got:\n%s", frameTimeout, frames.String())
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// framesShowEveryState reports whether text contains a state separator
// (views.go's stateLine: "<from> -> <to>") for every state in wantStates,
// accepting either the raw or HTML-escaped arrow (templ auto-escapes text
// content, and this package does not otherwise assert which it chooses).
func framesShowEveryState(text string, wantStates []string) bool {
	for _, want := range wantStates {
		if !strings.Contains(text, "-&gt; "+want) && !strings.Contains(text, "-> "+want) {
			return false
		}
	}
	return true
}

// assertResumeE2EFramesSawEveryState fails the test unless the live
// stream's collected text carries every one of resumeE2EWantStates' state
// separators (views.go's stateLine: "<from> -> <to>"), proving the resume
// and the rest of the run were actually watched over the SSE connection,
// not only visible after the fact through a store read.
func assertResumeE2EFramesSawEveryState(t *testing.T, frames string) {
	t.Helper()
	if !framesShowEveryState(frames, resumeE2EWantStates) {
		t.Errorf("live stream frames never showed every transition in %v; got:\n%s", resumeE2EWantStates, frames)
	}
}

// assertResumeE2EStateSequence asserts ticketID's "state" messages, in id
// order, are exactly resumeE2EWantStates -- the same check cmd/zing's own
// selftestE2E and serve_test.go's TestServe_RingToDone... make.
func assertResumeE2EStateSequence(t *testing.T, st *store.Store, ticketID int64) {
	t.Helper()
	msgs, err := st.ListMessages(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	var states []string
	var questions, answers, resolved int
	for i := range msgs {
		msg := &msgs[i]
		switch msg.Type {
		case "state":
			var sp response.StatePayload
			if err := json.Unmarshal(msg.Payload, &sp); err != nil {
				t.Fatalf("unmarshal state message %d: %v", msg.ID, err)
			}
			states = append(states, string(sp.To))
		case testMsgTypeQuestion:
			questions++
		case "answer":
			answers++
		case "resolved":
			resolved++
		}
	}

	if !slices.Equal(states, resumeE2EWantStates) {
		t.Errorf("state messages = %v, want %v", states, resumeE2EWantStates)
	}
	if questions != 1 {
		t.Errorf("question messages = %d, want exactly 1", questions)
	}
	if answers != 1 {
		t.Errorf("answer messages = %d, want exactly 1", answers)
	}
	if resolved != 1 {
		t.Errorf("resolved messages = %d, want exactly 1", resolved)
	}
}

// assertResumeE2EResumedOnce asserts the ticket's one planning session
// resumed exactly once (job/skeleton.go bumps Session.Resumes on every
// resume commit), the "the planning session resumes once" checkpoint
// design section 12 row 7 and section 14's dispatcher contract both name.
func assertResumeE2EResumedOnce(t *testing.T, st *store.Store, ticketID int64) {
	t.Helper()
	sessions, err := st.SessionsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	var planning []store.Session
	for _, sess := range sessions {
		if sess.Job == "planning" {
			planning = append(planning, sess)
		}
	}
	if len(planning) != 1 {
		t.Fatalf("planning sessions for ticket %d = %d, want exactly 1", ticketID, len(planning))
	}
	if planning[0].Resumes != 1 {
		t.Errorf("planning session Resumes = %d, want exactly 1 (one resume, from clearing the questions wait)", planning[0].Resumes)
	}
}
