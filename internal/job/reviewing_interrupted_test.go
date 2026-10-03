// reviewing_interrupted_test.go tests design D5 (section 7.4) and D9 for
// the reviewing job: DISCUSS and CONTINUE each resume an interrupted lens
// session free, carrying the interrupted input, while ROUND (D9) re-runs
// the whole round fresh rather than resume any lens. It reuses
// reviewing_test.go's and postbuild_test.go's own shared fixtures
// (reviewTicketReady, reviewRunsSince, reviewScriptsFS, reviewScriptKey,
// discussGroupReady, answerReviewQuestion, recordingRuntime, pbClaim,
// pbApply, pbGetTicket) and drives the real reviewingHandler directly, as
// reviewing_test.go's own tests do.
package job

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"zing/internal/response"
	"zing/internal/runtime"
)

// TestReviewDiscussInterruptedResumeIsFree proves design section 7.4's
// review DISCUSS row: a discuss resume normally charges (today's
// hardcoded true), but one cut short by InterruptRuns resumes free on the
// next tick, carrying the interrupted input next to the finding and the
// owner's own note the pending group still carries.
func TestReviewDiscussInterruptedResumeIsFree(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, rt, scripts, findingID := discussGroupReady(t, "please check the error path again")
	scripts[reviewScriptKey(discussLens, 2)] = &fstest.MapFile{Data: []byte(reviewOKScript)}

	canceledRT := &pbScriptedRuntime{t: t, steps: []pbScriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, err: runtime.ErrCanceled},
	}}
	deps := pbClaim(t, s, canceledRT, ticket.ID)
	_, err := (reviewingHandler{}).Run(t.Context(), ticket, deps) // discuss: interrupted mid-flight
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	applied, interruptErr := s.InterruptRuns(t.Context(), ticket.ID, deps.Owner, deps.Expires)
	if interruptErr != nil {
		t.Fatalf("InterruptRuns: %v", interruptErr)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, rt, ticket.ID)                                // rt: the original recordingRuntime, its Fake session untouched
	commit, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2) // discuss again: free resume
	if err != nil {
		t.Fatalf("discuss resume: %v", err)
	}
	req := rt.lastRequest(t)
	if !strings.Contains(req.Prompt, findingID) {
		t.Errorf("resume prompt does not carry the finding id %q:\n%s", findingID, req.Prompt)
	}
	if !strings.Contains(req.Prompt, interruptedResumeText) {
		t.Errorf("resume prompt does not carry the interrupted input:\n%s", req.Prompt)
	}
	if commit.Session == nil || commit.Session.BumpResumes {
		t.Errorf("commit.Session = %+v, want BumpResumes=false (the resume was not charged)", commit.Session)
	}
}

// TestReviewContinueInterruptedResumeIsFree proves design section 7.4's
// review CONTINUE row: an answered asking lens's own resume normally
// charges (today's hardcoded true), but one cut short by InterruptRuns
// resumes free on the next tick, carrying the interrupted input next to
// the owner's own answers.
func TestReviewContinueInterruptedResumeIsFree(t *testing.T) {
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

	commit, err := (reviewingHandler{}).Run(t.Context(), ticket, deps) // round 1: fidelity asks
	if err != nil {
		t.Fatalf("Run (round): %v", err)
	}
	pbApply(t, s, ticket, commit)

	q := newestOpenQuestion(t, s, ticket.ID)
	answerReviewQuestion(t, s, ticket.ID, q.ID)

	// A scripted error alone is not enough to reproduce design section 7.2's
	// own interrupt path: tableCommit's ctx.Err() check only fires when the
	// handler's own ctx argument is genuinely cancelled (what runAndCommit's
	// real force-cancel does), not merely when a runtime call returns
	// runtime.ErrCanceled on its own. cancelAfterN (below) cancels the real
	// ctx itself before reporting the error, exactly as a real runtime does
	// when its own process observes the cancellation.
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	canceledRT := &cancelAfterN{n: 1, cancel: cancel2}
	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, canceledRT, ticket.ID)
	_, err = (reviewingHandler{}).Run(ctx2, ticket2, deps2) // continue: interrupted mid-flight
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	applied, interruptErr := s.InterruptRuns(t.Context(), ticket.ID, deps2.Owner, deps2.Expires)
	if interruptErr != nil {
		t.Fatalf("InterruptRuns: %v", interruptErr)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	rec := &recordingRuntime{inner: rt} // rt: the same Fake, its fidelity session still at turn 1
	ticket3 := pbGetTicket(t, s, ticket.ID)
	deps3 := pbClaim(t, s, rec, ticket.ID)
	commit3, err := (reviewingHandler{}).Run(t.Context(), ticket3, deps3) // continue again: free resume
	if err != nil {
		t.Fatalf("continue resume: %v", err)
	}
	if commit3.Next != stateJudging {
		t.Fatalf("commit3.Next = %q, want judging (fidelity's own free resume came back clean)", commit3.Next)
	}
	req := rec.lastRequest(t)
	if !strings.Contains(req.Prompt, interruptedResumeText) {
		t.Errorf("resume prompt does not carry the interrupted input:\n%s", req.Prompt)
	}
	pbApply(t, s, ticket, commit3)

	resumedRuns := reviewRunsSince(t, s, ticket.ID, before)
	var fidelityRuns int
	for _, r := range resumedRuns {
		if *r.Lens == lensFidelity {
			fidelityRuns++
		}
	}
	// Round 1's own first turn, the interrupted continue resume (reserved
	// but never terminalized by a commit), and this free resume: 3 runs,
	// only one of which (the free resume) ever actually charged.
	if fidelityRuns != 3 {
		t.Fatalf("fidelity runs since round 1 = %d, want 3 (first turn, the interrupted resume, and the free resume)", fidelityRuns)
	}
}

// cancelAfterN is a test-only runtime.Runtime for TestReviewRoundInterruptedRerunsFresh:
// it serves n-1 ok "finding-free" responses, then on its n'th call cancels
// the shared context (simulating the dispatcher's own force-cancel mid
// round, design section 7.2) and returns runtime.ErrCanceled, exactly as a
// real runtime does when its own ctx observes the cancellation. Every call
// after that also returns runtime.ErrCanceled at once, since its own ctx is
// now done.
type cancelAfterN struct {
	mu     sync.Mutex
	calls  int
	cancel context.CancelFunc
	n      int
}

func (c *cancelAfterN) Run(ctx context.Context, _ runtime.RunRequest) (runtime.RunResult, error) {
	if ctx.Err() != nil {
		return runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, runtime.ErrCanceled
	}
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	if n >= c.n {
		c.cancel()
		return runtime.RunResult{ExitCode: -1, AgentTime: time.Second}, runtime.ErrCanceled
	}
	return runtime.RunResult{
		Response:  &response.FindingsResponse{Job: response.JobReview, Outcome: response.OutcomeOk},
		SessionID: fmt.Sprintf("round-cancel-sess-%d", n), ExitCode: 0, AgentTime: time.Second,
	}, nil
}

// TestReviewRoundInterruptedRerunsFresh proves design D9: a ROUND cut
// short mid-fan-out (several lenses already answered ok, one cancelled,
// the rest never reached) commits nothing at all -- tableCommit's own
// ctx.Err() check discards every attempt, finished or not, the instant the
// dispatcher's own context is cancelled -- so InterruptRuns is the only
// thing that terminalizes those reserved-but-uncommitted runs, and the
// next tick re-enters ROUND exactly as if nothing had run: no "review
// round" marker exists yet, so every one of the seven lenses runs again
// fresh.
func TestReviewRoundInterruptedRerunsFresh(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticket, before := reviewTicketReady(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rt := &cancelAfterN{n: 3, cancel: cancel}
	deps := pbClaim(t, s, rt, ticket.ID)
	deps.LensesParallel = 1 // serial, so cancelAfterN's own call order is deterministic

	_, err := (reviewingHandler{}).Run(ctx, ticket, deps) // round 1: interrupted partway through
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	markers, err := s.MarkersWithPrefix(t.Context(), ticket.ID, reviewRoundMarkerPrefix)
	if err != nil {
		t.Fatalf("MarkersWithPrefix: %v", err)
	}
	if len(markers) != 0 {
		t.Fatalf("review round markers after the interrupt = %+v, want none (nothing committed)", markers)
	}

	interruptedRuns := reviewRunsSince(t, s, ticket.ID, before)
	if len(interruptedRuns) == 0 {
		t.Fatal("no lens runs were reserved before the interrupt")
	}

	applied, interruptErr := s.InterruptRuns(t.Context(), ticket.ID, deps.Owner, deps.Expires)
	if interruptErr != nil {
		t.Fatalf("InterruptRuns: %v", interruptErr)
	}
	if !applied {
		t.Fatal("InterruptRuns: applied = false, want true")
	}

	lastInterruptedID := interruptedRuns[len(interruptedRuns)-1].ID
	ticket2 := pbGetTicket(t, s, ticket.ID)
	deps2 := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), ticket.ID) // every lens ok, round 1, turn 1
	commit2, err := (reviewingHandler{}).Run(t.Context(), ticket2, deps2)    // round 1 again: must run fresh
	if err != nil {
		t.Fatalf("Run (fresh round): %v", err)
	}
	if commit2.Next != stateJudging {
		t.Fatalf("commit2.Next = %q, want judging (a clean fresh round)", commit2.Next)
	}
	pbApply(t, s, ticket2, commit2)

	freshRuns := reviewRunsSince(t, s, ticket.ID, lastInterruptedID)
	if len(freshRuns) != len(reviewLensNames) {
		t.Fatalf("fresh round runs = %d, want %d (every lens re-runs fresh, none resumed)", len(freshRuns), len(reviewLensNames))
	}
	for _, r := range freshRuns {
		if r.Turn != 0 {
			t.Errorf("fresh run (lens %v) turn = %d, want 0 (a brand new session, not a resume)", r.Lens, r.Turn)
		}
	}
}
