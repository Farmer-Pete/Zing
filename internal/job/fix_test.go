// fix_test.go tests task 1: the fix unit's identity, requests, and resume
// path (design section 5.2, 5.3, D18, D22, #28 gap 1). It reuses
// skeleton_test.go and building_test.go's shared fixtures (newJobTestStore,
// claim, apply, getTicket, buildTicketInBuilding, claimForBuild, buildStep,
// helloTxt, testNoopShellCmd, scriptedRuntime, recordingRuntime,
// assertFenced, questionResult, findOpenQuestionByKind, invalidResult) and
// drives job.DriveFix directly: it is not wired into job.Registry() yet
// (design section 5.5's post-build prelude, task 3), so there is no
// ticket-state handler to go through. openFixRequest and fixRequestMessage
// are unexported (fix_internal_test.go, package job); this file writes a
// request marker directly, in fixRequestMessage's own documented format,
// and builds job.FixRequest literals since every one of its fields is
// exported.
package job_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"zing/internal/job"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// fixTestCmd is the shell test command this file's own scenarios override
// TestCmd with: it writes hello.txt for real, the same technique
// building_test.go's own perimeterScenario uses, since a scriptedRuntime's
// buildStep carries no fake-runtime tree effect of its own (design section
// 9.3) to create the file its claim names.
const fixTestCmd = "printf 'hello, world\\n' > hello.txt && test -f hello.txt"

// fixTestCmd2 is fixTestCmd with different content, for
// TestSecondFixAdvancesAfterFirstLanded's own second fix unit: it must
// still claim hello.txt (a plan-declared path, so CHECK never routes it
// through DESCRIBE/ASK as an undeclared extra), but write different bytes,
// since a repeat of fixTestCmd's own write after the first fix has already
// landed hello.txt with identical content leaves nothing for git to see as
// changed.
const fixTestCmd2 = "printf 'second fix\\n' > hello.txt && test -f hello.txt"

// testFixCILogText is the fixed fix-request text every FixKindCILog
// scenario in this file (and building_test.go's own cross-unit fix
// scenario) uses when the exact wording does not matter, named once so
// goconst has nothing to flag across the two files.
const testFixCILogText = "log tail"

// testFixLabel is runtime.RunRequest.Label for every fix run, first turn
// and every resume alike (design section 6.3's own table), named once so
// goconst has nothing to flag across this file's repeated assertions.
const testFixLabel = "fix"

// withFixTestCmd returns deps with its one project's TestCmd overridden to
// fixTestCmd and LintCmd to testNoopShellCmd, keyed by ticket's own
// project id.
func withFixTestCmd(deps job.Deps, ticket store.Ticket) job.Deps {
	proj := deps.Projects[ticket.ProjectID]
	proj.TestCmd = fixTestCmd
	proj.LintCmd = testNoopShellCmd
	deps.Projects = map[int64]job.Project{ticket.ProjectID: proj}
	return deps
}

// withFixTestCmd2 is withFixTestCmd for fixTestCmd2 (a distinct file), the
// ticket's second, independent fix unit.
func withFixTestCmd2(deps job.Deps, ticket store.Ticket) job.Deps {
	proj := deps.Projects[ticket.ProjectID]
	proj.TestCmd = fixTestCmd2
	proj.LintCmd = testNoopShellCmd
	deps.Projects = map[int64]job.Project{ticket.ProjectID: proj}
	return deps
}

// writeFixRequestMarker inserts a "fix requested <kind> after run <R>"
// marker directly, fixRequestMessage's own documented format (design
// section 5.1, 5.2): job_test cannot call that unexported function, so
// this rebuilds its exact wire shape. It returns the marker's own message
// id, a fix request's identity (FixRequest.MessageID).
func writeFixRequestMarker(t *testing.T, s *store.Store, ticketID int64, kind job.FixKind, text string, afterRunID int64) int64 {
	t.Helper()
	body := fmt.Sprintf("fix requested %s after run %d\n%s", kind, afterRunID, text)
	id, err := s.InsertMessage(t.Context(), store.Message{TicketID: ticketID, Type: "update", Author: "system", Body: body})
	if err != nil {
		t.Fatalf("insert fix request marker: %v", err)
	}
	return id
}

// TestFixKindThreadsSubject proves design section 8's own table for all
// three kinds: the run request's own Label is "fix" regardless of kind, the
// prompt carries the kind's own input label with the fix text fenced, and
// the landed build_report's Title is the kind's own commit subject with
// TaskN 0.
func TestFixKindThreadsSubject(t *testing.T) {
	for _, tc := range []struct {
		kind    job.FixKind
		subject string
		label   string
	}{
		{job.FixKindFindings, "Fix review findings", "findings"},
		{job.FixKindFailure, "Fix failed scenarios", "failure"},
		{job.FixKindCILog, "Fix the failing check", "ci_log"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			s, _, ticketID := buildTicketInBuilding(t)
			mid := writeFixRequestMarker(t, s, ticketID, tc.kind, "do the thing", 0)
			ticket := getTicket(t, s, ticketID)

			scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "fix-sess-"+string(tc.kind))}}
			rec := &recordingRuntime{rt: scriptRT}
			deps := claimForBuild(t, s, rec, ticketID)

			req := job.FixRequest{MessageID: mid, Kind: tc.kind, Text: "do the thing", AfterRunID: 0}
			commit, err := job.DriveFix(t.Context(), ticket, deps, req)
			if err != nil {
				t.Fatalf("DriveFix: %v", err)
			}
			if rec.lastReq.Label != testFixLabel {
				t.Errorf("RunRequest.Label = %q, want \"fix\"", rec.lastReq.Label)
			}
			assertFenced(t, rec.lastReq.Prompt, tc.label, "do the thing")

			if len(commit.Artifacts) != 1 {
				t.Fatalf("commit.Artifacts = %+v, want one build_report", commit.Artifacts)
			}
			var report response.BuildReport
			if unmarshalErr := json.Unmarshal(commit.Artifacts[0].Payload, &report); unmarshalErr != nil {
				t.Fatalf("unmarshal build_report: %v", unmarshalErr)
			}
			if report.Title != tc.subject {
				t.Errorf("report.Title = %q, want %q", report.Title, tc.subject)
			}
			if report.TaskN != 0 {
				t.Errorf("report.TaskN = %d, want 0", report.TaskN)
			}
		})
	}
}

// TestDriveFixRunsFirstTurn proves design section 5.3 step 1's "none" case:
// with no session yet after the request's own watermark, DriveFix runs
// RUN's first turn labeled "fix", with req.Text fenced, and the stored run
// carries a nil task_n (design section 8's own storage rule, never 0, even
// though its build_report's own TaskN field is 0).
func TestDriveFixRunsFirstTurn(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	mid := writeFixRequestMarker(t, s, ticketID, job.FixKindCILog, testFixCILogText, 0)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "fix-first-sess")}}
	rec := &recordingRuntime{rt: scriptRT}
	deps := claimForBuild(t, s, rec, ticketID)

	req := job.FixRequest{MessageID: mid, Kind: job.FixKindCILog, Text: testFixCILogText, AfterRunID: 0}
	commit, err := job.DriveFix(t.Context(), ticket, deps, req)
	if err != nil {
		t.Fatalf("DriveFix: %v", err)
	}
	if rec.lastReq.Label != testFixLabel {
		t.Errorf("RunRequest.Label = %q, want \"fix\"", rec.lastReq.Label)
	}
	assertFenced(t, rec.lastReq.Prompt, "ci_log", testFixCILogText)
	if len(commit.Runs) != 1 {
		t.Fatalf("commit.Runs = %+v, want exactly one", commit.Runs)
	}
	runID := commit.Runs[0].ID
	apply(t, s, ticket, commit)

	runs, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	found := false
	for _, r := range runs {
		if r.ID == runID {
			found = true
			if r.TaskN != nil {
				t.Errorf("run.TaskN = %d, want nil", *r.TaskN)
			}
		}
	}
	if !found {
		t.Fatalf("RunsForTicket(%d) = %+v, want to find run %d", ticketID, runs, runID)
	}
}

// TestDriveFixResumesAfterAnswer proves #28 gap 1's own new capability
// (design section 5.4 change 1): an owner's answer to a fix run's own
// build-job question resumes that same session (round.SessionID), with the
// owner's answer text in the prompt -- unlike the old AdvanceFix, which
// left this resume path out of its own scope entirely.
func TestDriveFixResumesAfterAnswer(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	mid := writeFixRequestMarker(t, s, ticketID, job.FixKindCILog, testFixCILogText, 0)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{questionResult(response.JobBuild, "fix-q-sess")}}
	deps := claimForBuild(t, s, scriptRT, ticketID)
	req := job.FixRequest{MessageID: mid, Kind: job.FixKindCILog, Text: testFixCILogText, AfterRunID: 0}

	commit, err := job.DriveFix(t.Context(), ticket, deps, req) // RUN: asks a question
	if err != nil {
		t.Fatalf("DriveFix (RUN, question): %v", err)
	}
	apply(t, s, ticket, commit)

	fixQ := findOpenQuestionByKind(t, s, ticketID, response.QuestionKindQuestion)
	if _, draftErr := s.SaveDraft(t.Context(), store.DraftInput{TicketID: ticketID, QuestionID: &fixQ.ID, Text: "Retry with a smaller batch."}); draftErr != nil {
		t.Fatalf("SaveDraft: %v", draftErr)
	}
	if _, sendErr := s.SendBatch(t.Context(), ticketID); sendErr != nil {
		t.Fatalf("SendBatch: %v", sendErr)
	}

	scriptRT.steps = append(scriptRT.steps, buildStep([]string{helloTxt}, 0, 0, nil, "fix-q-sess"))
	ticket = getTicket(t, s, ticketID)
	deps2 := claimForBuild(t, s, scriptRT, ticketID)
	resumeCommit, err := job.DriveFix(t.Context(), ticket, deps2, req) // resumes the answered round
	if err != nil {
		t.Fatalf("DriveFix (resume after answer): %v", err)
	}
	if scriptRT.reqs[len(scriptRT.reqs)-1].SessionID != "fix-q-sess" {
		t.Errorf("resume request SessionID = %q, want %q", scriptRT.reqs[len(scriptRT.reqs)-1].SessionID, "fix-q-sess")
	}
	if !strings.Contains(scriptRT.reqs[len(scriptRT.reqs)-1].Prompt, "Retry with a smaller batch.") {
		t.Errorf("resume prompt = %q, want the owner's own answer text", scriptRT.reqs[len(scriptRT.reqs)-1].Prompt)
	}
	apply(t, s, ticket, resumeCommit)
}

// TestDriveFixResumesClaimErrors proves review F003/F005's own claims-
// pending resume case, generalized to a fix unit through advanceCheckedRun
// (design section 5.4 change 1): a fix unit's own RUN claims a passing
// test_exit the real command contradicts, so CHECK writes a claim-errors-
// pending marker and the next DriveFix tick resumes it, still labeled
// "fix" and still carrying a nil task_n.
func TestDriveFixResumesClaimErrors(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	mid := writeFixRequestMarker(t, s, ticketID, job.FixKindCILog, testFixCILogText, 0)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "fix-pending-sess")}}
	rec := &recordingRuntime{rt: scriptRT}
	deps := claimForBuild(t, s, rec, ticketID)
	req := job.FixRequest{MessageID: mid, Kind: job.FixKindCILog, Text: testFixCILogText, AfterRunID: 0}

	commit, err := job.DriveFix(t.Context(), ticket, deps, req) // RUN: claims hello.txt, writes nothing
	if err != nil {
		t.Fatalf("DriveFix (RUN): %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := claimForBuild(t, s, rec, ticketID)
	checkCommit, err := job.DriveFix(t.Context(), ticket, deps2, req) // CHECK: claim errors pending (test_exit false)
	if err != nil {
		t.Fatalf("DriveFix (CHECK): %v", err)
	}
	if len(checkCommit.Messages) != 1 || !strings.HasPrefix(checkCommit.Messages[0].Body, "claim errors pending run ") {
		t.Fatalf("CHECK commit.Messages = %+v, want the pending marker", checkCommit.Messages)
	}
	apply(t, s, ticket, checkCommit)

	scriptRT.steps = append(scriptRT.steps, buildStep([]string{helloTxt}, 0, 0, nil, "fix-pending-sess"))
	ticket = getTicket(t, s, ticketID)
	deps3 := claimForBuild(t, s, rec, ticketID)
	resumeCommit, err := job.DriveFix(t.Context(), ticket, deps3, req) // resume: claims
	if err != nil {
		t.Fatalf("DriveFix (resume): %v", err)
	}
	if rec.lastReq.Label != testFixLabel {
		t.Errorf("resume request Label = %q, want %q", rec.lastReq.Label, testFixLabel)
	}
	if len(resumeCommit.Runs) != 1 {
		t.Fatalf("commit.Runs = %+v, want exactly one", resumeCommit.Runs)
	}
	runID := resumeCommit.Runs[0].ID
	apply(t, s, ticket, resumeCommit)

	runs, err := s.RunsForTicket(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("RunsForTicket: %v", err)
	}
	found := false
	for _, r := range runs {
		if r.ID == runID {
			found = true
			if r.TaskN != nil {
				t.Errorf("resume run.TaskN = %d, want nil", *r.TaskN)
			}
		}
	}
	if !found {
		t.Fatalf("RunsForTicket(%d) = %+v, want to find run %d", ticketID, runs, runID)
	}
}

// TestDriveFixResumesInvalidOutput proves advanceUnit's own "error,
// invalid" branch (design section 5.4 change 1) carries a fix unit exactly
// as it carries a task unit: a first invalid output is not escalated, and
// the next DriveFix tick resumes the same session with the invalid reason
// in the prompt.
func TestDriveFixResumesInvalidOutput(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	mid := writeFixRequestMarker(t, s, ticketID, job.FixKindCILog, testFixCILogText, 0)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{invalidResult("not well-formed", "fix-invalid-sess")}}
	deps := claimForBuild(t, s, scriptRT, ticketID)
	req := job.FixRequest{MessageID: mid, Kind: job.FixKindCILog, Text: testFixCILogText, AfterRunID: 0}

	firstCommit, err := job.DriveFix(t.Context(), ticket, deps, req) // RUN: invalid, first strike
	if err != nil {
		t.Fatalf("DriveFix (RUN, invalid): %v", err)
	}
	if firstCommit.Escalation != nil {
		t.Fatalf("first invalid commit.Escalation = %+v, want nil (first strike)", firstCommit.Escalation)
	}
	apply(t, s, ticket, firstCommit)

	scriptRT.steps = append(scriptRT.steps, buildStep([]string{helloTxt}, 0, 0, nil, "fix-invalid-sess"))
	ticket = getTicket(t, s, ticketID)
	deps2 := claimForBuild(t, s, scriptRT, ticketID)
	_, err = job.DriveFix(t.Context(), ticket, deps2, req) // resume: invalid
	if err != nil {
		t.Fatalf("DriveFix (resume, invalid): %v", err)
	}
	lastReq := scriptRT.reqs[len(scriptRT.reqs)-1]
	if !strings.Contains(lastReq.Prompt, "not well-formed") {
		t.Errorf("resume prompt = %q, want the invalid reason", lastReq.Prompt)
	}
	if lastReq.SessionID != "fix-invalid-sess" {
		t.Errorf("resume request SessionID = %q, want %q", lastReq.SessionID, "fix-invalid-sess")
	}
}

// TestDriveFixResumesInterrupted proves advanceUnit's own "error,
// interrupted" branch (design section 5.4 change 1) carries a fix unit
// exactly as it carries a task unit (design section 6.3/6.10, section
// 14's "the process dies during a build run" row): a claims resume that
// runtime.ErrCanceled interrupts leaves the session's newest run reserved
// with no outcome; ExpireClaims reconciles it to "error", and the session
// stays open, ready for the next DriveFix tick to resume with the raw
// "interrupted" input.
func TestDriveFixResumesInterrupted(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	mid := writeFixRequestMarker(t, s, ticketID, job.FixKindCILog, testFixCILogText, 0)
	ticket := getTicket(t, s, ticketID)

	mismatchRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "fix-mismatch-sess")}}
	deps := claimForBuild(t, s, mismatchRT, ticketID)
	req := job.FixRequest{MessageID: mid, Kind: job.FixKindCILog, Text: testFixCILogText, AfterRunID: 0}

	commit, err := job.DriveFix(t.Context(), ticket, deps, req) // RUN: claims hello.txt, writes nothing
	if err != nil {
		t.Fatalf("DriveFix (RUN): %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := claimForBuild(t, s, mismatchRT, ticketID)
	checkCommit, err := job.DriveFix(t.Context(), ticket, deps2, req) // CHECK: pending marker
	if err != nil {
		t.Fatalf("DriveFix (CHECK): %v", err)
	}
	apply(t, s, ticket, checkCommit)

	canceledRT := &scriptedRuntime{t: t, steps: []scriptedStep{
		{res: runtime.RunResult{ExitCode: -1, AgentTime: 0}, err: runtime.ErrCanceled},
	}}
	ticket = getTicket(t, s, ticketID)
	deps3 := claimForBuild(t, s, canceledRT, ticketID)
	_, err = job.DriveFix(t.Context(), ticket, deps3, req) // resume: claims, interrupted mid-flight
	if !errors.Is(err, runtime.ErrCanceled) {
		t.Fatalf("err = %v, want errors.Is(err, runtime.ErrCanceled)", err)
	}

	if _, expireErr := s.ExpireClaims(t.Context(), time.Now().Add(20*time.Minute)); expireErr != nil {
		t.Fatalf("ExpireClaims: %v", expireErr)
	}

	maxResumes := deps.Machine.Jobs["build"].MaxResumes
	sess, state, err := s.LatestSession(t.Context(), ticketID, "build", maxResumes)
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if sess.ExternalID == nil {
		t.Fatal("session has no external id after the canceled resume")
	}
	if sess.Resumes != 1 {
		t.Fatalf("sessions.resumes after the canceled resume = %d, want 1 (charged at Reserve, design section 4.2)", sess.Resumes)
	}
	if state != store.SessionOpen {
		t.Fatalf("session state after the canceled resume = %v, want SessionOpen", state)
	}

	resumeRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "fix-mismatch-sess")}}
	ticket = getTicket(t, s, ticketID)
	deps4 := claimForBuild(t, s, resumeRT, ticketID)
	resumeCommit, err := job.DriveFix(t.Context(), ticket, deps4, req) // resume: "interrupted"
	if err != nil {
		t.Fatalf("DriveFix (resume, interrupted): %v", err)
	}
	if !strings.Contains(resumeRT.reqs[0].Prompt, "interrupted") {
		t.Errorf("resume prompt = %q, want the fixed interrupted wording", resumeRT.reqs[0].Prompt)
	}
	apply(t, s, ticket, resumeCommit)
}

// TestDriveFixLandWritesLandedMarker proves the fix driver's own LAND
// addition (design section 5.1, 5.3): a fix unit that checks clean lands
// in one commit, exactly as a task unit does, and that commit also carries
// "fix landed <req.MessageID> sha <sha>" -- the marker openFixRequest
// reads to know this request's own unit has landed.
func TestDriveFixLandWritesLandedMarker(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	mid := writeFixRequestMarker(t, s, ticketID, job.FixKindFailure, "scenario 2 failed: timeout", 0)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "fix-land-sess")}}
	deps := withFixTestCmd(claimForBuild(t, s, scriptRT, ticketID), ticket)
	req := job.FixRequest{MessageID: mid, Kind: job.FixKindFailure, Text: "scenario 2 failed: timeout", AfterRunID: 0}

	commit, err := job.DriveFix(t.Context(), ticket, deps, req) // RUN: first turn
	if err != nil {
		t.Fatalf("DriveFix (RUN): %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := withFixTestCmd(claimForBuild(t, s, scriptRT, ticketID), ticket)
	landCommit, err := job.DriveFix(t.Context(), ticket, deps2, req) // CHECK, clean, straight to LAND
	if err != nil {
		t.Fatalf("DriveFix (CHECK+LAND): %v", err)
	}
	if len(landCommit.Artifacts) != 1 {
		t.Fatalf("commit.Artifacts = %+v, want one landed build_report", landCommit.Artifacts)
	}
	var landed response.BuildReport
	if unmarshalErr := json.Unmarshal(landCommit.Artifacts[0].Payload, &landed); unmarshalErr != nil {
		t.Fatalf("unmarshal landed build_report: %v", unmarshalErr)
	}
	if landed.CommitSHA == nil {
		t.Fatal("landed.CommitSHA = nil, want a sha")
	}
	if landed.TaskN != 0 {
		t.Errorf("landed.TaskN = %d, want 0", landed.TaskN)
	}
	if landCommit.Next != "" {
		t.Errorf("commit.Next = %q, want empty (a fix never transitions the ticket)", landCommit.Next)
	}

	wantPrefix := "fix landed " + strconv.FormatInt(mid, 10) + " sha "
	found := false
	for _, m := range landCommit.Messages {
		if strings.HasPrefix(m.Body, wantPrefix) {
			found = true
		}
	}
	if !found {
		t.Fatalf("LAND commit.Messages = %+v, want one starting with %q", landCommit.Messages, wantPrefix)
	}
	apply(t, s, ticket, landCommit)
}

// TestSecondFixAdvancesAfterFirstLanded proves design D22's own fix for
// #28 gap 1: the old AdvanceFix treated the ticket's first landed fix as
// every fix landed, so a second fix request could never advance past its
// own first DriveFix tick. Here, a second request -- opened after the
// first has landed, with its own watermark at MaxRunID -- runs its own
// first turn and lands under its own message id, proving the two units
// are never confused.
func TestSecondFixAdvancesAfterFirstLanded(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	mid1 := writeFixRequestMarker(t, s, ticketID, job.FixKindCILog, testFixCILogText, 0)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "fix1-sess")}}
	deps := withFixTestCmd(claimForBuild(t, s, scriptRT, ticketID), ticket)
	req1 := job.FixRequest{MessageID: mid1, Kind: job.FixKindCILog, Text: testFixCILogText, AfterRunID: 0}

	c1, err := job.DriveFix(t.Context(), ticket, deps, req1) // RUN: first turn
	if err != nil {
		t.Fatalf("DriveFix (fix 1, RUN): %v", err)
	}
	apply(t, s, ticket, c1)

	ticket = getTicket(t, s, ticketID)
	deps2 := withFixTestCmd(claimForBuild(t, s, scriptRT, ticketID), ticket)
	land1, err := job.DriveFix(t.Context(), ticket, deps2, req1) // CHECK, clean, LAND
	if err != nil {
		t.Fatalf("DriveFix (fix 1, LAND): %v", err)
	}
	if landedMessageID(t, land1.Messages) != mid1 {
		t.Fatalf("fix 1's own landed marker names message %d, want %d", landedMessageID(t, land1.Messages), mid1)
	}
	apply(t, s, ticket, land1)

	watermark, err := s.MaxRunID(t.Context(), ticketID)
	if err != nil {
		t.Fatalf("MaxRunID: %v", err)
	}
	mid2 := writeFixRequestMarker(t, s, ticketID, job.FixKindFailure, "second problem", watermark)
	scriptRT.steps = append(scriptRT.steps, buildStep([]string{helloTxt}, 0, 0, nil, "fix2-sess"))
	ticket = getTicket(t, s, ticketID)
	deps3 := withFixTestCmd2(claimForBuild(t, s, scriptRT, ticketID), ticket)
	req2 := job.FixRequest{MessageID: mid2, Kind: job.FixKindFailure, Text: "second problem", AfterRunID: watermark}

	c2, err := job.DriveFix(t.Context(), ticket, deps3, req2) // RUN: the second fix's own first turn
	if err != nil {
		t.Fatalf("DriveFix (fix 2, RUN): %v", err)
	}
	if len(c2.Runs) != 1 {
		t.Fatalf("fix 2's own RUN commit.Runs = %+v, want exactly one (the old bug returned ErrNoAction here)", c2.Runs)
	}
	apply(t, s, ticket, c2)

	ticket = getTicket(t, s, ticketID)
	deps4 := withFixTestCmd2(claimForBuild(t, s, scriptRT, ticketID), ticket)
	land2, err := job.DriveFix(t.Context(), ticket, deps4, req2) // CHECK, clean, LAND
	if err != nil {
		t.Fatalf("DriveFix (fix 2, LAND): %v", err)
	}
	if landedMessageID(t, land2.Messages) != mid2 {
		t.Fatalf("fix 2's own landed marker names message %d, want %d", landedMessageID(t, land2.Messages), mid2)
	}
}

// landedMessageID finds the one "fix landed <id> sha ..." marker among
// msgs and returns its own <id>, failing the test when none is found.
func landedMessageID(t *testing.T, msgs []store.Message) int64 {
	t.Helper()
	for _, m := range msgs {
		if !strings.HasPrefix(m.Body, "fix landed ") {
			continue
		}
		fields := strings.Fields(m.Body)
		if len(fields) < 3 {
			continue
		}
		id, err := strconv.ParseInt(fields[2], 10, 64)
		if err == nil {
			return id
		}
	}
	t.Fatalf("msgs = %+v, want one \"fix landed <id> sha ...\" marker", msgs)
	return 0
}
