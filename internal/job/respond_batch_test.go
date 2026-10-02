package job

// respond_batch_test.go holds the three M4 task 4 tests PKG9-PLAN.md
// section 19.5 task 4 names that shipping_test.go's own respond section
// left out (design sections 5.6, 9.2, 14): a batch whose own count of
// actionable threads is 101, a ticket whose own batch number reaches 101,
// and findUniqueRespondStarted's own "no unique started marker" guard. It
// is a separate file, not an addition to shipping_test.go, only to avoid
// colliding with another agent's own edits to that file; it reuses
// shipping_test.go's own harness (shipPublished, shipPollRun, shipClaim,
// shipGreenCI, shipThread, shipHumanComment, respondScriptsFS,
// shipRespondReplyScript) and postbuild_test.go's own (pbApply,
// pbGetTicket), same package.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// seedRespondSkippedBatches writes n "respond batch k skipped" markers
// directly (k from 1 to n), standing in for n cheap closed batches with no
// actionable threads rather than driving n real POLL/RESPOND cycles:
// startRespondBatch's own prevMax read (respondNewestBatchN) counts every
// "respond batch " marker's own leading number regardless of its kind
// (started, retry, skipped, or stale), so the next real actionable poll
// picks up at n+1.
func seedRespondSkippedBatches(t *testing.T, s *store.Store, ticketID int64, n int) {
	t.Helper()
	owner := "seed-respond-skip"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("seedRespondSkippedBatches: claim: claimed=%v err=%v", claimed, err)
	}
	msgs := make([]store.Message, n)
	for i := range n {
		msgs[i] = store.Message{
			TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("respond batch %d skipped", i+1),
		}
	}
	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires, Messages: msgs,
	})
	if err != nil || !applied {
		t.Fatalf("seedRespondSkippedBatches: commit: applied=%v err=%v", applied, err)
	}
}

// TestRespondBatch101 proves PKG9-PLAN.md section 19.5 task 4's own "the
// 101st batch of a ticket validates and stores": response.RespondArtifact's
// own Batch field (internal/response/types.go) carries no maximum, so a
// three-digit batch number is no different from any other. With 100 earlier
// batches already closed, POLL's own row 5 (design section 8.5) opens batch
// 101, and RESPOND's own ok outcome (9.2) stores its artifact through the
// real commit path (pbApply, store.Store.CommitHandlerResult), which
// validates every artifact against its own JSON Schema (schema.go) before
// it stores -- that call succeeding against a real batch number of 101 is
// what "validates" means here, not a separate check.
func TestRespondBatch101(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr := shipPublished(t)
	seedRespondSkippedBatches(t, s, ticket.ID, 100)

	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	local := shipHeadSHA(t, s, ticket)
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}
	gh.threads = []orchestrator.Thread{
		shipThread(shipRespondThreadID, "greet.go", 3, shipHumanComment("c1", "reviewer1", "please fix this", when)),
	}

	pollCommit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	wantPrefix := "respond batch 101 started sha " + local + " after run "
	found := false
	for _, m := range pollCommit.Messages {
		if strings.HasPrefix(m.Body, wantPrefix) {
			found = true
		}
	}
	if !found {
		t.Fatalf("pollCommit.Messages = %+v, want a %q marker", pollCommit.Messages, wantPrefix)
	}
	pbApply(t, s, ticket, pollCommit)

	// respondScriptsFS (shipping_test.go) always keys its own scripts
	// "respond/1/<turn>.xml": batch 1's own label. runtime.RunRequest.Label
	// is strconv.Itoa(n) (respondRunFirst, respond.go), so batch 101's own
	// first turn looks up "respond/101/1.xml" instead.
	rt := runtime.NewFake(fstest.MapFS{"respond/101/1.xml": &fstest.MapFile{Data: []byte(shipRespondReplyScript)}})
	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	runCommit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runCommit.Escalation != nil {
		t.Fatalf("got an escalation, want a stored artifact: %+v", runCommit.Escalation.Payload)
	}
	if len(runCommit.Artifacts) != 1 {
		t.Fatalf("Artifacts = %+v, want exactly one", runCommit.Artifacts)
	}
	var artifact response.RespondArtifact
	if unmarshalErr := json.Unmarshal(runCommit.Artifacts[0].Payload, &artifact); unmarshalErr != nil {
		t.Fatalf("unmarshal artifact: %v", unmarshalErr)
	}
	if artifact.Batch != 101 {
		t.Errorf("Batch = %d, want 101", artifact.Batch)
	}

	pbApply(t, s, ticket, runCommit)
}

// shipManyThreads builds n unresolved, actionable orchestrator.Thread
// values, each with a distinct raw id and one human comment: design section
// 14's "101 or more actionable threads" edge case, and
// response.RespondArtifact's own jsonschema maxItems=1000 (internal/response/types.go).
func shipManyThreads(n int, at time.Time) []orchestrator.Thread {
	threads := make([]orchestrator.Thread, n)
	for i := range n {
		rawID := fmt.Sprintf("RT_many_thread_%d", i)
		threads[i] = shipThread(rawID, "greet.go", i+1, shipHumanComment(fmt.Sprintf("c%d", i), "reviewer1", "please fix this", at))
	}
	return threads
}

// TestRespondBatchOf101Threads proves PKG9-PLAN.md section 19.5 task 4's
// own "101 actionable threads: one batch, 101 actions, one artifact
// stored" (design section 14's edge case): POLL's own row 5 (8.5) batches
// every actionable thread in one "respond batch 1 started" marker, not
// several smaller batches, and RESPOND's own ok outcome, once every tid has
// an action, stores exactly one artifact carrying all 101.
func TestRespondBatchOf101Threads(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, ticket, gh, tr := shipPublished(t)

	const n = 101
	gh.threads = shipManyThreads(n, when)
	runs, required := shipGreenCI()
	gh.runs, gh.required = runs, required
	local := shipHeadSHA(t, s, ticket)
	gh.prState = orchestrator.PRState{Draft: true, HeadSHA: local, BaseRef: pbFixtureDefaultBranch}

	pollCommit, err := shipPollRun(t, s, ticket, gh, tr)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	wantPrefix := "respond batch 1 started sha " + local + " after run "
	var markerBody string
	for _, m := range pollCommit.Messages {
		if strings.HasPrefix(m.Body, wantPrefix) {
			markerBody = m.Body
		}
	}
	if markerBody == "" {
		t.Fatalf("pollCommit.Messages = %+v, want a %q marker", pollCommit.Messages, wantPrefix)
	}
	pbApply(t, s, ticket, pollCommit)

	line2, _, rawErr := respondBatchRawLines(markerBody)
	if rawErr != nil {
		t.Fatalf("respondBatchRawLines: %v", rawErr)
	}
	tids := strings.Split(line2, ",")
	if len(tids) != n {
		t.Fatalf("batch tids = %d, want %d", len(tids), n)
	}

	var script strings.Builder
	script.WriteString(`<zing job="respond" outcome="ok">` + "\n")
	for _, id := range tids {
		fmt.Fprintf(&script, "  <thread id=%q action=\"reply\">Thanks for flagging this -- fixed as described.</thread>\n", id)
	}
	script.WriteString("</zing>")

	rt := runtime.NewFake(respondScriptsFS(script.String()))
	deps := shipClaim(t, s, rt, ticket.ID, gh, tr)
	runCommit, err := (shipHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runCommit.Escalation != nil {
		t.Fatalf("got an escalation, want a stored artifact: %+v", runCommit.Escalation.Payload)
	}
	for _, m := range runCommit.Messages {
		if strings.HasPrefix(m.Body, "respond coverage failed run ") {
			t.Fatalf("got a coverage failure, want full coverage: %q", m.Body)
		}
	}
	if len(runCommit.Artifacts) != 1 {
		t.Fatalf("Artifacts = %+v, want exactly one", runCommit.Artifacts)
	}
	var artifact response.RespondArtifact
	if unmarshalErr := json.Unmarshal(runCommit.Artifacts[0].Payload, &artifact); unmarshalErr != nil {
		t.Fatalf("unmarshal artifact: %v", unmarshalErr)
	}
	if artifact.Batch != 1 {
		t.Errorf("Batch = %d, want 1", artifact.Batch)
	}
	if len(artifact.Threads) != n {
		t.Errorf("Threads = %d actions, want %d", len(artifact.Threads), n)
	}
	if len(artifact.Seen) != n {
		t.Errorf("Seen = %d entries, want %d", len(artifact.Seen), n)
	}

	pbApply(t, s, ticket, runCommit)
}

// TestRespondRetryNeedsUniqueStarted proves PKG9-PLAN.md section 5.6's own
// respond cap_resumes row: "code reads the one `respond batch <n> started`
// marker (zero or two or more is the error `job: respond batch <n> has no
// unique started marker`)". findUniqueRespondStarted (respond.go) is the
// function that reads it; this calls it directly, the same shape a
// resumes_exhausted retry (retryCapResumesRespond) would hit if a batch's
// own marker history were ever corrupted this way.
func TestRespondRetryNeedsUniqueStarted(t *testing.T) {
	t.Parallel()

	t.Run("zero", func(t *testing.T) {
		t.Parallel()
		markers := []store.MessageRow{
			{Body: "respond batch 1 skipped"},
		}
		_, err := findUniqueRespondStarted(markers, 1)
		if err == nil {
			t.Fatal("findUniqueRespondStarted: got nil error, want one")
		}
		want := "job: respond batch 1 has no unique started marker"
		if err.Error() != want {
			t.Errorf("findUniqueRespondStarted: err = %q, want %q", err.Error(), want)
		}
	})

	t.Run("two", func(t *testing.T) {
		t.Parallel()
		sha := strings.Repeat("a", 40)
		markers := []store.MessageRow{
			{Body: fmt.Sprintf("respond batch 1 started sha %s after run 1\nt1\nseen t1=abc", sha)},
			{Body: fmt.Sprintf("respond batch 1 started sha %s after run 2\nt1\nseen t1=def", sha)},
		}
		_, err := findUniqueRespondStarted(markers, 1)
		if err == nil {
			t.Fatal("findUniqueRespondStarted: got nil error, want one")
		}
		want := "job: respond batch 1 has no unique started marker"
		if err.Error() != want {
			t.Errorf("findUniqueRespondStarted: err = %q, want %q", err.Error(), want)
		}
	})
}
