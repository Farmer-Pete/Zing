package prompt

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	zing "zing"
	"zing/internal/response"
)

var update = flag.Bool("update", false, "update the committed golden prompts in internal/prompt/testdata")

const goldenDir = "testdata"

// healthCheckPlan is the stored-plan XML shared by every golden case that
// needs one: build-first, build-fix, and review-first.
const healthCheckPlan = "<plan><objective>Add a health check endpoint.</objective></plan>"

// readAsset reads one file through zing.Assets, the same embed.FS
// production code reads prompt and style files from, so these goldens
// track the pinned prompt text (internal/machine/prompts_test.go pins the
// files themselves; this test pins what Assemble does with them).
func readAsset(t *testing.T, path string) string {
	t.Helper()
	b, err := zing.Assets.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(b)
}

// planLensSection is PlanLensSection (jobs.go, task 7b moved the
// implementation there so internal/job's review tick can call it too),
// failing the test instead of returning an error.
func planLensSection(t *testing.T, text string) string {
	t.Helper()
	s, err := PlanLensSection(text)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// schemasFor renders the schema block for each outcome, in the order
// given, with response.RenderTemplate — the plan's confirmed renderer
// name (plan section 4.2).
func schemasFor(t *testing.T, job response.Job, outcomes ...response.Outcome) []string {
	t.Helper()
	out := make([]string, len(outcomes))
	for i, oc := range outcomes {
		s, err := response.RenderTemplate(job, oc)
		if err != nil {
			t.Fatalf("RenderTemplate(%s, %s): %v", job, oc, err)
		}
		out[i] = s
	}
	return out
}

// goldenCase is one canonical assembly: a name (its golden filename) and
// the Input Assemble renders. Fence is set to the fixed-nonce testFence by
// runGoldenCase, not by the case itself.
type goldenCase struct {
	name string
	in   func(t *testing.T) Input
}

func goldenCases() []goldenCase {
	return []goldenCase{
		{
			name: "classify",
			in: func(t *testing.T) Input {
				t.Helper()
				jobPrompt := readAsset(t, "prompts/classify.md")
				ticket := "Title: Login button does nothing\n\n" +
					"Body: Clicking login does nothing in Safari on the account page."
				in := ForClassify(jobPrompt, ticket, nil)
				in.Schemas = schemasFor(t, response.JobClassify,
					response.OutcomeBug, response.OutcomeFeature, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			name: "planning-feature-first",
			in: func(t *testing.T) Input {
				t.Helper()
				jobPrompt := readAsset(t, "prompts/planning-feature.md")
				styles := []string{
					readAsset(t, "prompts/style/prose.md"),
					readAsset(t, "prompts/style/agent-docs.md"),
				}
				ticket := "Title: Add dark mode\n\n" +
					"Body: Users want a dark theme toggle in settings."
				in := ForPlanningFirst(jobPrompt, styles, ticket, nil)
				in.Schemas = schemasFor(t, response.JobPlanning,
					response.OutcomeQuestions, response.OutcomeReady, response.OutcomeChildren,
					response.OutcomeNothingToDo, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			// Carries one answer, one findings block, one validation
			// block, and one invalid reason (plan section 4.2's
			// confirmation test for TASK 3).
			name: "planning-resume",
			in: func(t *testing.T) Input {
				t.Helper()
				in := ForPlanningResume([]NamedInput{
					Answer("Q1 Which storage backend for the cache? -> a (SQLite): keep it simple for now."),
					Findings("problem: plan/tasks/task[2] has no test named."),
					Validation("plan/tasks/task[2]/tests/test[1]/seam: no such file internal/x/y_test.go"),
					Invalid("output was not one <zing> document"),
				})
				in.Schemas = schemasFor(t, response.JobPlanning,
					response.OutcomeQuestions, response.OutcomeReady, response.OutcomeChildren,
					response.OutcomeNothingToDo, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			name: "planreview",
			in: func(t *testing.T) Input {
				t.Helper()
				jobPrompt := readAsset(t, "prompts/planreview.md")
				lensSections := []string{
					planLensSection(t, readAsset(t, "prompts/lenses/problem.md")),
					planLensSection(t, readAsset(t, "prompts/lenses/correctness.md")),
				}
				ticket := "Title: Add dark mode\n\n" +
					"Body: Users want a dark theme toggle in settings."
				scenarios := "given the settings page, when the user toggles dark mode, " +
					"then the theme switches immediately"
				plan := "<plan><objective>Add a dark mode toggle.</objective></plan>"
				in := ForPlanReview(jobPrompt, lensSections, ticket, scenarios, plan, nil)
				in.Schemas = schemasFor(t, response.JobPlanreview,
					response.OutcomeOk, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			name: "build-first",
			in: func(t *testing.T) Input {
				t.Helper()
				jobPrompt := readAsset(t, "prompts/build.md")
				task := BuildTask{
					N: 1, Total: 3,
					Title: "Add the ping handler",
					Text:  "Add a GET /ping handler that returns 200 and the body \"pong\".",
					Test:  "TestPingHandlerReturnsPong",
				}
				ticket := "Title: Add a health check\n\n" +
					"Body: Add a ping endpoint so uptime monitoring has something to hit."
				plan := healthCheckPlan
				accepted := []string{"internal/health/ping.go", "internal/health/ping_test.go"}
				in, err := ForBuild(jobPrompt, task, "go test ./...", "make lint", ticket, plan, "", accepted, nil)
				if err != nil {
					t.Fatalf("ForBuild: %v", err)
				}
				in.Schemas = schemasFor(t, response.JobBuild,
					response.OutcomeOk, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			// Carries one answer, one claims block, one check block
			// (#55), and one invalid reason (plan section 6.3's resume
			// inputs table).
			name: "build-resume",
			in: func(t *testing.T) Input {
				t.Helper()
				in := ForBuildResume([]NamedInput{
					Answer("Q1 Which status code on a degraded dependency? -> a (503): keep it simple."),
					{Label: "claims", Text: "internal/health/ping.go: declared but not written", Untrusted: true},
					Check("test command: go test ./...\nexit code: 1\noutput:\n--- FAIL: TestPingHandlerReturnsPong"),
					Invalid("output was not one <zing> document"),
				})
				in.Schemas = schemasFor(t, response.JobBuild,
					response.OutcomeOk, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			name: "build-fix",
			in: func(t *testing.T) Input {
				t.Helper()
				jobPrompt := readAsset(t, "prompts/build.md")
				ticket := "Title: Add a health check\n\n" +
					"Body: Add a ping endpoint so uptime monitoring has something to hit."
				plan := healthCheckPlan
				findings := "problem: internal/health/ping.go returns 500 on success."
				in, err := ForFix(jobPrompt, "Fix review findings", "findings", findings,
					"go test ./...", "make lint", ticket, plan, "", nil, nil)
				if err != nil {
					t.Fatalf("ForFix: %v", err)
				}
				in.Schemas = schemasFor(t, response.JobBuild,
					response.OutcomeOk, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			name: "perimeter",
			in: func(t *testing.T) Input {
				t.Helper()
				jobPrompt := readAsset(t, "prompts/perimeter.md")
				path := "internal/health/status.go"
				hunk := "@@ -0,0 +1,3 @@\n+package health\n+\n+const statusOK = \"ok\"\n"
				in := ForPerimeter(jobPrompt, path, hunk, nil)
				in.Schemas = schemasFor(t, response.JobPerimeter,
					response.OutcomeOk, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			// Carries one answer (plan section 6.2's resume input for the
			// owner's answer to a perimeter run's own question).
			name: "perimeter-resume",
			in: func(t *testing.T) Input {
				t.Helper()
				in := ForPerimeterResume([]NamedInput{
					Answer("Which style fits the repo? -> a (hyphen): matches the style guide."),
				})
				in.Schemas = schemasFor(t, response.JobPerimeter,
					response.OutcomeOk, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			name: "review-first",
			in: func(t *testing.T) Input {
				t.Helper()
				jobPrompt := readAsset(t, "prompts/review.md")
				codeSection, err := CodeLensSection(readAsset(t, "prompts/lenses/correctness.md"))
				if err != nil {
					t.Fatalf("CodeLensSection: %v", err)
				}
				plan := healthCheckPlan
				diff := "diff --git a/internal/health/ping.go b/internal/health/ping.go\n" +
					"+func Ping() string { return \"pong\" }\n"
				in, err := ForReview(jobPrompt, "correctness", "a1b2c3d", codeSection, plan, diff, nil)
				if err != nil {
					t.Fatalf("ForReview: %v", err)
				}
				in.Schemas = schemasFor(t, response.JobReview,
					response.OutcomeOk, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			// Carries one findings block and one notes block (design section
			// 6.6's discuss-resume inputs).
			name: "review-discuss",
			in: func(t *testing.T) Input {
				t.Helper()
				in := ForReviewDiscuss([]NamedInput{
					Findings("r1f1: internal/health/ping.go:12 returns 500 on success."),
					Notes("r1f1: the handler is supposed to degrade, not fail."),
				})
				in.Schemas = schemasFor(t, response.JobReview,
					response.OutcomeOk, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			name: "judge-first",
			in: func(t *testing.T) Input {
				t.Helper()
				jobPrompt := readAsset(t, "prompts/judge.md")
				ticket := "Title: Add a health check\n\n" +
					"Body: Add a ping endpoint so uptime monitoring has something to hit."
				in := ForJudge(jobPrompt, ticket, nil)
				in.Schemas = schemasFor(t, response.JobJudge,
					response.OutcomeOk, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			// Carries one answers block (plan section 7.2's resume input for
			// an answered judge question).
			name: "judge-resume",
			in: func(t *testing.T) Input {
				t.Helper()
				in := ForJudgeResume([]NamedInput{
					Answers("Q1: which exit code counts as a pass? -> a: 0 only."),
				})
				in.Schemas = schemasFor(t, response.JobJudge,
					response.OutcomeOk, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			name: "respond-first",
			in: func(t *testing.T) Input {
				t.Helper()
				jobPrompt := readAsset(t, "prompts/respond.md")
				styles := []string{readAsset(t, "prompts/style/prose.md")}
				plan := healthCheckPlan
				diff := "diff --git a/internal/health/ping.go b/internal/health/ping.go\n" +
					"+func Ping() string { return \"pong\" }\n"
				threads := "thread t1\n" +
					"file internal/health/ping.go:12\n" +
					"comment by @alice at 2026-09-30T12:00:00Z:\n" +
					"Why does this return 500 on success?"
				in := ForRespond(jobPrompt, styles, plan, diff, threads, nil)
				in.Schemas = schemasFor(t, response.JobRespond,
					response.OutcomeOk, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
		{
			// Carries one answers block (plan section 9.2's resume input for
			// an answered respond question).
			name: "respond-resume",
			in: func(t *testing.T) Input {
				t.Helper()
				in := ForRespondResume([]NamedInput{
					Answers("Q1: should thread t1 be a fix or a reply? -> a: reply, the code is correct."),
				})
				in.Schemas = schemasFor(t, response.JobRespond,
					response.OutcomeOk, response.OutcomeQuestion, response.OutcomeError)
				return in
			},
		},
	}
}

// TestAssemble_MatchesGolden is the golden test for the four canonical
// assemblies (plan section 4.2, TASK 3): classify, the planning-feature
// first turn, a planning resume carrying one answer, one findings block,
// one validation block, and one invalid reason, and a plan review. Run
// with -update to (re)write internal/prompt/testdata; without it, every
// case's assembled bytes must equal its committed golden exactly. Every
// case uses the fixed-nonce testFence, so the fenced bytes are pinned too.
func TestAssemble_MatchesGolden(t *testing.T) {
	t.Parallel()
	cases := goldenCases()

	if *update {
		for _, tc := range cases {
			in := tc.in(t)
			in.Fence = testFence
			got := Assemble(in)
			path := filepath.Join(goldenDir, tc.name+".txt")
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := tc.in(t)
			in.Fence = testFence
			got := Assemble(in)

			path := filepath.Join(goldenDir, tc.name+".txt")
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v (run go test -run TestAssemble_MatchesGolden -update)", path, err)
			}
			if got != string(want) {
				t.Errorf("%s: assembled prompt differs from golden; run with -update\n--- got ---\n%s\n--- want ---\n%s",
					path, got, want)
			}
		})
	}
}
