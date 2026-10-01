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
	"testing"
	"testing/fstest"
	"time"

	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/sandbox"
	"zing/internal/store"
)

// judgingTestPlanPayload is internal/store/examples/artifacts/plan.json,
// verbatim: a fully valid "plan" artifact payload (cmd/zing/scenarios_test.go's
// own planExample before its rewrite). Its content is irrelevant to
// writeScenariosFile, which never reads the plan artifact's payload, only
// CurrentCohort's own run id and version.
const judgingTestPlanPayload = `{
  "overview": {
    "objective": "Stop checkout from crashing on an empty cart.",
    "context": "internal/cart handles cart state; internal/checkout reads it at payment time.",
    "problem": {
      "text": "checkout panics when cart.Items is nil instead of an empty slice.",
      "loop": {
        "cmd": "go test ./internal/cart/... -run TestEmptyCart",
        "text": "fails: nil pointer dereference in checkout.Total"
      },
      "repro": "create a cart, call Checkout without adding items",
      "hypotheses": [
        {
          "rank": 1,
          "cause": "NewCart never initializes Items",
          "prediction": "initializing Items to []Item{} makes the loop pass"
        }
      ]
    },
    "goals": ["checkout never panics on an empty cart"],
    "nongoals": ["changing the checkout API"]
  },
  "design": {
    "demo": {
      "cmd": "go run ./cmd/demo -empty-cart",
      "text": "an empty cart checks out for zero dollars instead of crashing"
    },
    "shape": "NewCart initializes Items to an empty slice; checkout reads it unchanged.",
    "changes": [
      {
        "path": "internal/cart/cart.go",
        "symbol": "NewCart",
        "kind": "modified",
        "callers": "checkout.New",
        "callees": "none",
        "before": "Items field left at its zero value (nil)",
        "after": "Items: make([]Item, 0)"
      }
    ],
    "types": [],
    "migrations": { "migrations": [] }
  },
  "delivery": {
    "files": [
      { "path": "internal/cart/cart.go", "action": "modify", "reason": "initialize Items to an empty slice" }
    ],
    "deletions": { "deletions": [] },
    "tests": [
      {
        "name": "TestEmptyCart_ReturnsEmptyOrder",
        "seam": "cart.NewCart",
        "kind": "regression",
        "mocks": "",
        "asserts": "checkout of a freshly created cart returns a zero-item order, no panic"
      }
    ],
    "tasks": [
      { "n": 1, "test": "TestEmptyCart_ReturnsEmptyOrder", "demo": true, "text": "Initialize cart.Items to an empty slice in NewCart." }
    ]
  },
  "review": {
    "trust_root": "none",
    "alternatives": ["guard checkout.Total with a nil check instead of fixing the source"],
    "risks": ["other constructors that build a Cart by struct literal still skip this initializer"]
  }
}`

// judgingTestScenarioPayload builds a schema-valid "scenario" artifact
// payload (internal/store/schemas/artifacts/scenario.json requires
// check_cmd present, even as an empty string).
func judgingTestScenarioPayload(t *testing.T, id, kind, given, when, then string) []byte {
	t.Helper()
	payload := struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		CheckCmd string `json:"check_cmd"`
		Given    string `json:"given"`
		When     string `json:"when"`
		Then     string `json:"then"`
	}{id, kind, "", given, when, then}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal scenario payload: %v", err)
	}
	return b
}

// seedSealedScenarioCohort claims ticketID, reserves one "planning" run,
// stores a plan artifact and n scenario artifacts under it (unsealed), then
// seals the whole cohort: the shape writeScenariosFile's own ScenariosForRun
// read (sealedOnly true) requires.
func seedSealedScenarioCohort(t *testing.T, s *store.Store, ticketID int64, n int) int64 {
	t.Helper()
	ctx := t.Context()
	owner := "judging-test-seed-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(ctx, ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("seedSealedScenarioCohort: claim: claimed=%v err=%v", claimed, err)
	}

	rsv, reserveErr := s.Reserve(ctx, ticketID, owner, expires, store.SessionUpsert{Job: "planning", Runtime: runtimeFake}, store.RunSeed{Model: "fake-model"})
	if reserveErr != nil {
		t.Fatalf("seedSealedScenarioCohort: reserve: %v", reserveErr)
	}
	runID := rsv.RunID

	if _, insertErr := s.InsertArtifact(ctx, store.Artifact{
		TicketID: ticketID, RunID: &runID, Type: "plan", Version: 1, Payload: []byte(judgingTestPlanPayload),
	}); insertErr != nil {
		t.Fatalf("seedSealedScenarioCohort: insert plan: %v", insertErr)
	}

	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("s%d", i)
		payload := judgingTestScenarioPayload(t, id, "behavior", fmt.Sprintf("given %d", i), fmt.Sprintf("when %d", i), fmt.Sprintf("then %d", i))
		if _, insertErr := s.InsertArtifact(ctx, store.Artifact{
			TicketID: ticketID, RunID: &runID, Type: "scenario", Version: 1, Payload: payload,
		}); insertErr != nil {
			t.Fatalf("seedSealedScenarioCohort: insert scenario %s: %v", id, insertErr)
		}
	}

	// InsertArtifact writes outside the claim ceremony, so the ticket is
	// still claimed under (owner, expires) here, exactly what Seal's own
	// fence needs.
	applied, err := s.CommitHandlerResult(ctx, store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires,
		Seal: &store.SealRequest{RunID: runID, PlanVersion: 1, ExpectedCount: n, At: time.Now()},
	})
	if err != nil || !applied {
		t.Fatalf("seedSealedScenarioCohort: seal: applied=%v err=%v", applied, err)
	}
	return runID
}

// TestWriteScenariosFile proves writeScenariosFile's hook (judging.go,
// PKG9-PLAN.md section 7.3, D19): the scenarios directory and file it
// creates carry the exact modes 0700 and 0600, the file's content is the
// ticket's sealed cohort rendered the same way renderScenariosXML (and so
// zing scenarios) renders it, and req.Env gains ZING_SCENARIOS_FILE
// pointing at the file.
func TestWriteScenariosFile(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)

	const scenarioCount = 2
	runID := seedSealedScenarioCohort(t, s, ticketID, scenarioCount)

	dataDir := t.TempDir()
	d := Deps{Store: s, DataDir: dataDir}

	req := &runtime.RunRequest{}
	path, cleanup, err := writeScenariosFile(d, ticket)(t.Context(), store.Reserved{RunID: 42}, req)
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Logf("cleanup: %v", cleanupErr)
		}
	})

	wantDir := filepath.Join(dataDir, "judge", "42")
	wantPath := filepath.Join(wantDir, "scenarios.xml")
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}

	dirInfo, err := os.Stat(wantDir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dirInfo.Mode().Perm() != judgeScenariosDirPerm {
		t.Errorf("dir mode = %v, want %v", dirInfo.Mode().Perm(), os.FileMode(judgeScenariosDirPerm))
	}

	fileInfo, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if fileInfo.Mode().Perm() != judgeScenariosFilePerm {
		t.Errorf("file mode = %v, want %v", fileInfo.Mode().Perm(), os.FileMode(judgeScenariosFilePerm))
	}

	rows, err := s.ScenariosForRun(t.Context(), ticketID, runID, true)
	if err != nil {
		t.Fatalf("ScenariosForRun: %v", err)
	}
	if len(rows) != scenarioCount {
		t.Fatalf("ScenariosForRun returned %d rows, want %d (the sealed cohort)", len(rows), scenarioCount)
	}
	wantData, err := renderScenariosXML(rows)
	if err != nil {
		t.Fatalf("renderScenariosXML: %v", err)
	}
	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, wantData) {
		t.Errorf("file content =\n%s\nwant\n%s", got, wantData)
	}

	wantEnv := "ZING_SCENARIOS_FILE=" + wantPath
	if !slices.Contains(req.Env, wantEnv) {
		t.Errorf("req.Env = %v, want it to contain %q", req.Env, wantEnv)
	}
}

// TestScenariosDirRemovedAfterRun proves the hook's own cleanup removes the
// whole per-run scenarios directory, not just the file inside it
// (PKG9-PLAN.md section 7.3).
func TestScenariosDirRemovedAfterRun(t *testing.T) {
	t.Parallel()
	s := newRunJobTestStore(t)
	ticketID := seedRunJobTicket(t, s)
	ticket := getRunJobTicket(t, s, ticketID)

	seedSealedScenarioCohort(t, s, ticketID, 2) // a sealed cohort needs 2 to 30 scenario rows

	d := Deps{Store: s, DataDir: t.TempDir()}

	path, cleanup, err := writeScenariosFile(d, ticket)(t.Context(), store.Reserved{RunID: 7}, &runtime.RunRequest{})
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("scenarios file missing before cleanup: %v", statErr)
	}

	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	runDir := filepath.Dir(path)
	if _, statErr := os.Stat(runDir); !os.IsNotExist(statErr) {
		t.Errorf("run dir %s still exists after cleanup (stat err = %v)", runDir, statErr)
	}
}

// ---- M2 task 7a: judgeHandler's own START and RUN (design section 7) -----
//
// This harness reuses postbuild_test.go's own pb-prefixed fixtures
// (pbSeedQueuedGitBackedTicket, pbAdvanceQueuedToPlanning,
// pbAdvancePlanningWithAnAnswer, pbAdvanceBuilding, pbTicketInReviewing,
// pbClaim, pbApply, pbGetTicket, pbFakeRuntime) and reviewing_test.go's own
// (reviewTicketReady, reviewScriptsFS, reviewMarker, recordingRuntime):
// same package, so all of it is reachable from here without its own
// skeleton_test.go-style reimplementation (judgeHandler is not yet
// job.Registry()'s own "judging" entry -- see judging.go's own package doc
// comment -- so every test below drives it directly, the same way
// reviewing_test.go drove reviewingHandler before task 10 wired it in).

// judgeTicketReady drives a fresh, git-backed ticket through planning, a
// real three-task build, and one clean review round into "judging"
// (reviewing.go's own roundCommit already sets Next judging in that same
// commit, so one reviewingHandler.Run call suffices): a stored plan, a real
// worktree, and the sealed two-scenario cohort fixtures/scripts/
// planning/2.xml writes (s1 behavior, check "curl -sf localhost:8080/hello";
// s2 negative, no check), with no judge round marker yet.
func judgeTicketReady(t *testing.T) (s *store.Store, ticket store.Ticket) {
	t.Helper()
	s, reviewTicket, _ := reviewTicketReady(t)
	deps := pbClaim(t, s, runtime.NewFake(reviewScriptsFS(nil)), reviewTicket.ID)

	commit, err := (reviewingHandler{}).Run(t.Context(), reviewTicket, deps)
	if err != nil {
		t.Fatalf("judgeTicketReady: review Run: %v", err)
	}
	if commit.Next != stateJudging {
		t.Fatalf("judgeTicketReady: review commit.Next = %q, want %q", commit.Next, stateJudging)
	}
	pbApply(t, s, reviewTicket, commit)
	return s, pbGetTicket(t, s, reviewTicket.ID)
}

// judgeWorktreeDir returns the judge checkout's own path for ticketID
// (orchestrator/judge.go's own <local_path>/.zing/judge/<ticket_id>,
// design section 10.2), read back through the ticket's own stored project
// rather than hardcoded, so a test never repeats the layout rule.
func judgeWorktreeDir(t *testing.T, s *store.Store, ticketID int64) string {
	t.Helper()
	proj, err := s.ProjectForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	return filepath.Join(proj.LocalPath, ".zing", "judge", strconv.FormatInt(ticketID, 10))
}

// assertJudgeWorktreeGone fails unless ticketID's own judge checkout does
// not exist on disk (design section 7.2: "A deferred call removes the
// worktree on every path").
func assertJudgeWorktreeGone(t *testing.T, s *store.Store, ticketID int64) {
	t.Helper()
	dir := judgeWorktreeDir(t, s, ticketID)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("judge worktree %s still exists (stat err = %v)", dir, err)
	}
}

// judgeVerdictXML renders one passing <verdict> element (internal/response/
// examples/judge-ok.xml's own shape): no test in this file needs a failing
// one (JudgePasses and EVALUATE are task 8's own work).
func judgeVerdictXML(scenario, evidence string) string {
	return fmt.Sprintf("  <verdict scenario=%q result=\"pass\">\n    <evidence>%s</evidence>\n  </verdict>\n", scenario, evidence)
}

// judgeOkScript joins verdict elements (judgeVerdictXML) into one judge
// "ok" document.
func judgeOkScript(verdicts ...string) string {
	return "<zing job=\"judge\" outcome=\"ok\">\n" + strings.Join(verdicts, "") + "</zing>"
}

// judgeOkBothScript is the clean, fully-covered round 1 turn every test
// that does not care about coverage, question, or error handling uses: one
// passing verdict per scenario of judgeTicketReady's own cohort (s1, s2).
var judgeOkBothScript = judgeOkScript(
	judgeVerdictXML("s1", `curl -sf localhost:8080/hello printed "hello, world" with a 200 status.`),
	judgeVerdictXML("s2", "curl -X POST localhost:8080/hello returned 405, since only GET is registered for the route."),
)

// judgeErrorScript is a minimal judge "error" document (internal/response/
// examples/build-error.xml's own shape, job "judge").
const judgeErrorScript = `<zing job="judge" outcome="error">
  <error code="cannot_run">
    <what>the scenarios file could not be read</what>
    <why>ZING_SCENARIOS_FILE named a path the sandbox denied</why>
    <tried>zing scenarios</tried>
  </error>
</zing>`

// judgeQuestionScript is a minimal judge "question" document (design
// section 7.2, N1: a plain agent question, the same universal shape every
// other job's own first-turn question takes).
const judgeQuestionScript = `<zing job="judge" outcome="question">
  <question key="Q1">
    <title>Which port does the service listen on?</title>
    <body>The scenarios assume 8080; say if that is wrong.</body>
    <option key="a">8080 is correct</option>
    <recommended>a</recommended>
  </question>
</zing>`

// judgeScriptsFS builds an in-memory fs.FS carrying one or more judge
// round-1 turns, keyed "judge/1/<turn>.xml" (runtime.Fake's own scriptKey,
// design section 7.4): scripts[0] is turn 1, scripts[1] turn 2, and so on.
func judgeScriptsFS(scripts ...string) fstest.MapFS {
	m := make(fstest.MapFS, len(scripts))
	for i, text := range scripts {
		m[fmt.Sprintf("judge/1/%d.xml", i+1)] = &fstest.MapFile{Data: []byte(text)}
	}
	return m
}

// judgeAnswerOpenQuestion answers ticketID's one open question with option
// (postbuild_test.go's own pbAnswerFixtureQuestion always answers "b",
// which judgeQuestionScript's own single option "a" does not offer).
func judgeAnswerOpenQuestion(t *testing.T, s *store.Store, ticketID int64, option string) {
	t.Helper()
	open, err := s.QuestionsByState(t.Context(), ticketID, "open")
	if err != nil {
		t.Fatalf("QuestionsByState(open): %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("QuestionsByState(open) = %d questions, want exactly 1", len(open))
	}
	result, err := s.AnswerQuestion(t.Context(), store.AnswerInput{TicketID: ticketID, QuestionID: open[0].ID, Option: option})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("AnswerQuestion: Accepted = false, Conflict = %q, want accepted", result.Conflict)
	}
}

// ---- TestJudgeStartWritesWatermark ------------------------------------------

// TestJudgeStartWritesWatermark proves START (design section 7.2): no
// transition, one update message, exactly "judge round 1 started sha
// <HeadSHA> after run <MaxRunID>" read back at the moment START ran.
func TestJudgeStartWritesWatermark(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	deps := pbClaim(t, s, pbFakeRuntime(t), ticket.ID)

	wantMaxRunID, err := s.MaxRunID(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	proj := deps.Projects[ticket.ProjectID]
	wt, _, err := proj.Orch.EnsureWorktree(t.Context(), ticket.ID, ticket.Title)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	wantSHA, err := proj.Orch.HeadSHA(t.Context(), wt)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commit.Next != "" || commit.Waiting != nil {
		t.Fatalf("commit = {Next: %q, Waiting: %v}, want both empty (START makes no transition and waits on nothing)", commit.Next, commit.Waiting)
	}
	if len(commit.Messages) != 1 {
		t.Fatalf("commit.Messages has %d entries, want 1", len(commit.Messages))
	}
	pbApply(t, s, ticket, commit)

	marker, ok := reviewMarker(t, s, ticket.ID, judgeRoundMarkerPrefix)
	if !ok {
		t.Fatal(`no "judge round " marker`)
	}
	want := fmt.Sprintf("judge round 1 started sha %s after run %d", wantSHA, wantMaxRunID)
	if marker.Body != want {
		t.Errorf("marker body = %q, want %q", marker.Body, want)
	}
}

// judgeAdvanceStart runs judgeHandler's own START once and applies it,
// returning the ticket re-read afterward: every RUN-focused test below
// needs this step first (design section 7.1: "none -> START round 1").
func judgeAdvanceStart(t *testing.T, s *store.Store, rt runtime.Runtime, ticket store.Ticket) store.Ticket {
	t.Helper()
	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("judgeAdvanceStart: Run: %v", err)
	}
	if commit.Next != "" {
		t.Fatalf("judgeAdvanceStart: commit.Next = %q, want empty", commit.Next)
	}
	pbApply(t, s, ticket, commit)
	return pbGetTicket(t, s, ticket.ID)
}

// ---- TestJudgeRunStoresVerdicts ---------------------------------------------

// TestJudgeRunStoresVerdicts proves RUN's own clean ok outcome (design
// section 7.2 step 5): two verdict artifacts, kinds and round and sha
// matching the cohort and the round, and the "judge round 1 verdicts run
// <rid>" marker.
func TestJudgeRunStoresVerdicts(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if len(commit.Runs) != 1 || commit.Runs[0].Outcome == nil || *commit.Runs[0].Outcome != string(response.OutcomeOk) {
		t.Fatalf("commit.Runs = %+v, want exactly one ok run", commit.Runs)
	}
	if len(commit.Artifacts) != 2 {
		t.Fatalf("commit.Artifacts has %d entries, want 2", len(commit.Artifacts))
	}
	pbApply(t, s, ticket, commit)

	rows, err := s.Verdicts(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("Verdicts: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("Verdicts returned %d rows, want 2", len(rows))
	}
	for _, row := range rows {
		v := row.Verdict
		if v.Round != 1 {
			t.Errorf("verdict %s round = %d, want 1", v.Scenario, v.Round)
		}
		if v.Result != response.ResultPass {
			t.Errorf("verdict %s result = %q, want pass", v.Scenario, v.Result)
		}
		if v.SHA == "" {
			t.Errorf("verdict %s sha is empty", v.Scenario)
		}
		switch v.Scenario {
		case "s1":
			if v.Kind != response.ScenarioKindBehavior {
				t.Errorf("s1 kind = %q, want behavior", v.Kind)
			}
		case "s2":
			if v.Kind != response.ScenarioKindNegative {
				t.Errorf("s2 kind = %q, want negative", v.Kind)
			}
		default:
			t.Errorf("unexpected verdict scenario %q", v.Scenario)
		}
	}

	marker, ok := reviewMarker(t, s, ticket.ID, "judge round 1 verdicts run")
	if !ok {
		t.Fatal(`no "judge round 1 verdicts run" marker`)
	}
	if !strings.HasPrefix(marker.Body, "judge round 1 verdicts run ") {
		t.Errorf("marker body = %q", marker.Body)
	}
}

// ---- TestJudgeRunRemovesWorktree ---------------------------------------------

// TestJudgeRunRemovesWorktree proves RUN's own judge checkout is removed
// after ok, error, and exec failure alike (design section 7.2 step 2: "A
// deferred call removes the worktree on every path").
func TestJudgeRunRemovesWorktree(t *testing.T) {
	t.Parallel()

	t.Run("ok", func(t *testing.T) {
		t.Parallel()
		s, ticket := judgeTicketReady(t)
		rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
		ticket = judgeAdvanceStart(t, s, rt, ticket)

		deps := pbClaim(t, s, rt, ticket.ID)
		if _, err := (judgeHandler{}).Run(t.Context(), ticket, deps); err != nil {
			t.Fatalf("RUN: %v", err)
		}
		assertJudgeWorktreeGone(t, s, ticket.ID)
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		s, ticket := judgeTicketReady(t)
		rt := runtime.NewFake(judgeScriptsFS(judgeErrorScript))
		ticket = judgeAdvanceStart(t, s, rt, ticket)

		deps := pbClaim(t, s, rt, ticket.ID)
		if _, err := (judgeHandler{}).Run(t.Context(), ticket, deps); err != nil {
			t.Fatalf("RUN: %v", err)
		}
		assertJudgeWorktreeGone(t, s, ticket.ID)
	})

	t.Run("exec failure", func(t *testing.T) {
		t.Parallel()
		s, ticket := judgeTicketReady(t)
		fake := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
		ticket = judgeAdvanceStart(t, s, fake, ticket)

		scripted := &pbScriptedRuntime{t: t, steps: []pbScriptedStep{
			{res: runtime.RunResult{ExitCode: -1}, err: runtime.ErrStart},
		}}
		deps := pbClaim(t, s, scripted, ticket.ID)
		if _, err := (judgeHandler{}).Run(t.Context(), ticket, deps); err != nil {
			t.Fatalf("RUN: %v", err)
		}
		assertJudgeWorktreeGone(t, s, ticket.ID)
	})
}

// ---- TestJudgeCoverageFailureResumes, TestJudgeSecondCoverageFailureEscalates

// TestJudgeCoverageFailureResumes proves RUN's own first coverage failure
// (design section 7.2 step 5): the round's first turn covers only s1, so
// CheckCoverage reports s2 missing, the run still terminalizes ok, and the
// commit writes "judge coverage failed run <rid>" with the error text, no
// escalation and no transition. The next tick's cap gate then resumes with
// a "coverage" input, and a fully-covered second turn stores both verdicts
// and also writes "judge coverage delivered run <rid1>" (rid1 the first
// turn's own run id) in the same commit.
func TestJudgeCoverageFailureResumes(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	incomplete := judgeOkScript(judgeVerdictXML("s1", "ran it"))
	rt := runtime.NewFake(judgeScriptsFS(incomplete, judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	firstCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN (first turn): %v", err)
	}
	if firstCommit.Next != "" || firstCommit.Escalation != nil {
		t.Fatalf("first commit = {Next: %q, Escalation: %v}, want neither (a first coverage failure only marks and stays)", firstCommit.Next, firstCommit.Escalation)
	}
	if len(firstCommit.Artifacts) != 0 {
		t.Fatalf("first commit.Artifacts has %d entries, want 0 (incomplete verdicts are never stored)", len(firstCommit.Artifacts))
	}
	firstRunID := firstCommit.Runs[0].ID
	pbApply(t, s, ticket, firstCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	failedMarker, ok := reviewMarker(t, s, ticket.ID, fmt.Sprintf(judgeCoverageFailedFmt, firstRunID))
	if !ok {
		t.Fatal(`no "judge coverage failed run <rid>" marker`)
	}
	if !strings.Contains(failedMarker.Body, "missing verdict for scenario s2") {
		t.Errorf("coverage failed marker body = %q, want it to report s2 missing", failedMarker.Body)
	}

	deps = pbClaim(t, s, rt, ticket.ID)
	secondCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN (coverage resume): %v", err)
	}
	if len(secondCommit.Artifacts) != 2 {
		t.Fatalf("second commit.Artifacts has %d entries, want 2", len(secondCommit.Artifacts))
	}
	deliveredBody := fmt.Sprintf(judgeCoverageDeliveredFmt, firstRunID)
	found := false
	for _, m := range secondCommit.Messages {
		if m.Body == deliveredBody {
			found = true
		}
	}
	if !found {
		t.Errorf("second commit.Messages = %+v, want it to include %q", secondCommit.Messages, deliveredBody)
	}
	pbApply(t, s, ticket, secondCommit)

	rows, err := s.Verdicts(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("Verdicts: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("Verdicts returned %d rows, want 2", len(rows))
	}
}

// TestJudgeSecondCoverageFailureEscalates proves RUN's own second coverage
// failure (design section 7.2 step 5): a coverage resume that is itself
// still incomplete terminalizes ok and escalates response_invalid, origin
// judge, instead of writing a second "coverage failed" marker.
func TestJudgeSecondCoverageFailureEscalates(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	incomplete := judgeOkScript(judgeVerdictXML("s1", "ran it"))
	rt := runtime.NewFake(judgeScriptsFS(incomplete, incomplete))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	firstCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN (first turn): %v", err)
	}
	pbApply(t, s, ticket, firstCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	deps = pbClaim(t, s, rt, ticket.ID)
	secondCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN (coverage resume): %v", err)
	}
	if secondCommit.Escalation == nil {
		t.Fatal("second commit.Escalation is nil, want response_invalid")
	}
	if secondCommit.Escalation.Payload.Code != string(response.EscalationCodeResponseInvalid) {
		t.Errorf("escalation code = %q, want %q", secondCommit.Escalation.Payload.Code, response.EscalationCodeResponseInvalid)
	}
	if secondCommit.Escalation.Payload.Origin != string(response.EscalationOriginJudge) {
		t.Errorf("escalation origin = %q, want %q", secondCommit.Escalation.Payload.Origin, response.EscalationOriginJudge)
	}
	if !strings.Contains(secondCommit.Escalation.Payload.Why, "missing verdict for scenario s2") {
		t.Errorf("escalation why = %q, want it to report s2 missing", secondCommit.Escalation.Payload.Why)
	}
	if len(secondCommit.Artifacts) != 0 {
		t.Errorf("second commit.Artifacts has %d entries, want 0", len(secondCommit.Artifacts))
	}
	if secondCommit.Waiting == nil || *secondCommit.Waiting != "questions" {
		t.Errorf("second commit.Waiting = %v, want questions", secondCommit.Waiting)
	}
}

// ---- TestJudgeQuestionResumes -----------------------------------------------

// TestJudgeQuestionResumes proves decision tree step (1) (design section
// 7.1): a first-turn plain agent question waits; once answered, the same
// session resumes with an "answers" input and a fully-covered second turn
// stores both verdicts and resolves the question.
func TestJudgeQuestionResumes(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeQuestionScript, judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	firstCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN (first turn): %v", err)
	}
	if firstCommit.Waiting == nil || *firstCommit.Waiting != "questions" {
		t.Fatalf("first commit.Waiting = %v, want questions", firstCommit.Waiting)
	}
	pbApply(t, s, ticket, firstCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	questionRunID := firstCommit.Runs[0].ID
	questionSession, err := s.RunByID(t.Context(), questionRunID)
	if err != nil {
		t.Fatalf("RunByID: %v", err)
	}

	judgeAnswerOpenQuestion(t, s, ticket.ID, "a")

	deps = pbClaim(t, s, rt, ticket.ID)
	secondCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN (answered resume): %v", err)
	}
	if len(secondCommit.Artifacts) != 2 {
		t.Fatalf("second commit.Artifacts has %d entries, want 2", len(secondCommit.Artifacts))
	}
	if len(secondCommit.ResolveQuestions) != 1 {
		t.Fatalf("second commit.ResolveQuestions has %d entries, want 1", len(secondCommit.ResolveQuestions))
	}
	if secondCommit.Session == nil || secondCommit.Session.ID == nil || *secondCommit.Session.ID != questionSession.SessionID {
		t.Errorf("second commit.Session = %+v, want the same session %d the question's own run belonged to", secondCommit.Session, questionSession.SessionID)
	}
	pbApply(t, s, ticket, secondCommit)

	rows, err := s.Verdicts(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("Verdicts: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("Verdicts returned %d rows, want 2", len(rows))
	}

	final := pbGetTicket(t, s, ticket.ID)
	if final.WaitingOn != nil {
		t.Errorf("final ticket WaitingOn = %v, want nil", final.WaitingOn)
	}
}

// ---- TestJudgeErrorEscalatesOriginJudge -------------------------------------

// TestJudgeErrorEscalatesOriginJudge proves the universal error outcome
// (design section 6.8, 7.2 step 6): the agent's own error document
// escalates with origin judge, its own code, what, why, and tried.
func TestJudgeErrorEscalatesOriginJudge(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeErrorScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want one")
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginJudge) {
		t.Errorf("escalation origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginJudge)
	}
	if commit.Escalation.Payload.Code != string(response.ErrorCodeCannotRun) {
		t.Errorf("escalation code = %q, want %q", commit.Escalation.Payload.Code, response.ErrorCodeCannotRun)
	}
	if commit.Escalation.Payload.What != "the scenarios file could not be read" {
		t.Errorf("escalation what = %q", commit.Escalation.Payload.What)
	}
	if commit.Escalation.Payload.Why != "ZING_SCENARIOS_FILE named a path the sandbox denied" {
		t.Errorf("escalation why = %q", commit.Escalation.Payload.Why)
	}
	if commit.Escalation.Payload.Tried != "zing scenarios" {
		t.Errorf("escalation tried = %q", commit.Escalation.Payload.Tried)
	}
}

// ---- TestJudgeInvalidOutputChain --------------------------------------------

// TestJudgeInvalidOutputChain proves D14's own invalid-output chain (design
// section 5.4, 7.2): the first invalid output only marks "response invalid
// run <rid>" and stays (no escalation, no transition); a second consecutive
// invalid output on the resume it triggers escalates response_invalid.
func TestJudgeInvalidOutputChain(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	const invalidReason = "no zing element in final message"
	scripted := &pbScriptedRuntime{t: t, steps: []pbScriptedStep{
		{res: runtime.RunResult{SessionID: "scripted-judge-invalid", ExitCode: -1, AgentTime: time.Second}, err: &runtime.InvalidOutputError{Reason: invalidReason}},
		{res: runtime.RunResult{SessionID: "scripted-judge-invalid", ExitCode: -1, AgentTime: time.Second}, err: &runtime.InvalidOutputError{Reason: invalidReason}},
	}}
	ticket = judgeAdvanceStart(t, s, scripted, ticket)

	deps := pbClaim(t, s, scripted, ticket.ID)
	firstCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN (first turn): %v", err)
	}
	if firstCommit.Next != "" || firstCommit.Escalation != nil {
		t.Fatalf("first commit = {Next: %q, Escalation: %v}, want neither (one invalid output only marks)", firstCommit.Next, firstCommit.Escalation)
	}
	found := false
	for _, m := range firstCommit.Messages {
		if strings.HasPrefix(m.Body, "response invalid run ") {
			found = true
		}
	}
	if !found {
		t.Errorf("first commit.Messages = %+v, want a \"response invalid run \" marker", firstCommit.Messages)
	}
	pbApply(t, s, ticket, firstCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	deps = pbClaim(t, s, scripted, ticket.ID)
	secondCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN (invalid resume): %v", err)
	}
	if secondCommit.Escalation == nil {
		t.Fatal("second commit.Escalation is nil, want response_invalid")
	}
	if secondCommit.Escalation.Payload.Code != string(response.EscalationCodeResponseInvalid) {
		t.Errorf("escalation code = %q, want %q", secondCommit.Escalation.Payload.Code, response.EscalationCodeResponseInvalid)
	}
	if secondCommit.Escalation.Payload.Origin != string(response.EscalationOriginJudge) {
		t.Errorf("escalation origin = %q, want %q", secondCommit.Escalation.Payload.Origin, response.EscalationOriginJudge)
	}
}

// ---- TestJudgeNoSealedScenariosEscalates ------------------------------------

// judgeTicketNoSealedScenarios seeds a git-backed ticket with a stored plan
// (START's own precondition) but zero scenario artifacts, moved straight to
// "judging" through CommitHandlerResult (bypassing job.ValidateCommit's
// legal-edge check, the same pbSeedTicketInState shortcut postbuild_test.go
// already uses): this test cares only about RUN's own first-turn sealed-
// scenario check, not how a ticket really reaches judging with none.
func judgeTicketNoSealedScenarios(t *testing.T) (s *store.Store, ticket store.Ticket) {
	t.Helper()
	s = newPostbuildTestStore(t)
	ticketID := pbSeedQueuedGitBackedTicket(t, s)

	owner := "judge-no-scenarios-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil || !claimed {
		t.Fatalf("judgeTicketNoSealedScenarios: claim: claimed=%v err=%v", claimed, err)
	}
	rsv, err := s.Reserve(t.Context(), ticketID, owner, expires, store.SessionUpsert{Job: string(response.JobPlanning), Runtime: runtimeFake}, store.RunSeed{Model: "fake-model"})
	if err != nil {
		t.Fatalf("judgeTicketNoSealedScenarios: reserve: %v", err)
	}
	_, err = s.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, RunID: &rsv.RunID, Type: "plan", Version: 1, Payload: []byte(judgingTestPlanPayload),
	})
	if err != nil {
		t.Fatalf("judgeTicketNoSealedScenarios: insert plan: %v", err)
	}

	applied, err := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
		TicketID: ticketID, Owner: owner, Expires: expires, Next: stateJudging, Reason: "test setup",
	})
	if err != nil || !applied {
		t.Fatalf("judgeTicketNoSealedScenarios: commit: applied=%v err=%v", applied, err)
	}
	return s, pbGetTicket(t, s, ticketID)
}

// TestJudgeNoSealedScenariosEscalates proves RUN's own step 1 (design
// section 7.2): no cohort, or a cohort with zero sealed scenario rows,
// escalates environment, origin judge, "the ticket has no sealed
// scenarios" -- after START has already run (this check is RUN's, not
// START's own: judging.go's judgeScenariosFor is called only from runFirst
// and the resume branches, never from start).
func TestJudgeNoSealedScenariosEscalates(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketNoSealedScenarios(t)
	rt := pbFakeRuntime(t)
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation is nil, want environment")
	}
	if commit.Escalation.Payload.Code != string(response.EscalationCodeEnvironment) {
		t.Errorf("escalation code = %q, want %q", commit.Escalation.Payload.Code, response.EscalationCodeEnvironment)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginJudge) {
		t.Errorf("escalation origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginJudge)
	}
	if commit.Escalation.Payload.What != judgeNoSealedScenariosWhat {
		t.Errorf("escalation what = %q, want %q", commit.Escalation.Payload.What, judgeNoSealedScenariosWhat)
	}
}

// ---- TestJudgePromptCarriesNoPlan -------------------------------------------

// TestJudgePromptCarriesNoPlan proves N6 (design section 0, 7.2): the judge
// never receives the plan. ForJudge's own signature already carries no
// plan parameter; this proves the recorded prompt RUN actually sent also
// carries none of fixtures/scripts/planning/2.xml's own distinguishing
// plan text.
func TestJudgePromptCarriesNoPlan(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rec := &recordingRuntime{inner: runtime.NewFake(judgeScriptsFS(judgeOkBothScript))}
	ticket = judgeAdvanceStart(t, s, rec, ticket)

	deps := pbClaim(t, s, rec, ticket.ID)
	if _, err := (judgeHandler{}).Run(t.Context(), ticket, deps); err != nil {
		t.Fatalf("RUN: %v", err)
	}

	req := rec.lastRequest(t)
	for _, text := range []string{"greet package", "Greet function", "hello.txt", "<plan>"} {
		if strings.Contains(req.Prompt, text) {
			t.Errorf("judge prompt contains plan text %q:\n%s", text, req.Prompt)
		}
	}
}

// ---- TestJudgeRunsWithJudgeCodexHome ----------------------------------------

// TestJudgeRunsWithJudgeCodexHome proves D27 (design section 7.3): every
// judge run, first turn and resume alike, carries CODEX_HOME=
// <Deps.JudgeCodexHome> in req.Env.
func TestJudgeRunsWithJudgeCodexHome(t *testing.T) {
	t.Parallel()
	const wantHome = "/test/judge/codex/home"

	s, ticket := judgeTicketReady(t)
	incomplete := judgeOkScript(judgeVerdictXML("s1", "ran it"))
	rec := &recordingRuntime{inner: runtime.NewFake(judgeScriptsFS(incomplete, judgeOkBothScript))}

	startDeps := pbClaim(t, s, rec, ticket.ID)
	startDeps.JudgeCodexHome = wantHome
	startCommit, err := (judgeHandler{}).Run(t.Context(), ticket, startDeps)
	if err != nil {
		t.Fatalf("START: %v", err)
	}
	pbApply(t, s, ticket, startCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	firstDeps := pbClaim(t, s, rec, ticket.ID)
	firstDeps.JudgeCodexHome = wantHome
	firstCommit, err := (judgeHandler{}).Run(t.Context(), ticket, firstDeps)
	if err != nil {
		t.Fatalf("RUN (first turn): %v", err)
	}
	pbApply(t, s, ticket, firstCommit)
	ticket = pbGetTicket(t, s, ticket.ID)

	resumeDeps := pbClaim(t, s, rec, ticket.ID)
	resumeDeps.JudgeCodexHome = wantHome
	if _, err := (judgeHandler{}).Run(t.Context(), ticket, resumeDeps); err != nil {
		t.Fatalf("RUN (coverage resume): %v", err)
	}

	rec.mu.Lock()
	reqs := append([]runtime.RunRequest{}, rec.reqs...)
	rec.mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("recorded %d requests, want 2 (first turn, coverage resume)", len(reqs))
	}
	wantFlag := "CODEX_HOME=" + wantHome
	for i, req := range reqs {
		if !slices.Contains(req.Env, wantFlag) {
			t.Errorf("request %d Env = %v, want it to contain %q", i, req.Env, wantFlag)
		}
	}
}

// ---- TestJudgeWorktreeRemoveFailureLogged (deferred from M2 task 4) --------

// judgeFailingRemoveRunner wraps a real orchestrator.Runner and fails every
// "git worktree remove" call past its own skipRemoves allowance: JudgeWorktree
// itself issues one such call before ever creating a checkout (orchestrator/
// judge.go's own step 1, removing a crash's leftover), on a path nothing has
// touched yet, so skipRemoves 1 lets that one succeed for real (git's own
// harmless "is not a working tree") and fails only the next one -- the
// judging handler's own deferred JudgeTree.Remove.
type judgeFailingRemoveRunner struct {
	inner       orchestrator.Runner
	skipRemoves int
}

func (r *judgeFailingRemoveRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	if name == "git" && slices.Contains(args, "remove") {
		if r.skipRemoves > 0 {
			r.skipRemoves--
		} else {
			return "fatal: forced by test", errors.New("judgeFailingRemoveRunner: forced git worktree remove failure")
		}
	}
	return r.inner.Run(ctx, dir, name, args...)
}

func (r *judgeFailingRemoveRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	return r.inner.Output(ctx, dir, name, args...)
}

// TestJudgeWorktreeRemoveFailureLogged proves a deferred JudgeTree.Remove
// error logs at WARN with ticket_id and run_id (PKG9-PLAN.md section 10.2,
// deferred from M2 task 4 to this task). Not parallel: it calls
// slog.SetDefault to capture a log line, which swaps the process-wide
// default logger (runjob_test.go's own TestRunJobWithHookCleanupFailureLogged
// is the pattern this mirrors).
func TestJudgeWorktreeRemoveFailureLogged(t *testing.T) {
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	proj, err := s.ProjectForTicket(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("ProjectForTicket: %v", err)
	}
	// skipRemoves 1 lets JudgeWorktree's own leftover-removal call (step 1,
	// nothing exists there yet) go through to the real runner, and fails
	// only the next "git worktree remove" call: the judging handler's own
	// deferred JudgeTree.Remove.
	failing := &judgeFailingRemoveRunner{inner: orchestrator.NewRunner(), skipRemoves: 1}
	orch, repoGit, ok := pbOrchestratorFor(t, proj.LocalPath, failing)
	if !ok {
		t.Fatal("pbOrchestratorFor: not a git repository")
	}

	deps := pbClaim(t, s, rt, ticket.ID)
	deps.Projects = map[int64]Project{ticket.ProjectID: {Orch: orch, RepoGit: repoGit, TestCmd: deps.Projects[ticket.ProjectID].TestCmd, LintCmd: deps.Projects[ticket.ProjectID].LintCmd}}

	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if len(commit.Artifacts) != 2 {
		t.Fatalf("commit.Artifacts has %d entries, want 2 (the removal failure must not replace the run's own result)", len(commit.Artifacts))
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "judge worktree removal failed") {
		t.Fatalf("log missing \"judge worktree removal failed\"; got:\n%s", logged)
	}
	if !strings.Contains(logged, fmt.Sprintf("ticket_id=%d", ticket.ID)) {
		t.Errorf("log missing ticket_id=%d; got:\n%s", ticket.ID, logged)
	}
	if !strings.Contains(logged, fmt.Sprintf("run_id=%d", commit.Runs[0].ID)) {
		t.Errorf("log missing run_id=%d; got:\n%s", commit.Runs[0].ID, logged)
	}
}

// ---- M2 task 8: CHECK, EVALUATE, and judge escalations --------------------

// judgeCheckScenarioCmd is the fixture cohort's own one check command
// (fixtures/scripts/planning/2.xml's own s1, design section 7.5): every
// CHECK test below scripts exactly this command's own result instead of
// letting a real curl dial a server this suite never starts.
const judgeCheckScenarioCmd = "curl -sf localhost:8080/hello"

// judgeCheckStep is one scripted result judgeScriptedCheckCommands returns
// for one call to judgeCheckScenarioCmd, in order.
type judgeCheckStep struct {
	exit int
	err  error
}

// judgeScriptedCheckCommands wraps a real CommandRunner (real for any
// command that is not judgeCheckScenarioCmd, nil for a test that never
// exercises the fix driver's own CHECK step) so CHECK's own re-run of the
// fixture cohort's one check command returns a scripted result instead of
// dialing out. gotDir is the last dir CHECK ran judgeCheckScenarioCmd in,
// for TestJudgeCheckRunsInFreshCheckout.
type judgeScriptedCheckCommands struct {
	real   CommandRunner
	steps  []judgeCheckStep
	i      int
	gotDir string
}

func (c *judgeScriptedCheckCommands) Run(ctx context.Context, dir, repoGit, shellCmd string, timeout time.Duration) (int, error) {
	if shellCmd == judgeCheckScenarioCmd {
		if c.i >= len(c.steps) {
			return 0, fmt.Errorf("judgeScriptedCheckCommands: no more scripted steps for %q", shellCmd)
		}
		step := c.steps[c.i]
		c.i++
		c.gotDir = dir
		return step.exit, step.err
	}
	if c.real != nil {
		return c.real.Run(ctx, dir, repoGit, shellCmd, timeout)
	}
	return 0, nil
}

// TestJudgeCheckOverridesVerdict proves CHECK's own override row (design
// section 7.4, 7.5): the judge said pass, the re-run exits 1, and the
// scenario's newest verdict row becomes fail, CheckExit 1, with the
// re-run note appended to the judge's own evidence.
func TestJudgeCheckOverridesVerdict(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	pbApply(t, s, ticket, runCommit)

	checks := &judgeScriptedCheckCommands{steps: []judgeCheckStep{{exit: 1}}}
	deps2 := pbClaim(t, s, rt, ticket.ID)
	deps2.Commands = checks
	checkCommit, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2)
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if len(checkCommit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %+v, want exactly one override row", checkCommit.Artifacts)
	}
	var override response.VerdictArtifact
	if unmarshalErr := json.Unmarshal(checkCommit.Artifacts[0].Payload, &override); unmarshalErr != nil {
		t.Fatalf("unmarshal override: %v", unmarshalErr)
	}
	if override.Scenario != "s1" {
		t.Errorf("override.Scenario = %q, want s1", override.Scenario)
	}
	if override.Result != response.ResultFail {
		t.Errorf("override.Result = %q, want fail", override.Result)
	}
	if override.CheckExit == nil || *override.CheckExit != 1 {
		t.Errorf("override.CheckExit = %v, want 1", override.CheckExit)
	}
	if !strings.HasSuffix(override.Evidence, "Zing re-ran the check command: exit 1.") {
		t.Errorf("override.Evidence = %q, want it to end with the re-run note", override.Evidence)
	}
	pbApply(t, s, ticket, checkCommit)

	marker, ok := reviewMarker(t, s, ticket.ID, "judge check 1 s1 exit")
	if !ok {
		t.Fatal(`no "judge check 1 s1 exit" marker`)
	}
	if marker.Body != "judge check 1 s1 exit 1" {
		t.Errorf("marker body = %q, want %q", marker.Body, "judge check 1 s1 exit 1")
	}

	rows, err := s.Verdicts(t.Context(), ticket.ID)
	if err != nil {
		t.Fatalf("Verdicts: %v", err)
	}
	row, ok := judgeNewestVerdict(rows, 1, "s1")
	if !ok {
		t.Fatal("no newest verdict for s1 round 1")
	}
	if row.Verdict.Result != response.ResultFail {
		t.Errorf("newest s1 verdict result = %q, want fail (the override must win)", row.Verdict.Result)
	}
}

// TestJudgeCheckTimeout proves CHECK's own timeout row (design section
// 7.5 step 3): ErrCommandTimeout becomes exit -1, CheckExit -1, and the
// evidence's own re-run note names the timeout, not an exit code.
func TestJudgeCheckTimeout(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	pbApply(t, s, ticket, runCommit)

	checks := &judgeScriptedCheckCommands{steps: []judgeCheckStep{{exit: 0, err: ErrCommandTimeout}}}
	deps2 := pbClaim(t, s, rt, ticket.ID)
	deps2.Commands = checks
	checkCommit, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2)
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if len(checkCommit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %+v, want exactly one override row", checkCommit.Artifacts)
	}
	var override response.VerdictArtifact
	if unmarshalErr := json.Unmarshal(checkCommit.Artifacts[0].Payload, &override); unmarshalErr != nil {
		t.Fatalf("unmarshal override: %v", unmarshalErr)
	}
	if override.CheckExit == nil || *override.CheckExit != -1 {
		t.Errorf("override.CheckExit = %v, want -1", override.CheckExit)
	}
	if !strings.HasSuffix(override.Evidence, "Zing re-ran the check command: timed out after 10m.") {
		t.Errorf("override.Evidence = %q, want it to end with the timeout note", override.Evidence)
	}
	pbApply(t, s, ticket, checkCommit)

	marker, ok := reviewMarker(t, s, ticket.ID, "judge check 1 s1 exit")
	if !ok {
		t.Fatal(`no "judge check 1 s1 exit" marker`)
	}
	if marker.Body != "judge check 1 s1 exit -1" {
		t.Errorf("marker body = %q, want %q", marker.Body, "judge check 1 s1 exit -1")
	}
}

// TestJudgeCheckRunsInFreshCheckout proves CHECK opens its own judge
// checkout at the round's own frozen sha and removes it afterward (design
// section 7.5 step 1), independent of RUN's own checkout, already removed
// by the time CHECK runs.
func TestJudgeCheckRunsInFreshCheckout(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	pbApply(t, s, ticket, runCommit)
	assertJudgeWorktreeGone(t, s, ticket.ID) // RUN's own checkout is already gone

	wantDir := judgeWorktreeDir(t, s, ticket.ID)
	checks := &judgeScriptedCheckCommands{steps: []judgeCheckStep{{exit: 0}}}
	deps2 := pbClaim(t, s, rt, ticket.ID)
	deps2.Commands = checks
	checkCommit, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2)
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	if checks.gotDir != wantDir {
		t.Errorf("CHECK ran the check command in %q, want the judge checkout %q", checks.gotDir, wantDir)
	}
	pbApply(t, s, ticket, checkCommit)
	assertJudgeWorktreeGone(t, s, ticket.ID)
}

// TestJudgePassMovesToShipping proves EVALUATE's own pass branch (design
// section 7.6): every scenario pass (s1's own check confirms it) moves the
// ticket to shipping with the "judge passed" reason and writes "judge
// round 1 passed".
func TestJudgePassMovesToShipping(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	pbApply(t, s, ticket, runCommit)

	checks := &judgeScriptedCheckCommands{steps: []judgeCheckStep{{exit: 0}}}
	deps2 := pbClaim(t, s, rt, ticket.ID)
	deps2.Commands = checks
	checkCommit, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2)
	if err != nil {
		t.Fatalf("CHECK: %v", err)
	}
	pbApply(t, s, ticket, checkCommit)

	deps3 := pbClaim(t, s, rt, ticket.ID)
	evalCommit, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps3)
	if err != nil {
		t.Fatalf("EVALUATE: %v", err)
	}
	if evalCommit.Next != stateShipping || evalCommit.Reason != reasonJudgePassed {
		t.Errorf("commit = (Next=%q, Reason=%q), want (shipping, %q)", evalCommit.Next, evalCommit.Reason, reasonJudgePassed)
	}
	pbApply(t, s, ticket, evalCommit)

	marker, ok := reviewMarker(t, s, ticket.ID, "judge round 1 passed")
	if !ok {
		t.Fatal(`no "judge round 1 passed" marker`)
	}
	if marker.Body != "judge round 1 passed" {
		t.Errorf("marker body = %q, want %q", marker.Body, "judge round 1 passed")
	}
	if final := pbGetTicket(t, s, ticket.ID); final.State != stateShipping {
		t.Errorf("final ticket state = %q, want shipping", final.State)
	}
}

// TestJudgePerformanceNeverBlocks proves EVALUATE routes on JudgePasses
// (design section 7.4, 7.6), not a literal all-pass check: a failing
// performance row never blocks the pass. evaluate is called directly
// (its pass branch touches no store read), so this proves EVALUATE's own
// commit, not just the already-proven JudgePasses rule (judgerules_test.go's
// own TestJudgePasses).
func TestJudgePerformanceNeverBlocks(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	deps := pbClaim(t, s, pbFakeRuntime(t), ticket.ID)
	sha := strings.Repeat("b", 40)
	final := []response.VerdictArtifact{
		{Scenario: "s1", Result: response.ResultPass, Evidence: "ok", Kind: response.ScenarioKindBehavior, Round: 1, SHA: sha},
		{Scenario: "s2", Result: response.ResultPass, Evidence: "ok", Kind: response.ScenarioKindNegative, Round: 1, SHA: sha},
		{Scenario: "s3", Result: response.ResultFail, Evidence: "slow", Kind: response.ScenarioKindPerformance, Round: 1, SHA: sha},
	}
	commit, err := (judgeHandler{}).evaluate(t.Context(), ticket, deps, 1, final)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if commit.Next != stateShipping || commit.Reason != reasonJudgePassed {
		t.Errorf("commit = (Next=%q, Reason=%q), want (shipping, %q): a failing performance row must never block the pass", commit.Next, commit.Reason, reasonJudgePassed)
	}
}

// TestJudgeFailRequestsFixWithoutScenarioText proves EVALUATE's own fail
// branch under the loop gate (design section 7.6): the failed-ids marker,
// then a "failure" fix request whose text is scrubbed of the scenario's
// own given/when/then/check text (judgerules.go's scrubScenarioText,
// reused here through renderFixFailures), so a fix run never sees what
// the scenario actually checks.
func TestJudgeFailRequestsFixWithoutScenarioText(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	deps := pbClaim(t, s, pbFakeRuntime(t), ticket.ID)
	sha := strings.Repeat("c", 40)
	const s1Then = `the response is 200 with body "hello, world"`
	final := []response.VerdictArtifact{
		{Scenario: "s1", Result: response.ResultFail, Evidence: "ran curl; expected " + s1Then + ", got connection refused", Kind: response.ScenarioKindBehavior, Round: 1, SHA: sha},
		{Scenario: "s2", Result: response.ResultPass, Evidence: "ok", Kind: response.ScenarioKindNegative, Round: 1, SHA: sha},
	}
	commit, err := (judgeHandler{}).evaluate(t.Context(), ticket, deps, 1, final)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(commit.Messages) != 2 {
		t.Fatalf("commit.Messages = %+v, want [failed marker, fix request]", commit.Messages)
	}
	if commit.Messages[0].Body != "judge round 1 failed\ns1" {
		t.Errorf("failed marker = %q, want %q", commit.Messages[0].Body, "judge round 1 failed\ns1")
	}
	fixBody := commit.Messages[1].Body
	if !strings.HasPrefix(fixBody, "fix requested failure after run ") {
		t.Errorf("fix request body = %q, want the fix requested failure prefix", fixBody)
	}
	if strings.Contains(fixBody, s1Then) {
		t.Errorf("fix request body contains the raw scenario text %q, want it scrubbed", s1Then)
	}
	if !strings.Contains(fixBody, "[scenario text removed]") {
		t.Errorf("fix request body = %q, want it to contain the scrub marker", fixBody)
	}
	if !strings.Contains(fixBody, "s1: ") {
		t.Errorf("fix request body = %q, want the s1 block", fixBody)
	}
}

// judgeSeedFixRequestedFailureMarkers inserts n raw "fix requested failure
// after run <i>" markers directly (fixRequestMessage), so a loop-gate test
// can put the ticket at exactly k open-or-closed requests without actually
// driving k fix rounds to landing.
func judgeSeedFixRequestedFailureMarkers(t *testing.T, s *store.Store, ticket store.Ticket, n int) {
	t.Helper()
	for i := range n {
		msg, msgErr := fixRequestMessage(ticket, FixKindFailure, fmt.Sprintf("seed failure %d", i), int64(i))
		if msgErr != nil {
			t.Fatalf("fixRequestMessage: %v", msgErr)
		}
		if _, insertErr := s.InsertMessage(t.Context(), msg); insertErr != nil {
			t.Fatalf("InsertMessage: %v", insertErr)
		}
	}
}

// TestJudgeLoopGate proves EVALUATE's own loop gate (design section 7.6):
// at jobs.judge.max_loops (machine.toml's judge job, 2) already-seen "fix
// requested failure" markers, a further failing round escalates
// loops_exhausted instead of opening a third request.
func TestJudgeLoopGate(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	judgeSeedFixRequestedFailureMarkers(t, s, ticket, 2)

	deps := pbClaim(t, s, pbFakeRuntime(t), ticket.ID)
	sha := strings.Repeat("d", 40)
	final := []response.VerdictArtifact{
		{Scenario: "s1", Result: response.ResultFail, Evidence: "still failing", Kind: response.ScenarioKindBehavior, Round: 1, SHA: sha},
		{Scenario: "s2", Result: response.ResultPass, Evidence: "ok", Kind: response.ScenarioKindNegative, Round: 1, SHA: sha},
	}
	commit, err := (judgeHandler{}).evaluate(t.Context(), ticket, deps, 1, final)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if commit.Escalation == nil {
		t.Fatal("commit.Escalation = nil, want loops_exhausted")
	}
	if commit.Escalation.Payload.Code != string(response.EscalationCodeLoopsExhausted) {
		t.Errorf("escalation code = %q, want %q", commit.Escalation.Payload.Code, response.EscalationCodeLoopsExhausted)
	}
	if commit.Escalation.Payload.Origin != string(response.EscalationOriginJudge) {
		t.Errorf("escalation origin = %q, want %q", commit.Escalation.Payload.Origin, response.EscalationOriginJudge)
	}
	if len(commit.Messages) != 1 || commit.Messages[0].Body != "judge round 1 failed\ns1" {
		t.Errorf("commit.Messages = %+v, want exactly the failed marker", commit.Messages)
	}
}

// TestJudgeLoopsRetry proves resolvePostBuildEscalation's own "judge
// loops_exhausted" row (design section 5.6): the owner's retry opens a
// "failure" fix request carrying the escalation's own Tried text, the loop
// gate skipped for this one request, with no fresh judge run.
func TestJudgeLoopsRetry(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)

	qID := pbEscalateDirect(t, s, ticket.ID, nil, nil, response.EscalationCodeLoopsExhausted, response.EscalationOriginJudge)
	pbAnswerEscalation(t, s, ticket.ID, qID, "a")

	deps := pbClaim(t, s, pbFakeRuntime(t), ticket.ID)
	commit, handled := pbRunPrelude(t, s, deps, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(commit.Runs) != 0 {
		t.Errorf("commit.Runs = %+v, want none (no fresh judge run for loops_exhausted)", commit.Runs)
	}
	if len(commit.Messages) != 1 {
		t.Fatalf("commit.Messages = %+v, want exactly one fix request message", commit.Messages)
	}
	if !strings.HasPrefix(commit.Messages[0].Body, "fix requested failure after run ") {
		t.Errorf("message body = %q, want the fix requested failure prefix", commit.Messages[0].Body)
	}
	if !strings.Contains(commit.Messages[0].Body, "what was tried") {
		t.Errorf("message body = %q, want the escalation's own Tried text", commit.Messages[0].Body)
	}
}

// TestJudgeCapResumesRetryStartsFresh proves resolvePostBuildEscalation's
// own "cap_resumes, exhausted session of job judge" row (design section
// 5.6): the retry writes "judge round <n> retry after run <R>" with no
// fresh run of its own; the next tick's own decision tree reads it as a
// fresh start of round n at that round's own sha, reserving a brand-new
// session rather than resuming the exhausted one.
func TestJudgeCapResumesRetryStartsFresh(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)
	rt := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	ticket = judgeAdvanceStart(t, s, rt, ticket)

	deps := pbClaim(t, s, rt, ticket.ID)
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if runCommit.Session == nil || runCommit.Session.ID == nil {
		t.Fatalf("RUN commit.Session = %+v, want a freshly reserved session id", runCommit.Session)
	}
	sessionID := *runCommit.Session.ID
	pbApply(t, s, ticket, runCommit)

	owner := "judge-cap-owner"
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	maxResumes := pbMachine(t).Jobs[jobJudgeName].MaxResumes
	for range maxResumes {
		claimed, claimErr := s.Claim(t.Context(), ticket.ID, owner, expires)
		if claimErr != nil || !claimed {
			t.Fatalf("bump claim: claimed=%v err=%v", claimed, claimErr)
		}
		applied, bumpErr := s.CommitHandlerResult(t.Context(), store.HandlerCommit{
			TicketID: ticket.ID, Owner: owner, Expires: expires,
			Session: &store.SessionUpsert{ID: &sessionID, BumpResumes: true},
		})
		if bumpErr != nil || !applied {
			t.Fatalf("bump CommitHandlerResult: applied=%v err=%v", applied, bumpErr)
		}
	}

	qID := pbEscalateDirect(t, s, ticket.ID, nil, &sessionID, response.EscalationCodeResumesExhausted, response.EscalationOriginCapResumes)
	pbAnswerEscalation(t, s, ticket.ID, qID, "a")

	deps2 := pbClaim(t, s, rt, ticket.ID)
	retryCommit, handled := pbRunPrelude(t, s, deps2, ticket.ID)
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if len(retryCommit.Runs) != 0 {
		t.Errorf("retryCommit.Runs = %+v, want none (deferred to the next tick)", retryCommit.Runs)
	}
	if len(retryCommit.Messages) != 1 || !strings.HasPrefix(retryCommit.Messages[0].Body, "judge round 1 retry after run ") {
		t.Fatalf("retryCommit.Messages = %+v, want exactly the retry marker", retryCommit.Messages)
	}

	rt2 := runtime.NewFake(judgeScriptsFS(judgeOkBothScript))
	deps3 := pbClaim(t, s, rt2, ticket.ID)
	freshCommit, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps3)
	if err != nil {
		t.Fatalf("Run after retry: %v", err)
	}
	if len(freshCommit.Runs) != 1 {
		t.Fatalf("freshCommit.Runs = %+v, want exactly one (a fresh first turn)", freshCommit.Runs)
	}
	if freshCommit.Session == nil || freshCommit.Session.ID == nil || *freshCommit.Session.ID == sessionID {
		t.Errorf("freshCommit.Session = %+v, want a freshly minted session (not the exhausted one, %d)", freshCommit.Session, sessionID)
	}
}

// judgeFixTestCmd is the fix driver's own project.TestCmd override (design
// section 5.3's own adopt/CHECK pattern, pbFixTestCmd's own trick):
// appends a line to hello.txt, which the earlier real build already
// created with the exact content pbFixTestCmd itself writes, so reusing
// that constant here would leave the tree byte-identical and CHECK would
// see no actual change for the claimed path.
const judgeFixTestCmd = `printf '\nfixed\n' >> hello.txt && test -f hello.txt`

// judgeFixBuildScript is the fix driver's own RUN turn (job "build", label
// "fix", design section 5.1): one claimed file change, hello.txt, matching
// judgeFixTestCmd's own edit, so CHECK's own cross-check of claimed against
// real changed paths agrees.
const judgeFixBuildScript = `<zing job="build" outcome="ok">
  <claims>
    <files_changed>
      <path>hello.txt</path>
    </files_changed>
    <test_exit>0</test_exit>
    <lint_exit>0</lint_exit>
  </claims>
  <report>Fixed the failing scenario's own check.</report>
  <notes></notes>
</zing>`

// driveJudgeFixToLanding drives an already-open "failure" fix request
// through the fix driver's own RUN then CHECK-and-LAND ticks (fix.go's own
// DriveFix, reached through judgeHandler.Run's own postBuildPrelude),
// mirroring reviewing_test.go's own driveReviewFixToLanding.
func driveJudgeFixToLanding(t *testing.T, s *store.Store, ticketID int64, rt runtime.Runtime, cmds CommandRunner) {
	t.Helper()
	for i := range 4 {
		ticket := pbGetTicket(t, s, ticketID)
		deps := pbWithTestCmd(pbClaim(t, s, rt, ticketID), ticket, judgeFixTestCmd)
		deps.Commands = cmds
		commit, err := (judgeHandler{}).Run(t.Context(), ticket, deps)
		if err != nil {
			t.Fatalf("driveJudgeFixToLanding: Run (step %d): %v", i, err)
		}
		pbApply(t, s, ticket, commit)
		for _, m := range commit.Messages {
			if strings.HasPrefix(m.Body, "fix landed ") {
				return
			}
		}
	}
	t.Fatal("driveJudgeFixToLanding: fix did not land within 4 ticks")
}

// TestJudgeFixThenPass proves the full judge-fails/fix/judge-passes loop
// (design section 13.2): round 1's own CHECK fails s1, EVALUATE requests a
// "failure" fix, the fix driver lands it, round 2 starts fresh, and its
// own CHECK passes s1, so EVALUATE moves the ticket to shipping.
func TestJudgeFixThenPass(t *testing.T) {
	t.Parallel()
	s, ticket := judgeTicketReady(t)

	scripts := judgeScriptsFS(judgeOkBothScript)
	scripts["judge/2/1.xml"] = &fstest.MapFile{Data: []byte(judgeOkBothScript)}
	scripts["build/fix/1.xml"] = &fstest.MapFile{Data: []byte(judgeFixBuildScript)}
	rt := runtime.NewFake(scripts)

	checks := &judgeScriptedCheckCommands{real: NewCommandRunner(sandbox.Off(), false), steps: []judgeCheckStep{{exit: 1}, {exit: 0}}}

	ticket = judgeAdvanceStart(t, s, rt, ticket) // START round 1

	deps := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), ticket, judgeFixTestCmd)
	deps.Commands = checks
	runCommit, err := (judgeHandler{}).Run(t.Context(), ticket, deps) // RUN round 1
	if err != nil {
		t.Fatalf("RUN round 1: %v", err)
	}
	pbApply(t, s, ticket, runCommit)

	deps2 := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), pbGetTicket(t, s, ticket.ID), judgeFixTestCmd)
	deps2.Commands = checks
	checkCommit, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps2) // CHECK s1: exit 1
	if err != nil {
		t.Fatalf("CHECK round 1: %v", err)
	}
	pbApply(t, s, ticket, checkCommit)

	deps3 := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), pbGetTicket(t, s, ticket.ID), judgeFixTestCmd)
	deps3.Commands = checks
	evalCommit, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps3) // EVALUATE: fail, fix request
	if err != nil {
		t.Fatalf("EVALUATE round 1: %v", err)
	}
	if len(evalCommit.Messages) != 2 || !strings.HasPrefix(evalCommit.Messages[1].Body, "fix requested failure after run ") {
		t.Fatalf("EVALUATE round 1 commit.Messages = %+v, want [failed marker, fix requested failure]", evalCommit.Messages)
	}
	pbApply(t, s, ticket, evalCommit)

	driveJudgeFixToLanding(t, s, ticket.ID, rt, checks)

	ticket = pbGetTicket(t, s, ticket.ID)
	if ticket.State != stateJudging {
		t.Fatalf("after the fix landed: ticket state = %q, want judging", ticket.State)
	}

	deps4 := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), ticket, judgeFixTestCmd)
	deps4.Commands = checks
	startCommit2, err := (judgeHandler{}).Run(t.Context(), ticket, deps4) // START round 2
	if err != nil {
		t.Fatalf("START round 2: %v", err)
	}
	pbApply(t, s, ticket, startCommit2)
	if _, ok := reviewMarker(t, s, ticket.ID, "judge round 2 started"); !ok {
		t.Fatal(`no "judge round 2 started" marker`)
	}

	deps5 := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), pbGetTicket(t, s, ticket.ID), judgeFixTestCmd)
	deps5.Commands = checks
	runCommit2, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps5) // RUN round 2
	if err != nil {
		t.Fatalf("RUN round 2: %v", err)
	}
	pbApply(t, s, ticket, runCommit2)

	deps6 := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), pbGetTicket(t, s, ticket.ID), judgeFixTestCmd)
	deps6.Commands = checks
	checkCommit2, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps6) // CHECK s1: exit 0
	if err != nil {
		t.Fatalf("CHECK round 2: %v", err)
	}
	pbApply(t, s, ticket, checkCommit2)

	deps7 := pbWithTestCmd(pbClaim(t, s, rt, ticket.ID), pbGetTicket(t, s, ticket.ID), judgeFixTestCmd)
	deps7.Commands = checks
	evalCommit2, err := (judgeHandler{}).Run(t.Context(), pbGetTicket(t, s, ticket.ID), deps7) // EVALUATE round 2: pass
	if err != nil {
		t.Fatalf("EVALUATE round 2: %v", err)
	}
	if evalCommit2.Next != stateShipping || evalCommit2.Reason != reasonJudgePassed {
		t.Errorf("round 2 commit = (Next=%q, Reason=%q), want (shipping, %q)", evalCommit2.Next, evalCommit2.Reason, reasonJudgePassed)
	}
}
