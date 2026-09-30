// fix.go is the fix unit (design section 5.2, 5.3, D18, D22): a fix
// request is named by an "update" marker written before its first run
// (fixRequestMessage, openFixRequest), and DriveFix runs the next step of
// the request's own unit -- RUN's first turn, the claims-pending resume,
// CHECK, DESCRIBE, ASK, or LAND -- sharing every step function of section
// 6 with a task unit (building.go's own check, land, describeOrAsk,
// describeOne, resolve, and advanceUnit, the shared unit-session switch);
// DriveFix only chooses the unit and its own current step.
//
// #28 gap 1 (D22): a fix unit's identity is its request marker plus a
// run-id watermark, not "any build_report row with task_n 0 ever landed"
// (the bug the old StartFix/AdvanceFix pair carried) -- openFixRequest and
// SessionAfter are what fix that.
//
// Nothing in serve calls DriveFix yet: Package 9's later tasks wire the
// three producers (a review's own findings, a failed scenario, a failing
// CI check) and the post-build prelude (section 5.5) that enters it.
package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"zing/internal/orchestrator"
	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// FixKind is the three shapes a fix run's own input can take (design
// section 8).
type FixKind string

const (
	FixKindFindings FixKind = "findings"
	FixKindFailure  FixKind = "failure"
	FixKindCILog    FixKind = "ci_log"
)

// Values returns the three kinds, in that order.
func (FixKind) Values() []string {
	return []string{string(FixKindFindings), string(FixKindFailure), string(FixKindCILog)}
}

// fixSubjectFor and fixInputLabelFor are design section 8's own table: the
// commit subject a landed fix carries (unit.Title) and the prompt input
// label its own text carries, one pair per FixKind. The input label
// happens to equal the kind's own wire value in all three rows, but the
// two are kept as separate maps since the plan names them as two distinct
// columns, not one.
var fixSubjectFor = map[FixKind]string{
	FixKindFindings: "Fix review findings",
	FixKindFailure:  "Fix failed scenarios",
	FixKindCILog:    "Fix the failing check",
}

var fixInputLabelFor = map[FixKind]string{
	FixKindFindings: "findings",
	FixKindFailure:  "failure",
	FixKindCILog:    "ci_log",
}

// FixRequest is one open fix unit, read back from its marker (design
// section 5.2, D18, D22).
type FixRequest struct {
	MessageID  int64 // the marker's message id; the unit's identity
	Kind       FixKind
	Text       string // lines 2.. of the marker; non-empty
	AfterRunID int64  // the watermark
}

// fixRequestMessage returns the marker that opens a fix unit (design
// section 5.1, 5.2): first line "fix requested <kind> after run <R>",
// lines 2.. the fix text. It fails before anything is written: an unknown
// kind (outside FixKind.Values) returns the error "job: unknown fix kind
// <kind>"; a text that is empty after strings.TrimSpace returns the error
// "job: fix input is empty".
func fixRequestMessage(t store.Ticket, kind FixKind, text string, afterRunID int64) (store.Message, error) {
	if _, known := fixSubjectFor[kind]; !known {
		return store.Message{}, fmt.Errorf("job: unknown fix kind %s", kind)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return store.Message{}, errors.New("job: fix input is empty")
	}
	body := fmt.Sprintf("fix requested %s after run %d\n%s", kind, afterRunID, text)
	return store.Message{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: body}, nil
}

// parseFixRequestLine parses a "fix requested <kind> after run <R>"
// marker's own first line (design section 5.1).
func parseFixRequestLine(firstLine string) (kind FixKind, afterRunID int64, ok bool) {
	fields := strings.Fields(firstLine)
	if len(fields) != 6 || fields[0] != "fix" || fields[1] != "requested" || fields[3] != "after" || fields[4] != "run" {
		return "", 0, false
	}
	r, err := strconv.ParseInt(fields[5], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return FixKind(fields[2]), r, true
}

// parseFixLandedMessageID parses a "fix landed <mid> sha <sha>" marker's
// own first line (design section 5.1), returning the request's own
// message id.
func parseFixLandedMessageID(firstLine string) (mid int64, ok bool) {
	fields := strings.Fields(firstLine)
	if len(fields) != 5 || fields[0] != "fix" || fields[1] != "landed" || fields[3] != "sha" {
		return 0, false
	}
	mid, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return 0, false
	}
	return mid, true
}

// fixRequestedPrefix and fixLandedPrefix are the two marker families
// openFixRequest reads with Store.MarkersWithPrefix (design section 5.1).
const (
	fixRequestedPrefix = "fix requested "
	fixLandedPrefix    = "fix landed "
)

// openFixRequest returns the newest "fix requested" marker that has no
// "fix landed <id> sha ..." marker (design section 5.2, D22). Two open
// requests is the error "job: ticket <id> has two open fix requests"
// (producers never write a request while one is open, so this is a bug,
// and it is loud). A marker whose first line does not parse is "job: fix
// request <mid>: malformed marker". ok is false, with no error, when no
// request is open.
func openFixRequest(ctx context.Context, d Deps, t store.Ticket) (FixRequest, bool, error) {
	requested, err := d.Store.MarkersWithPrefix(ctx, t.ID, fixRequestedPrefix)
	if err != nil {
		return FixRequest{}, false, fmt.Errorf("job: open fix request: %w", err)
	}
	landed, err := d.Store.MarkersWithPrefix(ctx, t.ID, fixLandedPrefix)
	if err != nil {
		return FixRequest{}, false, fmt.Errorf("job: open fix request: %w", err)
	}
	landedIDs := make(map[int64]bool, len(landed))
	for i := range landed {
		firstLine, _, _ := strings.Cut(landed[i].Body, "\n")
		if mid, ok := parseFixLandedMessageID(firstLine); ok {
			landedIDs[mid] = true
		}
	}

	var open []store.MessageRow
	for i := range requested {
		if !landedIDs[requested[i].ID] {
			open = append(open, requested[i])
		}
	}

	switch len(open) {
	case 0:
		return FixRequest{}, false, nil
	case 1:
		m := open[0]
		firstLine, text, _ := strings.Cut(m.Body, "\n")
		kind, afterRunID, ok := parseFixRequestLine(firstLine)
		if !ok {
			return FixRequest{}, false, fmt.Errorf("job: fix request %d: malformed marker", m.ID)
		}
		return FixRequest{MessageID: m.ID, Kind: kind, Text: text, AfterRunID: afterRunID}, true, nil
	default:
		return FixRequest{}, false, fmt.Errorf("job: ticket %d has two open fix requests", t.ID)
	}
}

// fixUnit is req's own unit (design section 5.3): "u := unit{TaskN: 0,
// Title: fixSubjectFor[req.Kind], FixRequestID: req.MessageID}".
func fixUnit(req FixRequest) unit {
	mid := req.MessageID
	return unit{TaskN: 0, Title: fixSubjectFor[req.Kind], FixRequestID: &mid}
}

// DriveFix runs one step of the fix unit named by req (design section 5.3,
// D22, #28 gap 1): an owner's answer to the fix run's own build-job
// question resumes that session (resumeBuildRound); otherwise, SessionAfter
// finds the request's own current session -- none (or a session whose
// external_id never got set) runs RUN's first turn with req.Text fenced;
// found hands off to advanceUnit, the same claims-pending resume, CHECK,
// DESCRIBE, ASK, and LAND switch a task unit shares.
//
// The round check only ever acts on an answered build-job round when
// SessionAfter, keyed by req's own watermark, confirms that round's own
// session belongs to this request: a ticket can carry another unit's own
// answered round at the same time (a task still awaiting its own answer,
// design D22), and this driver must never resume a round that is not its
// own. A fix's own perimeter-kind round (DESCRIBE/ASK) has no such check
// yet, so it is left for the state's own step machine; no producer wires
// one through DriveFix until a later task needs it.
//
// Step 0 reconciles the branch before CHECK or LAND run, exactly as
// building's own Run does (design section 5.4 change 4, #28 gap 4): a
// single unrecorded commit at the tip is adopted through h.adopt when it
// passes every check of Package 8's own adoption table, and any other
// mismatch between the branch and the recorded reports escalates
// environment, origin fix. Origin is tagged fix throughout (change 2,
// #28 gap 2): the two pre-reserve failures below call unitEscalation
// directly with u, and every shared step it calls into (check, land,
// describeOrAsk, describeOne, resolve, advanceUnit's own runFirst and
// runBuildResume) derives origin fix or build from the same u, by
// originFor's rule (u.TaskN == 0 is always a fix).
func DriveFix(ctx context.Context, t store.Ticket, d Deps, req FixRequest) (store.HandlerCommit, error) {
	h := buildingHandler{}
	u := fixUnit(req)
	maxResumes := d.Machine.Jobs[jobBuildName].MaxResumes

	rounds, err := d.Store.AnsweredRounds(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: answered rounds: %w", err)
	}
	if len(rounds) > 0 {
		round := rounds[0]
		newest := round.Questions[len(round.Questions)-1]
		if newest.ParentID == nil && round.Job == jobBuildName && round.SessionID != nil {
			fixSess, _, _, fixSessOK, sessErr := d.Store.SessionAfter(ctx, t.ID, jobBuildName, req.AfterRunID, maxResumes)
			if sessErr != nil {
				return store.HandlerCommit{}, fmt.Errorf("job: fix: session after: %w", sessErr)
			}
			if fixSessOK && fixSess.ID == *round.SessionID {
				if commit, again, resumeErr := h.resumeBuildRound(ctx, t, d, round, u); !again {
					return commit, resumeErr
				}
			}
		}
	}

	plan, _, havePlan, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: stored plan: %w", err)
	}
	if !havePlan {
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), noStoredPlanWhat, noStoredPlanWhy, ""), nil
	}

	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return store.HandlerCommit{}, ErrConfig
	}
	wt, created, err := proj.Orch.EnsureWorktree(ctx, t.ID, t.Title)
	if err != nil {
		return unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), worktreeNotPreparedWhat, worktreeNotPreparedWhy, err.Error()), nil
	}
	slog.Info("worktree ensured", "ticket_id", t.ID, "branch", wt.Branch(), "created", created)

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: build reports: %w", err)
	}

	// Step 0's branch reconcile (design section 5.3 step 0, 5.4 change 4,
	// #28 gap 4): the same read building's own Run does before choosing a
	// unit, so a fix commit git already holds -- landed by a previous tick
	// that then failed to store -- is adopted, or a foreign commit escalates,
	// before CHECK or LAND ever runs against the branch. u is always this
	// request's own fix unit here (origin fix throughout, change 2), unlike
	// building's own step 0, which has not chosen a unit yet.
	unrecorded, prefixOK, err := unrecordedCommits(ctx, proj, wt, reports)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if !prefixOK {
		return withBranch(unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), branchMissingRecordedWhat, branchMissingRecordedWhy, ""), wt), nil
	}
	switch len(unrecorded) {
	case 0:
		// continue to step 1
	case 1:
		commit, adoptErr := h.adopt(ctx, t, d, proj, wt, plan, reports, unrecorded[0])
		if adoptErr != nil {
			return store.HandlerCommit{}, adoptErr
		}
		return withBranch(commit, wt), nil
	default:
		return withBranch(unitEscalation(t, d, u, string(response.EscalationCodeEnvironment), foreignCommitsWhat, foreignCommitsWhy, ""), wt), nil
	}

	sess, state, newestRun, ok, err := d.Store.SessionAfter(ctx, t.ID, jobBuildName, req.AfterRunID, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: session after: %w", err)
	}

	if !ok || state == store.SessionIdless {
		commit, runErr := runFixFirst(ctx, t, d, proj, wt, plan, req, nil, nil)
		return withBranchResult(commit, runErr, wt)
	}

	commit, runErr := h.advanceUnit(ctx, t, d, proj, wt, plan, u, sess, state, newestRun, true, reports)
	return withBranchResult(commit, runErr, wt)
}

// runFixFirst is RUN's first turn for a fix unit (design section 5.3, and
// 5.4 change 3's own reuse for a retry): assembles prompt.ForFix's inputs
// from req and routes runJob's result through buildSuccessCommit, the same
// routing runFirst gives a task unit's own first turn. extra carries a
// retry's own "notes and error" input (task 3); a fresh request's own
// first RUN passes nil. resolveIDs resolves an escalation round's
// questions in the same commit as the fresh run's own terminalizing commit
// (task 3); a fresh request's own first RUN passes nil.
func runFixFirst(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, plan response.Plan, req FixRequest, extra []prompt.NamedInput, resolveIDs []int64) (store.HandlerCommit, error) {
	subject, knownKind := fixSubjectFor[req.Kind]
	if !knownKind {
		return store.HandlerCommit{}, fmt.Errorf("job: unknown fix kind %s", req.Kind)
	}
	label := fixInputLabelFor[req.Kind]

	jobCfg := d.Machine.Jobs[jobBuildName]
	promptText, err := readAsset(jobCfg.Prompt.Single)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: %w", err)
	}
	planXML, err := planXMLFor(plan)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: %w", err)
	}
	events, err := d.Store.FileEvents(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: file events: %w", err)
	}
	accepted := acceptedPaths(events)
	schemas, err := renderSchemas(response.JobBuild, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: %w", err)
	}

	in, err := prompt.ForFix(promptText, subject, label, req.Text, proj.TestCmd, proj.LintCmd, t.Title+"\n\n"+t.Body, planXML, accepted, extra)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: fix: %w", err)
	}
	in.Schemas = schemas
	assembled := prompt.Assemble(in)

	u := fixUnit(req)
	su := store.SessionUpsert{Job: jobBuildName, Runtime: jobCfg.Runtime}
	runReq := runtime.RunRequest{Job: response.JobBuild, Label: fixRunLabel, WorkDir: wt.Dir(), Prompt: assembled}
	return runAndRoute(ctx, d, t, jobBuildName, su, runReq, 0, freshSessionRecord, resolveIDs, response.EscalationOriginFix,
		func(rr runResult) (store.HandlerCommit, error) {
			return buildSuccessCommit(t, d, rr, freshSessionRecord(rr), resolveIDs, u)
		}, nil) // taskN nil: RunSeed.TaskN is nil for a fix (design section 8)
}
