// judging.go holds the judging state machine's own runJob hook (PKG9-PLAN.md
// section 7.3, D19): the judge never opens the database (N6), so it reads
// its scenarios from one file Zing writes for its run alone, right after
// Reserve fixes the run id. M2 task 7 wires the rest of the judging state
// machine (START, RUN, CHECK, EVALUATE); this file owns only the hook.
package job

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// judgeScenariosDirPerm and judgeScenariosFilePerm are the per-run
// scenarios folder's and file's own modes (PKG9-PLAN.md section 7.3, D19):
// private to the owner, since this is the one copy of the sealed scenario
// text a judge run's own sandbox is allowed to read.
const (
	judgeScenariosDirPerm  = 0o700
	judgeScenariosFilePerm = 0o600
)

// writeScenariosFile returns runJobWith's own afterReserve hook for a judge
// run (PKG9-PLAN.md section 7.3, D19): right after Reserve fixes the run
// id, it writes the ticket's current, sealed scenario cohort to
// <DATA_DIR>/judge/<run id>/scenarios.xml, one <scenario> element per line
// in artifacts.id (insertion) order -- the exact bytes zing scenarios now
// copies straight to stdout (cmd/zing/scenarios.go) -- appends
// ZING_SCENARIOS_FILE to req.Env, and returns the file's path (the judge
// profile's own SCENARIOS_FILE parameter) and a cleanup that removes the
// whole run directory. RUN's own step 1 (M2 task 7) already checks the
// ticket has a sealed cohort before ever calling runJobWith, so a missing
// or empty one here is a surprise this hook still refuses rather than
// writing an empty or stale file.
func writeScenariosFile(d Deps, t store.Ticket) afterReserve {
	return func(ctx context.Context, rsv store.Reserved, req *runtime.RunRequest) (string, func() error, error) {
		cohort, ok, err := d.Store.CurrentCohort(ctx, t.ID)
		if err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: current cohort for ticket %d: %w", t.ID, err)
		}
		if !ok || cohort.RunID == nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: ticket %d has no sealed scenarios", t.ID)
		}

		rows, err := d.Store.ScenariosForRun(ctx, t.ID, *cohort.RunID, true)
		if err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: scenarios for ticket %d: %w", t.ID, err)
		}
		if len(rows) == 0 {
			return "", noopCleanupErr, fmt.Errorf("job: judge: ticket %d has no sealed scenarios", t.ID)
		}

		data, err := renderScenariosXML(rows)
		if err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: render scenarios for ticket %d: %w", t.ID, err)
		}

		dir := filepath.Join(d.DataDir, "judge", strconv.FormatInt(rsv.RunID, 10))
		if err := os.MkdirAll(dir, judgeScenariosDirPerm); err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: create scenarios dir %s: %w", dir, err)
		}
		if err := os.Chmod(dir, judgeScenariosDirPerm); err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: chmod scenarios dir %s: %w", dir, err)
		}

		path := filepath.Join(dir, "scenarios.xml")
		if err := os.WriteFile(path, data, judgeScenariosFilePerm); err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: write scenarios file %s: %w", path, err)
		}
		if err := os.Chmod(path, judgeScenariosFilePerm); err != nil {
			return "", noopCleanupErr, fmt.Errorf("job: judge: chmod scenarios file %s: %w", path, err)
		}

		req.Env = append(req.Env, "ZING_SCENARIOS_FILE="+path)
		cleanup := func() error { return os.RemoveAll(dir) }
		return path, cleanup, nil
	}
}

// renderScenariosXML renders rows (ScenariosForRun's own insertion order)
// as one <scenario> element per line: the same encoding cmd/zing/scenarios.go
// used to print straight from the database, now written to the judge's own
// scenarios file instead and copied from there byte for byte.
func renderScenariosXML(rows []store.Artifact) ([]byte, error) {
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)
	for i, row := range rows {
		var sc response.Scenario
		if err := json.Unmarshal(row.Payload, &sc); err != nil {
			return nil, fmt.Errorf("unmarshal scenario artifact %d: %w", i, err)
		}
		if err := enc.EncodeElement(sc, xml.StartElement{Name: xml.Name{Local: "scenario"}}); err != nil {
			return nil, fmt.Errorf("encode scenario artifact %d: %w", i, err)
		}
		if err := enc.Flush(); err != nil {
			return nil, fmt.Errorf("flush scenario artifact %d: %w", i, err)
		}
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}
