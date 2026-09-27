// seed.go: the minimal per-kind fixture builder (design section 6.15).
// SeedQuestionFixtures inserts one open question of each of the six kinds
// on a ticket, through the store's validated inserts, so a seeded row is a
// row a real producer could have written. Task 6's own question-kinds test
// uses it directly; Task 12's SeedDemo (not built here) reuses it for the
// demo project and ticket.
package console

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"zing/internal/response"
	"zing/internal/store"
)

// seedQuestionKinds is the design section 8 closed set of question kinds,
// in the order SeedQuestionFixtures inserts them; each kind's fixture key is
// "Q" plus its 1-based index here ("Q1".."Q6").
var seedQuestionKinds = []response.QuestionKind{
	response.QuestionKindQuestion,
	response.QuestionKindGate,
	response.QuestionKindSplit,
	response.QuestionKindMerge,
	response.QuestionKindPerimeter,
	response.QuestionKindReview,
}

// msgStateOpen is the messages.state (and QuestionPayload.State) value
// every seeded question carries: SeedQuestionFixtures seeds only open
// questions, matching "inserts ... one open question of each of the six
// kinds" (design section 6.15).
const msgStateOpen = "open"

// helloHandlerPath is the one fixture path both SeedQuestionFixtures'
// perimeter question and SeedDemo's plan artifact name, named once so
// goconst's repeated-literal guard has one definition to point at.
const helloHandlerPath = "internal/hello/handler.go"

// SeedQuestionFixtures inserts one open question of each of the six kinds
// (design section 8) on ticketID, each with just enough payload to render
// and compose: an option kind (question, gate, split, merge) gets two
// options and a recommended option key; an item kind (perimeter, review)
// gets two or three items, each with a ref and text (design section 6.15).
// Every insert goes through store.InsertMessage, which validates the
// payload against the messages/question schema, the same validation a real
// planning, gate, split, perimeter, review, or merge producer's commit
// would satisfy.
//
// It is idempotent: it first lists ticketID's existing question messages
// and skips any fixture key ("Q1".."Q6", one per kind) already present, so
// calling it twice on the same ticket inserts nothing the second time.
func SeedQuestionFixtures(ctx context.Context, s *store.Store, ticketID int64) error {
	existing, err := s.ListMessages(ctx, ticketID)
	if err != nil {
		return fmt.Errorf("seed question fixtures: list messages: %w", err)
	}
	have := make(map[string]bool, len(existing))
	for i := range existing {
		if existing[i].Type != msgTypeQuestion || len(existing[i].Payload) == 0 {
			continue
		}
		var p response.QuestionPayload
		if json.Unmarshal(existing[i].Payload, &p) == nil {
			have[p.Key] = true
		}
	}

	for i, kind := range seedQuestionKinds {
		key := fmt.Sprintf("Q%d", i+1)
		if have[key] {
			continue
		}
		if err := seedOneQuestion(ctx, s, ticketID, key, kind); err != nil {
			return fmt.Errorf("seed question fixtures: %s %s: %w", key, kind, err)
		}
	}
	return nil
}

// seedOneQuestion inserts one open "question" message of kind on ticketID,
// through store.InsertMessage (design section 6.15: "through the store's
// validated inserts").
func seedOneQuestion(ctx context.Context, s *store.Store, ticketID int64, key string, kind response.QuestionKind) error {
	title, body := seedQuestionText(kind)
	payload, err := seedQuestionPayload(key, kind)
	if err != nil {
		return err
	}
	openState := msgStateOpen
	_, err = s.InsertMessage(ctx, store.Message{
		TicketID: ticketID, Type: msgTypeQuestion, Author: "zing",
		State:   &openState,
		Body:    title + "\n\n" + body,
		Payload: payload,
	})
	return err
}

// seedQuestionText returns the title (the <details> summary line) and body
// (the question's prose) fixture text for kind: the shape a planning commit
// writes together in one Body field, title first, then a blank line, then
// the body (splitQuestionBody, views.go).
func seedQuestionText(kind response.QuestionKind) (title, body string) {
	switch kind {
	case response.QuestionKindQuestion:
		return "How should the greeting read?", "Pick the greeting style for GET /hello."
	case response.QuestionKindGate:
		return "Approve the plan?", "Review the plan, scenarios, and decisions, then approve or reject it."
	case response.QuestionKindSplit:
		return "Split this ticket?", "The ticket looks large enough to split into independently buildable children."
	case response.QuestionKindMerge:
		return "Merge the PR?", "The build passed review and judging; approve the merge or hold it."
	case response.QuestionKindPerimeter:
		return "Confirm the file perimeter", "These are the files the plan expects to touch."
	case response.QuestionKindReview:
		return "Triage the review findings", "Decide each finding before the build continues."
	default:
		return "", ""
	}
}

// demoProjectName, demoProjectRepo, demoProjectPath, demoTrackerRef, and
// demoTicketTitle name the one fixed demo project and ticket SeedDemo seeds
// (design section 6.15): fixed literals, not generated, so a second call
// finds the same rows (EnsureProject on the name, then a lookup by tracker
// ref) instead of creating a duplicate. demoProjectPath is never read by
// anything SeedDemo does; the fake runtime and fixture tracker this repo
// runs elsewhere never touch the demo project, so the path names no real
// directory.
const (
	demoProjectName = "demo"
	demoProjectRepo = "https://example.invalid/demo"
	demoProjectPath = "/tmp/zing-demo-project"
	demoTrackerRef  = "demo-1"
	demoTicketTitle = "Add a hello endpoint"
)

// scenarioArtifactType and planreviewArtifactType are the artifacts.type
// literals a ready-checking and a plan-review commit use
// (internal/store/schemas/artifacts/scenario.json, planreview.json),
// matching views.go's own planArtifactType for "plan".
const (
	scenarioArtifactType   = "scenario"
	planreviewArtifactType = "planreview"
)

// demoRunOwner and demoRunModel are the fixed claim owner and model id
// seedDemoRun's own Claim/Reserve/CommitHandlerResult sequence uses to
// insert the demo cohort's one session and run (design section 7, Task 11:
// "SeedDemo inserts one session and one run for the demo ticket"): the same
// three-call shape a real planning tick's runJob uses (design section 4.5,
// 4.6), not a bespoke insert, so the seeded row is one a real producer
// could have written.
const (
	demoRunOwner = "seed-demo"
	demoRunModel = "claude-sonnet-5"
)

// demoRunLease bounds the Claim seedDemoRun takes: long enough that its own
// handful of milliseconds of work never lets the lease expire out from
// under its own CommitHandlerResult call, which fences on the exact
// (owner, expires) pair Claim set.
const demoRunLease = time.Hour

// SeedDemo seeds one demo project and one demo ticket carrying one session
// and run (Task 11), a stored plan artifact (a small valid response.Plan
// with a mermaid block in its Shape) and a scenario artifact set both
// carrying that run's id (so the gate's context region renders them through
// the real cohort path, not the legacy fallback), a "planreview" artifact
// at the plan's own version carrying a finding set, and one open question
// of each of the six kinds (design section 6.15, 7), every row through the
// store's validated inserts (InsertArtifact, InsertMessage, by way of
// SeedQuestionFixtures; Claim, Reserve, and CommitHandlerResult for the run
// itself), so a seeded row is a row a real producer could have written. It
// is idempotent: a second call finds the same project and ticket and skips
// every artifact, run, and question already present, inserting nothing
// new. SeedDemo never runs in a normal serve; it runs only behind
// `zing serve --seed-demo`, from cmd/zing's selftest, and from this
// package's own tests.
func SeedDemo(ctx context.Context, s *store.Store) error {
	projectID, err := s.EnsureProject(ctx, store.Project{
		Name: demoProjectName, RepoURL: demoProjectRepo, LocalPath: demoProjectPath, Tracker: "github",
	})
	if err != nil {
		return fmt.Errorf("seed demo: ensure project: %w", err)
	}

	ticketID, err := ensureDemoTicket(ctx, s, projectID)
	if err != nil {
		return fmt.Errorf("seed demo: %w", err)
	}

	if err := seedDemoCohort(ctx, s, ticketID); err != nil {
		return fmt.Errorf("seed demo: %w", err)
	}
	if err := SeedQuestionFixtures(ctx, s, ticketID); err != nil {
		return fmt.Errorf("seed demo: %w", err)
	}
	return nil
}

// seedDemoCohort seeds ticketID's one run, plan, scenario cohort, and
// planreview artifact together, exactly once: a second call finds the plan
// artifact already stored (GetArtifact returns ok == true) and does
// nothing, matching every other seedDemo* function's own idempotency check.
func seedDemoCohort(ctx context.Context, s *store.Store, ticketID int64) error {
	_, ok, err := s.GetArtifact(ctx, ticketID, planArtifactType)
	if err != nil {
		return fmt.Errorf("get plan artifact: %w", err)
	}
	if ok {
		return nil
	}

	runID, err := seedDemoRun(ctx, s, ticketID)
	if err != nil {
		return fmt.Errorf("seed run: %w", err)
	}
	if err := seedDemoPlan(ctx, s, ticketID, runID); err != nil {
		return fmt.Errorf("seed plan: %w", err)
	}
	if err := seedDemoScenarios(ctx, s, ticketID, runID); err != nil {
		return fmt.Errorf("seed scenarios: %w", err)
	}
	if err := seedDemoPlanReview(ctx, s, ticketID, runID); err != nil {
		return fmt.Errorf("seed planreview: %w", err)
	}
	return nil
}

// seedDemoRun claims the demo ticket just long enough to reserve and
// terminalize its one session and run (design section 7, Task 11), through
// the same Claim, Reserve, then CommitHandlerResult sequence a real
// planning tick's runJob uses (design section 4.5, 4.6): reserved under a
// claim, terminalized with a "ready" outcome (design section 6.8: "planning
// | ready, checks pass"), and its claim released by that same commit. It
// returns the new run's id.
func seedDemoRun(ctx context.Context, s *store.Store, ticketID int64) (int64, error) {
	expires := time.Now().Add(demoRunLease)
	claimed, err := s.Claim(ctx, ticketID, demoRunOwner, expires)
	if err != nil {
		return 0, fmt.Errorf("claim ticket %d: %w", ticketID, err)
	}
	if !claimed {
		return 0, fmt.Errorf("ticket %d is already claimed", ticketID)
	}

	reserved, err := s.Reserve(ctx, ticketID, demoRunOwner, expires,
		store.SessionUpsert{Job: "planning", Runtime: "fake"}, demoRunModel)
	if err != nil {
		return 0, fmt.Errorf("reserve run: %w", err)
	}

	outcome, exitCode, agentSeconds := "ready", 0, 12
	applied, err := s.CommitHandlerResult(ctx, store.HandlerCommit{
		TicketID: ticketID, Owner: demoRunOwner, Expires: expires,
		Runs: []store.Run{{ID: reserved.RunID, Outcome: &outcome, ExitCode: &exitCode, AgentSeconds: &agentSeconds}},
	})
	if err != nil {
		return 0, fmt.Errorf("terminalize run: %w", err)
	}
	if !applied {
		return 0, fmt.Errorf("ticket %d lost its claim before its run could be terminalized", ticketID)
	}
	return reserved.RunID, nil
}

// ensureDemoTicket returns the demo ticket's id under projectID, inserting
// it (queued, no payload) the first time and reusing the same row, found by
// its fixed tracker ref, on every later call.
func ensureDemoTicket(ctx context.Context, s *store.Store, projectID int64) (int64, error) {
	existing, ok, err := s.TicketByRef(ctx, projectID, demoTrackerRef)
	if err != nil {
		return 0, fmt.Errorf("ticket by ref: %w", err)
	}
	if ok {
		return existing.ID, nil
	}

	id, err := s.InsertTicket(ctx, store.Ticket{
		ProjectID:  projectID,
		TrackerRef: demoTrackerRef,
		Title:      demoTicketTitle,
		Body:       "Add a small hello endpoint so the demo console has something concrete to plan, scan, and review.",
		State:      "queued",
	})
	if err != nil {
		return 0, fmt.Errorf("insert ticket: %w", err)
	}
	return id, nil
}

// seedDemoPlan inserts ticketID's demo plan artifact carrying runID (design
// section 7, Task 11): seedDemoCohort's own idempotency check is what keeps
// a second SeedDemo call from reaching this function at all, so this always
// inserts (version defaults to 1, InsertArtifact's own rule for a fresh
// whole-document artifact).
func seedDemoPlan(ctx context.Context, s *store.Store, ticketID, runID int64) error {
	payload, err := json.Marshal(demoPlan())
	if err != nil {
		return fmt.Errorf("marshal plan: %w", err)
	}
	if _, err := s.InsertArtifact(ctx, store.Artifact{
		TicketID: ticketID, Type: planArtifactType, RunID: &runID, Payload: payload,
	}); err != nil {
		return fmt.Errorf("insert plan artifact: %w", err)
	}
	return nil
}

// seedDemoScenarios inserts ticketID's demo scenario set, one artifact row
// per response.Scenario at consecutive versions, every row carrying runID
// (design section 7, Task 11): the same cohort run seedDemoPlan's plan
// artifact carries, so views.go's loadScenarios reads them through
// ScenariosForRun, the real cohort path, rather than the legacy
// AllScenarios fallback.
func seedDemoScenarios(ctx context.Context, s *store.Store, ticketID, runID int64) error {
	for i, sc := range demoScenarios() {
		payload, err := json.Marshal(sc)
		if err != nil {
			return fmt.Errorf("marshal scenario %s: %w", sc.ID, err)
		}
		if _, err := s.InsertArtifact(ctx, store.Artifact{
			TicketID: ticketID, Type: scenarioArtifactType, Version: i + 1, RunID: &runID, Payload: payload,
		}); err != nil {
			return fmt.Errorf("insert scenario %s: %w", sc.ID, err)
		}
	}
	return nil
}

// seedDemoPlanReview inserts ticketID's demo "planreview" artifact at its
// plan cohort's exact version (design section 7, Task 11: "store the seeded
// findings as the demo planreview artifact at the plan's version"), so
// views.go's loadFindings (PlanReviewAt) finds it the same way a real plan
// review's commit (job/planning.go's planReviewOkCommit) would have stored
// it: as a planreviewFindingsPayload, not a bare []response.Finding.
func seedDemoPlanReview(ctx context.Context, s *store.Store, ticketID, runID int64) error {
	cohort, ok, err := s.CurrentCohort(ctx, ticketID)
	if err != nil {
		return fmt.Errorf("current cohort: %w", err)
	}
	if !ok {
		return fmt.Errorf("ticket %d has no plan cohort to review yet", ticketID)
	}

	payload, err := json.Marshal(planreviewFindingsPayload{Findings: demoFindings()})
	if err != nil {
		return fmt.Errorf("marshal findings: %w", err)
	}
	if _, err := s.InsertArtifact(ctx, store.Artifact{
		TicketID: ticketID, Type: planreviewArtifactType, Version: cohort.PlanVersion, RunID: &runID, Payload: payload,
	}); err != nil {
		return fmt.Errorf("insert planreview artifact: %w", err)
	}
	return nil
}

// demoPlan returns a small but fully valid response.Plan (design section
// 6.15): every field the artifacts/plan.json schema requires is present,
// and Design.Shape carries a mermaid fence, so the plan renderer (plan.go)
// has a real diagram to show.
func demoPlan() response.Plan {
	return response.Plan{
		Overview: response.Overview{
			Objective: "Add a small hello endpoint so the demo has something concrete to plan and review.",
			Context:   "A new HTTP handler in the demo project's `internal/hello` package.",
			Problem:   response.Problem{Text: "The demo project has no endpoint for the console's plan renderer to point at."},
			Goals:     []string{"Serve GET /hello with a friendly greeting"},
			NonGoals:  []string{"Authentication, or any endpoint beyond /hello"},
		},
		Design: response.Design{
			Demo: response.Demo{Cmd: "curl localhost:8080/hello", Text: "the server answers with a greeting"},
			Shape: "The handler sits behind the existing mux.\n\n" +
				"```mermaid\ngraph TD\n  Client -->|GET /hello| Handler\n  Handler --> Response\n```",
			Changes: []response.Change{
				{
					Path: helloHandlerPath, Symbol: "Handler", Kind: response.ChangeKindNew,
					Callers: "cmd/zing/main.go", Callees: "net/http",
					Before: "", After: "func Handler(w http.ResponseWriter, r *http.Request)",
				},
			},
			Types:      []response.TypeDef{},
			Migrations: response.Migrations{Items: []response.Migration{}},
		},
		Delivery: response.Delivery{
			Files: []response.FileChange{
				{Path: helloHandlerPath, Action: response.FileActionCreate, Reason: "the new GET /hello handler"},
			},
			Deletions: response.Deletions{Items: []response.Fence{}},
			Tests: []response.TestCase{
				{Name: "TestHandler_Returns200", Seam: "internal/hello.Handler", Kind: response.TestKindIntegration, Mocks: "", Asserts: "GET /hello returns 200 with a greeting body"},
			},
			Tasks: []response.Task{
				{N: 1, Test: "TestHandler_Returns200", Demo: true, Text: "Add the GET /hello handler."},
			},
		},
		Review: response.Review{
			TrustRoot:    "internal/hello, a new package with no prior trust root",
			Alternatives: []string{"Serve the greeting from a static file instead of a handler"},
			Risks:        []string{"None: this is a demo fixture, not shipped code"},
		},
	}
}

// demoScenarios returns the demo ticket's scenario set (design section
// 6.15): one behavior scenario and one negative scenario, each a fully
// valid response.Scenario.
func demoScenarios() []response.Scenario {
	return []response.Scenario{
		{
			ID: "s1", Kind: response.ScenarioKindBehavior, Check: "go test ./internal/hello/... -run TestHandler_Returns200",
			Given: "the server is running", When: "a client sends GET /hello", Then: "the response is 200 with a greeting body",
		},
		{
			ID: "s2", Kind: response.ScenarioKindNegative,
			Given: "the server is running", When: "a client sends POST /hello", Then: "the response is 405 Method Not Allowed",
		},
	}
}

// demoFindings returns the demo ticket's finding set (design section 6.15):
// two review findings, each a fully valid response.Finding.
func demoFindings() []response.Finding {
	return []response.Finding{
		{
			Lens: response.LensQuality, Severity: response.SeverityMinor, Location: "plan/design/shape",
			Text: "The shape section does not name the response content type.", Fix: "Add one sentence naming text/plain.",
		},
		{
			Lens: response.LensTests, Severity: response.SeverityMajor, Location: "plan/delivery/tests/test[1]",
			Text: "The only test covers the happy path.", Fix: "Add a scenario for a disallowed method.",
		},
	}
}

// seedQuestionPayload builds and marshals the QuestionPayload fixture for
// kind (design section 6.15): an option kind gets two options and a
// recommended option key; an item kind gets two or three items, each with a
// ref and text, and an empty (not nil) Options slice, since the
// messages/question schema requires the "options" property present even
// when it is empty.
func seedQuestionPayload(key string, kind response.QuestionKind) (json.RawMessage, error) {
	payload := response.QuestionPayload{
		Key: key, Kind: kind, State: response.QuestionStateOpen,
		Options: []response.Option{},
	}

	switch kind {
	case response.QuestionKindQuestion:
		payload.Recommended = "a"
		payload.Options = []response.Option{
			{Key: "a", Text: "Plain hello"},
			{Key: "b", Text: "hello, world"},
		}
	case response.QuestionKindGate:
		payload.Recommended = "a"
		payload.Options = []response.Option{
			{Key: "a", Text: "Approve the plan"},
			{Key: "b", Text: "Reject the plan"},
		}
	case response.QuestionKindSplit:
		payload.Recommended = "a"
		payload.Options = []response.Option{
			{Key: "a", Text: "Split into the proposed children"},
			{Key: "b", Text: "Keep this as one ticket"},
		}
	case response.QuestionKindMerge:
		payload.Recommended = "a"
		payload.Options = []response.Option{
			{Key: "a", Text: "Merge the PR"},
			{Key: "b", Text: "Hold the PR"},
		}
	case response.QuestionKindPerimeter:
		payload.Recommended = "Accept every file in the perimeter"
		payload.Items = []response.Item{
			{Ref: helloHandlerPath, Text: "new HTTP handler for GET /hello"},
			{Ref: "internal/hello/handler_test.go", Text: "test for the new handler"},
			{Ref: "cmd/zing/main.go", Text: "wires the new route"},
		}
	case response.QuestionKindReview:
		payload.Recommended = "Accept every finding"
		payload.Items = []response.Item{
			{Ref: "F1", Text: "missing error wrap on the store call"},
			{Ref: "F2", Text: "handler does not close the response body"},
		}
	default:
		return nil, fmt.Errorf("seed question fixtures: unknown question kind %q", kind)
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("seed question fixtures: marshal %s payload: %w", kind, err)
	}
	return b, nil
}
