// building_escalation_test.go tests task 13: design section 6.9's own
// escalation resolution for the building handler -- entered before step 0
// (design section 6.1), it resolves a retry, a back/reply-only choice, or
// an abandon against a building escalation's own origin (build, perimeter,
// fix, cap_resumes, cap_budget) and code (sandbox_unavailable overrides
// every origin's own row). cap_budget's retry row (#25, retryCapBudget) no
// longer re-escalates unconditionally: CapBudgetRetry covers the room case
// (the marker, no re-escalation) and CapBudgetRetryStillOverBudget covers
// the still-exhausted case (re-escalate wall_clock, as before).
//
// It reuses skeleton_test.go, building_test.go, and escalation_test.go's
// shared fixtures (newJobTestStore, claim, apply, getTicket,
// buildTicketInBuilding, claimForBuild, buildWorktreeFor, perimeterScenario,
// describeTick, perimeterStep, findOpenQuestionByKind,
// answerPerimeterQuestion, bumpResumesToCap, testExtraPath, testExtraReason,
// helloTxt, buildStep, escalateDirect, answerGateQuestion,
// reserveTerminalRun, recordingRuntime, assertFenced, testStateAbandoned)
// and drives job.Registry()["building"].Run directly, exactly as
// building_test.go's own tests do.
//
// Most cases here construct their escalation directly through escalateDirect
// (a store-level HandlerCommit.Escalation), the same accepted alternative
// escalation_test.go's own file header describes: this file's scope is
// resolution, and every upstream trigger already has its own coverage
// elsewhere. TestCapResumesRetryCarriesAnswers instead drives the real
// upstream flow (a real DESCRIBE/ASK/RESOLVE chain against a git-backed
// worktree), because it specifically needs a real rejected file in a real
// tree for the revert and the perimeter notice to mean anything.
package job_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"zing/internal/gitfixture"
	"zing/internal/job"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// testMarkerRetryRequested is building.go's own unexported marker text
// (job.markerRetryRequested), design section 6.9's own retry marker.
const testMarkerRetryRequested = "retry requested"

// testForeignLineGreet is foreignTaskPaths' own error line for greet.go
// (plan #51's own fixture plan, fixtures/scripts/planning/2.xml, gives it
// only to task 2): shared across building_test.go and this file's own
// TestFileGrant_ tests so goconst sees one definition, not several copies.
const testForeignLineGreet = "claims/files_changed: greet.go belongs to task 2, not task 1"

// runBuilding runs job.Registry()["building"] once against deps built for
// ticketID, mirroring planning_test.go's own runPlanning.
func runBuilding(t *testing.T, s *store.Store, deps job.Deps, ticketID int64) (store.HandlerCommit, error) {
	t.Helper()
	ticket := getTicket(t, s, ticketID)
	return job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps)
}

// ---- build/fix origin: with a run retries fresh, without one just marks --

// TestBuildingEscalationTable proves design section 6.9's own table, one
// subtest per row this file does not already give its own dedicated test:
// the cap_resumes row is TestCapResumesRetryCarriesAnswers, the
// sandbox_unavailable code override is TestSandboxUnavailableEscalates, the
// back/reply-only row is TestReplanUnsupportedText, and the abandon row is
// TestBuildingAbandonResolvesAll.
func TestBuildingEscalationTable(t *testing.T) {
	t.Parallel()
	t.Run("BuildRetryWithRun", func(t *testing.T) {
		t.Parallel()
		// "a retry | build, fix with a run": RUN first turn of the same
		// unit, fresh session, inputs notes and error (fenced), resolving
		// the round.
		s, _, ticketID := buildTicketInBuilding(t)
		runID, sessID := reserveTerminalRun(t, s, ticketID, "build", true)
		qID := escalateDirect(t, s, ticketID, &runID, &sessID, response.EscalationCodeRuntimeExecFailed, response.EscalationOriginBuild)
		answerGateQuestion(t, s, ticketID, qID, new("a"), "please look again")

		scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, nil, "retry-fresh-sess")}}
		rec := &recordingRuntime{rt: scriptRT}
		commit, err := runBuilding(t, s, claimForBuild(t, s, rec, ticketID), ticketID)
		if err != nil {
			t.Fatalf("escalation resolve (build retry with run) Run: %v", err)
		}
		if rec.lastReq.SessionID != "" {
			t.Errorf("RunRequest.SessionID = %q, want empty (a fresh session)", rec.lastReq.SessionID)
		}
		if commit.Session == nil || commit.Session.ID == nil || *commit.Session.ID == sessID {
			t.Fatalf("commit.Session = %+v, want a freshly reserved session, not %d", commit.Session, sessID)
		}
		assertFenced(t, rec.lastReq.Prompt, "notes", "please look again")
		assertFenced(t, rec.lastReq.Prompt, "error", "what happened")
		if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
			t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
		}
		if len(commit.Artifacts) != 1 {
			t.Errorf("commit.Artifacts = %+v, want one fresh build_report", commit.Artifacts)
		}
	})

	t.Run("BuildRetryNoRun", func(t *testing.T) {
		t.Parallel()
		// "a retry | build with no run (step 0, CHECK, LAND, RESOLVE
		// failures)": resolve the round, commit the marker "retry
		// requested", stay; no runtime call.
		s, rt, ticketID := buildTicketInBuilding(t)
		qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeEnvironment, response.EscalationOriginBuild)
		answerGateQuestion(t, s, ticketID, qID, new("a"), "")

		commit, err := runBuilding(t, s, claimForBuild(t, s, rt, ticketID), ticketID)
		if err != nil {
			t.Fatalf("escalation resolve (build retry, no run) Run: %v", err)
		}
		if len(commit.Messages) != 1 || commit.Messages[0].Body != testMarkerRetryRequested {
			t.Fatalf("commit.Messages = %+v, want one \"retry requested\" marker", commit.Messages)
		}
		if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
			t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
		}
		if commit.Escalation != nil || commit.Next != "" {
			t.Errorf("commit = %+v, want a plain marker commit, no escalation, no transition", commit)
		}
	})

	t.Run("PerimeterRetry", func(t *testing.T) {
		t.Parallel()
		// "a retry | perimeter": resolve the round, commit the marker
		// "retry requested", stay; the next tick's DESCRIBE retakes the
		// same path on its own, so no runtime call happens here either.
		s, rt, ticketID := buildTicketInBuilding(t)
		qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeEnvironment, response.EscalationOriginPerimeter)
		answerGateQuestion(t, s, ticketID, qID, new("a"), "")

		commit, err := runBuilding(t, s, claimForBuild(t, s, rt, ticketID), ticketID)
		if err != nil {
			t.Fatalf("escalation resolve (perimeter retry) Run: %v", err)
		}
		if len(commit.Messages) != 1 || commit.Messages[0].Body != testMarkerRetryRequested {
			t.Fatalf("commit.Messages = %+v, want one \"retry requested\" marker", commit.Messages)
		}
		if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
			t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
		}
	})

	t.Run("CapBudgetRetry", func(t *testing.T) {
		t.Parallel()
		// "a retry | cap_budget | retryCapBudget, budget has room": resolve
		// the round, commit the marker "retry requested"; no re-escalation,
		// no runtime call.
		s, _, ticketID := buildTicketInBuilding(t)
		qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeWallClock, response.EscalationOriginCapBudget)
		answerGateQuestion(t, s, ticketID, qID, new("a"), "")

		commit, err := runBuilding(t, s, claimForBuild(t, s, &scriptedRuntime{t: t}, ticketID), ticketID)
		if err != nil {
			t.Fatalf("escalation resolve (cap_budget retry) Run: %v", err)
		}
		if commit.Escalation != nil || commit.Next != "" {
			t.Errorf("commit = %+v, want a plain marker commit, no escalation, no transition", commit)
		}
		if commit.Waiting != nil {
			t.Errorf("commit.Waiting = %v, want nil", commit.Waiting)
		}
		if len(commit.Messages) != 1 || commit.Messages[0].Body != testMarkerRetryRequested {
			t.Fatalf("commit.Messages = %+v, want one %q marker", commit.Messages, testMarkerRetryRequested)
		}
		if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
			t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
		}
	})

	t.Run("CapBudgetRetryStillOverBudget", func(t *testing.T) {
		t.Parallel()
		// "a retry | cap_budget | retryCapBudget, spent agent seconds still
		// meet d.Budget": re-escalate wall_clock in this same commit,
		// resolving the round; no runtime call.
		s, _, ticketID := buildTicketInBuilding(t)
		qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeWallClock, response.EscalationOriginCapBudget)
		answerGateQuestion(t, s, ticketID, qID, new("a"), "")

		deps := claimForBuild(t, s, &scriptedRuntime{t: t}, ticketID)
		deps.Budget = 0 // still exhausted: retryCapBudget's own re-escalate branch

		commit, err := runBuilding(t, s, deps, ticketID)
		if err != nil {
			t.Fatalf("escalation resolve (cap_budget retry, still over budget) Run: %v", err)
		}
		if commit.Escalation == nil {
			t.Fatal("commit.Escalation is nil, want a re-escalated wall_clock")
		}
		if commit.Escalation.Payload.Code != string(response.EscalationCodeWallClock) ||
			commit.Escalation.Payload.Origin != string(response.EscalationOriginCapBudget) {
			t.Errorf("payload = %+v, want (wall_clock, cap_budget)", commit.Escalation.Payload)
		}
		if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
			t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
		}
	})

	t.Run("CapBudgetRaise", func(t *testing.T) {
		t.Parallel()
		// "d raise | cap_budget | retryCapBudget, raiseMinutes ==
		// budgetRaiseMinutes": resolve the round, commit the marker "retry
		// requested" plus one budget_raised event; no re-escalation, no
		// runtime call. Before task 3 this choice fell to the choice !=
		// escalationChoiceRetry row and re-escalated replan_unsupported.
		s, _, ticketID := buildTicketInBuilding(t)
		qID := escalateCapBudgetRaise(t, s, ticketID)
		answerGateQuestion(t, s, ticketID, qID, new("d"), "")

		commit, err := runBuilding(t, s, claimForBuild(t, s, &scriptedRuntime{t: t}, ticketID), ticketID)
		if err != nil {
			t.Fatalf("escalation resolve (cap_budget raise) Run: %v", err)
		}
		if commit.Escalation != nil || commit.Next != "" {
			t.Errorf("commit = %+v, want a plain marker commit, no escalation, no transition", commit)
		}
		if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
			t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
		}
		if len(commit.Messages) != 2 {
			t.Fatalf("commit.Messages = %+v, want a marker plus one budget_raised event", commit.Messages)
		}
		if commit.Messages[0].Body != testMarkerRetryRequested {
			t.Errorf("commit.Messages[0].Body = %q, want %q", commit.Messages[0].Body, testMarkerRetryRequested)
		}
		ev := commit.Messages[1]
		if ev.EventKind == nil || *ev.EventKind != store.EventKindBudgetRaised {
			t.Fatalf("commit.Messages[1].EventKind = %v, want %q", ev.EventKind, store.EventKindBudgetRaised)
		}
		var payload response.BudgetRaisedEvent
		if err = json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("unmarshal budget_raised payload: %v", err)
		}
		if payload.Minutes != 60 {
			t.Errorf("payload.Minutes = %d, want 60", payload.Minutes)
		}
	})
}

// TestSandboxUnavailableEscalates proves design section 6.9's own code
// override: "a retry | code sandbox_unavailable | as build with no run",
// regardless of the escalation's own origin -- here perimeter, to prove the
// code, not the origin, decides.
func TestSandboxUnavailableEscalates(t *testing.T) {
	t.Parallel()
	s, rt, ticketID := buildTicketInBuilding(t)
	qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeSandboxUnavailable, response.EscalationOriginPerimeter)
	answerGateQuestion(t, s, ticketID, qID, new("a"), "")

	commit, err := runBuilding(t, s, claimForBuild(t, s, rt, ticketID), ticketID) // no runtime call
	if err != nil {
		t.Fatalf("escalation resolve (sandbox_unavailable retry) Run: %v", err)
	}
	if len(commit.Messages) != 1 || commit.Messages[0].Body != testMarkerRetryRequested {
		t.Fatalf("commit.Messages = %+v, want one \"retry requested\" marker", commit.Messages)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
	}
}

// TestReplanUnsupportedText proves design D14's own row: choice b (back to
// planning) and a reply with no option at all both re-escalate
// replan_unsupported, origin unchanged, with the plan's exact What/Why
// text, resolving the round. #47 item 2 fixed escalateTx to stop offering
// "b" once the ticket is past planning, so a freshly raised escalation can
// no longer be answered with it, nor default to it (roundRecommendedOption,
// planning.go): both cases here instead answer one of the escalations the
// database already carried before that fix shipped (legacyEscalationQuestion,
// stored Recommended "b"), proving this unchanged resolution row still runs
// correctly against that still-real shape -- "ReplyOnly" proves
// roundRecommendedOption reads the stored recommendation back rather than
// assuming it, since a fresh escalation's own stored recommendation would
// resolve differently (TestEscalationReplyOnlyPostSealDefaultsToRetry).
func TestReplanUnsupportedText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		option *string
	}{
		{"Back", new("b")},
		{testCaseReplyOnly, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, rt, ticketID := buildTicketInBuilding(t)
			qID := legacyEscalationQuestion(t, s, ticketID, response.EscalationCodeEnvironment, response.EscalationOriginBuild)
			answerGateQuestion(t, s, ticketID, qID, tc.option, "let's replan instead")

			commit, err := runBuilding(t, s, claimForBuild(t, s, rt, ticketID), ticketID)
			if err != nil {
				t.Fatalf("escalation resolve (%s) Run: %v", tc.name, err)
			}
			if commit.Escalation == nil {
				t.Fatal("commit.Escalation = nil, want replan_unsupported")
			}
			p := commit.Escalation.Payload
			if p.Code != string(response.EscalationCodeReplanUnsupported) {
				t.Errorf("payload.Code = %q, want replan_unsupported", p.Code)
			}
			if p.Origin != string(response.EscalationOriginBuild) {
				t.Errorf("payload.Origin = %q, want build (unchanged)", p.Origin)
			}
			const wantWhat = "replanning after the build started is not built yet; retry or abandon"
			if p.What != wantWhat {
				t.Errorf("payload.What = %q, want %q", p.What, wantWhat)
			}
			const wantWhy = "the owner chose back to planning on a building ticket"
			if p.Why != wantWhy {
				t.Errorf("payload.Why = %q, want %q", p.Why, wantWhy)
			}
			if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
				t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
			}
		})
	}
}

// TestEscalationReplyOnlyPostSealDefaultsToRetry proves roundRecommendedOption
// (planning.go, #47 follow-up): a text-only reply (no option at all) on a
// freshly raised post-seal escalation resolves as Retry, not back to
// planning -- its own stored Recommended is "a" (escalationOptionsFor,
// store/commit.go, since back to planning is never offered post-seal any
// more), and roundChoice now reads that back instead of hardcoding "b".
// Same shape as BuildRetryNoRun (TestBuildingEscalationTable): resolve the
// round, commit the marker "retry requested", stay; no runtime call, no
// re-escalation.
func TestEscalationReplyOnlyPostSealDefaultsToRetry(t *testing.T) {
	t.Parallel()
	s, rt, ticketID := buildTicketInBuilding(t)
	qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeEnvironment, response.EscalationOriginBuild)
	answerGateQuestion(t, s, ticketID, qID, nil, "no option, just a note")

	commit, err := runBuilding(t, s, claimForBuild(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (reply-only, post-seal) Run: %v", err)
	}
	if commit.Escalation != nil {
		t.Errorf("commit.Escalation = %+v, want nil (no re-escalation)", commit.Escalation)
	}
	if len(commit.Messages) != 1 || commit.Messages[0].Body != testMarkerRetryRequested {
		t.Fatalf("commit.Messages = %+v, want one %q marker", commit.Messages, testMarkerRetryRequested)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
	}
}

// TestBuildingAbandonResolvesAll proves design section 6.9's choice c row:
// the ticket transitions straight to abandoned with no runtime call, every
// open or answered question resolves (ResolveAll), and job.ValidateCommit
// (apply's own check) accepts the building -> abandoned edge.
func TestBuildingAbandonResolvesAll(t *testing.T) {
	t.Parallel()
	s, rt, ticketID := buildTicketInBuilding(t)
	qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeEnvironment, response.EscalationOriginBuild)
	answerGateQuestion(t, s, ticketID, qID, new("c"), "I'm done with this one")

	commit, err := runBuilding(t, s, claimForBuild(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (abandon) Run: %v", err)
	}
	if commit.Next != testStateAbandoned {
		t.Fatalf("commit.Next = %q, want abandoned", commit.Next)
	}
	if !commit.ResolveAll {
		t.Error("commit.ResolveAll = false, want true")
	}

	apply(t, s, getTicket(t, s, ticketID), commit) // apply calls job.ValidateCommit: proves building -> abandoned is legal

	final := getTicket(t, s, ticketID)
	if final.State != testStateAbandoned {
		t.Errorf("final ticket state = %q, want abandoned", final.State)
	}
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 0 {
		t.Fatalf("QuestionsByState(open) after abandon = %v, %v, want none", open, err)
	}
	answered, err := s.QuestionsByState(t.Context(), ticketID, "answered")
	if err != nil || len(answered) != 0 {
		t.Fatalf("QuestionsByState(answered) after abandon = %v, %v, want none", answered, err)
	}
}

// TestCapResumesRetryCarriesAnswers proves design section 6.9's cap_resumes
// retry row for a build session with a preserved perimeter round (its own
// intro sentence: "the escalated session's job ... is read from
// EscalationPayload.SessionID", and the row's own "a preserved round of
// kind perimeter is applied here" sentence): it continues exactly where
// TestExhaustedKeepsRejectedDecision (task 12) leaves off. Once the owner
// retries a resumes_exhausted escalation already recorded for the build
// session (answerResume's own legacy branch, job.go), the fresh session's
// own prompt carries the perimeter notice raw (not fenced: design section
// 6.3's own resume input table), the rejected path is actually reverted
// from the tree, and both the escalation round and the preserved perimeter
// round resolve.
func TestCapResumesRetryCarriesAnswers(t *testing.T) {
	t.Parallel()
	s, ticketID, rid, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})
	scriptRT.steps = append(scriptRT.steps, perimeterStep("Adds a small helper.", "perim-sess-1"))
	describeTick(t, s, scriptRT, ticketID) // DESCRIBE + ASK

	q := findOpenQuestionByKind(t, s, ticketID, response.QuestionKindPerimeter)
	perimQID := q.ID
	answerPerimeterQuestion(t, s, ticketID, q.ID, map[string]response.Decision{testExtraPath: response.DecisionReject})

	const maxResumes = 3
	sess, _, err := s.LatestSession(t.Context(), ticketID, "build", maxResumes)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	owner := "cap-retry-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	bumpResumesToCap(t, s, ticketID, sess.ID, maxResumes, owner, expires)

	escQID := escalateDirect(t, s, ticketID, nil, &sess.ID, response.EscalationCodeResumesExhausted, response.EscalationOriginCapResumes)
	answerGateQuestion(t, s, ticketID, escQID, new("a"), "please retry")

	scriptRT.steps = append(scriptRT.steps, buildStep([]string{helloTxt}, nil, "cap-retry-fresh-sess"))
	rec := &recordingRuntime{rt: scriptRT}
	ticket := getTicket(t, s, ticketID)
	deps2 := claimForBuild(t, s, rec, ticketID)
	retryCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps2) // cap_resumes retry
	if err != nil {
		t.Fatalf("cap_resumes retry Run: %v", err)
	}
	if rec.lastReq.SessionID != "" {
		t.Errorf("RunRequest.SessionID = %q, want empty (a fresh session)", rec.lastReq.SessionID)
	}
	if !containsAll(rec.lastReq.Prompt, "perimeter:\n", testExtraPath, "were reverted") {
		t.Errorf("prompt does not carry the perimeter notice for %q:\n%s", testExtraPath, rec.lastReq.Prompt)
	}
	if len(retryCommit.ResolveQuestions) != 2 {
		t.Fatalf("commit.ResolveQuestions = %v, want 2 (the escalation round and the preserved perimeter round)", retryCommit.ResolveQuestions)
	}
	for _, want := range []int64{escQID, perimQID} {
		found := false
		for _, id := range retryCommit.ResolveQuestions {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Errorf("commit.ResolveQuestions = %v, want it to include %d", retryCommit.ResolveQuestions, want)
		}
	}
	apply(t, s, ticket, retryCommit)

	_, wt := buildWorktreeFor(t, deps2, getTicket(t, s, ticketID))
	if _, statErr := os.Stat(filepath.Join(wt.Dir(), testExtraPath)); statErr == nil {
		t.Errorf("stat %s after the cap retry succeeded, want it gone (reverted)", testExtraPath)
	} else if !os.IsNotExist(statErr) {
		t.Errorf("stat %s after the cap retry: %v, want a not-exist error", testExtraPath, statErr)
	}

	rounds, err := s.AnsweredRounds(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("AnsweredRounds: %v", err)
	}
	for _, r := range rounds {
		if r.RunID != nil && *r.RunID == rid {
			t.Errorf("AnsweredRounds after the cap retry = %+v, still has run %d's round, want it resolved", rounds, rid)
		}
	}
}

// TestTaskEscalationOriginStillBuild is TestFixEscalationOriginFix's own
// mirror (fix_test.go, design section 5.4 change 2, #28 gap 2): the same
// six shared-step call sites, driven for a task unit instead of a fix
// unit, still escalate origin "build" now that unitEscalation and
// originFor derive it from the unit (u.TaskN != 0) rather than the
// constant every one of these call sites hardcoded before task 2.
func TestTaskEscalationOriginStillBuild(t *testing.T) {
	t.Parallel()
	assertBuildOrigin := func(t *testing.T, commit store.HandlerCommit) {
		t.Helper()
		if commit.Escalation == nil {
			t.Fatal("commit.Escalation = nil, want an escalation")
		}
		if commit.Escalation.Payload.Origin != string(response.EscalationOriginBuild) {
			t.Errorf("escalation origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginBuild)
		}
	}

	t.Run("CHECK command failure", func(t *testing.T) {
		t.Parallel()
		s, rt, ticketID := buildTicketInBuilding(t)
		deps := claimForBuild(t, s, rt, ticketID)
		commit, err := runBuilding(t, s, deps, ticketID) // RUN task 1
		if err != nil {
			t.Fatalf("RUN: %v", err)
		}
		apply(t, s, getTicket(t, s, ticketID), commit)

		deps2 := claimForBuild(t, s, rt, ticketID)
		deps2.RequireSandbox = true
		deps2.Commands = job.NewCommandRunner(sandbox.Off(), true)
		checkCommit, err := runBuilding(t, s, deps2, ticketID) // CHECK: sandbox unavailable
		if err != nil {
			t.Fatalf("CHECK: %v", err)
		}
		assertBuildOrigin(t, checkCommit)
		if checkCommit.Escalation.Payload.Code != string(response.EscalationCodeSandboxUnavailable) {
			t.Errorf("escalation code = %q, want %q", checkCommit.Escalation.Payload.Code, response.EscalationCodeSandboxUnavailable)
		}
	})

	t.Run("LAND signing failure", func(t *testing.T) {
		t.Parallel()
		s, rt, ticketID := buildTicketInBuilding(t)
		deps := claimForBuild(t, s, rt, ticketID)
		commit, err := runBuilding(t, s, deps, ticketID) // RUN task 1 (the fake runtime writes hello.txt)
		if err != nil {
			t.Fatalf("RUN: %v", err)
		}
		apply(t, s, getTicket(t, s, ticketID), commit)

		deps2 := claimForBuild(t, s, rt, ticketID)
		_, wt := buildWorktreeFor(t, deps2, getTicket(t, s, ticketID))
		badKey := filepath.Join(t.TempDir(), "no-such-signing-key")
		if out, cfgErr := gitfixture.Git(t.Context(), wt.Dir(), "config", "user.signingKey", badKey); cfgErr != nil {
			t.Fatalf("git config user.signingKey: %v: %s", cfgErr, out)
		}

		landCommit, err := runBuilding(t, s, deps2, ticketID) // CHECK, clean, LAND: signing fails
		if err != nil {
			t.Fatalf("CHECK+LAND: %v", err)
		}
		assertBuildOrigin(t, landCommit)
		const wantWhat = "commit signing failed"
		if !strings.Contains(landCommit.Escalation.Body, wantWhat) {
			t.Errorf("escalation body = %q, want it to contain %q", landCommit.Escalation.Body, wantWhat)
		}
	})

	t.Run("DESCRIBE unclaimed extra", func(t *testing.T) {
		t.Parallel()
		s, ticketID, _, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})

		ticket := getTicket(t, s, ticketID)
		deps := claimForBuild(t, s, scriptRT, ticketID)
		_, wt := buildWorktreeFor(t, deps, ticket)
		const surprisePath = "aaa_surprise.go" // sorts before testExtraPath ("extra1.go"): the first undescribed extra
		if writeErr := os.WriteFile(filepath.Join(wt.Dir(), surprisePath), []byte("surprise\n"), 0o600); writeErr != nil {
			t.Fatalf("write %s: %v", surprisePath, writeErr)
		}

		commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // DESCRIBE: unclaimed extra
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertBuildOrigin(t, commit)
	})

	t.Run("RESOLVE revert failure", func(t *testing.T) {
		t.Parallel()
		s, ticketID, _, scriptRT := perimeterScenario(t, map[string]string{testExtraPath: testExtraReason})
		scriptRT.steps = append(scriptRT.steps, perimeterStep("Adds a small helper.", "task-resolve-perim-sess"))
		describeTick(t, s, scriptRT, ticketID) // DESCRIBE + ASK

		q := findOpenQuestionByKind(t, s, ticketID, response.QuestionKindPerimeter)
		answerPerimeterQuestion(t, s, ticketID, q.ID, map[string]response.Decision{testExtraPath: response.DecisionReject})

		ticket := getTicket(t, s, ticketID)
		deps := claimForBuild(t, s, scriptRT, ticketID)
		_, wt := buildWorktreeFor(t, deps, ticket)
		if chmodErr := os.Chmod(wt.Dir(), 0o555); chmodErr != nil {
			t.Fatalf("chmod worktree dir: %v", chmodErr)
		}
		t.Cleanup(func() {
			if chmodErr := os.Chmod(wt.Dir(), 0o755); chmodErr != nil {
				t.Logf("restore worktree dir permissions: %v", chmodErr)
			}
		})

		resolveCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RESOLVE: revert fails
		if err != nil {
			t.Fatalf("RESOLVE: %v", err)
		}
		assertBuildOrigin(t, resolveCommit)
	})

	t.Run("build run error outcome", func(t *testing.T) {
		t.Parallel()
		s, _, ticketID := buildTicketInBuilding(t)
		ticket := getTicket(t, s, ticketID)

		scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{fixErrorStep("task-error-sess")}}
		deps := claimForBuild(t, s, scriptRT, ticketID)

		commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN: error outcome
		if err != nil {
			t.Fatalf("RUN: %v", err)
		}
		assertBuildOrigin(t, commit)
	})

	t.Run("resume exec failure", func(t *testing.T) {
		t.Parallel()
		s, _, ticketID := buildTicketInBuilding(t)

		scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, nil, "task-exec-fail-sess")}}
		deps := claimForBuild(t, s, scriptRT, ticketID)
		commit, err := runBuilding(t, s, deps, ticketID) // RUN: claims hello.txt, writes nothing
		if err != nil {
			t.Fatalf("RUN: %v", err)
		}
		apply(t, s, getTicket(t, s, ticketID), commit)

		deps2 := claimForBuild(t, s, scriptRT, ticketID)
		checkCommit, err := runBuilding(t, s, deps2, ticketID) // CHECK: claim errors pending
		if err != nil {
			t.Fatalf("CHECK: %v", err)
		}
		apply(t, s, getTicket(t, s, ticketID), checkCommit)

		scriptRT.steps = append(scriptRT.steps, scriptedStep{res: runtime.RunResult{ExitCode: -1, AgentTime: 0}, err: runtime.ErrStart})
		deps3 := claimForBuild(t, s, scriptRT, ticketID)
		resumeCommit, err := runBuilding(t, s, deps3, ticketID) // resume: exec failure
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		assertBuildOrigin(t, resumeCommit)
		if resumeCommit.Escalation.Payload.Code != string(response.EscalationCodeRuntimeExecFailed) {
			t.Errorf("escalation code = %q, want %q", resumeCommit.Escalation.Payload.Code, response.EscalationCodeRuntimeExecFailed)
		}
	})
}

// containsAll reports whether s contains every one of subs.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// ---- plan #51: a sealed plan's file grant unblocks the builder ------------

// wantFileGrantGreet is the grant TestFileGrant_ForeignPathEscalationOffersOption
// and TestFileGrant_PickLetsNextSubmissionPass both build their scenario
// around (design "shape" rule 1): task 1's builder also changes greet.go,
// a file the fixture plan (fixtures/scripts/planning/2.xml) gives only to
// task 2.
var wantFileGrantGreet = &response.FileGrant{Task: 1, Paths: []string{greetGo}}

// withGreetAndHelloProject overrides ticket's own project commands to write
// both hello.txt and greet.go, exactly as TestCheckRejectsAnotherTasksFile's
// own override does.
func withGreetAndHelloProject(deps job.Deps, ticket store.Ticket) job.Deps {
	proj := deps.Projects[ticket.ProjectID]
	proj.TestCmd = "printf 'hello, world\\n' > hello.txt && printf 'package greet\\n' > greet.go && test -f hello.txt"
	proj.LintCmd = testNoopShellCmd
	deps.Projects = map[int64]job.Project{ticket.ProjectID: proj}
	return deps
}

// planGapErrorStep builds a scriptedStep whose Response is a minimal
// ErrorResponse code plan_gap (design section 6.8's universal "error"
// outcome): the shape a builder sends when it cannot work around CHECK's
// own claim-errors marker.
func planGapErrorStep(sessionID string) scriptedStep {
	return scriptedStep{res: runtime.RunResult{
		Response: &response.ErrorResponse{
			Job: response.JobBuild, Outcome: response.OutcomeError,
			Error: response.RunError{Code: response.ErrorCodePlanGap, What: "greet.go belongs to another task", Why: "the plan assigns it to task 2"},
		},
		SessionID: sessionID, ExitCode: 0, AgentTime: time.Second,
	}}
}

// fileGrantEscalatedTicket drives a fresh ticket through the shape
// TestFileGrant_ForeignPathEscalationOffersOption and
// TestFileGrant_PickLetsNextSubmissionPass both continue (plan #51): task
// 1's builder also changes greet.go; CHECK rejects it with "greet.go
// belongs to task 2, not task 1"; the resumed builder gives up with an
// ErrorResponse plan_gap, so building escalates with Grant set
// (attachFileGrant). Returns the store, ticket id, and the escalation's
// own open question, already applied.
func fileGrantEscalatedTicket(t *testing.T) (*store.Store, int64, store.MessageRow) {
	t.Helper()
	s, _, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt, greetGo}, nil, "grant-sess")}}
	deps := withGreetAndHelloProject(claimForBuild(t, s, scriptRT, ticketID), ticket)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN: claims hello.txt, greet.go
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := withGreetAndHelloProject(claimForBuild(t, s, scriptRT, ticketID), ticket)
	checkCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps2) // CHECK: claim errors pending
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if len(checkCommit.Messages) != 1 || !strings.Contains(checkCommit.Messages[0].Body, testForeignLineGreet) {
		t.Fatalf("CHECK commit.Messages = %+v, want it to contain %q", checkCommit.Messages, testForeignLineGreet)
	}
	apply(t, s, ticket, checkCommit)

	resumeRT := &scriptedRuntime{t: t, steps: []scriptedStep{planGapErrorStep("grant-sess")}}
	ticket = getTicket(t, s, ticketID)
	deps3 := claimForBuild(t, s, resumeRT, ticketID)
	escCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps3) // resume: error outcome plan_gap
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if escCommit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want an escalation")
	}
	if !reflect.DeepEqual(escCommit.Escalation.Payload.Grant, wantFileGrantGreet) {
		t.Fatalf("commit.Escalation.Payload.Grant = %+v, want %+v", escCommit.Escalation.Payload.Grant, wantFileGrantGreet)
	}
	apply(t, s, ticket, escCommit)

	q := findOpenQuestionByKind(t, s, ticketID, response.QuestionKindQuestion)
	return s, ticketID, q
}

// TestFileGrant_ForeignPathEscalationOffersOption proves attachFileGrant and
// escalateTx's own option d (plan #51, design "shape" rules 1-2): a build
// escalation whose newest claim-errors marker names a foreign path offers
// "Let task 1 also change greet.go" after Retry and Abandon, Recommended
// unchanged at "a".
func TestFileGrant_ForeignPathEscalationOffersOption(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	_, _, q := fileGrantEscalatedTicket(t)

	var qp response.QuestionPayload
	if err := json.Unmarshal(q.Payload, &qp); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	wantOptions := []response.Option{
		{Key: "a", Text: testEscalationTextRetry},
		{Key: "c", Text: testEscalationTextAbandon},
		{Key: "d", Text: "Let task 1 also change greet.go"},
	}
	if !reflect.DeepEqual(qp.Options, wantOptions) {
		t.Errorf("question.Options = %+v, want %+v", qp.Options, wantOptions)
	}
	if qp.Recommended != "a" {
		t.Errorf("question.Recommended = %q, want a", qp.Recommended)
	}
}

// TestFileGrant_PickLetsNextSubmissionPass proves picking option d applies
// the grant and reruns task 1 fresh (plan #51, design "shape" rules 3-6):
// the fresh run's own prompt carries the grant note, commit.GrantFiles is
// set and the question resolves, the stored plan ends up with greet.go
// task "1 2" plus one owner_edit event, and the next CHECK accepts
// greet.go for task 1 and lands it.
func TestFileGrant_PickLetsNextSubmissionPass(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, ticketID, q := fileGrantEscalatedTicket(t)

	answerGateQuestion(t, s, ticketID, q.ID, new("d"), "")

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt, greetGo}, nil, "grant-fresh-sess")}}
	rec := &recordingRuntime{rt: scriptRT}
	ticket := getTicket(t, s, ticketID)
	deps := withGreetAndHelloProject(claimForBuild(t, s, rec, ticketID), ticket)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // pick d: fresh run carrying the grant
	if err != nil {
		t.Fatalf("grant pick Run: %v", err)
	}
	if rec.lastReq.SessionID != "" {
		t.Errorf("RunRequest.SessionID = %q, want empty (a fresh session)", rec.lastReq.SessionID)
	}
	const wantGrantText = "Let task 1 also change greet.go"
	if !strings.Contains(rec.lastReq.Prompt, wantGrantText) {
		t.Errorf("prompt = %q, want it to contain %q", rec.lastReq.Prompt, wantGrantText)
	}
	if commit.GrantFiles == nil || !reflect.DeepEqual(commit.GrantFiles, wantFileGrantGreet) {
		t.Errorf("commit.GrantFiles = %+v, want %+v", commit.GrantFiles, wantFileGrantGreet)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != q.ID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, q.ID)
	}
	apply(t, s, ticket, commit)

	plan, _, ok, err := s.StoredPlan(t.Context(), ticketID)
	if err != nil || !ok {
		t.Fatalf("StoredPlan: ok=%v err=%v", ok, err)
	}
	greetTask := ""
	for _, f := range plan.Delivery.Files {
		if f.Path == greetGo {
			greetTask = f.Task
		}
	}
	if greetTask != "1 2" {
		t.Errorf("stored plan's greet.go task = %q, want %q", greetTask, "1 2")
	}

	events, err := s.Events(t.Context(), ticketID, store.EventKindOwnerEdit, store.EventFilter{})
	if err != nil {
		t.Fatalf("Events(owner_edit): %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("owner_edit events = %d, want 1", len(events))
	}
	var ev response.OwnerEditEvent
	if err = json.Unmarshal(events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal owner_edit event: %v", err)
	}
	if ev.Target != "plan_file" || ev.Ref != greetGo || ev.Old != "2" || ev.New != "1 2" {
		t.Errorf("event = %+v, want target plan_file ref greet.go old 2 new 1 2", ev)
	}

	ticket = getTicket(t, s, ticketID)
	deps2 := withGreetAndHelloProject(claimForBuild(t, s, scriptRT, ticketID), ticket)
	checkCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps2) // CHECK: greet.go now accepted for task 1
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	for _, m := range checkCommit.Messages {
		if strings.Contains(m.Body, "claim errors pending") {
			t.Fatalf("CHECK commit.Messages = %+v, want no claim errors pending", checkCommit.Messages)
		}
	}
	if len(checkCommit.Artifacts) != 1 {
		t.Fatalf("CHECK commit.Artifacts = %+v, want exactly one landed build_report", checkCommit.Artifacts)
	}
	var landed response.BuildReport
	if err := json.Unmarshal(checkCommit.Artifacts[0].Payload, &landed); err != nil {
		t.Fatalf("unmarshal landed build_report: %v", err)
	}
	if landed.CommitSHA == nil {
		t.Error("landed.CommitSHA = nil, want a sha")
	}
	if landed.TaskN != 1 {
		t.Errorf("landed.TaskN = %d, want 1", landed.TaskN)
	}
}

// TestFileGrant_NoOptionWithoutForeignLines proves attachFileGrant's own
// negative case (plan #51, design "shape" rule 1): a claim-errors marker
// with no "belongs to" line (a files_changed mismatch, not a foreign path)
// leaves Grant nil, so escalateTx offers only Retry and Abandon.
func TestFileGrant_NoOptionWithoutForeignLines(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, rt, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	mismatchRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt, phantomTxt}, nil, "no-grant-sess")}}
	deps := withHelloAlwaysProject(claimForBuild(t, s, mismatchRT, ticketID), ticket)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := withHelloAlwaysProject(claimForBuild(t, s, rt, ticketID), ticket)
	checkCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps2) // CHECK: claim errors pending, no foreign line
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if len(checkCommit.Messages) != 1 || strings.Contains(checkCommit.Messages[0].Body, "belongs to") {
		t.Fatalf("CHECK commit.Messages = %+v, want a pending marker with no \"belongs to\" line", checkCommit.Messages)
	}
	apply(t, s, ticket, checkCommit)

	resumeRT := &scriptedRuntime{t: t, steps: []scriptedStep{planGapErrorStep("no-grant-sess")}}
	ticket = getTicket(t, s, ticketID)
	deps3 := withHelloAlwaysProject(claimForBuild(t, s, resumeRT, ticketID), ticket)
	escCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps3) // resume: error outcome plan_gap
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if escCommit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want an escalation")
	}
	if escCommit.Escalation.Payload.Grant != nil {
		t.Errorf("commit.Escalation.Payload.Grant = %+v, want nil", escCommit.Escalation.Payload.Grant)
	}
	apply(t, s, ticket, escCommit)

	q := findOpenQuestionByKind(t, s, ticketID, response.QuestionKindQuestion)
	var qp response.QuestionPayload
	if err := json.Unmarshal(q.Payload, &qp); err != nil {
		t.Fatalf("unmarshal question payload: %v", err)
	}
	wantOptions := []response.Option{{Key: "a", Text: testEscalationTextRetry}, {Key: "c", Text: testEscalationTextAbandon}}
	if !reflect.DeepEqual(qp.Options, wantOptions) {
		t.Errorf("question.Options = %+v, want %+v", qp.Options, wantOptions)
	}
}

// TestFileGrant_NewestMarkerOfSessionWinsOverOlderForeignLine proves
// attachFileGrant's own "newest" qualifier (design "shape" rule 1, review
// r1f5): a marker naming a run outside the escalated session, even the
// newest row in the table, is skipped; once the newest in-session marker is
// found, an older in-session marker's own foreign line is never consulted.
func TestFileGrant_NewestMarkerOfSessionWinsOverOlderForeignLine(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, _, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt, greetGo}, nil, "newest-marker-sess")}}
	deps := withGreetAndHelloProject(claimForBuild(t, s, scriptRT, ticketID), ticket)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN: claims hello.txt, greet.go
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := withGreetAndHelloProject(claimForBuild(t, s, scriptRT, ticketID), ticket)
	checkCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps2) // CHECK: the real foreign-line marker
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if len(checkCommit.Messages) != 1 || !strings.Contains(checkCommit.Messages[0].Body, testForeignLineGreet) {
		t.Fatalf("CHECK commit.Messages = %+v, want it to contain %q", checkCommit.Messages, testForeignLineGreet)
	}
	apply(t, s, ticket, checkCommit)

	sessions, err := s.SessionsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("SessionsForTicket: %v", err)
	}
	var sessionID int64
	for _, sess := range sessions {
		if sess.ExternalID != nil && *sess.ExternalID == "newest-marker-sess" {
			sessionID = sess.ID
		}
	}
	if sessionID == 0 {
		t.Fatal(`session "newest-marker-sess" not found`)
	}
	runIDs, err := s.SessionRunIDs(t.Context(), sessionID)
	if err != nil || len(runIDs) == 0 {
		t.Fatalf("SessionRunIDs(%d) = %v, %v, want at least one", sessionID, runIDs, err)
	}
	inSessionRunID := runIDs[len(runIDs)-1]

	// A newer, in-session marker with no foreign line: what the resumed
	// builder's own next CHECK would write had it dropped greet.go but hit
	// a different claim mismatch instead. It must win over the older
	// marker's own foreign line.
	insertUpdateMarker(t, s, ticketID, fmt.Sprintf("claim errors pending run %d\nclaims/files_changed: missing phantom.txt", inSessionRunID))

	// An even newer marker naming a run outside the escalated session
	// entirely: it must be skipped, falling through to the in-session
	// marker above, not the real CHECK marker further back.
	outsideRunID, _ := reserveTerminalRun(t, s, ticketID, string(response.JobBuild), false)
	insertUpdateMarker(t, s, ticketID, fmt.Sprintf("claim errors pending run %d\n%s", outsideRunID, testForeignLineGreet))

	resumeRT := &scriptedRuntime{t: t, steps: []scriptedStep{planGapErrorStep("newest-marker-sess")}}
	ticket = getTicket(t, s, ticketID)
	deps3 := claimForBuild(t, s, resumeRT, ticketID)
	escCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps3) // resume: error outcome plan_gap
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(resumeRT.reqs) != 1 {
		t.Fatalf("resumeRT.reqs = %d, want 1 (its scripted step must have run)", len(resumeRT.reqs))
	}
	if escCommit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want an escalation")
	}
	payload := escCommit.Escalation.Payload
	if payload.Origin != string(response.EscalationOriginBuild) {
		t.Errorf("commit.Escalation.Payload.Origin = %q, want %q", payload.Origin, response.EscalationOriginBuild)
	}
	if payload.Code != "plan_gap" {
		t.Errorf("commit.Escalation.Payload.Code = %q, want %q", payload.Code, "plan_gap")
	}
	if payload.SessionID == nil || *payload.SessionID != sessionID {
		t.Errorf("commit.Escalation.Payload.SessionID = %v, want %d", payload.SessionID, sessionID)
	}
	if payload.Grant != nil {
		t.Errorf("commit.Escalation.Payload.Grant = %+v, want nil (the newest in-session marker has no foreign line)", payload.Grant)
	}
}

// TestFileGrant_OutOfSessionMarkerAloneIsIgnored is
// TestFileGrant_NewestMarkerOfSessionWinsOverOlderForeignLine's control: with
// no newer in-session marker to shadow it, a newer marker naming a run
// outside the escalated session is still skipped, so attachFileGrant falls
// through to the real CHECK marker further back and still builds a Grant
// from its foreign line. This shows the walk actually consults session
// membership, not just marker recency: with attachFileGrant's session
// filter removed, this test would instead see Grant built from the
// out-of-session marker, which happens to match here too, so the sibling
// test's nil is this test's non-nil, not the other way around.
func TestFileGrant_OutOfSessionMarkerAloneIsIgnored(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, _, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt, greetGo}, nil, "out-of-session-sess")}}
	deps := withGreetAndHelloProject(claimForBuild(t, s, scriptRT, ticketID), ticket)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN: claims hello.txt, greet.go
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := withGreetAndHelloProject(claimForBuild(t, s, scriptRT, ticketID), ticket)
	checkCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps2) // CHECK: the real foreign-line marker
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if len(checkCommit.Messages) != 1 || !strings.Contains(checkCommit.Messages[0].Body, testForeignLineGreet) {
		t.Fatalf("CHECK commit.Messages = %+v, want it to contain %q", checkCommit.Messages, testForeignLineGreet)
	}
	apply(t, s, ticket, checkCommit)

	// A newer marker naming a run outside the escalated session, with its
	// own distinct foreign line (a different path than the real marker's
	// greet.go): if attachFileGrant's session filter let this through, the
	// resulting Grant would name other.go, not greet.go.
	const outOfSessionForeignLine = "claims/files_changed: other.go belongs to task 2, not task 1"
	outsideRunID, _ := reserveTerminalRun(t, s, ticketID, string(response.JobBuild), false)
	insertUpdateMarker(t, s, ticketID, fmt.Sprintf("claim errors pending run %d\n%s", outsideRunID, outOfSessionForeignLine))

	resumeRT := &scriptedRuntime{t: t, steps: []scriptedStep{planGapErrorStep("out-of-session-sess")}}
	ticket = getTicket(t, s, ticketID)
	deps3 := claimForBuild(t, s, resumeRT, ticketID)
	escCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps3) // resume: error outcome plan_gap
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !reflect.DeepEqual(escCommit.Escalation.Payload.Grant, wantFileGrantGreet) {
		t.Errorf("commit.Escalation.Payload.Grant = %+v, want %+v (the real CHECK marker, since the only newer marker belongs to a different session)", escCommit.Escalation.Payload.Grant, wantFileGrantGreet)
	}
}

// TestFileGrant_CapResumesOriginAlsoOffersOption proves owner decision Q2
// (review r1f4): attachFileGrant offers the grant on a cap_resumes
// escalation too, not only a build escalation's own error outcome. Task 1's
// resume budget runs out while a "greet.go belongs to task 2" claim-errors
// marker is still pending, so RESOLVE escalates resumes_exhausted instead of
// ever calling the runtime again, and that escalation's own payload still
// carries the grant.
func TestFileGrant_CapResumesOriginAlsoOffersOption(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end flow; runs in the full suite")
	}
	t.Parallel()
	s, _, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt, greetGo}, nil, "cap-resumes-grant-sess")}}
	deps := withGreetAndHelloProject(claimForBuild(t, s, scriptRT, ticketID), ticket)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RUN: claims hello.txt, greet.go
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := withGreetAndHelloProject(claimForBuild(t, s, scriptRT, ticketID), ticket)
	checkCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps2) // CHECK: claim errors pending
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if len(checkCommit.Messages) != 1 || !strings.Contains(checkCommit.Messages[0].Body, testForeignLineGreet) {
		t.Fatalf("CHECK commit.Messages = %+v, want it to contain %q", checkCommit.Messages, testForeignLineGreet)
	}
	apply(t, s, ticket, checkCommit)

	const maxResumes = 3
	sess, _, err := s.LatestSession(t.Context(), ticketID, "build", maxResumes)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	owner := "cap-resumes-grant-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	bumpResumesToCap(t, s, ticketID, sess.ID, maxResumes, owner, expires)

	ticket = getTicket(t, s, ticketID)
	deps3 := withGreetAndHelloProject(claimForBuild(t, s, scriptRT, ticketID), ticket)
	escCommit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps3) // RESOLVE: exhausted, escalates cap_resumes
	if err != nil {
		t.Fatalf("RESOLVE: %v", err)
	}
	if escCommit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want resumes_exhausted")
	}
	if escCommit.Escalation.Payload.Origin != string(response.EscalationOriginCapResumes) {
		t.Fatalf("payload.Origin = %q, want cap_resumes", escCommit.Escalation.Payload.Origin)
	}
	if !reflect.DeepEqual(escCommit.Escalation.Payload.Grant, wantFileGrantGreet) {
		t.Fatalf("commit.Escalation.Payload.Grant = %+v, want %+v", escCommit.Escalation.Payload.Grant, wantFileGrantGreet)
	}
}

// grantlessDOptionQuestion inserts an escalation message carrying a Grant-
// less payload (payload.Grant stays nil, exactly as every stored escalation
// predating plan #51 reads back), plus a linked question whose Options
// still include "d" -- the shape a row stored before this feature shipped,
// or one escalateTx wrote for a Grant that was valid then and is gone now,
// would carry. Mirrors legacyEscalationQuestion's own direct-InsertMessage
// construction. Returns the linked question's id.
func grantlessDOptionQuestion(t *testing.T, s *store.Store, ticketID int64, code response.EscalationCode, origin response.EscalationOrigin) int64 {
	t.Helper()
	payload := testEscalationPayload(code, origin)
	body := string(code) + ": " + payload.What
	escPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("grantlessDOptionQuestion: marshal escalation payload: %v", err)
	}
	escID, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, Type: testMsgTypeEscalation, Author: testAuthorZing, Body: body, Payload: escPayload,
	})
	if err != nil {
		t.Fatalf("grantlessDOptionQuestion: InsertMessage(escalation): %v", err)
	}

	qPayload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindQuestion, State: response.QuestionStateOpen,
		Recommended: "a",
		Options: []response.Option{
			{Key: "a", Text: testEscalationTextRetry},
			{Key: "c", Text: testEscalationTextAbandon},
			{Key: "d", Text: "Let task 1 also change greet.go"},
		},
	})
	if err != nil {
		t.Fatalf("grantlessDOptionQuestion: marshal question payload: %v", err)
	}
	qID, err := s.InsertMessage(t.Context(), store.Message{
		TicketID: ticketID, ParentID: &escID, Type: testMsgTypeQuestion, Author: testAuthorZing,
		State: new("open"), Body: body + "\n\nHow should Zing proceed?", Payload: qPayload,
	})
	if err != nil {
		t.Fatalf("grantlessDOptionQuestion: InsertMessage(question): %v", err)
	}
	return qID
}

// TestFileGrant_PickWithoutGrantFallsBackToReplan proves design "shape" rule
// 4's own negative half (review r1f1): answering "d" on an escalation whose
// payload carries no Grant at all takes the existing "anything but retry"
// branch (enterFromEscalationRound), exactly as any other non-retry,
// non-abandon choice does, instead of calling retryFreshRun with a nil
// grant.
func TestFileGrant_PickWithoutGrantFallsBackToReplan(t *testing.T) {
	t.Parallel()
	s, rt, ticketID := buildTicketInBuilding(t)
	qID := grantlessDOptionQuestion(t, s, ticketID, response.EscalationCodePlanGap, response.EscalationOriginBuild)
	answerGateQuestion(t, s, ticketID, qID, new("d"), "")

	commit, err := runBuilding(t, s, claimForBuild(t, s, rt, ticketID), ticketID)
	if err != nil {
		t.Fatalf("escalation resolve (d, no grant) Run: %v", err)
	}
	if commit.Escalation == nil || commit.Escalation.Payload.Code != string(response.EscalationCodeReplanUnsupported) {
		t.Fatalf("commit.Escalation = %+v, want replan_unsupported", commit.Escalation)
	}
	if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
		t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
	}
}
