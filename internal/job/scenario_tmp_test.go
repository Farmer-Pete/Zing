package job

import (
	"strings"
	"testing"

	"zing/internal/response"
)

// TestCheckScenarioShape_RejectsHostTmp is a regression test for a live
// judge round: every sealed check built its binary under /tmp, which the
// build sandbox denies, so Zing's own re-run of each check failed although
// the judge (which rewrote the path) saw them pass. A check that names
// /tmp is refused at planning time, pointing at $TMPDIR.
func TestCheckScenarioShape_RejectsHostTmp(t *testing.T) {
	t.Parallel()
	scenarios := []response.Scenario{
		{ID: "s1", Then: "prints the hash", Check: `sh -c 'go build -o /tmp/zing-s1 ./cmd/zing && /tmp/zing-s1 version'`},
		{ID: "s2", Then: "prints one line", Check: `sh -c 'go build -o "$TMPDIR/zing-s2" ./cmd/zing'`},
	}
	errs := checkScenarioShape(scenarios)
	if len(errs) != 1 || errs[0].Path != "scenarios/scenario[0]/check" || !strings.Contains(errs[0].Msg, "$TMPDIR") {
		t.Fatalf("checkScenarioShape = %+v, want one error on scenario[0]/check naming $TMPDIR", errs)
	}
}
