package job

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"zing/internal/runtime"
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
