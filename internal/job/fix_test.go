// fix_test.go tests task 14: the fix-run entry point (design section 8),
// StartFix and AdvanceFix. It reuses skeleton_test.go and building_test.go's
// shared fixtures (newJobTestStore, claim, apply, getTicket,
// buildTicketInBuilding, claimForBuild, buildStep, helloTxt,
// testNoopShellCmd, scriptedRuntime, recordingRuntime, assertFenced) and
// drives job.StartFix and job.AdvanceFix directly: neither function is
// wired into job.Registry() (design section 8: "nothing in serve calls
// it"), so there is no ticket-state handler to go through.
package job_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"zing/internal/job"
	"zing/internal/response"
	"zing/internal/store"
)

// fixTestCmd is the shell test command this file's own scenarios override
// TestCmd with: it writes hello.txt for real, the same technique
// building_test.go's own perimeterScenario uses, since a scriptedRuntime's
// buildStep carries no fake-runtime tree effect of its own (design section
// 9.3) to create the file its claim names.
const fixTestCmd = "printf 'hello, world\\n' > hello.txt && test -f hello.txt"

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

// TestStartFixSubjectAndLabel proves design section 8's own table for all
// three kinds: the run request's own Label is "fix" regardless of kind, the
// prompt carries the kind's own input label with the fix text fenced, and
// the landed build_report's Title is the kind's own commit subject with
// TaskN 0.
func TestStartFixSubjectAndLabel(t *testing.T) {
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
			ticket := getTicket(t, s, ticketID)

			scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "fix-sess-"+string(tc.kind))}}
			rec := &recordingRuntime{rt: scriptRT}
			deps := claimForBuild(t, s, rec, ticketID)

			commit, err := job.StartFix(t.Context(), ticket, deps, job.FixInput{Kind: tc.kind, Text: "do the thing"})
			if err != nil {
				t.Fatalf("StartFix: %v", err)
			}
			if rec.lastReq.Label != "fix" {
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

// TestStartFixRejectsUnknownKind proves design section 8's own validation
// rule: a kind outside FixKind.Values() returns the fixed error, before any
// worktree or run is touched (the deps' own runtime never sees a call).
func TestStartFixRejectsUnknownKind(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, &scriptedRuntime{t: t}, ticketID) // no runtime call expected

	_, err := job.StartFix(t.Context(), ticket, deps, job.FixInput{Kind: job.FixKind("bogus"), Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "job: unknown fix kind bogus") {
		t.Fatalf("StartFix error = %v, want it to contain %q", err, "job: unknown fix kind bogus")
	}
}

// TestStartFixRejectsEmptyText proves design section 8's own validation
// rule: text that is empty after strings.TrimSpace returns the fixed
// error, again with no runtime call.
func TestStartFixRejectsEmptyText(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)
	deps := claimForBuild(t, s, &scriptedRuntime{t: t}, ticketID) // no runtime call expected

	_, err := job.StartFix(t.Context(), ticket, deps, job.FixInput{Kind: job.FixKindFindings, Text: "   \n\t "})
	if err == nil || err.Error() != "job: fix input is empty" {
		t.Fatalf("StartFix error = %v, want \"job: fix input is empty\"", err)
	}
}

// TestFixTextIsFenced proves in.Text always enters the prompt fenced
// (design section 8), independent of TestStartFixSubjectAndLabel's own
// per-kind table.
func TestFixTextIsFenced(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "fix-fence-sess")}}
	rec := &recordingRuntime{rt: scriptRT}
	deps := claimForBuild(t, s, rec, ticketID)

	_, err := job.StartFix(t.Context(), ticket, deps, job.FixInput{Kind: job.FixKindCILog, Text: "the CI log tail"})
	if err != nil {
		t.Fatalf("StartFix: %v", err)
	}
	assertFenced(t, rec.lastReq.Prompt, "ci_log", "the CI log tail")
}

// TestFixLandsOneCommit proves the fix unit shares CHECK and LAND verbatim
// with a task unit (design section 8): a claim that truthfully names
// hello.txt, with no test or lint failure and no extra, checks clean on
// the first try and lands in the very next AdvanceFix tick, exactly as
// TestLastResumeReturningOkLands proves for a task. The landed
// build_report carries TaskN 0, and the ticket never transitions (a fix
// is not a plan task, so section 6.8's "last task" rule never fires for
// one).
func TestFixLandsOneCommit(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "fix-land-sess")}}
	deps := withFixTestCmd(claimForBuild(t, s, scriptRT, ticketID), ticket)

	commit, err := job.StartFix(t.Context(), ticket, deps, job.FixInput{Kind: job.FixKindFailure, Text: "scenario 2 failed: timeout"})
	if err != nil {
		t.Fatalf("StartFix: %v", err)
	}
	apply(t, s, ticket, commit)

	ticket = getTicket(t, s, ticketID)
	deps2 := withFixTestCmd(claimForBuild(t, s, scriptRT, ticketID), ticket)
	landCommit, err := job.AdvanceFix(t.Context(), ticket, deps2) // CHECK, clean, straight to LAND
	if err != nil {
		t.Fatalf("AdvanceFix (CHECK+LAND): %v", err)
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
	apply(t, s, ticket, landCommit)
}

// TestAdvanceFixNoActionAfterLanding proves design section 8's own
// stopping rule: once the newest fix unit has landed, AdvanceFix returns
// job.ErrNoAction, with no runtime call.
func TestAdvanceFixNoActionAfterLanding(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "fix-noaction-sess")}}
	deps := withFixTestCmd(claimForBuild(t, s, scriptRT, ticketID), ticket)
	apply(t, s, ticket, mustStartFix(t, ticket, deps, job.FixInput{Kind: job.FixKindCILog, Text: "log tail"}))

	ticket = getTicket(t, s, ticketID)
	deps2 := withFixTestCmd(claimForBuild(t, s, scriptRT, ticketID), ticket)
	landCommit, err := job.AdvanceFix(t.Context(), ticket, deps2)
	if err != nil {
		t.Fatalf("AdvanceFix (CHECK+LAND): %v", err)
	}
	apply(t, s, ticket, landCommit)

	ticket = getTicket(t, s, ticketID)
	deps3 := withFixTestCmd(claimForBuild(t, s, &scriptedRuntime{t: t}, ticketID), ticket) // no runtime call expected
	_, err = job.AdvanceFix(t.Context(), ticket, deps3)
	if !errors.Is(err, job.ErrNoAction) {
		t.Fatalf("AdvanceFix after landing: err = %v, want job.ErrNoAction", err)
	}
}

// TestFixRunHasNoTaskN proves design section 8's own storage rule: a fix
// run's own task_n column is nil (RunSeed.TaskN nil), never 0, even though
// its build_report's own TaskN field is 0.
func TestFixRunHasNoTaskN(t *testing.T) {
	s, _, ticketID := buildTicketInBuilding(t)
	ticket := getTicket(t, s, ticketID)

	scriptRT := &scriptedRuntime{t: t, steps: []scriptedStep{buildStep([]string{helloTxt}, 0, 0, nil, "fix-tasknil-sess")}}
	deps := claimForBuild(t, s, scriptRT, ticketID)

	commit, err := job.StartFix(t.Context(), ticket, deps, job.FixInput{Kind: job.FixKindCILog, Text: "log tail"})
	if err != nil {
		t.Fatalf("StartFix: %v", err)
	}
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

// mustStartFix runs job.StartFix and fails the test on error, returning the
// commit for the caller to apply.
func mustStartFix(t *testing.T, ticket store.Ticket, deps job.Deps, in job.FixInput) store.HandlerCommit {
	t.Helper()
	commit, err := job.StartFix(t.Context(), ticket, deps, in)
	if err != nil {
		t.Fatalf("StartFix: %v", err)
	}
	return commit
}
