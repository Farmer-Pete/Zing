// reviewing_test.go tests task 10: ROUND (design section 6.2), ASKED and
// CONTINUE (6.2a), and FIXREQ (6.8); and task 11: TRIAGE (6.5) and DISCUSS
// (6.6), plus the Task 10 handoff's own runLensesParallel deadlock fix
// (TestRunLensesParallelRejectsOutOfRangeConfig) -- all through the real
// reviewingHandler. It reuses postbuild_test.go's own harness
// (newPostbuildTestStore, pbClaim, pbTicketInReviewing, pbApply,
// pbGetTicket, pbMachine), package job (unreachable from job_test), plus
// its own small fixture-script builder: a review round's seven lens turns,
// served from an in-memory fs.FS rather than the checked-in
// fixtures/scripts/review tree (selftest's and the console e2e's own demo,
// task 10's Files list), so each test scripts exactly the lens outcomes it
// needs.
package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"zing/internal/gitfixture"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// lensFidelity names the one lens most of this file's own question and
// continue scenarios script as the asker: named once since it repeats
// across so many of them (goconst).
const lensFidelity = "fidelity"

// simplificationRunLabel names the simplification lens's own first-turn
// run label: several two-in-a-row escalation scenarios script that one
// lens's exec failure, so it repeats across them (goconst).
const simplificationRunLabel = "1-simplification"

// lensTests, testsRunLabel, testsSessionID, and testsInvalidDetail name the
// tests lens's own invalid-retry scenarios (issue #32): this file's task 1
// tests all drive that lens through an invalid first turn and a same-tick
// retry, so each repeats across several of them (goconst).
const (
	lensTests          = "tests"
	testsRunLabel      = "1-" + lensTests
	testsSessionID     = "tests-sess"
	testsInvalidDetail = "line 4: closing tag mismatch"
)

// reviewLensNames is machine.toml's own jobs.review.lenses, spelled out
// literally: reviewing_test.go builds its own in-memory scripts keyed by
// these names, and a handful of tests need the plain list to build a
// tailored override.
var reviewLensNames = []string{
	"simplification", "correctness", "security", lensFidelity, lensTests, "quality", "observability",
}

// reviewOKScript is the fake runtime's own minimal "ok" document: a review
// lens that found nothing.
const reviewOKScript = `<zing job="review" outcome="ok"></zing>`

// reviewScriptKey is the fake runtime's own "<job>/<label>/<turn>.xml" key
// for round 1's lens turn (internal/runtime/fake.go's own scriptKey,
// unreachable from here, hence the small local copy): every test in this
// file drives round 1 only (re-review, round 2+, is task 12's own work),
// so this builds that round's own label directly.
func reviewScriptKey(lens string, turn int) string {
	return fmt.Sprintf("review/1-%s/%d.xml", lens, turn)
}

// reviewScriptsFS builds an in-memory fs.FS with round 1's own first turn
// for every lens in reviewLensNames set to reviewOKScript, then overrides
// (or adds) exactly the paths overrides names: a test that wants one lens
// to ask, fail, or return a finding sets that one path (reviewScriptKey)
// and leaves every other lens clean. Every test in this file drives round
// 1 only (re-review, round 2+, is task 12's own work), so this builds that
// round directly rather than taking a round number no caller varies.
func reviewScriptsFS(overrides map[string]string) fstest.MapFS {
	m := make(fstest.MapFS, len(reviewLensNames)+len(overrides))
	for _, lens := range reviewLensNames {
		m[reviewScriptKey(lens, 1)] = &fstest.MapFile{Data: []byte(reviewOKScript)}
	}
	for path, content := range overrides {
		m[path] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

// findingScript is one review lens's "ok" document carrying exactly one
// finding, at greetGoLine5 (TestDiagDumpDiff, a throwaway, never committed,
// used only to learn this tree's own real diff, confirmed it is inside
// pbTicketInReviewing's own diff): every test in this file that needs an
// in-diff location uses that same one, so this builds it directly rather
// than taking a location no caller varies.
func findingScript(lens, severity, text, fix string) string {
	return findingScriptAt(lens, severity, greetGoLine5, text, fix)
}

// findingScriptAt is findingScript with an explicit location: task 11's own
// tests that need two findings of two different lenses in one round use
// this with greetGoLine5 and greetGoLine2, so DedupFindings (6.3) keeps
// them as two separate rows rather than merging same-location reports from
// different lenses into one.
func findingScriptAt(lens, severity, location, text, fix string) string {
	return fmt.Sprintf(`<zing job="review" outcome="ok">
<finding lens="%s" severity="%s" location="%s">
<text>%s</text>
<fix>%s</fix>
</finding>
</zing>`, lens, severity, location, text, fix)
}

// reviewQuestionScript is one review lens's own "question" document: a
// plain agent question (kind "question", not the 6.4 review question),
// design section 7.2's universal shape.
func reviewQuestionScript(title, body string) string {
	return fmt.Sprintf(`<zing job="review" outcome="question">
<question key="Q1">
<title>%s</title>
<body>%s</body>
<option key="a">keep it as is</option>
<option key="b">change it</option>
<recommended>a</recommended>
</question>
</zing>`, title, body)
}

// reviewAnswerOption is the option key every reviewQuestionScript test
// answers with (design section 7.2, N1): a resumed lens's own session id
// and its resume charge are what these tests actually verify, not which
// option the owner chose.
const reviewAnswerOption = "a"

// reviewErrorScript is one review lens's own "error" document (design
// section 7.2's universal error outcome).
func reviewErrorScript(code, what, why, tried string) string {
	return fmt.Sprintf(`<zing job="review" outcome="error">
<error code="%s">
<what>%s</what>
<why>%s</why>
<tried>%s</tried>
</error>
</zing>`, code, what, why, tried)
}

// greetGoLine5 is a location inside pbTicketInReviewing's own real diff
// (greet.go is a brand-new file in that diff, lines 1-7): func Greet's own
// opening line, a safe in-diff finding location every finding-bearing test
// below uses.
const greetGoLine5 = "greet.go:5"

// greetGoLine2 is a second, distinct location inside the same diff (greet.go's
// own package line): the task 11 tests that need two findings at two
// locations in one round use this one and greetGoLine5, so DedupFindings
// (6.3) never merges them into one row.
const greetGoLine2 = "greet.go:2"

// twoFindingScript is one lens's own "ok" document carrying two findings at
// two distinct locations (design section 6.6's own batching tests: two
// findings of one lens, both discussed, carried in one resume).
func twoFindingScript(lens, sev1, loc1, text1, fix1, sev2, loc2, text2, fix2 string) string {
	return fmt.Sprintf(`<zing job="review" outcome="ok">
<finding lens="%s" severity="%s" location="%s">
<text>%s</text>
<fix>%s</fix>
</finding>
<finding lens="%s" severity="%s" location="%s">
<text>%s</text>
<fix>%s</fix>
</finding>
</zing>`, lens, sev1, loc1, text1, fix1, lens, sev2, loc2, text2, fix2)
}

// reviewTicketReady is pbTicketInReviewing plus the ticket and the run id
// watermark just before review's own first round: reviewing_test.go's own
// tests read that watermark to bound which run ids belong to the round
// they just drove.
func reviewTicketReady(t *testing.T) (s *store.Store, ticket store.Ticket, beforeRunID int64) {
	t.Helper()
	s, ticketID := pbTicketInReviewing(t)
	ticket = pbGetTicket(t, s, ticketID)
	before, err := s.MaxRunID(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	return s, ticket, before
}

// reviewRunsSince returns every review-lens run (runs.lens set) strictly
// after afterRunID, up to ticketID's own current MaxRunID, in id order: a
// test's own way to inspect exactly what one ROUND or CONTINUE call just
// reserved, since the store carries no dedicated "list runs" read.
func reviewRunsSince(t *testing.T, s *store.Store, ticketID, afterRunID int64) []store.Run {
	t.Helper()
	newest, err := s.MaxRunID(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	var out []store.Run
	for id := afterRunID + 1; id <= newest; id++ {
		r, runErr := s.RunByID(t.Context(), id)
		if runErr != nil {
			continue
		}
		if r.Lens != nil {
			out = append(out, r)
		}
	}
	return out
}

// answerReviewQuestion answers questionID with reviewAnswerOption (design
// section 7.2, N1): the owner's answer resumes the session that asked.
func answerReviewQuestion(t *testing.T, s *store.Store, ticketID, questionID int64) {
	t.Helper()
	result, err := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: questionID, Option: reviewAnswerOption})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("AnswerQuestion: Accepted = false, Conflict = %q, want accepted", result.Conflict)
	}
}

// newestOpenQuestion returns the newest still-open question of ticketID: a
// review round with a single asking lens carries exactly one at a time, so
// tests that drive one lens through several asks reuse this rather than
// re-deriving the "answer whichever one is open now" idiom each time.
func newestOpenQuestion(t *testing.T, s *store.Store, ticketID int64) store.MessageRow {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) == 0 {
		t.Fatalf("QuestionsByState(open) for ticket %d: no open question", ticketID)
	}
	return open[len(open)-1]
}

// reviewMarker returns the newest "update" message of ticketID whose first
// line starts with prefix: reviewing_test.go's own small MarkersWithPrefix
// wrapper, since most assertions below only care about the newest one.
func reviewMarker(t *testing.T, s *store.Store, ticketID int64, prefix string) (store.MessageRow, bool) {
	t.Helper()
	markers, err := s.MarkersWithPrefix(t.Context(), ticketID, prefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix(%q): %v", prefix, err)
	}
	if len(markers) == 0 {
		return store.MessageRow{}, false
	}
	return markers[len(markers)-1], true
}

// findingArtifactsByRound returns every stored finding artifact of round,
// held or not, in Findings' own artifact-id order.
// findingArtifactsByRound returns round 1's own stored finding artifacts:
// every test in this file drives round 1 only (re-review, round 2+, is
// task 12's own work), so this reads that round directly rather than
// taking a round number no caller varies.
func findingArtifactsByRound(t *testing.T, s *store.Store, ticketID int64) []response.FindingArtifact {
	t.Helper()
	rows, err := s.Findings(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	var out []response.FindingArtifact
	for i := range rows {
		if rows[i].Finding.Round == 1 {
			out = append(out, rows[i].Finding)
		}
	}
	return out
}

// ---- TestRoundRunsSevenLensesInParallel ------------------------------------

func TestRoundRunsSevenLensesInParallel(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)
	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pbApply(t, s, ticket, commit)

	after := pbGetTicket(t, s, ticket.ID)
	if after.State != stateJudging {
		t.Fatalf("ticket state = %q, want %q", after.State, stateJudging)
	}

	runs := reviewRunsSince(t, s, ticket.ID, before)
	if len(runs) != 7 {
		t.Fatalf("review runs = %d, want 7", len(runs))
	}
	seenLens := make(map[string]bool, 7)
	seenSession := make(map[int64]bool, 7)
	for _, r := range runs {
		if r.Lens == nil {
			t.Fatalf("run %d: Lens = nil, want set", r.ID)
		}
		seenLens[*r.Lens] = true
		seenSession[r.SessionID] = true
		if r.Turn != 0 {
			t.Errorf("run %d: Turn = %d, want 0 (a first turn)", r.ID, r.Turn)
		}
		if r.Outcome == nil || *r.Outcome != "ok" {
			t.Errorf("run %d: Outcome = %v, want ok", r.ID, r.Outcome)
		}
	}
	for _, lens := range reviewLensNames {
		if !seenLens[lens] {
			t.Errorf("lens %s never ran", lens)
		}
	}
	if len(seenSession) != 7 {
		t.Errorf("distinct sessions = %d, want 7 (one per lens)", len(seenSession))
	}
}

// ---- TestRoundRespectsMaxLensesParallel ------------------------------------

// concurrencyTracker wraps another Runtime, recording the peak number of
// concurrent Run calls it ever saw (design section 6.2 step 7: the
// semaphore bound); a short sleep before delegating gives seven goroutines
// enough of a window to actually overlap.
type concurrencyTracker struct {
	inner runtime.Runtime
	sleep time.Duration

	mu      sync.Mutex
	current int
	peak    int
}

func (c *concurrencyTracker) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	c.mu.Lock()
	c.current++
	if c.current > c.peak {
		c.peak = c.current
	}
	c.mu.Unlock()

	select {
	case <-time.After(c.sleep):
	case <-ctx.Done():
	}

	res, err := c.inner.Run(ctx, req)

	c.mu.Lock()
	c.current--
	c.mu.Unlock()
	return res, err
}

// Not parallel: it measures real wall-clock concurrency (peak simultaneous
// lens runs, via concurrencyTracker's 20ms sleep) against the rest of the
// suite's own goroutines competing for GOMAXPROCS; under load from sibling
// parallel tests this flaked, observing a peak of 1 instead of 2 (seen under
// go test -race ./internal/job/...), not a bug in runLensesParallel itself.
func TestRoundRespectsMaxLensesParallel(t *testing.T) {
	s, ticket, before := reviewTicketReady(t)
	tracker := &concurrencyTracker{inner: runtime.NewFake(reviewScriptsFS(nil)), sleep: 20 * time.Millisecond}
	deps := pbClaim(t, s, tracker, ticket.ID)
	deps.LensesParallel = 2

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pbApply(t, s, ticket, commit)

	tracker.mu.Lock()
	peak := tracker.peak
	tracker.mu.Unlock()
	if peak > 2 {
		t.Errorf("peak concurrent lens runs = %d, want <= 2", peak)
	}
	if peak < 2 {
		t.Errorf("peak concurrent lens runs = %d, want == 2 (parallelism was never exercised)", peak)
	}

	runs := reviewRunsSince(t, s, ticket.ID, before)
	if len(runs) != 7 {
		t.Fatalf("review runs = %d, want 7", len(runs))
	}
}

// waitFor receives the next value from ch, bounded only by go test's
// -timeout: if t.Deadline() reports none (-timeout 0), it waits forever.
// Otherwise it fails with a message naming what it was waiting for once
// nine tenths of the time remaining before the deadline has passed, well
// before go test's own timeout panic.
func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()

	deadline, ok := t.Deadline()
	if !ok {
		return <-ch
	}

	timer := time.NewTimer(time.Until(deadline) * 9 / 10)
	defer timer.Stop()

	select {
	case v := <-ch:
		return v
	case <-timer.C:
		t.Fatalf("%s: still waiting near go test's -timeout", what)
		var zero T
		return zero
	}
}

// ---- TestRunLensesParallelRejectsOutOfRangeConfig ---------------------------

// TestRunLensesParallelRejectsOutOfRangeConfig proves the handoff fix for
// Package 9 Task 10's own bug: runLensesParallel used to size its semaphore
// channel at make(chan struct{}, d.LensesParallel) with no floor, so
// LensesParallel == 0 -- the zero value a Deps literal that forgot to set
// it carries -- made a capacity-0 channel every goroutine's own "case sem
// <- struct{}{}" blocks on forever, since nothing ever reads from a channel
// no send has completed on: a deadlock, not an error. It must instead
// refuse any value outside [1,7] with ErrConfig, before build is ever
// called and before any lens ever reserves a run.
func TestRunLensesParallelRejectsOutOfRangeConfig(t *testing.T) {
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)
	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), ticket.ID)

	for _, n := range []int{-1, 0, 8, 100} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			deps := deps // each parallel subtest gets its own copy to set
			deps.LensesParallel = n
			called := false
			// build's own SessionUpsert and RunRequest returns are always the
			// zero value: this case proves build is never even called, so
			// what it would have returned never matters (unparam).
			build := func(response.Lens) (store.SessionUpsert, runtime.RunRequest, func(runResult) *store.SessionUpsert) { //nolint:unparam // see above
				called = true
				return store.SessionUpsert{}, runtime.RunRequest{}, freshSessionRecord
			}

			done := make(chan struct{})
			var attempts []lensAttempt
			var err error
			go func() {
				attempts, err = runLensesParallel(t.Context(), deps, ticket, reviewLenses(deps), nil, build)
				close(done)
			}()
			waitFor(t, done, "runLensesParallel to return (a deadlock hangs here)")

			if !errors.Is(err, ErrConfig) {
				t.Fatalf("err = %v, want errors.Is(err, ErrConfig)", err)
			}
			if called {
				t.Error("build was called; want no lens ever started")
			}
			if attempts != nil {
				t.Errorf("attempts = %v, want nil", attempts)
			}
		})
	}

	runs := reviewRunsSince(t, s, ticket.ID, before)
	if len(runs) != 0 {
		t.Errorf("reserved runs = %d, want 0 (the range check runs before any reserve)", len(runs))
	}
}

// ---- TestRoundCleanMovesToJudging -------------------------------------------

func TestRoundCleanMovesToJudging(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != stateJudging || commit.Reason != reasonReviewClean {
		t.Fatalf("commit = {Next: %q, Reason: %q}, want {%q, %q}", commit.Next, commit.Reason, stateJudging, reasonReviewClean)
	}
	pbApply(t, s, ticket, commit)

	if marker, ok := reviewMarker(t, s, ticket.ID, "review round 1 done"); !ok {
		t.Error(`no "review round 1 done" marker`)
	} else if !strings.Contains(marker.Body, "kept 0 dropped 0 merged 0") {
		t.Errorf("done marker body = %q, want it to report kept 0 dropped 0 merged 0", marker.Body)
	}
}

// ---- TestRoundBelowFloorRequestsFix -----------------------------------------

// TestRoundBelowFloorRequestsFix proves design section 6.2's own "one or
// more survivors" row (6.8, FIXREQ): a single at-or-below-floor finding
// (pbFloor is minor) opens a fix request in the same commit as the round's
// own "done" marker, and the ticket stays in reviewing.
func TestRoundBelowFloorRequestsFix(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey("correctness", 1): findingScript("correctness", "minor", "the return could be a constant", "extract a const"),
	})
	deps := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != "" {
		t.Fatalf("commit.Next = %q, want empty (a fix request stays in reviewing)", commit.Next)
	}
	pbApply(t, s, ticket, commit)

	if _, ok := reviewMarker(t, s, ticket.ID, "review round 1 done"); !ok {
		t.Error(`no "review round 1 done" marker`)
	}
	fixReq, ok := reviewMarker(t, s, ticket.ID, fixRequestedFindingsPrefix)
	if !ok {
		t.Fatal(`no "fix requested findings after run" marker`)
	}
	if !strings.Contains(fixReq.Body, greetGoLine5) {
		t.Errorf("fix request body = %q, want it to quote the finding's own location %s", fixReq.Body, greetGoLine5)
	}

	findings := findingArtifactsByRound(t, s, ticket.ID)
	if len(findings) != 1 {
		t.Fatalf("round 1 findings = %d, want 1", len(findings))
	}
	if findings[0].Decision == nil || *findings[0].Decision != response.FindingAccept {
		t.Errorf("finding decision = %v, want accept", findings[0].Decision)
	}
	if findings[0].ID != findingID1 {
		t.Errorf("finding id = %q, want r1f1", findings[0].ID)
	}
}

// ---- TestRoundAboveFloorAsks -------------------------------------------------

// TestRoundAboveFloorAsks proves design section 6.4: a finding above the
// floor (pbFloor is minor; major is above it) posts the review question,
// waiting on "review", its payload carrying the finding's own text.
func TestRoundAboveFloorAsks(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	const findingText = "this branch never returns an error"
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey("security", 1): findingScript("security", "major", findingText, "check the error"),
	})
	deps := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Waiting == nil || *commit.Waiting != waitingFlagReview {
		t.Fatalf("commit.Waiting = %v, want %q", commit.Waiting, waitingFlagReview)
	}
	pbApply(t, s, ticket, commit)

	open := newestOpenQuestion(t, s, ticket.ID)
	var payload response.QuestionPayload
	if err = json.Unmarshal(open.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	if payload.Kind != response.QuestionKindReview {
		t.Errorf("payload.Kind = %q, want %q", payload.Kind, response.QuestionKindReview)
	}
	if len(payload.Options) != 0 {
		t.Errorf("payload.Options = %v, want none", payload.Options)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("payload.Items = %d, want 1", len(payload.Items))
	}
	if payload.Items[0].Ref != findingID1 {
		t.Errorf("item ref = %q, want r1f1", payload.Items[0].Ref)
	}
	if !strings.Contains(payload.Items[0].Text, findingText) {
		t.Errorf("item text = %q, want it to hold %q", payload.Items[0].Text, findingText)
	}

	findings := findingArtifactsByRound(t, s, ticket.ID)
	if len(findings) != 1 || findings[0].Decision != nil {
		t.Errorf("findings = %+v, want one undecided finding", findings)
	}
}

// ---- TestRoundBudgetEscalates ------------------------------------------------

// TestRoundBudgetEscalates proves design section 6.2's own ErrBudget row: an
// already-exhausted budget fails every lens's own runJob call before any of
// them ever reserves a run.
func TestRoundBudgetEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)
	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), ticket.ID)
	deps.Budget = 0

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want set")
	}
	if commit.Escalation.Payload.Code != string(response.EscalationCodeWallClock) {
		t.Errorf("escalation code = %q, want %q", commit.Escalation.Payload.Code, response.EscalationCodeWallClock)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginCapBudget) {
		t.Errorf("escalation origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginCapBudget)
	}
	pbApply(t, s, ticket, commit)

	runs := reviewRunsSince(t, s, ticket.ID, before)
	if len(runs) != 0 {
		t.Errorf("review runs = %d, want 0 (the budget check runs before Reserve)", len(runs))
	}
}

// ---- TestRoundClaimLostAfterOneReservation ----------------------------------

// TestRoundClaimLostAfterOneReservation proves design section 6.2's own
// ErrClaimLost row: the first lens to reserve succeeds; every later Reserve
// call returns store.ErrClaimLost (the claim already moved on), and the
// round returns that wrapped error with no commit at all.
func TestRoundClaimLostAfterOneReservation(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)
	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), ticket.ID)
	// Serialized on purpose: with every lens racing the semaphore at once
	// (the default LensesParallel 7), a synthetic ErrClaimLost from an
	// already-serviced call can cancel roundCtx while the one real Reserve
	// call below is still its own in-flight transaction, losing the real
	// reservation this test means to prove happened. LensesParallel 1 makes
	// "first call reserves for real, every later one is synthetic" exact:
	// no second call can even start until the first's whole runJob call,
	// Reserve included, has returned.
	deps.LensesParallel = 1

	realReserve := deps.Reserve
	var calls atomic.Int32
	deps.Reserve = func(ctx context.Context, ticketID int64, su store.SessionUpsert, seed store.RunSeed) (store.Reserved, error) {
		if calls.Add(1) == 1 {
			return realReserve(ctx, ticketID, su, seed)
		}
		return store.Reserved{}, store.ErrClaimLost
	}

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if !errors.Is(err, store.ErrClaimLost) {
		t.Fatalf("err = %v, want errors.Is(err, store.ErrClaimLost)", err)
	}
	if commit.TicketID != 0 || commit.Next != "" || commit.Escalation != nil || len(commit.Messages) != 0 {
		t.Errorf("commit = %+v, want the zero value (no commit)", commit)
	}

	runs := reviewRunsSince(t, s, ticket.ID, before)
	if len(runs) != 1 {
		t.Fatalf("reserved runs = %d, want exactly 1", len(runs))
	}
	if runs[0].Outcome != nil {
		t.Errorf("run %d outcome = %v, want nil (the placeholder Reserve wrote, untouched: no commit ever terminalized it)", runs[0].ID, *runs[0].Outcome)
	}
}

// ---- TestRoundVoidWhenHeadMoves ----------------------------------------------

// movingHeadRuntime commits one more change to the worktree the first time
// any lens is asked to run, so the round's own post-run HeadSHA no longer
// matches the sha it read before launching the lenses (design section 6.2
// step 8).
type movingHeadRuntime struct {
	inner runtime.Runtime
	move  func()

	mu    sync.Mutex
	moved bool
}

func (m *movingHeadRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	m.mu.Lock()
	if !m.moved {
		m.moved = true
		m.move()
	}
	m.mu.Unlock()
	return m.inner.Run(ctx, req)
}

func TestRoundVoidWhenHeadMoves(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), ticket.ID)
	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	rt := &movingHeadRuntime{
		inner: runtime.NewFake(reviewScriptsFS(nil)),
		move: func() {
			if addErr := gitfixture.AddFile(t.Context(), wt.Dir(), "late.txt", []byte("late\n")); addErr != nil {
				t.Fatalf("add late commit: %v", addErr)
			}
		},
	}
	set, err := runtime.NewSet(map[string]runtime.Runtime{pbRuntimeClaude: rt, pbRuntimeCodex: rt, "fake": rt})
	if err != nil {
		t.Fatalf("runtime.NewSet: %v", err)
	}
	deps.Runtimes = set

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want empty (a void round stays in reviewing)", commit.Next)
	}
	pbApply(t, s, ticket, commit)

	marker, ok := reviewMarker(t, s, ticket.ID, "review round 1 void")
	if !ok {
		t.Fatal(`no "review round 1 void" marker`)
	}
	if !strings.Contains(marker.Body, "head moved from") {
		t.Errorf("void marker body = %q, want it to report the moved head", marker.Body)
	}
}

// ---- TestRoundRefusesDirtyTree -----------------------------------------------

// TestRoundRefusesDirtyTree proves design section 6.2 step 4: an uncommitted
// change in the worktree escalates before any lens ever runs.
func TestRoundRefusesDirtyTree(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)
	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), ticket.ID)
	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	dirtyPath := "dirty.txt"
	if writeErr := os.WriteFile(filepath.Join(wt.Dir(), dirtyPath), []byte("uncommitted\n"), 0o600); writeErr != nil {
		t.Fatalf("write dirty file: %v", writeErr)
	}

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want set")
	}
	if commit.Escalation.Payload.What != treeDirtyBeforeReviewWhat {
		t.Errorf("escalation What = %q, want %q", commit.Escalation.Payload.What, treeDirtyBeforeReviewWhat)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginReview) {
		t.Errorf("escalation origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginReview)
	}
	if !strings.Contains(commit.Escalation.Payload.Tried, dirtyPath) {
		t.Errorf("escalation Tried = %q, want it to quote %s", commit.Escalation.Payload.Tried, dirtyPath)
	}

	runs := reviewRunsSince(t, s, ticket.ID, before)
	if len(runs) != 0 {
		t.Errorf("review runs = %d, want 0 (no lens ever ran)", len(runs))
	}
}

// ---- TestRoundRefusesUnrecordedCommit ----------------------------------------

// TestRoundRefusesUnrecordedCommit proves design section 6.2 step 3: a
// commit on the ticket branch that Zing never recorded escalates before any
// lens ever runs (with no fix open, an unrecorded commit is not Zing's).
func TestRoundRefusesUnrecordedCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)
	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), ticket.ID)
	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if addErr := gitfixture.AddFile(t.Context(), wt.Dir(), "foreign.txt", []byte("not zing's\n")); addErr != nil {
		t.Fatalf("add foreign commit: %v", addErr)
	}

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want set")
	}
	if commit.Escalation.Payload.What != branchUnrecordedWhat {
		t.Errorf("escalation What = %q, want %q", commit.Escalation.Payload.What, branchUnrecordedWhat)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginReview) {
		t.Errorf("escalation origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginReview)
	}

	runs := reviewRunsSince(t, s, ticket.ID, before)
	if len(runs) != 0 {
		t.Errorf("review runs = %d, want 0 (no lens ever ran)", len(runs))
	}
}

// ---- TestHeldRowsNeverRouted -------------------------------------------------

// TestHeldRowsNeverRouted proves design section 6.2a: "Held rows never
// reach routing, a question, or a fix list: every reader of round findings
// skips Held rows, except CONTINUE." acceptedRoundFindings is enterFromDone's
// own reader.
func TestHeldRowsNeverRouted(t *testing.T) {
	t.Parallel()
	accept := response.FindingAccept
	rows := []store.FindingRow{
		{Finding: response.FindingArtifact{ID: "r1h1", Round: 1, Held: true, Decision: &accept}},
		{Finding: response.FindingArtifact{ID: findingID1, Round: 1, Held: false, Decision: &accept}},
		{Finding: response.FindingArtifact{ID: "r1h2", Round: 1, Held: true}},
	}
	got := acceptedRoundFindings(rows, 1)
	if len(got) != 1 || got[0].ID != findingID1 {
		t.Fatalf("acceptedRoundFindings = %+v, want exactly [r1f1] (held rows excluded)", got)
	}
}

// ---- TestRoundLensErrorEscalates ---------------------------------------------

// TestRoundLensErrorEscalates proves design section 6.2's own "any lens
// returned error" row: the agent's own code, what, why, and tried carry
// straight through to the escalation, tagged origin review, and the round
// writes its own "review round 1 failed" marker in the same commit.
func TestRoundLensErrorEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey("quality", 1): reviewErrorScript("other", "could not review the diff", "the tool crashed", "re-read the file"),
	})
	deps := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want set")
	}
	if commit.Escalation.Payload.Code != "other" {
		t.Errorf("escalation code = %q, want other", commit.Escalation.Payload.Code)
	}
	if commit.Escalation.Payload.What != "could not review the diff" {
		t.Errorf("escalation What = %q, want the agent's own text", commit.Escalation.Payload.What)
	}
	if commit.Escalation.Payload.Why != "the tool crashed" {
		t.Errorf("escalation Why = %q, want the agent's own text", commit.Escalation.Payload.Why)
	}
	if commit.Escalation.Payload.Tried != "re-read the file" {
		t.Errorf("escalation Tried = %q, want the agent's own text", commit.Escalation.Payload.Tried)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginReview) {
		t.Errorf("escalation origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginReview)
	}
	if commit.Waiting == nil || *commit.Waiting != waitingFlagQuestions {
		t.Errorf("commit.Waiting = %v, want %q", commit.Waiting, waitingFlagQuestions)
	}

	found := false
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, "review round 1 failed") {
			found = true
			if !strings.Contains(m.Body, "lens quality returned an error") {
				t.Errorf("failed marker body = %q, want it to name lens quality", m.Body)
			}
		}
	}
	if !found {
		t.Error(`no "review round 1 failed" message in the commit`)
	}

	pbApply(t, s, ticket, commit)
	after := pbGetTicket(t, s, ticket.ID)
	if after.State != stateReviewing {
		t.Errorf("ticket state = %q, want reviewing (unchanged)", after.State)
	}
}

// ---- TestRoundLensFailureCancelsOthers ---------------------------------------

// TestRoundLensFailureCancelsOthers proves design section 6.2 step 7: the
// first lens result that is not a parsed ok or question document cancels
// every lens still waiting on the semaphore, so fewer than all seven ever
// reserve a run.
// firstCallFailsRuntime fails its very first call (whichever lens happens
// to reach it first) and delegates every later call to inner: with
// LensesParallel 1, the very first lens to reach the runtime is the only
// one guaranteed to run before roundCtx is ever cancelled, so scripting by
// call order rather than by a named lens keeps this test free of any
// assumption about which lens wins that initial race. It returns an
// ExecError, not an InvalidOutputError (issue #32): an invalid document no
// longer cancels the round on its own turn -- it is retried in place first
// -- so this test's own cancellation proof needs a failure that still does.
type firstCallFailsRuntime struct {
	inner runtime.Runtime
	calls atomic.Int32
}

func (r *firstCallFailsRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	if r.calls.Add(1) == 1 {
		return runtime.RunResult{ExitCode: 1, AgentTime: time.Second}, &runtime.ExecError{ExitCode: 1}
	}
	return r.inner.Run(ctx, req)
}

// TestRoundLensFailureCancelsOthers proves design section 6.2 step 7: the
// first lens result that is not a parsed ok or question document cancels
// roundCtx, so a goroutine still waiting on the size-1 semaphore when that
// happens reserves nothing. LensesParallel 1 forces every lens through one
// at a time, so at most one lens can ever be mid-flight when the first
// call's own failure fires the cancellation.
func TestRoundLensFailureCancelsOthers(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)
	rt := &firstCallFailsRuntime{inner: runtime.NewFake(reviewScriptsFS(nil))}
	deps := pbClaim(t, s, rt, ticket.ID)
	deps.LensesParallel = 1

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, "review round 1 failed") {
			found = true
		}
	}
	if !found {
		t.Fatal(`no "review round 1 failed" message in the commit`)
	}
	pbApply(t, s, ticket, commit)

	runs := reviewRunsSince(t, s, ticket.ID, before)
	if len(runs) >= len(reviewLensNames) {
		t.Errorf("reserved runs = %d, want fewer than %d (cancellation should have skipped at least one lens)", len(runs), len(reviewLensNames))
	}
}

// ---- TestRoundSecondFailureEscalates -----------------------------------------

// labelResultRuntime returns results[req.Label]() when set, else delegates
// to inner: reviewing_test.go's own way to script a hand-built
// (RunResult, error) pair (an *InvalidOutputError, an exec failure) that no
// fixture XML document can express, keyed by lens rather than call order so
// it stays safe for ROUND's own concurrent calls.
type labelResultRuntime struct {
	inner   runtime.Runtime
	results map[string]func() (runtime.RunResult, error)
}

func (r *labelResultRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	if fn, ok := r.results[req.Label]; ok {
		return fn()
	}
	return r.inner.Run(ctx, req)
}

// TestRoundSecondFailureEscalates proves design section 6.2 row 7's own
// two-in-a-row rule: the same lens failing the same way twice in a row
// (the same round number, since a "failed" marker never advances n)
// escalates on the second attempt, where the first attempt only marked it.
func TestRoundSecondFailureEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	// An ExecError, not an InvalidOutputError (issue #32): an invalid
	// document is now retried in place on its first failure, so it never
	// reaches a second round at all unless its own retry is also invalid
	// (TestRoundInvalidLensTwiceEscalates covers that path instead).
	execFn := func() (runtime.RunResult, error) {
		return runtime.RunResult{ExitCode: 1, AgentTime: time.Second}, &runtime.ExecError{ExitCode: 1}
	}
	rt := &labelResultRuntime{
		inner:   runtime.NewFake(reviewScriptsFS(nil)),
		results: map[string]func() (runtime.RunResult, error){simplificationRunLabel: execFn},
	}
	deps := pbClaim(t, s, rt, ticket.ID)

	commit1, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (1st): %v", err)
	}
	if commit1.Escalation != nil {
		t.Fatalf("commit1.Escalation = %+v, want nil (the first failure only marks)", commit1.Escalation)
	}
	pbApply(t, s, ticket, commit1)
	if _, ok := reviewMarker(t, s, ticket.ID, "review round 1 failed"); !ok {
		t.Fatal(`no "review round 1 failed" marker after the first failure`)
	}

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (2nd): %v", err)
	}
	if commit2.Escalation == nil {
		t.Fatal("commit2.Escalation = nil, want set (two failures in a row)")
	}
	if commit2.Escalation.Payload.Code != string(response.EscalationCodeRuntimeExecFailed) {
		t.Errorf("escalation code = %q, want %q", commit2.Escalation.Payload.Code, response.EscalationCodeRuntimeExecFailed)
	}
	if commit2.Escalation.Payload.What != lensFailedTwiceWhat {
		t.Errorf("escalation What = %q, want %q", commit2.Escalation.Payload.What, lensFailedTwiceWhat)
	}
	if commit2.Escalation.Payload.Origin != string(response.EscalationOriginReview) {
		t.Errorf("escalation origin = %q, want %q", commit2.Escalation.Payload.Origin, response.EscalationOriginReview)
	}
}

// TestRoundRetryResetsFailedCount proves issue #32's own Retry reset: the
// owner's Retry on the two-in-a-row escalation starts a fresh count, so the
// very next failure only marks instead of escalating again at once
// (enterRound's own "retry requested" check, newer than the newest "review
// round <n>" marker).
func TestRoundRetryResetsFailedCount(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	execFn := func() (runtime.RunResult, error) {
		return runtime.RunResult{ExitCode: 1, AgentTime: time.Second}, &runtime.ExecError{ExitCode: 1}
	}
	rt := &labelResultRuntime{
		inner:   runtime.NewFake(reviewScriptsFS(nil)),
		results: map[string]func() (runtime.RunResult, error){simplificationRunLabel: execFn},
	}

	// 1st failure: only marks.
	ticket1 := pbGetTicket(t, s, ticket.ID)
	deps1 := pbClaim(t, s, rt, ticket.ID)
	commit1, err := (reviewingHandler{}).Run(t.Context(), ticket1, deps1)
	if err != nil {
		t.Fatalf("Run (1st failure): %v", err)
	}
	if commit1.Escalation != nil {
		t.Fatalf("commit1.Escalation = %+v, want nil", commit1.Escalation)
	}
	pbApply(t, s, ticket, commit1)

	// 2nd failure in a row: escalates.
	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (2nd failure): %v", err)
	}
	if commit2.Escalation == nil {
		t.Fatal("commit2.Escalation = nil, want set (two failures in a row)")
	}
	pbApply(t, s, ticket, commit2)

	// The owner answers Retry.
	q := newestOpenQuestion(t, s, ticket.ID)
	pbAnswerEscalation(t, s, ticket.ID, q.ID, escalationChoiceRetry)

	// The prelude resolves the escalation: a plain "retry requested" marker.
	ticket3 := pbGetTicket(t, s, ticket.ID)
	deps3 := pbClaim(t, s, rt, ticket.ID)
	commit3, err := (reviewingHandler{}).Run(t.Context(), ticket3, deps3)
	if err != nil {
		t.Fatalf("Run (retry): %v", err)
	}
	if len(commit3.Messages) != 1 || commit3.Messages[0].Body != markerRetryRequested {
		t.Fatalf("commit3.Messages = %+v, want one %q marker", commit3.Messages, markerRetryRequested)
	}
	pbApply(t, s, ticket, commit3)

	// The first failure after the Retry only marks, with no escalation.
	ticket4 := pbGetTicket(t, s, ticket.ID)
	deps4 := pbClaim(t, s, rt, ticket.ID)
	commit4, err := (reviewingHandler{}).Run(t.Context(), ticket4, deps4)
	if err != nil {
		t.Fatalf("Run (1st failure after retry): %v", err)
	}
	if commit4.Escalation != nil {
		t.Fatalf("commit4.Escalation = %+v, want nil (the Retry reset the count)", commit4.Escalation)
	}
	foundFailed := false
	for _, m := range commit4.Messages {
		if strings.HasPrefix(m.Body, "review round 1 failed") {
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Fatalf(`commit4.Messages = %+v, want a "review round 1 failed" message`, commit4.Messages)
	}
	pbApply(t, s, ticket, commit4)

	// The second failure in a row past the Retry escalates again.
	ticket5 := pbGetTicket(t, s, ticket.ID)
	deps5 := pbClaim(t, s, rt, ticket.ID)
	commit5, err := (reviewingHandler{}).Run(t.Context(), ticket5, deps5)
	if err != nil {
		t.Fatalf("Run (2nd failure after retry): %v", err)
	}
	if commit5.Escalation == nil {
		t.Fatal("commit5.Escalation = nil, want set (two failures in a row, past the retry)")
	}
	if commit5.Escalation.Payload.What != lensFailedTwiceWhat {
		t.Errorf("escalation What = %q, want %q", commit5.Escalation.Payload.What, lensFailedTwiceWhat)
	}
}

// ---- TestRoundInvalidLensRetriedKeepsFindings ------------------------------

// TestRoundInvalidLensRetriedKeepsFindings is task 1's own named test
// (issue #32): the tests lens's first turn returns an invalid document
// carrying a session id; today that cancels every other lens and fails the
// whole round. Once the lens is retried in place instead, the round keeps
// every lens's own work and escalates nothing.
func TestRoundInvalidLensRetriedKeepsFindings(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)

	// Every lens but fidelity (whose own finding needs a PlanRef,
	// FilterFindings) and tests reports one clean nit at the same location,
	// so DedupFindings merges them into one artifact; tests's own retried
	// finding, below, uses a distinct location so it stays a separate
	// artifact and its own RunID can be checked.
	overrides := map[string]string{}
	for _, lens := range reviewLensNames {
		if lens == lensFidelity || lens == lensTests {
			continue
		}
		overrides[reviewScriptKey(lens, 1)] = findingScript(lens, "nit", "a nit from "+lens, "polish it")
	}
	fake := runtime.NewFake(reviewScriptsFS(overrides))

	var retryReq runtime.RunRequest
	rt := &labelStepsRuntime{
		inner: fake,
		steps: map[string][]func(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error){
			testsRunLabel: {
				func(_ context.Context, _ runtime.RunRequest) (runtime.RunResult, error) {
					return runtime.RunResult{SessionID: testsSessionID, ExitCode: 1, AgentTime: time.Second},
						&runtime.InvalidOutputError{Reason: testReasonFailedValidation, Detail: testsInvalidDetail}
				},
				func(_ context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
					retryReq = req
					// A distinct location from the other lenses' shared
					// greetGoLine5 nit, so DedupFindings keeps the tests
					// lens's own finding as its own artifact rather than
					// merging it into the other lenses' row.
					doc, parseErr := response.Parse([]byte(findingScriptAt(lensTests, "nit", greetGoLine2, "a nit from tests", "polish it")))
					if parseErr != nil {
						t.Fatalf("parse tests retry script: %v", parseErr)
					}
					return runtime.RunResult{Response: doc.Response, SessionID: testsSessionID, ExitCode: 0, AgentTime: time.Second}, nil
				},
			},
		},
	}
	deps := pbClaim(t, s, rt, ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("commit.Escalation = %+v, want nil", commit.Escalation)
	}
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, "review round 1 failed") {
			t.Fatalf("commit carries a failed marker, want none: %q", m.Body)
		}
	}

	pbApply(t, s, ticket, commit)

	doneMarker, ok := reviewMarker(t, s, ticket.ID, "review round 1 done")
	if !ok {
		t.Fatal(`no "review round 1 done" marker`)
	}
	firstLine, _, _ := strings.Cut(doneMarker.Body, "\n")
	match := reviewRoundDoneLine.FindStringSubmatch(firstLine)
	if match == nil {
		t.Fatalf("done marker first line = %q, did not match the done pattern", firstLine)
	}
	if gotLenses := strings.Split(match[3], ","); !slices.Equal(gotLenses, reviewLensNames) {
		t.Errorf("done marker lenses = %v, want %v (all seven)", gotLenses, reviewLensNames)
	}

	runs := reviewRunsSince(t, s, ticket.ID, before)
	if len(runs) != 8 {
		t.Fatalf("reserved runs = %d, want 8 (seven lenses plus the tests retry)", len(runs))
	}
	var testsRuns []store.Run
	errorCount := 0
	for _, r := range runs {
		isError := r.Outcome != nil && *r.Outcome == string(response.OutcomeError)
		if isError {
			errorCount++
		}
		if r.Lens != nil && *r.Lens == lensTests {
			testsRuns = append(testsRuns, r)
		}
	}
	if errorCount != 1 {
		t.Errorf("runs with outcome error = %d, want 1 (only the discarded first tests turn)", errorCount)
	}
	if len(testsRuns) != 2 {
		t.Fatalf("tests runs = %d, want 2", len(testsRuns))
	}
	// reviewRunsSince returns runs in ascending id order, so testsRuns[0] is
	// the discarded first turn and testsRuns[1] is the retry.
	firstTestsRunID, retryTestsRunID := testsRuns[0].ID, testsRuns[1].ID
	isError := func(r store.Run) bool { return r.Outcome != nil && *r.Outcome == string(response.OutcomeError) }
	if !isError(testsRuns[0]) {
		t.Errorf("tests run %d outcome = %v, want error (the discarded first turn)", testsRuns[0].ID, testsRuns[0].Outcome)
	}
	if isError(testsRuns[1]) {
		t.Errorf("tests run %d outcome = error, want ok (the retry)", testsRuns[1].ID)
	}
	if testsRuns[0].SessionID != testsRuns[1].SessionID {
		t.Errorf("tests runs session ids = %d, %d, want equal (the retry resumes the first turn's own session)",
			testsRuns[0].SessionID, testsRuns[1].SessionID)
	}

	invalidMarker, ok := reviewMarker(t, s, ticket.ID, fmt.Sprintf("response invalid run %d", firstTestsRunID))
	if !ok {
		t.Fatalf("no %q marker", fmt.Sprintf("response invalid run %d", firstTestsRunID))
	}
	if !strings.Contains(invalidMarker.Body, testReasonFailedValidation) {
		t.Errorf("invalid marker body = %q, want it to hold the validator's reason", invalidMarker.Body)
	}

	if retryReq.SessionID != testsSessionID {
		t.Errorf("retry request SessionID = %q, want %q", retryReq.SessionID, testsSessionID)
	}
	if retryReq.Label != testsRunLabel {
		t.Errorf("retry request Label = %q, want %q", retryReq.Label, testsRunLabel)
	}
	if !strings.Contains(retryReq.Prompt, reviewInvalidRetryHeader) {
		t.Errorf("retry prompt = %q, want it to contain %q", retryReq.Prompt, reviewInvalidRetryHeader)
	}
	openFence := strings.Index(retryReq.Prompt, "<<<UNTRUSTED")
	detailIdx := strings.Index(retryReq.Prompt, testsInvalidDetail)
	closeFence := strings.LastIndex(retryReq.Prompt, "<<<END")
	if openFence == -1 || detailIdx == -1 || closeFence == -1 || openFence >= detailIdx || detailIdx >= closeFence {
		t.Errorf("retry prompt = %q, want the validator's detail fenced between <<<UNTRUSTED and <<<END markers", retryReq.Prompt)
	}

	sess, _, sessErr := s.SessionByID(t.Context(), testsRuns[0].SessionID, deps.Machine.Jobs[jobReviewName].MaxResumes)
	if sessErr != nil {
		t.Fatalf("SessionByID: %v", sessErr)
	}
	if sess.Resumes != 0 {
		t.Errorf("tests session Resumes = %d, want 0 (the retry does not charge a resume)", sess.Resumes)
	}

	findings := findingArtifactsByRound(t, s, ticket.ID)
	gotLensSet := map[response.Lens]bool{}
	for _, f := range findings {
		for _, l := range f.Lenses {
			gotLensSet[l] = true
		}
	}
	for _, lens := range reviewLensNames {
		if lens == lensFidelity {
			continue
		}
		if !gotLensSet[response.Lens(lens)] {
			t.Errorf("findings lenses = %v, missing %q", gotLensSet, lens)
		}
	}

	rows, err := s.Findings(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	foundTestsRow := false
	for _, row := range rows {
		if row.Finding.Round != 1 {
			continue
		}
		for _, l := range row.Finding.Lenses {
			if l == lensTests {
				foundTestsRow = true
				if row.RunID == nil {
					t.Errorf("tests finding RunID = nil, want %d (the retry run, not the discarded first turn)", retryTestsRunID)
				} else if *row.RunID != retryTestsRunID {
					t.Errorf("tests finding RunID = %d, want %d (the retry run, not the discarded first turn)", *row.RunID, retryTestsRunID)
				}
			}
		}
	}
	if !foundTestsRow {
		t.Error("no stored finding names the tests lens")
	}
}

// ---- TestRoundInvalidLensTwiceEscalates -------------------------------------

// TestRoundInvalidLensTwiceEscalates proves the new "invalid twice" row
// (design section 6.2 step 7, issue #32): a lens whose first turn and whose
// same-tick retry are both invalid escalates response_invalid in that same
// round, naming the lens and pointing at the retry's own marker, with no
// second round ever needed.
func TestRoundInvalidLensTwiceEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)
	invalidFn := func() (runtime.RunResult, error) {
		return runtime.RunResult{SessionID: testsSessionID, ExitCode: 1, AgentTime: time.Second},
			&runtime.InvalidOutputError{Reason: testReasonFailedValidation, Detail: testsInvalidDetail}
	}
	rt := &labelResultRuntime{
		inner:   runtime.NewFake(reviewScriptsFS(nil)),
		results: map[string]func() (runtime.RunResult, error){testsRunLabel: invalidFn},
	}
	deps := pbClaim(t, s, rt, ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want set (invalid twice in a row)")
	}
	if commit.Escalation.Payload.Code != string(response.EscalationCodeResponseInvalid) {
		t.Errorf("escalation code = %q, want %q", commit.Escalation.Payload.Code, response.EscalationCodeResponseInvalid)
	}
	wantWhat := fmt.Sprintf(lensInvalidTwiceWhat, lensTests)
	if commit.Escalation.Payload.What != wantWhat {
		t.Errorf("escalation What = %q, want %q", commit.Escalation.Payload.What, wantWhat)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginReview) {
		t.Errorf("escalation origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginReview)
	}

	found := false
	for _, m := range commit.Messages {
		if m.Body == "review round 1 failed\nlens tests: invalid output" {
			found = true
		}
	}
	if !found {
		t.Error(`no "review round 1 failed\nlens tests: invalid output" message in the commit`)
	}

	var invalidMarkers []store.Message
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, "response invalid run ") {
			invalidMarkers = append(invalidMarkers, m)
		}
	}
	if len(invalidMarkers) != 2 {
		t.Fatalf("invalid markers = %d, want 2", len(invalidMarkers))
	}
	if !strings.Contains(invalidMarkers[1].Body, testsInvalidDetail) {
		t.Errorf("second invalid marker = %q, want it to hold the validator's detail", invalidMarkers[1].Body)
	}

	pbApply(t, s, ticket, commit)

	runs := reviewRunsSince(t, s, ticket.ID, before)
	var testsRuns []store.Run
	for _, r := range runs {
		if r.Lens != nil && *r.Lens == lensTests {
			testsRuns = append(testsRuns, r)
		}
	}
	if len(testsRuns) != 2 {
		t.Fatalf("tests runs = %d, want 2", len(testsRuns))
	}
	secondRunID := testsRuns[1].ID
	if commit.Escalation.RunID == nil || *commit.Escalation.RunID != secondRunID {
		t.Errorf("escalation RunID = %v, want %d (the retry's own run)", commit.Escalation.RunID, secondRunID)
	}
	wantWhy := fmt.Sprintf("zing document failed validation; validator errors in response invalid run %d", secondRunID)
	if commit.Escalation.Payload.Why != wantWhy {
		t.Errorf("escalation Why = %q, want %q", commit.Escalation.Payload.Why, wantWhy)
	}
	if strings.Contains(commit.Escalation.Payload.Why, "closing tag mismatch") {
		t.Errorf("escalation Why = %q, want it to hold no validator detail", commit.Escalation.Payload.Why)
	}
}

// ---- TestRoundInvalidThenExecFailure ----------------------------------------

// TestRoundInvalidThenExecFailure proves the invalid-twice row (above) only
// ever matches a lens whose retry is itself invalid: a lens whose retry
// instead fails to execute takes the ordinary exec-failure row, with no
// escalation on a first failed round, and carries exactly the one invalid
// marker its first turn wrote.
func TestRoundInvalidThenExecFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)
	rt := &labelStepsRuntime{
		inner: runtime.NewFake(reviewScriptsFS(nil)),
		steps: map[string][]func(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error){
			testsRunLabel: {
				func(_ context.Context, _ runtime.RunRequest) (runtime.RunResult, error) {
					return runtime.RunResult{SessionID: testsSessionID, ExitCode: 1, AgentTime: time.Second},
						&runtime.InvalidOutputError{Reason: testReasonFailedValidation, Detail: testsInvalidDetail}
				},
				func(_ context.Context, _ runtime.RunRequest) (runtime.RunResult, error) {
					return runtime.RunResult{ExitCode: 1, AgentTime: time.Second}, &runtime.ExecError{ExitCode: 1}
				},
			},
		},
	}
	deps := pbClaim(t, s, rt, ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Fatalf("commit.Escalation = %+v, want nil (first failed round)", commit.Escalation)
	}
	found := false
	for _, m := range commit.Messages {
		if m.Body == "review round 1 failed\nlens tests: exec failed" {
			found = true
		}
	}
	if !found {
		t.Error(`no "review round 1 failed\nlens tests: exec failed" message in the commit`)
	}

	var invalidMarkers []store.Message
	for _, m := range commit.Messages {
		if strings.HasPrefix(m.Body, "response invalid run ") {
			invalidMarkers = append(invalidMarkers, m)
		}
	}
	if len(invalidMarkers) != 1 {
		t.Fatalf("invalid markers = %d, want 1", len(invalidMarkers))
	}

	pbApply(t, s, ticket, commit)

	runs := reviewRunsSince(t, s, ticket.ID, before)
	var testsRuns []store.Run
	for _, r := range runs {
		if r.Lens != nil && *r.Lens == lensTests {
			if r.Outcome == nil || *r.Outcome != string(response.OutcomeError) {
				t.Errorf("tests run %d outcome = %v, want error", r.ID, r.Outcome)
			}
			testsRuns = append(testsRuns, r)
		}
	}
	if len(testsRuns) != 2 {
		t.Fatalf("tests runs = %d, want 2", len(testsRuns))
	}
	// reviewRunsSince returns runs in ascending id order: testsRuns[0] is
	// the discarded first turn whose invalid marker this checks.
	firstTestsRunID := testsRuns[0].ID
	wantPrefix := fmt.Sprintf("response invalid run %d", firstTestsRunID)
	if !strings.HasPrefix(invalidMarkers[0].Body, wantPrefix) {
		t.Errorf("invalid marker body = %q, want it to start with %q", invalidMarkers[0].Body, wantPrefix)
	}
}

// ---- TestRoundInvalidAfterCancelNotRetried ----------------------------------

// TestRoundInvalidAfterCancelNotRetried proves the retry's own cancellation
// guard (design section 6.2 step 7, issue #32): once another lens has
// already failed the round, a lens whose own turn comes back invalid after
// that is not retried at all.
func TestRoundInvalidAfterCancelNotRetried(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)

	// obsBlocked orders the two failures without a sleep: simplification's
	// own failure (and so the round's own cancellation) cannot happen until
	// observability has already reserved its run and is itself blocked on
	// roundCtx, so this test never races the semaphore to prove its point.
	obsBlocked := make(chan struct{})
	rt := &labelStepsRuntime{
		inner: runtime.NewFake(reviewScriptsFS(nil)),
		steps: map[string][]func(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error){
			simplificationRunLabel: {
				func(_ context.Context, _ runtime.RunRequest) (runtime.RunResult, error) {
					<-obsBlocked
					return runtime.RunResult{ExitCode: 1, AgentTime: time.Second}, &runtime.ExecError{ExitCode: 1}
				},
			},
			"1-observability": {
				func(ctx context.Context, _ runtime.RunRequest) (runtime.RunResult, error) {
					close(obsBlocked)
					<-ctx.Done()
					return runtime.RunResult{SessionID: "obs-sess", ExitCode: 1, AgentTime: time.Second},
						&runtime.InvalidOutputError{Reason: testReasonFailedValidation}
				},
				func(_ context.Context, _ runtime.RunRequest) (runtime.RunResult, error) {
					t.Error("retry started after the round was already canceled")
					return runtime.RunResult{}, errors.New("must not be called")
				},
			},
		},
	}
	deps := pbClaim(t, s, rt, ticket.ID)
	deps.LensesParallel = 7

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, m := range commit.Messages {
		if m.Body == "review round 1 failed\nlens simplification: exec failed" {
			found = true
		}
	}
	if !found {
		t.Error(`no "review round 1 failed\nlens simplification: exec failed" message in the commit`)
	}

	pbApply(t, s, ticket, commit)

	runs := reviewRunsSince(t, s, ticket.ID, before)
	obsCount := 0
	for _, r := range runs {
		if r.Lens != nil && *r.Lens == "observability" {
			obsCount++
		}
	}
	if obsCount != 1 {
		t.Errorf("observability runs = %d, want 1 (no retry once the round is canceled)", obsCount)
	}
}

// ---- TestRoundInvalidLensRetryLogs -------------------------------------------

// jsonLogLine is one slog JSON record this test cares about: the handler
// and level fields are irrelevant to it, so only Msg and the attributes it
// asserts on are decoded.
type jsonLogLine struct {
	Msg        string `json:"msg"`
	TicketID   int64  `json:"ticket_id"`
	Lens       string `json:"lens"`
	SessionID  int64  `json:"session_id"`
	RunID      int64  `json:"run_id"`
	FirstRunID int64  `json:"first_run_id"`
	Reason     string `json:"reason"`
	Outcome    string `json:"outcome"`
	ErrKind    string `json:"err_kind"`
}

// TestRoundInvalidLensRetryLogs proves design section 9's own rule for the
// new retry records (issue #32): "started" and "finished" each log exactly
// once, carrying the closed reason, the outcome, and both run ids, never
// the validator's own detail (runjob_test.go's own slog-swap pattern,
// :1619, hence no t.Parallel here).
func TestRoundInvalidLensRetryLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	s, ticket, before := reviewTicketReady(t)

	overrides := map[string]string{}
	for _, lens := range reviewLensNames {
		if lens == lensFidelity || lens == lensTests {
			continue
		}
		overrides[reviewScriptKey(lens, 1)] = findingScript(lens, "nit", "a nit from "+lens, "polish it")
	}
	fake := runtime.NewFake(reviewScriptsFS(overrides))
	rt := &labelStepsRuntime{
		inner: fake,
		steps: map[string][]func(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error){
			testsRunLabel: {
				func(_ context.Context, _ runtime.RunRequest) (runtime.RunResult, error) {
					return runtime.RunResult{SessionID: testsSessionID, ExitCode: 1, AgentTime: time.Second},
						&runtime.InvalidOutputError{Reason: testReasonFailedValidation, Detail: testsInvalidDetail}
				},
				func(_ context.Context, _ runtime.RunRequest) (runtime.RunResult, error) {
					doc, parseErr := response.Parse([]byte(findingScript(lensTests, "nit", "a nit from tests", "polish it")))
					if parseErr != nil {
						t.Fatalf("parse tests retry script: %v", parseErr)
					}
					return runtime.RunResult{Response: doc.Response, SessionID: testsSessionID, ExitCode: 0, AgentTime: time.Second}, nil
				},
			},
		},
	}
	deps := pbClaim(t, s, rt, ticket.ID)

	var buf bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prior) })

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pbApply(t, s, ticket, commit)

	raw := buf.String()
	if strings.Contains(raw, testsInvalidDetail) {
		t.Error("captured log carries the validator's own Detail, want it absent")
	}

	var started, finished []jsonLogLine
	for line := range strings.SplitSeq(strings.TrimRight(raw, "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec jsonLogLine
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		switch rec.Msg {
		case "review lens retry started":
			started = append(started, rec)
		case "review lens retry finished":
			finished = append(finished, rec)
		}
	}
	if len(started) != 1 {
		t.Fatalf("\"review lens retry started\" records = %d, want 1", len(started))
	}
	if len(finished) != 1 {
		t.Fatalf("\"review lens retry finished\" records = %d, want 1", len(finished))
	}
	if started[0].Lens != lensTests || started[0].Reason != testReasonFailedValidation {
		t.Errorf("started record = %+v, want lens tests and the closed reason", started[0])
	}
	bothSet := finished[0].RunID != 0 && finished[0].FirstRunID != 0
	distinct := finished[0].RunID != finished[0].FirstRunID
	if !bothSet || !distinct {
		t.Errorf("finished record = %+v, want distinct non-zero run_id and first_run_id", finished[0])
	}
	if finished[0].Outcome != string(response.OutcomeOk) {
		t.Errorf("finished record outcome = %q, want %q", finished[0].Outcome, response.OutcomeOk)
	}
	if finished[0].ErrKind != "" {
		t.Errorf("finished record err_kind = %q, want empty", finished[0].ErrKind)
	}

	runs := reviewRunsSince(t, s, ticket.ID, before)
	var testsRuns []store.Run
	for _, r := range runs {
		if r.Lens != nil && *r.Lens == lensTests {
			testsRuns = append(testsRuns, r)
		}
	}
	if len(testsRuns) != 2 {
		t.Fatalf("tests runs = %d, want 2", len(testsRuns))
	}
	firstRun, retryRun := testsRuns[0], testsRuns[1]

	if started[0].TicketID != ticket.ID {
		t.Errorf("started record ticket_id = %d, want %d", started[0].TicketID, ticket.ID)
	}
	if started[0].SessionID == 0 || started[0].SessionID != firstRun.SessionID {
		t.Errorf("started record session_id = %d, want %d (the tests lens's own session)", started[0].SessionID, firstRun.SessionID)
	}
	if started[0].RunID != firstRun.ID {
		t.Errorf("started record run_id = %d, want %d (the first, discarded turn)", started[0].RunID, firstRun.ID)
	}

	if finished[0].TicketID != ticket.ID {
		t.Errorf("finished record ticket_id = %d, want %d", finished[0].TicketID, ticket.ID)
	}
	if finished[0].SessionID != started[0].SessionID {
		t.Errorf("finished record session_id = %d, want %d (the same session as started)", finished[0].SessionID, started[0].SessionID)
	}
	if finished[0].RunID != retryRun.ID {
		t.Errorf("finished record run_id = %d, want %d (the retry's own run)", finished[0].RunID, retryRun.ID)
	}
	if finished[0].FirstRunID != firstRun.ID {
		t.Errorf("finished record first_run_id = %d, want %d (the first, discarded turn)", finished[0].FirstRunID, firstRun.ID)
	}
}

// ---- TestRoundLensQuestionPostsAndStops ---------------------------------------

// TestRoundLensQuestionPostsAndStops proves design section 6.2a's own ASKED
// row: one lens asking, every other lens ok, posts that lens's own question
// with its run's own id and waits on "questions", without advancing state.
func TestRoundLensQuestionPostsAndStops(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey(lensFidelity, 1): reviewQuestionScript("Which style?", "please pick a or b"),
	})
	deps := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != "" {
		t.Errorf("commit.Next = %q, want empty", commit.Next)
	}
	if commit.Waiting == nil || *commit.Waiting != waitingFlagQuestions {
		t.Fatalf("commit.Waiting = %v, want %q", commit.Waiting, waitingFlagQuestions)
	}

	var askingRunID int64
	for _, r := range commit.Runs {
		if r.Outcome != nil && *r.Outcome == string(response.OutcomeQuestion) {
			askingRunID = r.ID
		}
	}
	if askingRunID == 0 {
		t.Fatal("no run terminalized as question")
	}

	found := false
	for _, m := range commit.Messages {
		if m.Type == msgTypeQuestion {
			found = true
			if m.RunID == nil || *m.RunID != askingRunID {
				t.Errorf("question message RunID = %v, want %d (the asking lens's own run)", m.RunID, askingRunID)
			}
			if !strings.Contains(m.Body, "Which style?") {
				t.Errorf("question body = %q, want it to hold the lens's own title", m.Body)
			}
		}
	}
	if !found {
		t.Fatal("no question message in the commit")
	}

	pbApply(t, s, ticket, commit)
	after := pbGetTicket(t, s, ticket.ID)
	if after.State != stateReviewing {
		t.Errorf("ticket state = %q, want reviewing (unchanged)", after.State)
	}
	if after.WaitingOn == nil || *after.WaitingOn != waitingFlagQuestions {
		t.Errorf("ticket waiting_on = %v, want %q", after.WaitingOn, waitingFlagQuestions)
	}
}

// ---- TestRoundLensQuestionHoldsOthers -----------------------------------------

// TestRoundLensQuestionHoldsOthers proves design section 6.2a's own held
// rows: the six clean lenses each get one held finding row, unfiltered
// (r1h1..r1h6), and the marker's own "done" line names all six, in lens
// order.
func TestRoundLensQuestionHoldsOthers(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	overrides := map[string]string{
		reviewScriptKey(lensFidelity, 1): reviewQuestionScript("Which style?", "please pick a or b"),
	}
	for _, lens := range reviewLensNames {
		if lens == lensFidelity {
			continue
		}
		overrides[reviewScriptKey(lens, 1)] = findingScript(lens, "nit", "a nit from "+lens, "polish it")
	}
	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(overrides)), ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pbApply(t, s, ticket, commit)

	marker, ok := reviewMarker(t, s, ticket.ID, "review round 1 asked")
	if !ok {
		t.Fatal(`no "review round 1 asked" marker`)
	}
	lines := strings.Split(marker.Body, "\n")
	if len(lines) != 3 {
		t.Fatalf("asked marker has %d lines, want 3: %q", len(lines), marker.Body)
	}
	if !strings.HasPrefix(lines[1], "runs ") {
		t.Errorf("asked marker line 2 = %q, want it to start with \"runs \"", lines[1])
	}
	wantDone := "done simplification,correctness,security,tests,quality,observability"
	if lines[2] != wantDone {
		t.Errorf("asked marker line 3 = %q, want %q", lines[2], wantDone)
	}

	held := findingArtifactsByRound(t, s, ticket.ID)
	if len(held) != 6 {
		t.Fatalf("held findings = %d, want 6", len(held))
	}
	seenIDs := make(map[string]bool, 6)
	for _, f := range held {
		if !f.Held {
			t.Errorf("finding %s: Held = false, want true", f.ID)
		}
		if f.Decision != nil {
			t.Errorf("finding %s: Decision = %v, want nil (held rows are unfiltered, undecided)", f.ID, *f.Decision)
		}
		seenIDs[f.ID] = true
	}
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("r1h%d", i)
		if !seenIDs[id] {
			t.Errorf("held finding %s missing; got ids %v", id, seenIDs)
		}
	}
}

// ---- TestContinueResumesAskingSession -----------------------------------------

// TestContinueResumesAskingSession proves design section 6.2a step 2: once
// the owner answers the asking lens's own question, CONTINUE resumes that
// same session (same external id, one more resume charged), and no other
// lens gets a second run.
func TestContinueResumesAskingSession(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey(lensFidelity, 1): reviewQuestionScript("Which style?", "please pick a or b"),
		reviewScriptKey(lensFidelity, 2): reviewOKScript,
	})
	rt := runtime.NewFake(scripts)
	deps := pbClaim(t, s, rt, ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)

	roundRuns := reviewRunsSince(t, s, ticket.ID, before)
	var fidelitySessionID, afterRound int64
	for _, r := range roundRuns {
		if *r.Lens == lensFidelity {
			fidelitySessionID = r.SessionID
		}
		if r.ID > afterRound {
			afterRound = r.ID
		}
	}
	if fidelitySessionID == 0 {
		t.Fatal("fidelity's own run not found in round 1")
	}

	q := newestOpenQuestion(t, s, ticket.ID)
	answerReviewQuestion(t, s, ticket.ID, q.ID)

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (continue): %v", err)
	}
	if commit2.Next != stateJudging {
		t.Fatalf("commit2.Next = %q, want judging (fidelity's own resume came back clean)", commit2.Next)
	}
	pbApply(t, s, ticket, commit2)

	resumedRuns := reviewRunsSince(t, s, ticket.ID, afterRound)
	if len(resumedRuns) != 1 {
		t.Fatalf("runs after round 1 = %d, want exactly 1 (fidelity's own resume; no other lens runs again)", len(resumedRuns))
	}
	resumed := resumedRuns[0]
	if *resumed.Lens != lensFidelity {
		t.Errorf("resumed run lens = %q, want fidelity", *resumed.Lens)
	}
	if resumed.SessionID != fidelitySessionID {
		t.Errorf("resumed run session = %d, want %d (the same session)", resumed.SessionID, fidelitySessionID)
	}
	if resumed.Turn != 1 {
		t.Errorf("resumed run turn = %d, want 1", resumed.Turn)
	}

	maxResumes := deps.Machine.Jobs[jobReviewName].MaxResumes
	sess, _, err := s.SessionByID(t.Context(), fidelitySessionID, maxResumes)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess.Resumes != 1 {
		t.Errorf("session resumes = %d, want 1", sess.Resumes)
	}
}

// ---- TestContinueTwoAskersWaitForBoth ------------------------------------------

// TestContinueTwoAskersWaitForBoth proves design section 6.2a's own CONTINUE
// precondition: "once every question of the newest asked marker M is
// answered; while any is still open, ErrNoAction."
func TestContinueTwoAskersWaitForBoth(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	overrides := map[string]string{
		reviewScriptKey(lensFidelity, 1): reviewQuestionScript("Q-fidelity", "pick one"),
		reviewScriptKey("quality", 1):    reviewQuestionScript("Q-quality", "pick one"),
	}
	rt := runtime.NewFake(reviewScriptsFS(overrides))
	deps := pbClaim(t, s, rt, ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)

	open, err := s.QuestionsByState(t.Context(), ticket.ID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("open questions = %d, want 2", len(open))
	}
	answerReviewQuestion(t, s, ticket.ID, open[0].ID)

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	_, err = (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if !errors.Is(err, ErrNoAction) {
		t.Fatalf("err = %v, want ErrNoAction (one asker is still open)", err)
	}
}

// ---- TestContinueAsksAgainCarriesHeld -------------------------------------------

// TestContinueAsksAgainCarriesHeld proves design section 6.2a step 3: a
// resumed lens that asks again gives a new ASKED commit whose own "done"
// line still names every lens the original round already found done, and
// adds no new held rows of its own (fidelity returns no finding either
// time).
func TestContinueAsksAgainCarriesHeld(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	overrides := map[string]string{
		reviewScriptKey(lensFidelity, 1): reviewQuestionScript("Q1", "first ask"),
		reviewScriptKey(lensFidelity, 2): reviewQuestionScript("Q2", "asks again"),
	}
	for _, lens := range reviewLensNames {
		if lens == lensFidelity {
			continue
		}
		overrides[reviewScriptKey(lens, 1)] = findingScript(lens, "nit", "a nit from "+lens, "polish it")
	}
	rt := runtime.NewFake(reviewScriptsFS(overrides))
	deps := pbClaim(t, s, rt, ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)

	heldRound1 := findingArtifactsByRound(t, s, ticket.ID)
	if len(heldRound1) != 6 {
		t.Fatalf("held findings after round 1 = %d, want 6", len(heldRound1))
	}

	answerReviewQuestion(t, s, ticket.ID, newestOpenQuestion(t, s, ticket.ID).ID)

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (continue): %v", err)
	}
	if commit2.Waiting == nil || *commit2.Waiting != waitingFlagQuestions {
		t.Fatalf("commit2.Waiting = %v, want %q (fidelity asked again)", commit2.Waiting, waitingFlagQuestions)
	}
	pbApply(t, s, ticket, commit2)

	marker, ok := reviewMarker(t, s, ticket.ID, "review round 1 asked")
	if !ok {
		t.Fatal(`no "review round 1 asked" marker after the re-ask`)
	}
	lines := strings.Split(marker.Body, "\n")
	if len(lines) != 3 || lines[2] != "done simplification,correctness,security,tests,quality,observability" {
		t.Errorf("re-ask marker line 3 = %q, want the original six lenses carried forward", lines[2])
	}

	heldRound1After := findingArtifactsByRound(t, s, ticket.ID)
	if len(heldRound1After) != 6 {
		t.Errorf("held findings after the re-ask = %d, want still 6 (fidelity asking again adds none)", len(heldRound1After))
	}
}

// ---- TestContinueFailureDropsHeld ------------------------------------------------

// labelStepsRuntime runs, per Label, the next unconsumed function in
// steps[req.Label] (in order), falling back to inner once that label's own
// queue is empty: reviewing_test.go's own way to script "this lens's first
// turn is X, its resume is Y" without confusing the two by call order
// (unlike scriptedRuntime, package job_test, unreachable from here).
type labelStepsRuntime struct {
	inner runtime.Runtime

	mu    sync.Mutex
	steps map[string][]func(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error)
}

func (r *labelStepsRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	r.mu.Lock()
	var fn func(context.Context, runtime.RunRequest) (runtime.RunResult, error)
	if queue := r.steps[req.Label]; len(queue) > 0 {
		fn = queue[0]
		r.steps[req.Label] = queue[1:]
	}
	r.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return r.inner.Run(ctx, req)
}

// TestContinueFailureDropsHeld proves design section 6.2a step 3: a
// resumed lens that comes back with a failure (here, an invalid document)
// writes "review round 1 failed" and the round's own held rows are simply
// left in storage, unrouted (6.2a: "held rows never reach routing ...
// except CONTINUE" -- and this round never reaches a clean CONTINUE).
func TestContinueFailureDropsHeld(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	fake := runtime.NewFake(reviewScriptsFS(map[string]string{
		reviewScriptKey(lensFidelity, 1): reviewQuestionScript("Q1", "first ask"),
	}))
	rt := &labelStepsRuntime{
		inner: fake,
		steps: map[string][]func(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error){
			"1-fidelity": {
				func(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
					return fake.Run(ctx, req)
				},
				func(_ context.Context, _ runtime.RunRequest) (runtime.RunResult, error) {
					return runtime.RunResult{ExitCode: 1, AgentTime: time.Second}, &runtime.InvalidOutputError{Reason: "garbage"}
				},
			},
		},
	}
	deps := pbClaim(t, s, rt, ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)

	answerReviewQuestion(t, s, ticket.ID, newestOpenQuestion(t, s, ticket.ID).ID)

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (continue): %v", err)
	}
	if commit2.Next != "" {
		t.Errorf("commit2.Next = %q, want empty", commit2.Next)
	}
	found := false
	for _, m := range commit2.Messages {
		if strings.HasPrefix(m.Body, "review round 1 failed") {
			found = true
		}
	}
	if !found {
		t.Fatal(`no "review round 1 failed" message in the continue commit`)
	}
	if len(commit2.Artifacts) != 0 {
		t.Errorf("commit2.Artifacts = %d, want 0 (a failed continuation stores no new findings)", len(commit2.Artifacts))
	}
}

// ---- TestContinueInvalidLensRetried ------------------------------------------

// TestContinueInvalidLensRetried proves the retry also fires on a CONTINUE
// resume (design section 6.2a, issue #32), not just on ROUND's own first
// turn: fidelity's resumed turn comes back invalid, carrying the resume's
// own session id, and its own same-tick retry on that session succeeds, so
// CONTINUE still ends the round done with no escalation.
func TestContinueInvalidLensRetried(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	fake := runtime.NewFake(reviewScriptsFS(map[string]string{
		reviewScriptKey(lensFidelity, 1): reviewQuestionScript("Q1", "first ask"),
	}))
	var invalidSessionID string
	rt := &labelStepsRuntime{
		inner: fake,
		steps: map[string][]func(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error){
			"1-fidelity": {
				func(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
					return fake.Run(ctx, req)
				},
				func(_ context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
					invalidSessionID = req.SessionID
					return runtime.RunResult{SessionID: req.SessionID, ExitCode: 1, AgentTime: time.Second},
						&runtime.InvalidOutputError{Reason: testReasonFailedValidation, Detail: "bad doc"}
				},
				func(_ context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
					if req.SessionID != invalidSessionID {
						t.Errorf("retry request SessionID = %q, want %q", req.SessionID, invalidSessionID)
					}
					doc, parseErr := response.Parse([]byte(reviewOKScript))
					if parseErr != nil {
						t.Fatalf("parse ok script: %v", parseErr)
					}
					return runtime.RunResult{Response: doc.Response, SessionID: req.SessionID, ExitCode: 0, AgentTime: time.Second}, nil
				},
			},
		},
	}
	deps := pbClaim(t, s, rt, ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)

	answerReviewQuestion(t, s, ticket.ID, newestOpenQuestion(t, s, ticket.ID).ID)

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (continue): %v", err)
	}
	if commit2.Escalation != nil {
		t.Fatalf("commit2.Escalation = %+v, want nil", commit2.Escalation)
	}
	var sawFailed, sawDone bool
	for _, m := range commit2.Messages {
		if strings.HasPrefix(m.Body, "review round 1 failed") {
			sawFailed = true
		}
		if strings.HasPrefix(m.Body, "review round 1 done") {
			sawDone = true
		}
	}
	if sawFailed {
		t.Error("commit2 carries a \"review round 1 failed\" marker, want none")
	}
	if !sawDone {
		t.Error(`no "review round 1 done" marker in commit2`)
	}
}

// ---- TestContinueCapExhaustedEscalates -------------------------------------------

// TestContinueCapExhaustedEscalates proves design section 6.2a step 2's own
// cap gate: jobs.review.max_resumes is 2, so a third resume attempt on the
// same asking session finds it exhausted and escalates resumes_exhausted,
// origin cap_resumes, with no run started at all.
func TestContinueCapExhaustedEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey(lensFidelity, 1): reviewQuestionScript("Q1", "ask 1"),
		reviewScriptKey(lensFidelity, 2): reviewQuestionScript("Q2", "ask 2"),
		reviewScriptKey(lensFidelity, 3): reviewQuestionScript("Q3", "ask 3"),
	})
	rt := runtime.NewFake(scripts)
	deps := pbClaim(t, s, rt, ticket.ID)

	// Round: fidelity's own first turn asks.
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)
	answerReviewQuestion(t, s, ticket.ID, newestOpenQuestion(t, s, ticket.ID).ID)

	// Continue #1: resumes 0 -> 1, asks again.
	ticket = pbGetTicket(t, s, ticket.ID)
	deps = pbClaim(t, s, rt, ticket.ID)
	commit, err = (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (continue 1): %v", err)
	}
	pbApply(t, s, ticket, commit)
	answerReviewQuestion(t, s, ticket.ID, newestOpenQuestion(t, s, ticket.ID).ID)

	// Continue #2: resumes 1 -> 2, asks again.
	ticket = pbGetTicket(t, s, ticket.ID)
	deps = pbClaim(t, s, rt, ticket.ID)
	commit, err = (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (continue 2): %v", err)
	}
	pbApply(t, s, ticket, commit)
	answerReviewQuestion(t, s, ticket.ID, newestOpenQuestion(t, s, ticket.ID).ID)

	// Continue #3: the session's own resumes is now 2, at max_resumes:
	// exhausted, no run started.
	beforeThird, err := s.MaxRunID(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	ticket = pbGetTicket(t, s, ticket.ID)
	deps = pbClaim(t, s, rt, ticket.ID)
	commit, err = (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (continue 3): %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want set")
	}
	if commit.Escalation.Payload.Code != string(response.EscalationCodeResumesExhausted) {
		t.Errorf("escalation code = %q, want %q", commit.Escalation.Payload.Code, response.EscalationCodeResumesExhausted)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginCapResumes) {
		t.Errorf("escalation origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginCapResumes)
	}
	afterThird, err := s.MaxRunID(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	if afterThird != beforeThird {
		t.Errorf("MaxRunID changed from %d to %d, want unchanged (no run started)", beforeThird, afterThird)
	}
}

// ---- task 11: TRIAGE (6.5) and DISCUSS (6.6) -------------------------------

// recordingRuntime wraps another Runtime and records every RunRequest it
// receives, in call order: this file's own way to assert a resume's own
// Label and Prompt directly (the resume charge and session identity are
// asserted through the store's own Session and Run rows instead, the same
// pattern TestContinueResumesAskingSession already uses).
type recordingRuntime struct {
	inner runtime.Runtime

	mu   sync.Mutex
	reqs []runtime.RunRequest
}

func (r *recordingRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	return r.inner.Run(ctx, req)
}

// lastRequest returns the most recent RunRequest r has served.
func (r *recordingRuntime) lastRequest(t *testing.T) runtime.RunRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		t.Fatal("recordingRuntime: no request recorded")
	}
	return r.reqs[len(r.reqs)-1]
}

// itemRefByText returns the Ref of payload's own item whose Text contains
// want, failing the test if none or more than one does: reviewing_test.go's
// own way to find a finding's own stored id when a test cannot assume which
// of two findings DedupFindings' own (path, line) order gave the lower id.
func itemRefByText(t *testing.T, payload response.QuestionPayload, want string) string {
	t.Helper()
	var ref string
	matches := 0
	for _, it := range payload.Items {
		if strings.Contains(it.Text, want) {
			ref = it.Ref
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("itemRefByText(%q): matched %d items, want exactly 1", want, matches)
	}
	return ref
}

// answerReviewItems drives the real console draft/send path (SaveDraft,
// then SendBatch) against questionID: one item draft per entry of
// decisions, a free-text reply when note != "", all sent in one batch
// (design section 6.5's own precondition: every item decided, or a reply
// alone, either of which markAnsweredQuestionsTx marks answered).
func answerReviewItems(t *testing.T, s *store.Store, ticketID, questionID int64, decisions map[string]response.Decision, note string) {
	t.Helper()
	for ref, d := range decisions {
		if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &questionID, Item: &store.ItemDecision{Ref: ref, Decision: d}}); err != nil {
			t.Fatalf("SaveDraft(item %s): %v", ref, err)
		}
	}
	if note != "" {
		if _, err := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &questionID, Text: note}); err != nil {
			t.Fatalf("SaveDraft(text): %v", err)
		}
	}
	if _, err := s.SendBatch(t.Context(), ticketID); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
}

// runByFindingID returns the run that produced findingID's own newest
// stored row: DISCUSS's own session-grouping key (nextPendingDiscussGroup),
// read back the same way for a test's own assertions.
func runByFindingID(t *testing.T, s *store.Store, ticketID int64, findingID string) store.Run {
	t.Helper()
	findings, err := s.Findings(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	newest := newestFindingRowPerID(findings)
	row, ok := newest[findingID]
	if !ok || row.RunID == nil {
		t.Fatalf("runByFindingID(%q): no stored row with a run id", findingID)
	}
	run, err := s.RunByID(t.Context(), *row.RunID)
	if err != nil {
		t.Fatalf("RunByID: %v", err)
	}
	return run
}

// discussLens, discussSeverity, discussText, and discussFix are
// discussGroupReady's own fixed scenario: every one of this file's own
// discuss tests needs the same single above-floor finding, varying only
// its own note, so these are named constants rather than parameters no
// caller actually varies (unparam).
const (
	discussLens     = "security"
	discussSeverity = "major"
	discussText     = "unchecked input"
	discussFix      = "validate it"
)

// discussGroupReady drives reviewTicketReady's own ticket through ROUND 1
// (discussLens finds one above-floor finding at greetGoLine5) and TRIAGE
// (decision discuss, with note when note != ""), leaving exactly one
// pending discuss group, ready for reviewingHandler.Run to enter DISCUSS.
// scripts is the live (mutable) fs.FS backing rt, so a caller can add the
// resume's own next-turn script before driving DISCUSS. findingID is round
// 1's own only stored finding id (r1f1).
func discussGroupReady(t *testing.T, note string) (s *store.Store, ticket store.Ticket, rt *recordingRuntime, scripts fstest.MapFS, findingID string) {
	t.Helper()
	s, ticket0, _ := reviewTicketReady(t)
	scripts = reviewScriptsFS(map[string]string{
		reviewScriptKey(discussLens, 1): findingScript(discussLens, discussSeverity, discussText, discussFix),
	})
	rt = &recordingRuntime{inner: runtime.NewFake(scripts)}
	deps := pbClaim(t, s, rt, ticket0.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket0, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket0, commit)

	q := newestOpenQuestion(t, s, ticket0.ID)
	var payload response.QuestionPayload
	if err = json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("payload.Items = %d, want 1", len(payload.Items))
	}
	findingID = payload.Items[0].Ref

	answerReviewItems(t, s, ticket0.ID, q.ID, map[string]response.Decision{findingID: response.DecisionDiscuss}, note)

	ticket1 := pbGetTicket(t, s, ticket0.ID)
	deps1 := pbClaim(t, s, rt, ticket0.ID)
	commit1, err := (reviewingHandler{}).Run(t.Context(), ticket1, deps1)
	if err != nil {
		t.Fatalf("Run (triage): %v", err)
	}
	pbApply(t, s, ticket0, commit1)

	ticket = pbGetTicket(t, s, ticket0.ID)
	return s, ticket, rt, scripts, findingID
}

// ---- TestTriageStoresDecisions ----------------------------------------------

func TestTriageStoresDecisions(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey("security", 1): findingScriptAt("security", "major", greetGoLine5, "unchecked input", "validate it"),
		reviewScriptKey("quality", 1):  findingScriptAt("quality", "blocker", greetGoLine2, "breaks the build", "fix the build"),
	})
	deps := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)

	q := newestOpenQuestion(t, s, ticket.ID)
	var payload response.QuestionPayload
	if err = json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	accepted := itemRefByText(t, payload, "unchecked input")
	dropped := itemRefByText(t, payload, "breaks the build")

	answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{
		accepted: response.DecisionAccept,
		dropped:  response.DecisionDrop,
	}, "")

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (triage): %v", err)
	}
	if commit2.Next != "" {
		t.Errorf("commit2.Next = %q, want empty (no state transition in TRIAGE)", commit2.Next)
	}
	if len(commit2.ResolveQuestions) != 1 || commit2.ResolveQuestions[0] != q.ID {
		t.Errorf("commit2.ResolveQuestions = %v, want [%d]", commit2.ResolveQuestions, q.ID)
	}
	pbApply(t, s, ticket, commit2)

	findings, err := s.Findings(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	newest := newestFindingRowPerID(findings)
	if d := newest[accepted].Finding.Decision; d == nil || *d != response.FindingAccept {
		t.Errorf("accepted finding %s decision = %v, want accept", accepted, d)
	}
	if d := newest[dropped].Finding.Decision; d == nil || *d != response.FindingDrop {
		t.Errorf("dropped finding %s decision = %v, want drop", dropped, d)
	}

	resolved, err := s.QuestionsByState(t.Context(), ticket.ID, "resolved")
	if err != nil {
		t.Fatalf("QuestionsByState(resolved): %v", err)
	}
	found := false
	for _, r := range resolved {
		if r.ID == q.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("question %d not resolved after TRIAGE", q.ID)
	}
	if ticket2 := pbGetTicket(t, s, ticket.ID); ticket2.WaitingOn != nil {
		t.Errorf("ticket.WaitingOn = %v, want nil (cleared)", *ticket2.WaitingOn)
	}
}

// ---- TestTriageDefaultsToAccept ----------------------------------------------

// TestTriageDefaultsToAccept proves design section 6.5 step 2 and section
// 14's own edge case: an item the owner leaves undecided defaults to
// accept. The owner decides only one of the round's two above-floor
// findings and sends a free reply on the question, which
// markAnsweredQuestionsTx marks answered on its own (the other path design
// section 14 names: "the console marks the question answered only when
// every item has a decision" -- a reply is the other one).
func TestTriageDefaultsToAccept(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey("security", 1): findingScriptAt("security", "major", greetGoLine5, "unchecked input", "validate it"),
		reviewScriptKey("quality", 1):  findingScriptAt("quality", "blocker", greetGoLine2, "breaks the build", "fix the build"),
	})
	deps := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)

	q := newestOpenQuestion(t, s, ticket.ID)
	var payload response.QuestionPayload
	if err = json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	decided := itemRefByText(t, payload, "unchecked input")
	undecided := itemRefByText(t, payload, "breaks the build")

	answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{decided: response.DecisionDrop}, "going with the recommendation for the rest")

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (triage): %v", err)
	}
	pbApply(t, s, ticket, commit2)

	findings, err := s.Findings(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	newest := newestFindingRowPerID(findings)
	if d := newest[decided].Finding.Decision; d == nil || *d != response.FindingDrop {
		t.Errorf("decided finding %s decision = %v, want drop (unaffected by the default)", decided, d)
	}
	if d := newest[undecided].Finding.Decision; d == nil || *d != response.FindingAccept {
		t.Errorf("undecided finding %s decision = %v, want accept (the safe default)", undecided, d)
	}
}

// ---- TestTriageWritesNotes ---------------------------------------------------

// TestTriageWritesNotes proves design section 6.5 step 4 and D24: a
// discussed item's own "review note <id>" marker carries the owner's
// replies on the round, or the fixed "(the owner gave no note)" text when
// there are none.
func TestTriageWritesNotes(t *testing.T) {
	t.Parallel()
	t.Run("with a reply", func(t *testing.T) {
		t.Parallel()
		s, ticket, _ := reviewTicketReady(t)
		scripts := reviewScriptsFS(map[string]string{
			reviewScriptKey("security", 1): findingScript("security", "major", "unchecked input", "validate it"),
		})
		deps := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)
		commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("Run (round): %v", err)
		}
		pbApply(t, s, ticket, commit)

		q := newestOpenQuestion(t, s, ticket.ID)
		var payload response.QuestionPayload
		if err = json.Unmarshal(q.Payload, &payload); err != nil {
			t.Fatalf("unmarshal question payload: %v", err)
		}
		ref := payload.Items[0].Ref
		answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{ref: response.DecisionDiscuss}, "take another look at the error path")

		ticket2 := pbGetTicket(t, s, ticket.ID)
		deps2 := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)
		commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
		if err != nil {
			t.Fatalf("Run (triage): %v", err)
		}
		pbApply(t, s, ticket, commit2)

		marker, ok := reviewMarker(t, s, ticket.ID, "review note "+ref)
		if !ok {
			t.Fatalf("no %q marker", "review note "+ref)
		}
		if !strings.Contains(marker.Body, "take another look at the error path") {
			t.Errorf("note marker body = %q, want it to contain the owner's reply", marker.Body)
		}
	})

	t.Run("with none", func(t *testing.T) {
		t.Parallel()
		s, ticket, _ := reviewTicketReady(t)
		scripts := reviewScriptsFS(map[string]string{
			reviewScriptKey("security", 1): findingScript("security", "major", "unchecked input", "validate it"),
		})
		deps := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)
		commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("Run (round): %v", err)
		}
		pbApply(t, s, ticket, commit)

		q := newestOpenQuestion(t, s, ticket.ID)
		var payload response.QuestionPayload
		if err = json.Unmarshal(q.Payload, &payload); err != nil {
			t.Fatalf("unmarshal question payload: %v", err)
		}
		ref := payload.Items[0].Ref
		answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{ref: response.DecisionDiscuss}, "")

		ticket2 := pbGetTicket(t, s, ticket.ID)
		deps2 := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)
		commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
		if err != nil {
			t.Fatalf("Run (triage): %v", err)
		}
		pbApply(t, s, ticket, commit2)

		marker, ok := reviewMarker(t, s, ticket.ID, "review note "+ref)
		if !ok {
			t.Fatalf("no %q marker", "review note "+ref)
		}
		if !strings.Contains(marker.Body, reviewNoteNone) {
			t.Errorf("note marker body = %q, want it to contain %q", marker.Body, reviewNoteNone)
		}
	})
}

// ---- TestDiscussResumesLensSession -------------------------------------------

// TestDiscussResumesLensSession proves design section 6.6 steps 1-3: DISCUSS
// resumes the discussed finding's own lens session, under its own original
// label, with the finding and the owner's note as inputs, charging one
// resume.
func TestDiscussResumesLensSession(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, rt, scripts, findingID := discussGroupReady(t, "please check the error path again")
	scripts[reviewScriptKey("security", 2)] = &fstest.MapFile{Data: []byte(reviewOKScript)}

	run0 := runByFindingID(t, s, ticket.ID, findingID)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (discuss): %v", err)
	}
	pbApply(t, s, ticket, commit)

	req := rt.lastRequest(t)
	if req.Label != "1-security" {
		t.Errorf("label = %q, want %q", req.Label, "1-security")
	}
	if !strings.Contains(req.Prompt, findingID) {
		t.Errorf("prompt missing finding id %q; got:\n%s", findingID, req.Prompt)
	}
	if !strings.Contains(req.Prompt, "unchecked input") {
		t.Errorf("prompt missing the finding's own text; got:\n%s", req.Prompt)
	}
	if !strings.Contains(req.Prompt, "please check the error path again") {
		t.Errorf("prompt missing the owner's note; got:\n%s", req.Prompt)
	}

	maxResumes := deps.Machine.Jobs[jobReviewName].MaxResumes
	sess, _, err := s.SessionByID(t.Context(), run0.SessionID, maxResumes)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess.Resumes != 1 {
		t.Errorf("session resumes = %d, want 1", sess.Resumes)
	}

	newestRunID, err := s.MaxRunID(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	resumed, err := s.RunByID(t.Context(), newestRunID)
	if err != nil {
		t.Fatalf("RunByID: %v", err)
	}
	if resumed.SessionID != run0.SessionID {
		t.Errorf("resumed run session = %d, want %d (the same session)", resumed.SessionID, run0.SessionID)
	}
	if resumed.Turn != 1 {
		t.Errorf("resumed run turn = %d, want 1", resumed.Turn)
	}
	if resumed.Lens == nil || *resumed.Lens != "security" {
		t.Errorf("resumed run lens = %v, want security", resumed.Lens)
	}
}

// ---- TestDiscussBatchesOneSession --------------------------------------------

// TestDiscussBatchesOneSession proves design section 6.6's own batching
// rule: two findings of one lens, both discussed, are carried by one
// resume, charging one resume, and each writes its own "review discussed
// <id>" marker naming the same batch; a merged successor supersedes both.
func TestDiscussBatchesOneSession(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey("security", 1): twoFindingScript(
			"security", "major", greetGoLine2, "first finding", "fix the first",
			"major", greetGoLine5, "second finding", "fix the second",
		),
	})
	rt := &recordingRuntime{inner: runtime.NewFake(scripts)}
	deps := pbClaim(t, s, rt, ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)

	q := newestOpenQuestion(t, s, ticket.ID)
	var payload response.QuestionPayload
	if err = json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("payload.Items = %d, want 2", len(payload.Items))
	}
	ref1 := itemRefByText(t, payload, "first finding")
	ref2 := itemRefByText(t, payload, "second finding")

	answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{
		ref1: response.DecisionDiscuss,
		ref2: response.DecisionDiscuss,
	}, "")

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (triage): %v", err)
	}
	pbApply(t, s, ticket, commit2)

	run1 := runByFindingID(t, s, ticket.ID, ref1)
	run2 := runByFindingID(t, s, ticket.ID, ref2)
	if run1.SessionID != run2.SessionID {
		t.Fatalf("finding sessions = %d, %d, want the same (one lens)", run1.SessionID, run2.SessionID)
	}

	scripts[reviewScriptKey("security", 2)] = &fstest.MapFile{Data: []byte(findingScript("security", "minor", "revised, combined finding", "combined fix"))}

	ticket3 := pbGetTicket(t, s, ticket.ID)
	deps3 := pbClaim(t, s, rt, ticket.ID)
	commit3, err := (reviewingHandler{}).Run(t.Context(), ticket3, deps3)
	if err != nil {
		t.Fatalf("Run (discuss): %v", err)
	}
	pbApply(t, s, ticket, commit3)

	req := rt.lastRequest(t)
	if !strings.Contains(req.Prompt, "first finding") || !strings.Contains(req.Prompt, "second finding") {
		t.Errorf("prompt missing one of the group's own findings; got:\n%s", req.Prompt)
	}

	maxResumes := deps.Machine.Jobs[jobReviewName].MaxResumes
	sess, _, err := s.SessionByID(t.Context(), run1.SessionID, maxResumes)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess.Resumes != 1 {
		t.Errorf("session resumes = %d, want 1 (one resume for the whole batch)", sess.Resumes)
	}

	m1, ok1 := reviewMarker(t, s, ticket.ID, reviewDiscussedMarker(ref1))
	m2, ok2 := reviewMarker(t, s, ticket.ID, reviewDiscussedMarker(ref2))
	if !ok1 || !ok2 {
		t.Fatalf("discussed markers ok = %v, %v, want both true", ok1, ok2)
	}
	if !strings.Contains(m1.Body, "kept 1") || !strings.Contains(m2.Body, "kept 1") {
		t.Errorf("discussed markers = %q, %q, want both to report kept 1", m1.Body, m2.Body)
	}
	_, batch1, _ := strings.Cut(m1.Body, "\n")
	_, batch2, _ := strings.Cut(m2.Body, "\n")
	if batch1 != batch2 {
		t.Errorf("discussed markers name different batches: %q vs %q", batch1, batch2)
	}

	findings, err := s.Findings(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	var survivor *response.FindingArtifact
	for i := range findings {
		f := &findings[i].Finding
		if f.ID != ref1 && f.ID != ref2 && !f.Held {
			survivor = f
		}
	}
	if survivor == nil {
		t.Fatal("no survivor finding stored")
	}
	if len(survivor.Supersedes) != 2 || !slices.Contains(survivor.Supersedes, ref1) || !slices.Contains(survivor.Supersedes, ref2) {
		t.Errorf("survivor.Supersedes = %v, want [%s %s] (in some order)", survivor.Supersedes, ref1, ref2)
	}
}

// ---- TestDiscussTwoSessionsTwoTicks -------------------------------------------

// TestDiscussTwoSessionsTwoTicks proves design section 6.6's own "take the
// group whose lowest finding id is lowest" rule: two findings of two
// different lenses, both discussed, are two separate sessions, and one
// Run tick resolves only the lowest-id one, leaving the other pending for
// the next tick.
func TestDiscussTwoSessionsTwoTicks(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey("correctness", 1): findingScriptAt("correctness", "major", greetGoLine2, "first lens finding", "fix the first"),
		reviewScriptKey("security", 1):    findingScriptAt("security", "major", greetGoLine5, "second lens finding", "fix the second"),
	})
	rt := &recordingRuntime{inner: runtime.NewFake(scripts)}
	deps := pbClaim(t, s, rt, ticket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)

	q := newestOpenQuestion(t, s, ticket.ID)
	var payload response.QuestionPayload
	if err = json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	refFirst := itemRefByText(t, payload, "first lens finding")
	refSecond := itemRefByText(t, payload, "second lens finding")

	answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{
		refFirst:  response.DecisionDiscuss,
		refSecond: response.DecisionDiscuss,
	}, "")

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (triage): %v", err)
	}
	pbApply(t, s, ticket, commit2)

	runFirst := runByFindingID(t, s, ticket.ID, refFirst)
	runSecond := runByFindingID(t, s, ticket.ID, refSecond)
	if runFirst.Lens == nil || runSecond.Lens == nil {
		t.Fatal("both findings' own runs must carry a lens")
	}
	scripts[reviewScriptKey(*runFirst.Lens, 2)] = &fstest.MapFile{Data: []byte(reviewOKScript)}
	scripts[reviewScriptKey(*runSecond.Lens, 2)] = &fstest.MapFile{Data: []byte(reviewOKScript)}

	// Tick 1: the lowest finding id's own group resolves; the other stays
	// pending.
	ticket3 := pbGetTicket(t, s, ticket.ID)
	deps3 := pbClaim(t, s, rt, ticket.ID)
	commit3, err := (reviewingHandler{}).Run(t.Context(), ticket3, deps3)
	if err != nil {
		t.Fatalf("Run (discuss 1): %v", err)
	}
	pbApply(t, s, ticket, commit3)

	_, refFirstDone := reviewMarker(t, s, ticket.ID, reviewDiscussedMarker(refFirst))
	_, refSecondDone := reviewMarker(t, s, ticket.ID, reviewDiscussedMarker(refSecond))
	if refFirstDone == refSecondDone {
		t.Fatalf("exactly one of the two findings should be discussed after tick 1; refFirst done=%v refSecond done=%v", refFirstDone, refSecondDone)
	}

	// Tick 2: the remaining group resolves.
	ticket4 := pbGetTicket(t, s, ticket.ID)
	deps4 := pbClaim(t, s, rt, ticket.ID)
	commit4, err := (reviewingHandler{}).Run(t.Context(), ticket4, deps4)
	if err != nil {
		t.Fatalf("Run (discuss 2): %v", err)
	}
	pbApply(t, s, ticket, commit4)

	_, refFirstDone = reviewMarker(t, s, ticket.ID, reviewDiscussedMarker(refFirst))
	_, refSecondDone = reviewMarker(t, s, ticket.ID, reviewDiscussedMarker(refSecond))
	if !refFirstDone || !refSecondDone {
		t.Errorf("both findings should be discussed after tick 2; refFirst done=%v refSecond done=%v", refFirstDone, refSecondDone)
	}
}

// ---- TestDiscussWithdrawn -----------------------------------------------------

// TestDiscussWithdrawn proves design section 6.6 step 4's own "zero
// survivors" case: the lens returns no finding at all, so no new row is
// stored and no new question follows, only the group's own "review
// discussed <id>" marker, kept 0.
func TestDiscussWithdrawn(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, rt, scripts, findingID := discussGroupReady(t, "")
	scripts[reviewScriptKey("security", 2)] = &fstest.MapFile{Data: []byte(reviewOKScript)}

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (discuss): %v", err)
	}
	if len(commit.Artifacts) != 0 {
		t.Errorf("commit.Artifacts = %d, want 0 (withdrawn)", len(commit.Artifacts))
	}
	if commit.Waiting != nil {
		t.Errorf("commit.Waiting = %v, want nil (no new question)", *commit.Waiting)
	}
	pbApply(t, s, ticket, commit)

	marker, ok := reviewMarker(t, s, ticket.ID, reviewDiscussedMarker(findingID))
	if !ok {
		t.Fatal("no discussed marker written")
	}
	if !strings.Contains(marker.Body, "kept 0") {
		t.Errorf("discussed marker = %q, want it to report kept 0", marker.Body)
	}
}

// ---- TestDiscussRevisedAsksAgain ----------------------------------------------

// TestDiscussRevisedAsksAgain proves design section 6.6 step 4's own
// survivor case: the lens revises the finding, above the floor again, into
// a new row at the round's own next free id, superseding the discussed one,
// and a new review question follows.
func TestDiscussRevisedAsksAgain(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, rt, scripts, findingID := discussGroupReady(t, "")
	scripts[reviewScriptKey("security", 2)] = &fstest.MapFile{Data: []byte(findingScript("security", "blocker", "still unchecked, worse than thought", "validate it properly"))}

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (discuss): %v", err)
	}
	if commit.Waiting == nil || *commit.Waiting != waitingFlagReview {
		t.Fatalf("commit.Waiting = %v, want %q", commit.Waiting, waitingFlagReview)
	}
	pbApply(t, s, ticket, commit)

	open := newestOpenQuestion(t, s, ticket.ID)
	var payload response.QuestionPayload
	if err = json.Unmarshal(open.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("payload.Items = %d, want 1", len(payload.Items))
	}
	newID := payload.Items[0].Ref
	if newID == findingID {
		t.Fatalf("new item id = %q, want a fresh id distinct from %q", newID, findingID)
	}

	findings, err := s.Findings(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	newest := newestFindingRowPerID(findings)
	row, ok := newest[newID]
	if !ok {
		t.Fatalf("no stored row for the new id %q", newID)
	}
	if len(row.Finding.Supersedes) != 1 || row.Finding.Supersedes[0] != findingID {
		t.Errorf("Supersedes = %v, want [%s]", row.Finding.Supersedes, findingID)
	}
}

// ---- TestDiscussBelowFloorJoinsFixList -----------------------------------------

// TestDiscussBelowFloorJoinsFixList proves design section 6.6 step 4's own
// at-or-below-floor case: a revised survivor at or below the floor gets
// Decision accept directly, posts no question, and joins the round's own
// fix list on the next tick.
func TestDiscussBelowFloorJoinsFixList(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, rt, scripts, _ := discussGroupReady(t, "")
	scripts[reviewScriptKey("security", 2)] = &fstest.MapFile{Data: []byte(findingScript("security", "minor", "a small nit now", "small fix"))}

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (discuss): %v", err)
	}
	if commit.Waiting != nil {
		t.Errorf("commit.Waiting = %v, want nil (at or below the floor posts no question)", *commit.Waiting)
	}
	pbApply(t, s, ticket, commit)

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (fixreq): %v", err)
	}
	found := false
	for _, m := range commit2.Messages {
		if strings.HasPrefix(m.Body, fixRequestedFindingsPrefix) {
			found = true
			if !strings.Contains(m.Body, "a small nit now") {
				t.Errorf("fix request body = %q, want it to quote the revised finding", m.Body)
			}
		}
	}
	if !found {
		t.Fatal("no fix requested findings message after the below-floor discuss revision")
	}
}

// ---- TestDiscussExhaustedEscalatesOnce -----------------------------------------

// TestDiscussExhaustedEscalatesOnce proves design section 6.6 step 2's own
// cap gate: jobs.review.max_resumes is 2, so after two discuss resumes the
// session is exhausted; the escalation fires once, and a repeat before the
// owner retries finds it already escalated (ErrNoAction).
func TestDiscussExhaustedEscalatesOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, rt, scripts, _ := discussGroupReady(t, "")
	scripts[reviewScriptKey("security", 2)] = &fstest.MapFile{Data: []byte(reviewQuestionScript("Q1", "which way?"))}
	scripts[reviewScriptKey("security", 3)] = &fstest.MapFile{Data: []byte(reviewQuestionScript("Q2", "which way now?"))}

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (discuss 1): %v", err)
	}
	pbApply(t, s, ticket, commit)
	answerReviewQuestion(t, s, ticket.ID, newestOpenQuestion(t, s, ticket.ID).ID)

	ticket = pbGetTicket(t, s, ticket.ID)
	deps = pbClaim(t, s, rt, ticket.ID)
	commit, err = (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (discuss 2): %v", err)
	}
	pbApply(t, s, ticket, commit)
	answerReviewQuestion(t, s, ticket.ID, newestOpenQuestion(t, s, ticket.ID).ID)

	ticket = pbGetTicket(t, s, ticket.ID)
	deps = pbClaim(t, s, rt, ticket.ID)
	commit, err = (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (cap): %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want set")
	}
	if commit.Escalation.Payload.Code != string(response.EscalationCodeResumesExhausted) {
		t.Errorf("escalation code = %q, want %q", commit.Escalation.Payload.Code, response.EscalationCodeResumesExhausted)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginCapResumes) {
		t.Errorf("escalation origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginCapResumes)
	}
	pbApply(t, s, ticket, commit)

	ticket = pbGetTicket(t, s, ticket.ID)
	deps = pbClaim(t, s, rt, ticket.ID)
	_, err = (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if !errors.Is(err, ErrNoAction) {
		t.Fatalf("err = %v, want ErrNoAction (already escalated once)", err)
	}
}

// ---- TestDiscussHeadMovedEscalates ----------------------------------------------

// TestDiscussHeadMovedEscalates proves design section 6.6 step 1: the
// worktree's own HeadSHA must equal the round's own frozen sha before any
// discuss resume runs, else it escalates environment.
func TestDiscussHeadMovedEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, rt, _, _ := discussGroupReady(t, "")

	deps := pbClaim(t, s, rt, ticket.ID)
	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if addErr := gitfixture.AddFile(t.Context(), wt.Dir(), "late.txt", []byte("late\n")); addErr != nil {
		t.Fatalf("add late commit: %v", addErr)
	}

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want set")
	}
	if commit.Escalation.Payload.What != discussHeadMovedWhat {
		t.Errorf("escalation What = %q, want %q", commit.Escalation.Payload.What, discussHeadMovedWhat)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginReview) {
		t.Errorf("escalation origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginReview)
	}
}

// ---- TestDecidedRoundRequestsFix ----------------------------------------------

// TestDecidedRoundRequestsFix proves the ordinary path from TRIAGE to
// FIXREQ (design section 6.1 step 3, 6.8): once every above-floor finding
// of the round has a decision (here, accept and drop, no discuss), the next
// tick opens a fix request with the accepted finding's own fix text.
func TestDecidedRoundRequestsFix(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey("security", 1): findingScriptAt("security", "major", greetGoLine5, "unchecked input", "validate it"),
		reviewScriptKey("quality", 1):  findingScriptAt("quality", "blocker", greetGoLine2, "breaks the build", "fix the build"),
	})
	deps := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)

	q := newestOpenQuestion(t, s, ticket.ID)
	var payload response.QuestionPayload
	if err = json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	accepted := itemRefByText(t, payload, "unchecked input")
	dropped := itemRefByText(t, payload, "breaks the build")
	answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{
		accepted: response.DecisionAccept,
		dropped:  response.DecisionDrop,
	}, "")

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (triage): %v", err)
	}
	pbApply(t, s, ticket, commit2)

	ticket3 := pbGetTicket(t, s, ticket.ID)
	deps3 := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)
	commit3, err := (reviewingHandler{}).Run(t.Context(), ticket3, deps3)
	if err != nil {
		t.Fatalf("Run (fixreq): %v", err)
	}
	found := false
	for _, m := range commit3.Messages {
		if strings.HasPrefix(m.Body, fixRequestedFindingsPrefix) {
			found = true
			if !strings.Contains(m.Body, "unchecked input") {
				t.Errorf("fix request body = %q, want it to quote the accepted finding", m.Body)
			}
			if strings.Contains(m.Body, "breaks the build") {
				t.Errorf("fix request body = %q, want it to exclude the dropped finding", m.Body)
			}
		}
	}
	if !found {
		t.Fatal("no fix requested findings message")
	}
}

// ---- TestAllDroppedMovesToJudging ----------------------------------------------

// TestAllDroppedMovesToJudging proves design section 6.1 step 3's own clean
// path when every above-floor finding is dropped: the accepted list is
// empty, so the next tick moves straight to judging.
func TestAllDroppedMovesToJudging(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey("security", 1): findingScript("security", "major", "unchecked input", "validate it"),
	})
	deps := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)

	q := newestOpenQuestion(t, s, ticket.ID)
	var payload response.QuestionPayload
	if err = json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("payload.Items = %d, want 1", len(payload.Items))
	}
	answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{payload.Items[0].Ref: response.DecisionDrop}, "")

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)
	if err != nil {
		t.Fatalf("Run (triage): %v", err)
	}
	pbApply(t, s, ticket, commit2)

	ticket3 := pbGetTicket(t, s, ticket.ID)
	deps3 := pbClaim(t, s, runtime.NewFake(scripts), ticket.ID)
	commit3, err := (reviewingHandler{}).Run(t.Context(), ticket3, deps3)
	if err != nil {
		t.Fatalf("Run (route): %v", err)
	}
	if commit3.Next != stateJudging {
		t.Fatalf("commit3.Next = %q, want judging", commit3.Next)
	}
	if commit3.Reason != reasonReviewClean {
		t.Errorf("commit3.Reason = %q, want %q", commit3.Reason, reasonReviewClean)
	}
}

// ---- task 12: re-review, the loop gate, and review escalation retries ----

// reviewRoundScriptKey is reviewScriptKey generalized to any round (design
// section 6.7): "review/<round>-<lens>/1.xml", a round's own first turn --
// every lens round 2 or 3 selects here runs once, fresh, never resumed.
// Every test above this one drives round 1 only; the re-review tests below
// need round 2 and 3's own keys too.
func reviewRoundScriptKey(round int, lens string) string {
	return fmt.Sprintf("review/%d-%s/1.xml", round, lens)
}

// reReviewFixCmd stands in for the fix agent's own edit: CHECK re-runs the
// project's real test command (building.go), so the edit has to be real,
// not merely claimed by the scripted response below, the same technique
// postbuild_test.go's own pbFixTestCmd uses for hello.txt.
const reReviewFixCmd = "printf '\\n// reviewed\\n' >> greet.go && test -f greet.go"

// reReviewFixScript is the fix driver's own RUN turn (job "build", label
// "fix", design section 5.1): one claimed file change, greet.go, matching
// reReviewFixCmd's own edit, so CHECK's own cross-check of claimed against
// real changed paths agrees.
const reReviewFixScript = `<zing job="build" outcome="ok">
  <claims>
    <files_changed>
      <path>greet.go</path>
    </files_changed>
  </claims>
  <report>Reviewed and touched up greet.go.</report>
  <notes></notes>
</zing>`

// driveReviewFixToLanding drives an already-open fix request (FIXREQ's own
// "fix requested findings" marker, already applied) through the fix
// driver's own RUN then CHECK-and-LAND ticks (fix.go's own DriveFix,
// reached through reviewingHandler.Run's own postBuildPrelude): one tick
// reserves the fix's first run, the next checks it clean and lands it in
// the same commit (fix_test.go's own TestDriveFixLandWritesLandedMarker,
// package job_test, unreachable from here, proves that same two-tick
// shape). It stops as soon as a commit carries a "fix landed" marker, and
// fails if four ticks never produce one.
func driveReviewFixToLanding(t *testing.T, s *store.Store, ticketID int64, rt runtime.Runtime, testCmd string) {
	t.Helper()
	for i := range 4 {
		ticket := pbGetTicket(t, s, ticketID)
		deps := pbWithTestCmd(pbClaim(t, s, rt, ticketID), ticket, testCmd)
		commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("driveReviewFixToLanding: Run (step %d): %v", i, err)
		}
		pbApply(t, s, ticket, commit)
		for j := range commit.Messages {
			if strings.HasPrefix(commit.Messages[j].Body, "fix landed ") {
				return
			}
		}
	}
	t.Fatal("driveReviewFixToLanding: fix did not land after 4 ticks")
}

// ---- TestReReviewRunsSelectedLenses ----------------------------------------

// TestReReviewRunsSelectedLenses proves design section 13.1's own worked
// example end to end, adapted to this file's own fixture tree (greet.go
// and hello.txt standing in for 13.1's a.go and b.go): round 1 merges
// correctness and security's own two reports of greet.go:5 into one
// above-floor finding, accepted; quality's own above-floor finding at
// hello.txt:1 is discussed and withdrawn; the fix lands on greet.go; round
// 2 runs only correctness, security (greet.go changed, and the accepted
// finding's own row lists them, 6.7), and fidelity (always) -- quality is
// out (hello.txt never changed), and so is every lens round 1 never ran.
//
// This is also the task 12 handoff's own regression for lensesForRound's
// dedup (newestFindingRowPerID): round 1 ends with two rows for each
// finding id (the accepted one nil then accept; the discussed one nil then
// discuss), the exact duplicated-row shape a lensesForRound reading raw
// rows instead of the newest one per id would have to get right by luck.
func TestReReviewRunsSelectedLenses(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)
	scripts := reviewScriptsFS(map[string]string{
		reviewScriptKey("correctness", 1): findingScriptAt("correctness", "minor", greetGoLine5, "nil map write", "validate it"),
		reviewScriptKey(discussLens, 1):   findingScriptAt(discussLens, "major", greetGoLine5, "unchecked input", "validate it"),
		reviewScriptKey("quality", 1):     findingScriptAt("quality", "major", pbHelloTxt+":1", "breaks the build", "fix the build"),
	})
	rt := runtime.NewFake(scripts)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps) // ROUND 1
	if err != nil {
		t.Fatalf("Run (round 1): %v", err)
	}
	pbApply(t, s, ticket, commit)

	q := newestOpenQuestion(t, s, ticket.ID)
	var payload response.QuestionPayload
	if err = json.Unmarshal(q.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	acceptID := itemRefByText(t, payload, "unchecked input")
	discussID := itemRefByText(t, payload, "breaks the build")
	answerReviewItems(t, s, ticket.ID, q.ID, map[string]response.Decision{
		acceptID:  response.DecisionAccept,
		discussID: response.DecisionDiscuss,
	}, "hello.txt is generated, see the header")

	ticket1 := pbGetTicket(t, s, ticket.ID)
	deps1 := pbClaim(t, s, rt, ticket.ID)
	commit1, err := (reviewingHandler{}).Run(t.Context(), ticket1, deps1) // TRIAGE
	if err != nil {
		t.Fatalf("Run (triage): %v", err)
	}
	pbApply(t, s, ticket, commit1)

	scripts[reviewScriptKey("quality", 2)] = &fstest.MapFile{Data: []byte(reviewOKScript)}
	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2) // DISCUSS: quality withdraws
	if err != nil {
		t.Fatalf("Run (discuss): %v", err)
	}
	if len(commit2.Artifacts) != 0 {
		t.Errorf("commit2.Artifacts = %d, want 0 (quality withdrew)", len(commit2.Artifacts))
	}
	pbApply(t, s, ticket, commit2)

	ticket3 := pbGetTicket(t, s, ticket.ID)
	deps3 := pbClaim(t, s, rt, ticket.ID)
	commit3, err := (reviewingHandler{}).Run(t.Context(), ticket3, deps3) // decided round -> FIXREQ
	if err != nil {
		t.Fatalf("Run (fixreq): %v", err)
	}
	if len(commit3.Messages) != 1 || !strings.HasPrefix(commit3.Messages[0].Body, fixRequestedFindingsPrefix) {
		t.Fatalf("commit3.Messages = %+v, want one %q message", commit3.Messages, fixRequestedFindingsPrefix)
	}
	pbApply(t, s, ticket, commit3)

	scripts["build/fix/1.xml"] = &fstest.MapFile{Data: []byte(reReviewFixScript)}
	driveReviewFixToLanding(t, s, ticket.ID, rt, reReviewFixCmd)

	beforeRound2, err := s.MaxRunID(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	scripts[reviewRoundScriptKey(2, "correctness")] = &fstest.MapFile{Data: []byte(reviewOKScript)}
	scripts[reviewRoundScriptKey(2, discussLens)] = &fstest.MapFile{Data: []byte(reviewOKScript)}
	scripts[reviewRoundScriptKey(2, lensFidelity)] = &fstest.MapFile{Data: []byte(reviewOKScript)}

	ticket4 := pbGetTicket(t, s, ticket.ID)
	deps4 := pbClaim(t, s, rt, ticket.ID)
	commit4, err := (reviewingHandler{}).Run(t.Context(), ticket4, deps4) // ROUND 2
	if err != nil {
		t.Fatalf("Run (round 2): %v", err)
	}
	if commit4.Next != stateJudging {
		t.Fatalf("commit4.Next = %q, want %q (round 2 clean)", commit4.Next, stateJudging)
	}
	pbApply(t, s, ticket, commit4)

	runs := reviewRunsSince(t, s, ticket.ID, beforeRound2)
	if len(runs) != 3 {
		t.Fatalf("round 2 runs = %d, want 3", len(runs))
	}
	seen := make(map[string]bool, 3)
	for _, r := range runs {
		if r.Lens == nil {
			t.Fatalf("run %d: Lens = nil, want set", r.ID)
		}
		seen[*r.Lens] = true
	}
	for _, want := range []string{"correctness", discussLens, lensFidelity} {
		if !seen[want] {
			t.Errorf("round 2 never ran lens %s", want)
		}
	}
}

// ---- TestLoopGateMinorFindingsMoveToJudging --------------------------------

// TestLoopGateMinorFindingsMoveToJudging proves design section 6.8's own
// worked example, extended by issue #68: round 1 keeps a minor finding -- at
// or below the floor, so it routes straight to FIXREQ with no question --
// request 1; the fix lands but leaves the same defect; round 2 keeps it
// again, request 2; round 3 keeps it a third time, k = 2 = max_loops, and
// every finding left is still at or below the floor, so FIXREQ moves the
// ticket on to judging instead of escalating loops_exhausted, posting a
// message that lists the finding Zing let through unfixed.
func TestLoopGateMinorFindingsMoveToJudging(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	const loopLens = "quality"
	s, ticket, rt, scripts := driveReviewToCap(t)

	scripts[reviewRoundScriptKey(3, lensFidelity)] = &fstest.MapFile{Data: []byte(reviewOKScript)}
	scripts[reviewRoundScriptKey(3, loopLens)] = &fstest.MapFile{Data: []byte(findingScript(loopLens, "minor", "still not fixed", "add a comment"))}

	ticket3 := pbGetTicket(t, s, ticket.ID)
	deps3 := pbClaim(t, s, rt, ticket.ID)
	commit3, err := (reviewingHandler{}).Run(t.Context(), ticket3, deps3) // round 3: gate reached
	if err != nil {
		t.Fatalf("Run (round 3): %v", err)
	}
	if commit3.Escalation != nil {
		t.Fatalf("commit3.Escalation = %+v, want nil (every finding left is at or below the floor)", commit3.Escalation)
	}
	if commit3.Waiting != nil {
		t.Errorf("commit3.Waiting = %q, want nil", *commit3.Waiting)
	}
	if commit3.Next != stateJudging {
		t.Errorf("commit3.Next = %q, want %q", commit3.Next, stateJudging)
	}
	if commit3.Reason != reasonReviewAcceptedAtCap {
		t.Errorf("commit3.Reason = %q, want %q", commit3.Reason, reasonReviewAcceptedAtCap)
	}

	var acceptMsg *store.Message
	for i := range commit3.Messages {
		if strings.HasPrefix(commit3.Messages[i].Body, "Zing accepted ") {
			acceptMsg = &commit3.Messages[i]
		}
	}
	if acceptMsg == nil {
		t.Fatalf("commit3.Messages = %+v, want one starting %q", commit3.Messages, "Zing accepted ")
	}
	if !strings.HasPrefix(acceptMsg.Body, "Zing accepted one finding at the review fix loop cap") {
		t.Errorf("accept message = %q, want it to name one finding", acceptMsg.Body)
	}
	if !strings.Contains(acceptMsg.Body, "after 2 fix runs") {
		t.Errorf("accept message = %q, want it to name 2 fix runs", acceptMsg.Body)
	}
	if !strings.Contains(acceptMsg.Body, "- r3f1 minor ") || !strings.Contains(acceptMsg.Body, "still not fixed") {
		t.Errorf("accept message = %q, want the r3f1 minor finding listed", acceptMsg.Body)
	}

	var doneMsg *store.Message
	for i := range commit3.Messages {
		if strings.HasPrefix(commit3.Messages[i].Body, "review round 3 done") {
			doneMsg = &commit3.Messages[i]
		}
	}
	if doneMsg == nil {
		t.Fatalf("commit3.Messages = %+v, want round 3's own done marker alongside the accepted-findings message", commit3.Messages)
	}

	if len(commit3.Artifacts) != 1 {
		t.Fatalf("commit3.Artifacts = %+v, want exactly one (round 3's own r3f1 finding)", commit3.Artifacts)
	}
	var artifact response.FindingArtifact
	if err = json.Unmarshal(commit3.Artifacts[0].Payload, &artifact); err != nil {
		t.Fatalf("unmarshal commit3.Artifacts[0]: %v", err)
	}
	if artifact.ID != "r3f1" {
		t.Errorf("commit3.Artifacts[0] finding ID = %q, want %q", artifact.ID, "r3f1")
	}

	pbApply(t, s, ticket, commit3)

	final := pbGetTicket(t, s, ticket.ID)
	if final.State != stateJudging {
		t.Errorf("ticket state = %q, want %q", final.State, stateJudging)
	}
	if final.WaitingOn != nil {
		t.Errorf("ticket WaitingOn = %q, want nil", *final.WaitingOn)
	}
	rounds, err := s.AnsweredRounds(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("AnsweredRounds: %v", err)
	}
	if len(rounds) != 0 {
		t.Errorf("AnsweredRounds = %+v, want none (the gate asked the owner nothing)", rounds)
	}
}

// ---- TestReviewInfraRetryWritesMarker ---------------------------------------

// TestReviewInfraRetryWritesMarker proves design section 5.6's own "review,
// any other" retry row: a retry on an environment escalation of origin
// review writes the plain "retry requested" marker and resolves the round,
// exactly as every other job's own infra retry does.
func TestReviewInfraRetryWritesMarker(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)

	qID := pbEscalateDirect(t, s, ticket.ID, nil, nil, response.EscalationCodeEnvironment, response.EscalationOriginReview)
	pbAnswerEscalation(t, s, ticket.ID, qID, escalationChoiceRetry)

	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), ticket.ID)
	commit, handled := pbRunPrelude(t, s, deps, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(commit.Messages) != 1 || commit.Messages[0].Body != markerRetryRequested {
		t.Fatalf("commit.Messages = %+v, want one %q marker", commit.Messages, markerRetryRequested)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
	}
}

// ---- TestReviewLoopsRetryRequestsFix ----------------------------------------

// TestReviewLoopsRetryRequestsFix proves design section 5.6's own "review,
// loops_exhausted" retry row: a retry writes a fix request of kind findings
// straight from the escalation's own Tried text, bypassing FIXREQ's own
// max_loops gate entirely -- the one request 5.6 says to skip it for.
func TestReviewLoopsRetryRequestsFix(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, _ := reviewTicketReady(t)

	qID := pbEscalateDirect(t, s, ticket.ID, nil, nil, response.EscalationCodeLoopsExhausted, response.EscalationOriginReview)
	pbAnswerEscalation(t, s, ticket.ID, qID, escalationChoiceRetry)

	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), ticket.ID)
	commit, handled := pbRunPrelude(t, s, deps, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(commit.Messages) != 1 || !strings.HasPrefix(commit.Messages[0].Body, fixRequestedFindingsPrefix) {
		t.Fatalf("commit.Messages = %+v, want one %q message", commit.Messages, fixRequestedFindingsPrefix)
	}
	if !strings.Contains(commit.Messages[0].Body, "what was tried") {
		t.Errorf("fix request body = %q, want the escalation's own Tried text", commit.Messages[0].Body)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
	}
}

// ---- TestReviewCapResumesRetryAccepts ---------------------------------------

// TestReviewCapResumesRetryAccepts proves design section 5.6's own
// "cap_resumes, exhausted session of job review, from DISCUSS" row: once
// the discussed finding's own lens session is exhausted
// (TestDiscussExhaustedEscalatesOnce's own setup), a retry turns the still-
// pending finding into decision accept, in place -- same id, the owner's
// own notes appended to its fix text -- and resolves the escalation's own
// round.
func TestReviewCapResumesRetryAccepts(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, rt, scripts, findingID := discussGroupReady(t, "")
	scripts[reviewScriptKey(discussLens, 2)] = &fstest.MapFile{Data: []byte(reviewQuestionScript("Q1", "which way?"))}
	scripts[reviewScriptKey(discussLens, 3)] = &fstest.MapFile{Data: []byte(reviewQuestionScript("Q2", "which way now?"))}

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (discuss 1): %v", err)
	}
	pbApply(t, s, ticket, commit)
	answerReviewQuestion(t, s, ticket.ID, newestOpenQuestion(t, s, ticket.ID).ID)

	ticket = pbGetTicket(t, s, ticket.ID)
	deps = pbClaim(t, s, rt, ticket.ID)
	commit, err = (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (discuss 2): %v", err)
	}
	pbApply(t, s, ticket, commit)
	answerReviewQuestion(t, s, ticket.ID, newestOpenQuestion(t, s, ticket.ID).ID)

	ticket = pbGetTicket(t, s, ticket.ID)
	deps = pbClaim(t, s, rt, ticket.ID)
	commit, err = (reviewingHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run (cap): %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want set")
	}
	pbApply(t, s, ticket, commit)

	qID := newestOpenQuestion(t, s, ticket.ID).ID
	const note = "owner says accept it as is"
	option := escalationChoiceRetry
	if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticket.ID, QuestionID: &qID, Option: &option}); draftErr != nil {
		t.Fatalf("SaveDraft(option): %v", draftErr)
	}
	if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticket.ID, QuestionID: &qID, Text: note}); draftErr != nil {
		t.Fatalf("SaveDraft(text): %v", draftErr)
	}
	if _, sendErr := s.SendBatch(t.Context(), ticket.ID); sendErr != nil {
		t.Fatalf("SendBatch: %v", sendErr)
	}

	ticket = pbGetTicket(t, s, ticket.ID)
	deps = pbClaim(t, s, rt, ticket.ID)
	retryCommit, handled := pbRunPrelude(t, s, deps, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(retryCommit.Artifacts) != 1 {
		t.Fatalf("retryCommit.Artifacts = %+v, want exactly one (the pending finding, now accepted)", retryCommit.Artifacts)
	}
	var finding response.FindingArtifact
	if err := json.Unmarshal(retryCommit.Artifacts[0].Payload, &finding); err != nil {
		t.Fatalf("unmarshal finding artifact: %v", err)
	}
	if finding.ID != findingID {
		t.Errorf("finding.ID = %q, want %q", finding.ID, findingID)
	}
	if finding.Decision == nil || *finding.Decision != response.FindingAccept {
		t.Errorf("finding.Decision = %v, want accept", finding.Decision)
	}
	if !strings.Contains(finding.Fix, discussFix) || !strings.Contains(finding.Fix, note) {
		t.Errorf("finding.Fix = %q, want it to carry both the original fix text and the owner's own note", finding.Fix)
	}
	if len(retryCommit.ResolveQuestions) != 1 || retryCommit.ResolveQuestions[0] != qID {
		t.Errorf("retryCommit.ResolveQuestions = %v, want [%d]", retryCommit.ResolveQuestions, qID)
	}
}
