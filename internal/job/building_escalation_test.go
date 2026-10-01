// building_escalation_test.go tests task 13: design section 6.9's own
// escalation resolution for the building handler -- entered before step 0
// (design section 6.1), it resolves a retry, a back/reply-only choice, or
// an abandon against a building escalation's own origin (build, perimeter,
// fix, cap_resumes, cap_budget) and code (sandbox_unavailable overrides
// every origin's own row).
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
	"os"
	"path/filepath"
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

		scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "retry-fresh-sess")}}
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
		// "a retry | cap_budget | recapBudgetEscalation": re-escalate
		// wall_clock in this same commit, resolving the round, with no
		// runtime call.
		s, _, ticketID := buildTicketInBuilding(t)
		qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeWallClock, response.EscalationOriginCapBudget)
		answerGateQuestion(t, s, ticketID, qID, new("a"), "")

		commit, err := runBuilding(t, s, claimForBuild(t, s, &scriptedRuntime{t: t}, ticketID), ticketID)
		if err != nil {
			t.Fatalf("escalation resolve (cap_budget retry) Run: %v", err)
		}
		if commit.Escalation == nil {
			t.Fatal("commit.Escalation = nil, want a re-escalated wall_clock")
		}
		if commit.Escalation.Payload.Code != string(response.EscalationCodeWallClock) ||
			commit.Escalation.Payload.Origin != string(response.EscalationOriginCapBudget) {
			t.Errorf("payload = %+v, want (wall_clock, cap_budget)", commit.Escalation.Payload)
		}
		if len(commit.ResolveQuestions) != 1 || commit.ResolveQuestions[0] != qID {
			t.Errorf("commit.ResolveQuestions = %v, want [%d]", commit.ResolveQuestions, qID)
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
// text, resolving the round.
func TestReplanUnsupportedText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		option *string
	}{
		{"Back", new("b")},
		{"ReplyOnly", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, rt, ticketID := buildTicketInBuilding(t)
			qID := escalateDirect(t, s, ticketID, nil, nil, response.EscalationCodeEnvironment, response.EscalationOriginBuild)
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
// retries the resulting resumes_exhausted escalation, the fresh session's
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

	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, scriptRT, ticketID)
	commit, err := job.Registry()[testStateBuilding].Run(t.Context(), ticket, deps) // RESOLVE: exhausted, escalates
	if err != nil {
		t.Fatalf("RESOLVE: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want resumes_exhausted")
	}
	apply(t, s, ticket, commit)

	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil || len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %v, %v, want exactly one (the escalation's own question)", open, err)
	}
	escQID := open[0].ID
	answerGateQuestion(t, s, ticketID, escQID, new("a"), "please retry")

	scriptRT.steps = append(scriptRT.steps, buildStep([]string{helloTxt}, 0, 0, nil, "cap-retry-fresh-sess"))
	rec := &recordingRuntime{rt: scriptRT}
	ticket = getTicket(t, s, ticketID)
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
		s, _, ticketID := buildTicketInBuilding(t)

		scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "task-exec-fail-sess")}}
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
