package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"zing/internal/store"
)

// scenariosTestOwner is the fixed claim owner every scenarios_test.go
// fixture uses, so a single expires instant fences every Reserve call the
// same way InsertTicket fenced the ticket (store.Reserve, design section
// 4.5).
const scenariosTestOwner = "scenarios-test"

// scenariosTokenEnv is the env var name scenarios reads ZING_RUN_TOKEN
// from, named once here so every fixture's getenv stub agrees with it
// (goconst).
const scenariosTokenEnv = "ZING_RUN_TOKEN"

// planExample is internal/store/examples/artifacts/plan.json, verbatim: a
// fully valid "plan" artifact payload (design section 4.5's InsertArtifact
// validates every artifact against its schema on insert). Its content is
// irrelevant to zing scenarios, which never reads the plan artifact's
// payload -- only its run_id and version (store.CurrentCohort) -- so any
// schema-valid plan works as the cohort's producing artifact.
const planExample = `{
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

// newScenariosTestStore opens a fresh, migrated store in a temp directory,
// closed on test cleanup.
func newScenariosTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	return st
}

// seedClaimedTicket inserts one project and one queued ticket already
// claimed by scenariosTestOwner with a one-hour lease, returning the ticket
// id and the exact expires instant every Reserve call on it must fence
// against.
func seedClaimedTicket(t *testing.T, st *store.Store) (ticketID int64, expires time.Time) {
	t.Helper()
	ctx := t.Context()

	expires = time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	owner := scenariosTestOwner

	projectID, err := st.EnsureProject(ctx, store.Project{
		Name: "scenarios-test-project", RepoURL: "https://example.com/x", LocalPath: t.TempDir(), Tracker: testServeTracker,
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	ticketID, err = st.InsertTicket(ctx, store.Ticket{
		ProjectID: projectID, TrackerRef: "1", Title: "t", State: "queued",
		ClaimOwner: &owner, ClaimExpiresAt: &expires,
	})
	if err != nil {
		t.Fatalf("InsertTicket: %v", err)
	}
	return ticketID, expires
}

// reserveRun reserves and returns the run id of a fresh session of job job
// on ticketID, under the claim seedClaimedTicket already took.
func reserveRun(t *testing.T, st *store.Store, ticketID int64, expires time.Time, job string) int64 {
	t.Helper()
	rsv, err := st.Reserve(t.Context(), ticketID, scenariosTestOwner, expires,
		store.SessionUpsert{Job: job, Runtime: "fake"}, store.RunSeed{Model: "fake-model"})
	if err != nil {
		t.Fatalf("Reserve(%s): %v", job, err)
	}
	return rsv.RunID
}

// scenarioPayload builds a schema-valid "scenario" artifact payload
// (internal/store/schemas/artifacts/scenario.json requires check_cmd
// present, even as an empty string).
func scenarioPayload(t *testing.T, id, kind, checkCmd, given, when, then string) []byte {
	t.Helper()
	payload := struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		CheckCmd string `json:"check_cmd"`
		Given    string `json:"given"`
		When     string `json:"when"`
		Then     string `json:"then"`
	}{id, kind, checkCmd, given, when, then}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal scenario payload: %v", err)
	}
	return b
}

// insertPlan inserts a schema-valid "plan" artifact for ticketID at
// version, produced by runID (nil for a legacy plan with no producing run).
func insertPlan(t *testing.T, st *store.Store, ticketID int64, runID *int64, version int) {
	t.Helper()
	if _, err := st.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, RunID: runID, Type: "plan", Version: version, Payload: []byte(planExample),
	}); err != nil {
		t.Fatalf("InsertArtifact(plan): %v", err)
	}
}

// insertScenario inserts a schema-valid "scenario" artifact for ticketID,
// produced by runID, sealed at sealedAt (nil for unsealed).
func insertScenario(t *testing.T, st *store.Store, ticketID, runID int64, sealedAt *time.Time, id, kind, checkCmd, given, when, then string) {
	t.Helper()
	if _, err := st.InsertArtifact(t.Context(), store.Artifact{
		TicketID: ticketID, RunID: &runID, Type: "scenario", Version: 1,
		Payload: scenarioPayload(t, id, kind, checkCmd, given, when, then), SealedAt: sealedAt,
	}); err != nil {
		t.Fatalf("InsertArtifact(scenario %s): %v", id, err)
	}
}

// TestScenarios_JudgeFixturePrintsSealedScenariosInInsertionOrder pins the
// exact bytes zing scenarios prints for a judge run reading a ticket's
// current, sealed cohort (design section 8): three sealed scenarios, in
// insertion order, one XML element per line; the empty check_cmd on s1 omits
// the check attribute (Scenario.Check's omitempty tag); s2's check_cmd
// carries '&' and '<', escaped the standard way; the fourth, unsealed
// scenario (s4) never appears.
func TestScenarios_JudgeFixturePrintsSealedScenariosInInsertionOrder(t *testing.T) {
	st := newScenariosTestStore(t)
	ticketID, expires := seedClaimedTicket(t, st)

	planRunID := reserveRun(t, st, ticketID, expires, "planning")
	insertPlan(t, st, ticketID, &planRunID, 1)

	sealedAt := time.Now().UTC().Truncate(time.Second)
	insertScenario(t, st, ticketID, planRunID, &sealedAt, "s1", "behavior", "", "a signed-out user", "they open the dashboard", "they are redirected to sign in")
	insertScenario(t, st, ticketID, planRunID, &sealedAt, "s2", "negative", `go test -run "A && B < C"`, "a malformed token", "the middleware checks it", "the request is rejected")
	insertScenario(t, st, ticketID, planRunID, &sealedAt, "s3", "performance", "go test -bench BenchDashboard", "a warm cache", "the dashboard renders", "it renders under 200ms")
	insertScenario(t, st, ticketID, planRunID, nil, "s4", "behavior", "", "an unsealed scenario", "it is never reviewed", "it never ships")

	judgeRunID := reserveRun(t, st, ticketID, expires, "judge")

	var out, errOut bytes.Buffer
	getenv := func(k string) string {
		if k == scenariosTokenEnv {
			return strconv.FormatInt(judgeRunID, 10)
		}
		return ""
	}
	code := scenarios(t.Context(), st, getenv, &out, &errOut)

	if code != 0 {
		t.Errorf("code = %d, want 0 (stderr: %q)", code, errOut.String())
	}
	want := `<scenario id="s1" kind="behavior"><given>a signed-out user</given><when>they open the dashboard</when><then>they are redirected to sign in</then></scenario>` + "\n" +
		`<scenario id="s2" kind="negative" check="go test -run &#34;A &amp;&amp; B &lt; C&#34;"><given>a malformed token</given><when>the middleware checks it</when><then>the request is rejected</then></scenario>` + "\n" +
		`<scenario id="s3" kind="performance" check="go test -bench BenchDashboard"><given>a warm cache</given><when>the dashboard renders</when><then>it renders under 200ms</then></scenario>` + "\n"
	if out.String() != want {
		t.Errorf("stdout =\n%q\nwant\n%q", out.String(), want)
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want empty", errOut.String())
	}
}

// TestScenarios_NonJudgeRunExitsTwo proves a build (or any other non-judge)
// session's run is refused with the exact stderr text and nothing on
// stdout, even though the ticket has a perfectly good sealed cohort (design
// section 8 step 3).
func TestScenarios_NonJudgeRunExitsTwo(t *testing.T) {
	st := newScenariosTestStore(t)
	ticketID, expires := seedClaimedTicket(t, st)
	buildRunID := reserveRun(t, st, ticketID, expires, "build")

	var out, errOut bytes.Buffer
	getenv := func(k string) string {
		if k == scenariosTokenEnv {
			return strconv.FormatInt(buildRunID, 10)
		}
		return ""
	}
	code := scenarios(t.Context(), st, getenv, &out, &errOut)

	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if want := scenariosNotJudge + "\n"; errOut.String() != want {
		t.Errorf("stderr = %q, want %q", errOut.String(), want)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

// TestScenarios_NoRunContext covers every "no run context" trigger (design
// section 8 step 1-2): a missing ZING_RUN_TOKEN, a non-numeric one, and one
// naming a run that does not exist -- each exits 2 with the exact stderr
// text.
func TestScenarios_NoRunContext(t *testing.T) {
	st := newScenariosTestStore(t)

	cases := map[string]string{
		"missing token":     "",
		"non-numeric token": "abc",
		"unknown run id":    "999999",
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			getenv := func(k string) string {
				if k == scenariosTokenEnv {
					return token
				}
				return ""
			}
			code := scenarios(t.Context(), st, getenv, &out, &errOut)

			if code != 2 {
				t.Errorf("code = %d, want 2", code)
			}
			if want := scenariosNoRunContext + "\n"; errOut.String() != want {
				t.Errorf("stderr = %q, want %q", errOut.String(), want)
			}
			if out.String() != "" {
				t.Errorf("stdout = %q, want empty", out.String())
			}
		})
	}
}

// TestScenarios_RunContextInfraErrorExitsOne proves an operational
// RunContext failure (here, a closed store) is reported as exit 1, not the
// exit-2 "no run context" path a missing or unknown token takes (design
// section 8 step 2): only sql.ErrNoRows is a user-input problem.
func TestScenarios_RunContextInfraErrorExitsOne(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if closeErr := st.Close(); closeErr != nil {
		t.Fatalf("store.Close: %v", closeErr)
	}

	var out, errOut bytes.Buffer
	getenv := func(k string) string {
		if k == scenariosTokenEnv {
			return "1"
		}
		return ""
	}
	code := scenarios(t.Context(), st, getenv, &out, &errOut)

	if code != 1 {
		t.Errorf("code = %d, want 1 (operational error, not a bad token)", code)
	}
	if got := errOut.String(); got == scenariosNoRunContext+"\n" {
		t.Errorf("stderr = %q, want an operational error, not the no-run-context text", got)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

// TestScenarios_NoSealedRowsPrintsNothing proves a cohort whose scenarios
// are none of them sealed yet prints nothing and exits 0 (design section 8
// step 5): the gate has not sealed it, so the judge sees no scenarios,
// never a partial cohort.
func TestScenarios_NoSealedRowsPrintsNothing(t *testing.T) {
	st := newScenariosTestStore(t)
	ticketID, expires := seedClaimedTicket(t, st)

	planRunID := reserveRun(t, st, ticketID, expires, "planning")
	insertPlan(t, st, ticketID, &planRunID, 1)
	insertScenario(t, st, ticketID, planRunID, nil, "s1", "behavior", "", "g", "w", "t")

	judgeRunID := reserveRun(t, st, ticketID, expires, "judge")

	var out, errOut bytes.Buffer
	getenv := func(k string) string {
		if k == scenariosTokenEnv {
			return strconv.FormatInt(judgeRunID, 10)
		}
		return ""
	}
	code := scenarios(t.Context(), st, getenv, &out, &errOut)

	if code != 0 {
		t.Errorf("code = %d, want 0 (stderr: %q)", code, errOut.String())
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

// TestScenarios_NullCohortRunIDPrintsNothing proves a legacy plan artifact
// stored with no run_id (design section 4.5's Cohort.RunID doc comment)
// prints nothing and exits 0, rather than erroring: CurrentCohort still
// reports ok=true, but a nil RunID short-circuits before ScenariosForRun.
func TestScenarios_NullCohortRunIDPrintsNothing(t *testing.T) {
	st := newScenariosTestStore(t)
	ticketID, expires := seedClaimedTicket(t, st)
	insertPlan(t, st, ticketID, nil, 1)

	judgeRunID := reserveRun(t, st, ticketID, expires, "judge")

	var out, errOut bytes.Buffer
	getenv := func(k string) string {
		if k == scenariosTokenEnv {
			return strconv.FormatInt(judgeRunID, 10)
		}
		return ""
	}
	code := scenarios(t.Context(), st, getenv, &out, &errOut)

	if code != 0 {
		t.Errorf("code = %d, want 0 (stderr: %q)", code, errOut.String())
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}
