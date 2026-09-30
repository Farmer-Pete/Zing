// reviewing_test.go tests task 10: ROUND (design section 6.2), ASKED and
// CONTINUE (6.2a), and FIXREQ (6.8), through the real reviewingHandler.
// It reuses postbuild_test.go's own harness (newPostbuildTestStore, pbClaim,
// pbTicketInReviewing, pbApply, pbGetTicket, pbMachine), package job
// (unreachable from job_test), plus its own small fixture-script builder: a
// review round's seven lens turns, served from an in-memory fs.FS rather
// than the checked-in fixtures/scripts/review tree (selftest's and the
// console e2e's own demo, task 10's Files list), so each test scripts
// exactly the lens outcomes it needs.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// reviewLensNames is machine.toml's own jobs.review.lenses, spelled out
// literally: reviewing_test.go builds its own in-memory scripts keyed by
// these names, and a handful of tests need the plain list to build a
// tailored override.
var reviewLensNames = []string{
	"simplification", "correctness", "security", lensFidelity, "tests", "quality", "observability",
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
	return fmt.Sprintf(`<zing job="review" outcome="ok">
<finding lens="%s" severity="%s" location="%s">
<text>%s</text>
<fix>%s</fix>
</finding>
</zing>`, lens, severity, greetGoLine5, text, fix)
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

// ---- TestRoundCleanMovesToJudging -------------------------------------------

func TestRoundCleanMovesToJudging(t *testing.T) {
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
	if err := json.Unmarshal(open.Payload, &payload); err != nil {
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
// assumption about which lens wins that initial race.
type firstCallFailsRuntime struct {
	inner runtime.Runtime
	calls atomic.Int32
}

func (r *firstCallFailsRuntime) Run(ctx context.Context, req runtime.RunRequest) (runtime.RunResult, error) {
	if r.calls.Add(1) == 1 {
		return runtime.RunResult{SessionID: "first-call-fails", ExitCode: 1, AgentTime: time.Second},
			&runtime.InvalidOutputError{Reason: "not well-formed"}
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
	s, ticket, _ := reviewTicketReady(t)
	invalidFn := func() (runtime.RunResult, error) {
		return runtime.RunResult{SessionID: "invalid-sess", ExitCode: 1, AgentTime: time.Second},
			&runtime.InvalidOutputError{Reason: "not well-formed"}
	}
	rt := &labelResultRuntime{
		inner:   runtime.NewFake(reviewScriptsFS(nil)),
		results: map[string]func() (runtime.RunResult, error){"1-simplification": invalidFn},
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
	if commit2.Escalation.Payload.Code != string(response.EscalationCodeResponseInvalid) {
		t.Errorf("escalation code = %q, want %q", commit2.Escalation.Payload.Code, response.EscalationCodeResponseInvalid)
	}
	if commit2.Escalation.Payload.What != lensFailedTwiceWhat {
		t.Errorf("escalation What = %q, want %q", commit2.Escalation.Payload.What, lensFailedTwiceWhat)
	}
	if commit2.Escalation.Payload.Origin != string(response.EscalationOriginReview) {
		t.Errorf("escalation origin = %q, want %q", commit2.Escalation.Payload.Origin, response.EscalationOriginReview)
	}
}

// ---- TestRoundLensQuestionPostsAndStops ---------------------------------------

// TestRoundLensQuestionPostsAndStops proves design section 6.2a's own ASKED
// row: one lens asking, every other lens ok, posts that lens's own question
// with its run's own id and waits on "questions", without advancing state.
func TestRoundLensQuestionPostsAndStops(t *testing.T) {
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

// ---- TestContinueCapExhaustedEscalates -------------------------------------------

// TestContinueCapExhaustedEscalates proves design section 6.2a step 2's own
// cap gate: jobs.review.max_resumes is 2, so a third resume attempt on the
// same asking session finds it exhausted and escalates resumes_exhausted,
// origin cap_resumes, with no run started at all.
func TestContinueCapExhaustedEscalates(t *testing.T) {
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
