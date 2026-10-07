// shiprules.go holds M3's pure shipping rules (design section 8.3, 8.4,
// 8.5, 8.7): CI evaluation, the poll fingerprint and its backoff, the
// shared shipping counter, the CI log-tail text for a ci_log fix request,
// and parsing a PR number back out of tickets.pr_url.
//
// pollFingerprint's thread input is PollThread, not a full orchestrator
// thread: M4 task 1 has not landed in this worktree, so orchestrator.Thread
// and orchestrator.ThreadComment (and the commentDigest helper that reduces
// one to a digest) do not exist yet. pollFingerprint only ever needs a
// thread's id, its resolved flag, and a digest of its last comment (the
// fingerprint's "thread" line), so PollThread carries exactly that; M4
// task 1's caller computes it from the real thread.
package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/store"
)

// CIState is EvaluateCI's verdict on a commit's checks against the base
// branch's required checks (design section 8.4).
type CIState string

const (
	CIGreen       CIState = "green"
	CIPending     CIState = "pending"
	CIFailed      CIState = "failed"
	CIUnprotected CIState = "unprotected"
)

// GitHub's own check-run and status wire values that EvaluateCI and its
// tests compare against. ghFailure and ghError are spelled as conversions
// of existing constants (FixKindFailure, response.OutcomeError), not fresh
// literals, because those two strings happen to already be declared
// elsewhere in this package and in response; goconst flags a third raw
// occurrence of either.
const (
	ghCompleted      = "completed"
	ghSuccess        = "success"
	ghNeutral        = "neutral"
	ghSkipped        = "skipped"
	ghFailure        = string(FixKindFailure)
	ghError          = string(response.OutcomeError)
	ghGitHubActions  = "github-actions"
	ghTimedOut       = "timed_out"
	ghCancelled      = "cancelled"
	ghActionRequired = "action_required"
	ghStartupFailure = "startup_failure"
	ghStale          = "stale"
)

// CIResult is EvaluateCI's full result (design section 8.4): the state;
// the runs and statuses whose conclusion or state put it in failed (empty
// when the state is not failed); and the required checks no run or status
// matched, sorted (empty when none are missing).
type CIResult struct {
	State          CIState
	FailedRuns     []orchestrator.CheckRun
	FailedStatuses []orchestrator.CommitStatus
	Missing        []string
	// ReportedGreen is true when every run and status that has reported
	// passed, ignoring required checks that have not reported at all. A
	// draft is marked ready on it (bug fix: an AI-review check that skips
	// drafts never reported, so the draft never became ready); the merge
	// gate still needs State CIGreen.
	ReportedGreen bool
}

// goodConclusions is the set of CheckRun.Conclusion values a completed run
// must have to count toward green (design section 8.4 rule 3).
var goodConclusions = map[string]bool{ghSuccess: true, ghNeutral: true, ghSkipped: true}

// failedConclusions is the set of CheckRun.Conclusion values that fail CI
// outright, regardless of whether the run is required (design section 8.4
// rule 2).
var failedConclusions = map[string]bool{
	ghFailure:        true,
	ghTimedOut:       true,
	ghCancelled:      true,
	ghActionRequired: true,
	ghStartupFailure: true,
	ghStale:          true,
}

// EvaluateCI is design section 8.4's CI rule. Runs are first reduced to
// the newest per (name, app id), highest id winning, so a newer run of
// the same name from another app never hides the bound app's run. Then,
// first match wins: no required check at all is unprotected; any run
// (completed, with a conclusion in the failed set) or any status (state
// failure or error) -- required or not -- is failed; still running or
// queued, an empty or unrecognized conclusion or status, or a required
// check matched by no run and no status, is pending; otherwise green. A
// required check with AppID nil is matched by a run whose Name equals its
// Context or a status whose Context equals it; one with AppID set is
// matched only by a run whose Name and AppID both equal it, never by a
// status. Fail closed: a value EvaluateCI does not recognize never counts
// toward green.
func EvaluateCI(runs []orchestrator.CheckRun, statuses []orchestrator.CommitStatus, required []orchestrator.RequiredCheck) CIResult {
	reduced := newestRunPerNameApp(runs)
	missing := missingRequired(reduced, statuses, required)

	if len(required) == 0 {
		return CIResult{State: CIUnprotected, Missing: missing}
	}

	failedRuns := failedCheckRuns(reduced)
	failedStatuses := failedCommitStatuses(statuses)
	if len(failedRuns) > 0 || len(failedStatuses) > 0 {
		return CIResult{State: CIFailed, FailedRuns: failedRuns, FailedStatuses: failedStatuses, Missing: missing}
	}

	reportedGreen := (len(reduced) > 0 || len(statuses) > 0) && !ciPending(reduced, statuses, nil)
	if ciPending(reduced, statuses, missing) {
		return CIResult{State: CIPending, Missing: missing, ReportedGreen: reportedGreen}
	}
	return CIResult{State: CIGreen, Missing: missing, ReportedGreen: reportedGreen}
}

// newestRunPerNameApp reduces runs to the newest (highest id) per (Name,
// AppID), in first-seen order.
func newestRunPerNameApp(runs []orchestrator.CheckRun) []orchestrator.CheckRun {
	type key struct {
		name  string
		appID int64
	}

	best := make(map[key]orchestrator.CheckRun, len(runs))
	order := make([]key, 0, len(runs))
	for _, r := range runs {
		k := key{r.Name, r.AppID}
		cur, ok := best[k]
		if !ok {
			order = append(order, k)
			best[k] = r
			continue
		}
		if r.ID > cur.ID {
			best[k] = r
		}
	}

	out := make([]orchestrator.CheckRun, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

func failedCheckRuns(runs []orchestrator.CheckRun) []orchestrator.CheckRun {
	var out []orchestrator.CheckRun
	for _, r := range runs {
		if r.Status == ghCompleted && failedConclusions[r.Conclusion] {
			out = append(out, r)
		}
	}
	return out
}

func failedCommitStatuses(statuses []orchestrator.CommitStatus) []orchestrator.CommitStatus {
	var out []orchestrator.CommitStatus
	for _, s := range statuses {
		if s.State == ghFailure || s.State == ghError {
			out = append(out, s)
		}
	}
	return out
}

// ciPending reports whether, with no outright failure, anything still
// keeps CI from green: a run not exactly completed with a good
// conclusion, a status not exactly success, or a required check with no
// matching run or status.
func ciPending(runs []orchestrator.CheckRun, statuses []orchestrator.CommitStatus, missing []string) bool {
	if len(missing) > 0 {
		return true
	}
	for _, r := range runs {
		if r.Status != ghCompleted || !goodConclusions[r.Conclusion] {
			return true
		}
	}
	for _, s := range statuses {
		if s.State != ghSuccess {
			return true
		}
	}
	return false
}

// missingRequired returns the Context of every required check matched by
// no run and no status, sorted.
func missingRequired(runs []orchestrator.CheckRun, statuses []orchestrator.CommitStatus, required []orchestrator.RequiredCheck) []string {
	var names []string
	for _, rc := range required {
		if requiredMatched(rc, runs, statuses) {
			continue
		}
		names = append(names, rc.Context)
	}
	sort.Strings(names)
	return names
}

func requiredMatched(rc orchestrator.RequiredCheck, runs []orchestrator.CheckRun, statuses []orchestrator.CommitStatus) bool {
	if rc.AppID != nil {
		for _, r := range runs {
			if r.Name == rc.Context && r.AppID == *rc.AppID {
				return true
			}
		}
		return false
	}
	for _, r := range runs {
		if r.Name == rc.Context {
			return true
		}
	}
	for _, s := range statuses {
		if s.Context == rc.Context {
			return true
		}
	}
	return false
}

// PollThread is what pollFingerprint needs from one review thread: its
// id, whether GitHub currently reports it resolved, and a digest of its
// last comment. See the file comment for why this stands in for a full
// orchestrator thread type in this worktree.
type PollThread struct {
	TID               string
	IsResolved        bool
	LastCommentDigest string
}

// pollFingerprint is design section 8.3's fingerprint: the lowercase hex
// SHA-256 of a canonical, order-independent rendering of the pull
// request's state, its checks and statuses, its review threads, the base
// branch's required checks, and EvaluateCI's own result over all of it.
// Changing only branch protection (a required check added, removed, or
// rebound to another app) still changes the hash, because the "required"
// and "ci" lines cover it.
func pollFingerprint(pr orchestrator.PRState, runs []orchestrator.CheckRun, statuses []orchestrator.CommitStatus, required []orchestrator.RequiredCheck, threads []PollThread) string {
	var b strings.Builder

	fmt.Fprintf(&b, "head %s\n", pr.HeadSHA)
	fmt.Fprintf(&b, "pr %s merged=%t draft=%t\n", pr.State, pr.Merged, pr.Draft)

	sortedRuns := append([]orchestrator.CheckRun(nil), runs...)
	sort.Slice(sortedRuns, func(i, j int) bool {
		if sortedRuns[i].Name != sortedRuns[j].Name {
			return sortedRuns[i].Name < sortedRuns[j].Name
		}
		if sortedRuns[i].AppID != sortedRuns[j].AppID {
			return sortedRuns[i].AppID < sortedRuns[j].AppID
		}
		return sortedRuns[i].ID < sortedRuns[j].ID
	})
	for _, r := range sortedRuns {
		fmt.Fprintf(&b, "check %s %d %s %s\n", r.Name, r.AppID, r.Status, r.Conclusion)
	}

	sortedStatuses := append([]orchestrator.CommitStatus(nil), statuses...)
	sort.Slice(sortedStatuses, func(i, j int) bool { return sortedStatuses[i].Context < sortedStatuses[j].Context })
	for _, s := range sortedStatuses {
		fmt.Fprintf(&b, "status %s %s\n", s.Context, s.State)
	}

	sortedThreads := append([]PollThread(nil), threads...)
	sort.Slice(sortedThreads, func(i, j int) bool { return sortedThreads[i].TID < sortedThreads[j].TID })
	for _, th := range sortedThreads {
		fmt.Fprintf(&b, "thread %s %t %s\n", th.TID, th.IsResolved, th.LastCommentDigest)
	}

	sortedReq := append([]orchestrator.RequiredCheck(nil), required...)
	sort.Slice(sortedReq, func(i, j int) bool {
		if sortedReq[i].Context != sortedReq[j].Context {
			return sortedReq[i].Context < sortedReq[j].Context
		}
		return requiredAppIDText(sortedReq[i].AppID) < requiredAppIDText(sortedReq[j].AppID)
	})
	for _, rc := range sortedReq {
		fmt.Fprintf(&b, "required %s %s\n", rc.Context, requiredAppIDText(rc.AppID))
	}

	result := prCI(pr, runs, statuses, required)
	fmt.Fprintf(&b, "ci %s missing=%s\n", result.State, strings.Join(result.Missing, ","))

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func requiredAppIDText(appID *int64) string {
	if appID == nil {
		return "any"
	}
	return strconv.FormatInt(*appID, 10)
}

// nextInterval is design section 8.3's backoff: when prevFP is nil or
// differs from fp, the interval resets to 30; otherwise it doubles the
// previous interval (a nil prevIntervalS reads as 30), capped at 300.
func nextInterval(prevFP *string, prevIntervalS *int, fp string) int {
	if prevFP == nil || *prevFP != fp {
		return 30
	}
	prev := 30
	if prevIntervalS != nil {
		prev = *prevIntervalS
	}
	return min(2*prev, 300)
}

// shippingGate is design section 8.7's shared shipping counter: CI fixes
// and review-thread fixes draw from one limit, jobs.respond.max_loops. k
// is the number of "fix requested ci_log" markers plus "fix requested
// threads" markers already on the ticket, before this fix. When k is at
// or past maxLoops, ok is false and what, why, and tried hold the
// loops_exhausted escalation's fields: what names the count, why names
// the shared limit, and tried is kind on its own line, then -- for kind
// "threads" -- "respond <aid>" naming the respond artifact whose APPLY
// hit the gate, then the fix request text. When k has not reached the
// gate, ok is true and the fix may proceed; what, why, and tried are
// zero.
func shippingGate(k, maxLoops int, kind string, aid int64, request string) (ok bool, what, why, tried string) {
	if k < maxLoops {
		return true, "", "", ""
	}

	what = fmt.Sprintf("shipping needed more than %d fix runs", k)
	why = fmt.Sprintf("CI fixes and review-thread fixes share a limit of %d", maxLoops)

	lines := []string{kind}
	if kind == "threads" {
		lines = append(lines, fmt.Sprintf("respond %d", aid))
	}
	lines = append(lines, request)
	tried = strings.Join(lines, "\n")
	return false, what, why, tried
}

// actionsJobIDPattern pulls the workflow run id and the job id out of a
// check run's DetailsURL.
var actionsJobIDPattern = regexp.MustCompile(`/actions/runs/(\d+)/job/(\d+)`)

// rerunAppearWait is how long a re-run may take to show up in the
// check-run list before its old failed run counts again (decideCIRerun
// rule 1).
const rerunAppearWait = 10 * time.Minute

// infraRerunCap is the most infrastructure re-runs per check per sha
// (decideCIRerun rule 2).
const infraRerunCap = 3

// infraConclusions are the conclusions that mean the job never ran to a
// verdict: re-run, never fix.
var infraConclusions = map[string]bool{ghCancelled: true, ghStartupFailure: true, ghStale: true}

// rerunInfraWhat is rerunDecision.What for both infra escalate branches of
// decideCIRerun's per-check rule (rule 2).
const rerunInfraWhat = "a CI check keeps failing to start"

// rerunNoLogWhat is rerunDecision.What for decideCIRerun's no-log escalate
// branch (rule 4).
const rerunNoLogWhat = "a failed CI check has no readable log"

// rerunNameInvalidWhat is rerunDecision.What for an infra conclusion whose
// check name decideCIRerun refuses to plan a re-run for (checkNameValid).
const rerunNameInvalidWhat = "a CI check's name cannot be recorded"

// maxCheckNameRunes is the check_rerun and check_rerun_passed schemas' own
// maxLength on check, counted in runes to match JSON Schema's own count.
const maxCheckNameRunes = 200

// checkNameValid reports whether name fits check_rerun's schema (1 to 200
// runes): decideCIRerun never plans a re-run RerunJob would make real but
// the store would then refuse to record, which would leave it unrecorded
// and so re-run again every later tick, with neither budget nor cap ever
// taking hold.
func checkNameValid(name string) bool {
	n := utf8.RuneCountInString(name)
	return n >= 1 && n <= maxCheckNameRunes
}

// failedCheck is one failed check run together with what readFailedChecks
// could learn about it.
type failedCheck struct {
	Run orchestrator.CheckRun // a run from CIResult.FailedRuns
	// RunID is the Actions workflow run id; 0 unless AppSlug is
	// github-actions and DetailsURL matches actionsJobIDPattern.
	RunID int64
	// JobID is the Actions job id; 0 under the same condition as RunID.
	JobID int64
	// Log is JobLogTail's text (200-line cap); empty when LogErr is set
	// or no read happened.
	Log string
	// LogErr is JobLogTail's error; nil when read, or when no read was
	// attempted.
	LogErr error
}

// isActionsJob reports whether fc is a GitHub Actions run Zing found both
// workflow and job ids for: readFailedChecks sets RunID and JobID
// together, from the same actionsJobIDPattern match, so either both are
// nonzero or both are 0.
func (fc failedCheck) isActionsJob() bool {
	return fc.RunID != 0 && fc.JobID != 0
}

// logRead reports whether readFailedChecks attempted to read fc's job
// log: an Actions job whose conclusion is not infrastructure.
func (fc failedCheck) logRead() bool {
	return fc.isActionsJob() && !infraConclusions[fc.Run.Conclusion]
}

// readFailedChecks reads every failed run's facts, sorted by name. For a
// github-actions run whose DetailsURL matches actionsJobIDPattern, it
// parses RunID and JobID from the two groups inline (a ParseInt error
// leaves both 0). For such a run whose conclusion is not infrastructure
// (logRead), it fetches JobLogTail(jobID, 200) into Log or LogErr. It
// never reads an infrastructure run's log.
func readFailedChecks(ctx context.Context, checks Checks, owner, repo string, runs []orchestrator.CheckRun) []failedCheck {
	out := make([]failedCheck, 0, len(runs))
	for _, r := range runs {
		fc := failedCheck{Run: r}
		if r.AppSlug == ghGitHubActions {
			if m := actionsJobIDPattern.FindStringSubmatch(r.DetailsURL); m != nil {
				runID, err1 := strconv.ParseInt(m[1], 10, 64)
				jobID, err2 := strconv.ParseInt(m[2], 10, 64)
				if err1 == nil && err2 == nil {
					fc.RunID = runID
					fc.JobID = jobID
				}
			}
		}
		if fc.logRead() {
			tail, err := checks.JobLogTail(ctx, owner, repo, fc.JobID, 200)
			if err != nil {
				fc.LogErr = err
			} else {
				fc.Log = tail
			}
		}
		out = append(out, fc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Run.Name < out[j].Run.Name })
	return out
}

// workflowRunsInFlight returns the set of Actions workflow run ids that
// have at least one check run GitHub has not completed yet, built from
// every run Actions reports, not only the failed ones: a failed check's
// own sibling job in the same workflow run can still be queued or in
// progress, and GitHub refuses to re-run a job until its whole workflow
// run is complete.
func workflowRunsInFlight(runs []orchestrator.CheckRun) map[int64]bool {
	inFlight := make(map[int64]bool)
	for _, r := range runs {
		if r.AppSlug != ghGitHubActions || r.Status == ghCompleted {
			continue
		}
		m := actionsJobIDPattern.FindStringSubmatch(r.DetailsURL)
		if m == nil {
			continue
		}
		if runID, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			inFlight[runID] = true
		}
	}
	return inFlight
}

// ciLogTextFrom is design section 8.5's ci_log text, built from what
// readFailedChecks already read: the first 3 failed checks by name, each
// "check NAME (CONCLUSION)" then a newline and its Log; "check NAME
// (CONCLUSION): the log could not be read: ERR" when LogErr is set; or
// "check NAME (CONCLUSION) DETAILS-URL" when no log was read; then one
// "status CONTEXT (STATE) URL" line per failed status. Parts are joined
// by a blank line.
func ciLogTextFrom(failed []failedCheck, failedStatuses []orchestrator.CommitStatus) string {
	checks := failed
	if len(checks) > 3 {
		checks = checks[:3]
	}

	var parts []string
	for i := range checks {
		fc := &checks[i]
		r := fc.Run
		switch {
		case fc.LogErr != nil:
			parts = append(parts, fmt.Sprintf("check %s (%s): the log could not be read: %s", r.Name, r.Conclusion, fc.LogErr))
		case fc.logRead():
			parts = append(parts, fmt.Sprintf("check %s (%s)\n%s", r.Name, r.Conclusion, fc.Log))
		default:
			parts = append(parts, fmt.Sprintf("check %s (%s) %s", r.Name, r.Conclusion, r.DetailsURL))
		}
	}

	for _, s := range failedStatuses {
		parts = append(parts, fmt.Sprintf("status %s (%s) %s", s.Context, s.State, s.TargetURL))
	}

	return strings.Join(parts, "\n\n")
}

// failedTestPattern matches a go test failure header.
var failedTestPattern = regexp.MustCompile(`--- FAIL: (\S+)`)

// failedTestNames returns the distinct test names text's "--- FAIL: NAME"
// lines carry, in first-seen order, at most 20, each cut to 200 bytes.
func failedTestNames(text string) []string {
	matches := failedTestPattern.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool, len(matches))
	var out []string
	for _, m := range matches {
		name := m[1]
		if len(name) > 200 {
			name = name[:200]
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) == 20 {
			break
		}
	}
	return out
}

// checkKey identifies a check run by the check's name and the GitHub App
// that created it, so a run on the PR's head sha can be matched against
// main's own run of the same check.
type checkKey struct {
	Name  string
	AppID int64
}

// preExisting is decidePreExisting's own result.
type preExisting struct {
	Pre   bool     // true only when every failed PR check matched a failure on main
	Tests []string // distinct PR test names also failing on main, first-seen order
	NoRun []string // names of failed PR checks with no completed run on main, name order
}

// decidePreExisting compares each failed PR check's failing test names with
// main's run of the same check (design shape, "Rules of decidePreExisting").
func decidePreExisting(failed []failedCheck, failedStatuses int, base map[checkKey][]string) preExisting {
	var out preExisting
	seen := make(map[string]bool)
	all := len(failed) != 0 && failedStatuses == 0
	for i := range failed {
		fc := failed[i]
		names := failedTestNames(fc.Log)
		if len(names) == 0 {
			all = false
			continue
		}
		onBase, ok := base[checkKey{Name: fc.Run.Name, AppID: fc.Run.AppID}]
		if !ok {
			out.NoRun = append(out.NoRun, fc.Run.Name)
			all = false
			continue
		}
		matched := 0
		for _, n := range names {
			if !slices.Contains(onBase, n) {
				continue
			}
			matched++
			if !seen[n] {
				seen[n] = true
				out.Tests = append(out.Tests, n)
			}
		}
		if matched != len(names) {
			all = false
		}
	}
	out.Pre = all
	return out
}

// priorRerun is one check_rerun event already on the ticket for the head
// sha under consideration, decoded.
type priorRerun struct {
	Event response.CheckRerunEvent // decoded payload
	ID    int64                    // the message row's id, for the retry-marker filter
	At    time.Time                // the message row's CreatedAt, UTC
}

// plannedRerun is one check decideCIRerun has decided to re-run.
type plannedRerun struct {
	Event response.CheckRerunEvent // the event to write once RerunJob succeeds
	JobID int64                    // the job RerunJob re-runs
}

// rerunAction is rerunDecision's own verdict.
type rerunAction string

const (
	rerunFix      rerunAction = "fix"
	rerunNow      rerunAction = "now"
	rerunWait     rerunAction = "wait"
	rerunEscalate rerunAction = "escalate"
)

// rerunDecision is decideCIRerun's own result (design shape, "Per-check
// rule").
type rerunDecision struct {
	Action rerunAction
	Reruns []plannedRerun // non-empty only for Action rerunNow, sorted by check name
	What   string         // Action rerunEscalate only
	Why    string         // Action rerunEscalate only
	Tried  string         // Action rerunEscalate only
	Text   string         // ciLogTextFrom's text, set by ciRerunDecision for every action
}

// triedRunsText renders rerunDecision.Tried for an escalation that
// followed one or more prior re-runs: "re-ran workflow runs R1, R2, R3",
// or "" when events is empty.
func triedRunsText(events []priorRerun) string {
	if len(events) == 0 {
		return ""
	}
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = strconv.FormatInt(e.Event.RunID, 10)
	}
	return "re-ran workflow runs " + strings.Join(ids, ", ")
}

// decideCIRerun applies the per-check rule (design shape, "Per-check
// rule") to every failed check and aggregates: escalate over rerun over
// fix over wait. failed is sorted by name, as readFailedChecks returns it,
// so decideCIRerun does no sorting of its own. inFlight is
// workflowRunsInFlight's own result: a failed check whose workflow run
// still has an incomplete sibling job waits rather than re-runs, since
// GitHub refuses to re-run a single job until its whole workflow run is
// complete (r2f1) -- the same reason a tick never plans more than one
// re-run per workflow run id (the dedupe below), so two failed jobs from
// one run never race each other's own re-run call within a tick. That
// wait only ever replaces a verdict the rule below would otherwise plan
// as a re-run (r3f6): a budget already spent, a cap already hit, or a
// check the rules send straight to fix or escalate is never delayed by
// an unrelated job still running in the same workflow run.
func decideCIRerun(now time.Time, sha string, failed []failedCheck, failedStatuses int, prior []priorRerun, inFlight map[int64]bool) rerunDecision {
	type verdict struct {
		action rerunAction
		rerun  plannedRerun
		what   string
		why    string
		tried  string
	}

	sha7 := shortSHA(sha)
	var verdicts []verdict
	for i := range failed {
		fc := &failed[i]
		name := fc.Run.Name
		isActionsJob := fc.isActionsJob()

		var infra, used []priorRerun
		waiting := false
		for _, p := range prior {
			if p.Event.Check != name {
				continue
			}
			sameRun := p.Event.CheckRunID == fc.Run.ID
			recent := now.Sub(p.At) < rerunAppearWait
			if sameRun && recent {
				waiting = true
			}
			switch p.Event.Reason {
			case response.RerunReasonInfra:
				infra = append(infra, p)
			case response.RerunReasonFlaky, response.RerunReasonNoLog:
				used = append(used, p)
			}
		}
		if waiting {
			verdicts = append(verdicts, verdict{action: rerunWait})
			continue
		}
		// blockedByRun reports whether fc's workflow run still has an
		// incomplete sibling job (r3f6: checked only where a rule below
		// would otherwise plan a re-run, never ahead of a fix or escalate
		// verdict, so a budget already spent or a cap already hit is
		// never delayed by an unrelated job in the same run).
		blockedByRun := isActionsJob && inFlight[fc.RunID]

		if infraConclusions[fc.Run.Conclusion] {
			if !isActionsJob {
				verdicts = append(verdicts, verdict{
					action: rerunEscalate,
					what:   rerunInfraWhat,
					why:    fmt.Sprintf("%s ended %s on %s and is not a GitHub Actions job, so Zing cannot re-run it", name, fc.Run.Conclusion, sha7),
				})
				continue
			}
			if !checkNameValid(name) {
				verdicts = append(verdicts, verdict{
					action: rerunEscalate,
					what:   rerunNameInvalidWhat,
					why:    fmt.Sprintf("a check ended %s on %s with a name %d runes long, so Zing cannot record a re-run of it", fc.Run.Conclusion, sha7, utf8.RuneCountInString(name)),
				})
				continue
			}
			if len(infra) < infraRerunCap {
				if blockedByRun {
					verdicts = append(verdicts, verdict{action: rerunWait})
					continue
				}
				verdicts = append(verdicts, verdict{
					action: rerunNow,
					rerun: plannedRerun{
						Event: response.CheckRerunEvent{Check: name, SHA: sha, RunID: fc.RunID, CheckRunID: fc.Run.ID, Reason: response.RerunReasonInfra},
						JobID: fc.JobID,
					},
				})
				continue
			}
			verdicts = append(verdicts, verdict{
				action: rerunEscalate,
				what:   rerunInfraWhat,
				why:    fmt.Sprintf("%s ended %s on %s after %d re-runs", name, fc.Run.Conclusion, sha7, infraRerunCap),
				tried:  triedRunsText(infra),
			})
			continue
		}

		if !isActionsJob || !checkNameValid(name) {
			verdicts = append(verdicts, verdict{action: rerunFix})
			continue
		}

		if fc.LogErr != nil {
			if len(used) == 0 {
				if blockedByRun {
					verdicts = append(verdicts, verdict{action: rerunWait})
					continue
				}
				verdicts = append(verdicts, verdict{
					action: rerunNow,
					rerun: plannedRerun{
						Event: response.CheckRerunEvent{Check: name, SHA: sha, RunID: fc.RunID, CheckRunID: fc.Run.ID, Reason: response.RerunReasonNoLog},
						JobID: fc.JobID,
					},
				})
				continue
			}
			verdicts = append(verdicts, verdict{
				action: rerunEscalate,
				what:   rerunNoLogWhat,
				why:    fmt.Sprintf("%s failed again on %s and its log could not be read: %s", name, sha7, fc.LogErr),
				tried:  triedRunsText(used),
			})
			continue
		}

		if len(used) == 0 {
			if blockedByRun {
				verdicts = append(verdicts, verdict{action: rerunWait})
				continue
			}
			verdicts = append(verdicts, verdict{
				action: rerunNow,
				rerun: plannedRerun{
					Event: response.CheckRerunEvent{Check: name, SHA: sha, RunID: fc.RunID, CheckRunID: fc.Run.ID, Reason: response.RerunReasonFlaky, Tests: failedTestNames(fc.Log)},
					JobID: fc.JobID,
				},
			})
			continue
		}
		verdicts = append(verdicts, verdict{action: rerunFix})
	}

	for i := range verdicts {
		if verdicts[i].action == rerunEscalate {
			v := &verdicts[i]
			return rerunDecision{Action: rerunEscalate, What: v.what, Why: v.why, Tried: v.tried}
		}
	}

	// At most one planned re-run per workflow run id: calling RerunJob for
	// one job in a run puts the whole run back in progress, so a second
	// call for another job in that same run would hit GitHub's own "not
	// complete yet" refusal (r2f1). The dropped check is re-planned once
	// its run finishes again (inFlight, above).
	var reruns []plannedRerun
	seenRun := make(map[int64]bool, len(verdicts))
	for i := range verdicts {
		if verdicts[i].action != rerunNow {
			continue
		}
		r := verdicts[i].rerun
		if seenRun[r.Event.RunID] {
			continue
		}
		seenRun[r.Event.RunID] = true
		reruns = append(reruns, r)
	}
	if len(reruns) > 0 {
		return rerunDecision{Action: rerunNow, Reruns: reruns}
	}

	for i := range verdicts {
		if verdicts[i].action == rerunFix {
			return rerunDecision{Action: rerunFix}
		}
	}
	if failedStatuses > 0 {
		return rerunDecision{Action: rerunFix}
	}

	return rerunDecision{Action: rerunWait}
}

// rerunPassedNotes returns one check_rerun_passed event message per check
// that has a flaky or no_log check_rerun event in reruns, whose newest run
// by id in runs (same Name) is completed with a good conclusion and an id
// other than the event's check_run_id, and that has no event in passed
// for the same check. Tests come from the newest such check_rerun event.
// Messages are sorted by check name, and each one it writes also logs an
// info line (r3f9: the only caller that needs the decoded payload is this
// function's own log line, so it logs there instead of returning a
// second slice for rerunNotesFor to re-decode and log itself).
func rerunPassedNotes(ticketID int64, sha string, runs []orchestrator.CheckRun, reruns []response.CheckRerunEvent, passed []response.CheckRerunPassedEvent) ([]store.Message, error) {
	passedChecks := make(map[string]bool, len(passed))
	for _, p := range passed {
		passedChecks[p.Check] = true
	}

	newestEvent := make(map[string]response.CheckRerunEvent)
	for _, e := range reruns {
		if e.Reason != response.RerunReasonFlaky && e.Reason != response.RerunReasonNoLog {
			continue
		}
		newestEvent[e.Check] = e
	}

	newestRun := make(map[string]orchestrator.CheckRun)
	for _, r := range runs {
		cur, ok := newestRun[r.Name]
		if !ok || r.ID > cur.ID {
			newestRun[r.Name] = r
		}
	}

	names := make([]string, 0, len(newestEvent))
	for name := range newestEvent {
		names = append(names, name)
	}
	sort.Strings(names)

	var msgs []store.Message
	for _, name := range names {
		if passedChecks[name] {
			continue
		}
		event := newestEvent[name]
		run, ok := newestRun[name]
		isNewRun := ok && run.ID != event.CheckRunID
		runPassed := run.Status == ghCompleted && goodConclusions[run.Conclusion]
		if !isNewRun || !runPassed {
			continue
		}
		passedEvent := response.CheckRerunPassedEvent{
			Check: name,
			SHA:   sha,
			Tests: event.Tests,
		}
		msg, err := store.NewEvent(ticketID, store.EventKindCheckRerunPassed, passedEvent)
		if err != nil {
			return nil, fmt.Errorf("job: shipping: poll: check_rerun_passed event for %s: %w", name, err)
		}
		msgs = append(msgs, msg)
		slog.Info("ci check passed on re-run", "ticket_id", ticketID, "check", passedEvent.Check, "sha", passedEvent.SHA, "tests", passedEvent.Tests)
	}
	return msgs, nil
}

// prURLPattern is the shape tickets.pr_url must have for parsePRNumber to
// read its number back (design section 8.2).
var prURLPattern = regexp.MustCompile(`^https://[^/]+/[^/]+/[^/]+/pull/([1-9]\d*)$`)

// parsePRNumber parses the PR number back out of a pull request URL
// (design section 8.2). A URL that does not match is the error
// "job: shipping: pr url <url> has no number".
func parsePRNumber(url string) (int, error) {
	m := prURLPattern.FindStringSubmatch(url)
	if m == nil {
		return 0, fmt.Errorf("job: shipping: pr url %s has no number", url)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("job: shipping: pr url %s has no number", url)
	}
	return n, nil
}

// mergeDecision is design section 8.8's own merge rule, task 8's addition:
// rule.Auto off blocks outright ("merge.auto is off"); a changed path
// matching a ManualPaths glob blocks with every matching path named, in
// changed's own order ("the diff touches a manual-deploy path: <paths>");
// a changed path equal to a DependencyFiles entry, or whose base name
// equals one, blocks the same way ("the diff changes a dependency file:
// <paths>"); otherwise merge.auto's own rule allows it ("merge.auto is on
// and no rule blocks it"). changed is ChangedFilesSinceBase's own sorted
// result (orchestrator/review.go); mergeDecision does no sorting of its
// own.
func mergeDecision(rule MergeRule, changed []string) (auto bool, reason string) {
	if !rule.Auto {
		return false, "merge.auto is off"
	}

	var manual []string
	for _, p := range changed {
		if matchesAnyMergePattern(rule.ManualPaths, p) {
			manual = append(manual, p)
		}
	}
	if len(manual) > 0 {
		return false, "the diff touches a manual-deploy path: " + strings.Join(manual, ", ")
	}

	var dep []string
	for _, p := range changed {
		if isMergeDependencyFile(rule.DependencyFiles, p) {
			dep = append(dep, p)
		}
	}
	if len(dep) > 0 {
		return false, "the diff changes a dependency file: " + strings.Join(dep, ", ")
	}

	return true, "merge.auto is on and no rule blocks it"
}

// isMergeDependencyFile reports whether p is design section 8.8's own
// dependency-file match: equal to one of deps, or whose base name equals
// one (so "go.mod" at the repo root and "web/go.mod" in a subdirectory
// both match the entry "go.mod").
func isMergeDependencyFile(deps []string, p string) bool {
	base := path.Base(p)
	for _, d := range deps {
		if p == d || base == d {
			return true
		}
	}
	return false
}

func matchesAnyMergePattern(patterns []string, p string) bool {
	for _, pattern := range patterns {
		if matchesMergePattern(pattern, p) {
			return true
		}
	}
	return false
}

// matchesMergePattern is mergeDecision's own ManualPaths glob (design
// section 8.8: "the perimeter matcher, ** spans directories"). config.go's
// own default manual_paths includes "**/migrations/**", a leading-**
// shape internal/orchestrator/perimeter.go's own unexported matchPattern
// does not support (its own "/**" special case only ever matches a
// trailing segment); mergeDecision needs the fuller shape, so this is a
// small recursive-descent matcher over the pattern and path each split on
// "/" (the repo's own style: recursive descent for any parsing, no parser
// generators), not a copy of that narrower one. "**" matches zero or more
// whole segments; any other segment matches with path.Match, Go's own
// single-segment wildcard ("*", "?", "[...]").
func matchesMergePattern(pattern, p string) bool {
	return matchPathSegments(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchPathSegments(pat, seg []string) bool {
	if len(pat) == 0 {
		return len(seg) == 0
	}
	if pat[0] == "**" {
		if matchPathSegments(pat[1:], seg) {
			return true
		}
		return len(seg) > 0 && matchPathSegments(pat, seg[1:])
	}
	if len(seg) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], seg[0])
	if err != nil || !ok {
		return false
	}
	return matchPathSegments(pat[1:], seg[1:])
}

// attestAppStatuses adds a synthetic successful check run for each required
// check bound to an app that no check run matches but a successful commit
// status of the same context does, only when GitHub's own mergeableState is
// "clean" (bug fix: CodeRabbit reports a status, not a check run, so an
// app-bound CodeRabbit requirement stayed missing forever). GitHub enforces
// the app identity of a required status itself, so a spoofed status leaves
// the pull request "blocked" and gains nothing here.
func attestAppStatuses(runs []orchestrator.CheckRun, statuses []orchestrator.CommitStatus, required []orchestrator.RequiredCheck, mergeableState string) []orchestrator.CheckRun {
	if mergeableState != "clean" {
		return runs
	}
	out := runs
	for _, rc := range required {
		if rc.AppID == nil || requiredMatched(rc, runs, nil) {
			continue
		}
		for _, st := range statuses {
			if st.Context == rc.Context && st.State == ghSuccess {
				out = append(out, orchestrator.CheckRun{Name: rc.Context, AppID: *rc.AppID, Status: ghCompleted, Conclusion: ghSuccess})
				break
			}
		}
	}
	return out
}

// prCI is EvaluateCI for one pull request, with attestAppStatuses applied
// from its own mergeable state: POLL, MERGE, and the poll fingerprint all
// call this one function, so they never disagree on whether CI is green
// (bug fix: POLL asked to merge while MERGE's re-check refused).
func prCI(pr orchestrator.PRState, runs []orchestrator.CheckRun, statuses []orchestrator.CommitStatus, required []orchestrator.RequiredCheck) CIResult {
	return EvaluateCI(attestAppStatuses(runs, statuses, required, pr.MergeableState), statuses, required)
}

// baseMergePrefix is the "base merge " marker family a base merge request,
// its landed or closed end marker, and an escalation's Tried text all
// share (openBaseMergeRequest, pollMergeCount, baseMergeTriedID).
const baseMergePrefix = "base merge "

var (
	baseMergeRequestedLine = regexp.MustCompile(`^base merge requested after run (0|[1-9]\d*)$`)
	baseMergeBaseLine      = regexp.MustCompile(`^base (\S+) ([0-9a-f]{40})$`)
	baseMergePointLine     = regexp.MustCompile(`^point (review|judge|ci)$`)
	baseMergeRetryLine     = regexp.MustCompile(`^retry of ([1-9]\d*)$`)
	baseMergeEndLine       = regexp.MustCompile(`^base merge (?:landed ([1-9]\d*) sha [0-9a-f]{40}|closed ([1-9]\d*))$`)
	baseMergeTriedLine     = regexp.MustCompile(`^base merge ([1-9]\d*)$`)
)

// syncPoint names a baseSync call site (basesync.go): review before review
// round 1, judge before a judge round starts, or ci before shipping sends a
// CI-failure fix request. The empty string, syncPoint's zero value, means a
// request POLL wrote, not one of these three points.
type syncPoint string

const (
	syncPointReview syncPoint = "review"
	syncPointJudge  syncPoint = "judge"
	syncPointCI     syncPoint = "ci"
)

// baseMergeClosedBody renders the closed end marker baseMergeEndLine
// parses back: "base merge closed <id>".
func baseMergeClosedBody(id int64) string {
	return fmt.Sprintf("base merge closed %d", id)
}

// baseMergeLandedBody renders the landed end marker baseMergeEndLine
// parses back: "base merge landed <id> sha <sha>".
func baseMergeLandedBody(id int64, sha string) string {
	return fmt.Sprintf("base merge landed %d sha %s", id, sha)
}

// baseMergeRequest is one base-merge request marker, read back or about to
// be written (overview design, "Markers" table).
type baseMergeRequest struct {
	MessageID  int64     // the marker's message id; 0 before it is written
	AfterRunID int64     // the SessionAfter watermark
	BaseBranch string    // the project's default branch, as POLL read it
	BaseSHA    string    // 40 lowercase hex characters
	Point      syncPoint // "" for a request POLL wrote; else review, judge or ci
	RetryOf    int64     // 0 for a request POLL wrote; else the closed request's message id
	Notes      string    // owner retry notes, trimmed; set only when RetryOf is non-zero
}

// body renders the request marker: line 1 "base merge requested after run
// <R>", line 2 "base <branch> <sha>", then "point <name>" when Point is set,
// and for a retry a "retry of <id>" line then the notes (trimmed; omitted
// when empty).
func (r baseMergeRequest) body() string {
	lines := []string{
		fmt.Sprintf("base merge requested after run %d", r.AfterRunID),
		fmt.Sprintf("base %s %s", r.BaseBranch, r.BaseSHA),
	}
	if r.Point != "" {
		lines = append(lines, "point "+string(r.Point))
	}
	if r.RetryOf != 0 {
		lines = append(lines, fmt.Sprintf("retry of %d", r.RetryOf))
		if notes := strings.TrimSpace(r.Notes); notes != "" {
			lines = append(lines, notes)
		}
	}
	return strings.Join(lines, "\n")
}

// ErrMalformedBaseMergeRequest marks parseBaseMergeRequest's own "malformed
// marker" failure (every error it returns answers errors.Is(err,
// ErrMalformedBaseMergeRequest) true, through malformedBaseMergeRequestError's
// own Is method), so openBaseMerge (merge.go) can tell it apart from an
// ordinary store/infrastructure failure and escalate it instead of
// propagating a bare error: left alone, the malformed row would never
// parse on any later tick either, deadlocking the ticket (driveOpenMerge
// just reclaims and re-errors forever) while pollMergeCount still counts
// the row, so POLL can never open a fresh request to supersede it (review
// thread ta15433844ef97a61).
var ErrMalformedBaseMergeRequest = errors.New("job: malformed base merge request marker")

// malformedBaseMergeRequestError is parseBaseMergeRequest's own error
// value: its Error text stays exactly "job: base merge request <id>:
// malformed marker", with no wrapped suffix, while its Is method still
// lets errors.Is(err, ErrMalformedBaseMergeRequest) find it.
type malformedBaseMergeRequestError struct {
	rowID int64
}

func (e *malformedBaseMergeRequestError) Error() string {
	return fmt.Sprintf("job: base merge request %d: malformed marker", e.rowID)
}

func (e *malformedBaseMergeRequestError) Is(target error) bool {
	return target == ErrMalformedBaseMergeRequest
}

// parseBaseMergeRequest parses one request marker row. Line 1 or 2 not
// matching, a "point " line that does not match baseMergePointLine, or a
// retry line present but not "retry of <id>", is the error "job: base merge
// request <id>: malformed marker", which answers errors.Is(err,
// ErrMalformedBaseMergeRequest) true.
func parseBaseMergeRequest(row store.MessageRow) (baseMergeRequest, error) {
	malformed := &malformedBaseMergeRequestError{rowID: row.ID}

	lines := strings.Split(row.Body, "\n")
	if len(lines) < 2 {
		return baseMergeRequest{}, malformed
	}
	m1 := baseMergeRequestedLine.FindStringSubmatch(lines[0])
	if m1 == nil {
		return baseMergeRequest{}, malformed
	}
	afterRunID, err := strconv.ParseInt(m1[1], 10, 64)
	if err != nil {
		return baseMergeRequest{}, malformed
	}
	m2 := baseMergeBaseLine.FindStringSubmatch(lines[1])
	if m2 == nil {
		return baseMergeRequest{}, malformed
	}

	req := baseMergeRequest{MessageID: row.ID, AfterRunID: afterRunID, BaseBranch: m2[1], BaseSHA: m2[2]}
	i := 2
	if i < len(lines) && strings.HasPrefix(lines[i], "point ") {
		m := baseMergePointLine.FindStringSubmatch(lines[i])
		if m == nil {
			return baseMergeRequest{}, malformed
		}
		req.Point = syncPoint(m[1])
		i++
	}
	if i == len(lines) {
		return req, nil
	}

	m3 := baseMergeRetryLine.FindStringSubmatch(lines[i])
	if m3 == nil {
		return baseMergeRequest{}, malformed
	}
	retryOf, err := strconv.ParseInt(m3[1], 10, 64)
	if err != nil {
		return baseMergeRequest{}, malformed
	}
	req.RetryOf = retryOf
	if len(lines) > i+1 {
		req.Notes = strings.Join(lines[i+1:], "\n")
	}
	return req, nil
}

// isBaseMergeRequestRow reports whether row's first line is a request
// marker's own first line (baseMergeRequestedLine): the shared "is this a
// request row, or an end marker" test openBaseMergeRequest, pollMergeCount
// and baseMergeRequestByID all need.
func isBaseMergeRequestRow(row store.MessageRow) bool {
	firstLine, _, _ := strings.Cut(row.Body, "\n")
	return baseMergeRequestedLine.MatchString(firstLine)
}

// openBaseMergeRequest reads every "base merge " marker row (oldest
// first): request rows, and landed/closed rows naming a request id. It
// returns the one request no landed or closed row names. Two open is the
// error "job: ticket has two open base merge requests". Rows matching
// neither shape are ignored.
func openBaseMergeRequest(rows []store.MessageRow) (baseMergeRequest, bool, error) {
	var requests []store.MessageRow
	ended := make(map[int64]bool)
	for i := range rows {
		if isBaseMergeRequestRow(rows[i]) {
			requests = append(requests, rows[i])
			continue
		}
		firstLine, _, _ := strings.Cut(rows[i].Body, "\n")
		m := baseMergeEndLine.FindStringSubmatch(firstLine)
		if m == nil {
			continue
		}
		idText := m[1]
		if idText == "" {
			idText = m[2]
		}
		if id, err := strconv.ParseInt(idText, 10, 64); err == nil {
			ended[id] = true
		}
	}

	var open []store.MessageRow
	for i := range requests {
		if !ended[requests[i].ID] {
			open = append(open, requests[i])
		}
	}

	switch len(open) {
	case 0:
		return baseMergeRequest{}, false, nil
	case 1:
		req, err := parseBaseMergeRequest(open[0])
		if err != nil {
			return baseMergeRequest{}, false, err
		}
		return req, true, nil
	default:
		return baseMergeRequest{}, false, errors.New("job: ticket has two open base merge requests")
	}
}

// requestRowFlags reads a request row's own point and retry lines without
// failing: point is the name on the line right after the base line when it
// matches baseMergePointLine, else the empty string; retry is true when the
// line right after that (the point line, when present, else the base line)
// matches baseMergeRetryLine. A malformed row reports ("", false).
func requestRowFlags(row store.MessageRow) (point syncPoint, retry bool) {
	lines := strings.Split(row.Body, "\n")
	i := 2
	if i < len(lines) {
		if m := baseMergePointLine.FindStringSubmatch(lines[i]); m != nil {
			point = syncPoint(m[1])
			i++
		}
	}
	if i < len(lines) && baseMergeRetryLine.MatchString(lines[i]) {
		retry = true
	}
	return point, retry
}

// pollMergeCount is the number of request rows with no point line and no
// "retry of" line: the merges POLL itself started, counted against
// jobs.merge.max_loops. A malformed request row counts too. A request a
// baseSync point opened (point line set) is counted apart, by
// pointMergeCount, not here.
func pollMergeCount(rows []store.MessageRow) int {
	return pointMergeCount(rows, "")
}

// pointMergeCount is the number of request rows baseSync opened at point: a
// point line naming it and no retry line.
func pointMergeCount(rows []store.MessageRow, point syncPoint) int {
	count := 0
	for i := range rows {
		if !isBaseMergeRequestRow(rows[i]) {
			continue
		}
		p, retry := requestRowFlags(rows[i])
		if p == point && !retry {
			count++
		}
	}
	return count
}

// baseMergeRequestByID finds and parses the one request row with the
// given message id, for priorMergeRunID's (merge.go) own "carry over the
// closed predecessor's report" lookup. found is false when no row has
// that id. err is parseBaseMergeRequest's own error when the row's shape
// does not parse; the caller decides what a malformed predecessor marker
// means for it.
func baseMergeRequestByID(rows []store.MessageRow, id int64) (req baseMergeRequest, found bool, err error) {
	for i := range rows {
		if rows[i].ID != id || !isBaseMergeRequestRow(rows[i]) {
			continue
		}
		req, err = parseBaseMergeRequest(rows[i])
		return req, true, err
	}
	return baseMergeRequest{}, false, nil
}

// baseMergeTriedID parses an escalation's Tried text: first line "base
// merge <id>".
func baseMergeTriedID(tried string) (int64, bool) {
	firstLine, _, _ := strings.Cut(tried, "\n")
	m := baseMergeTriedLine.FindStringSubmatch(firstLine)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// mergeCheckText is the merge CHECK verdict: "" when every command
// passed, markers is empty and outside is empty; otherwise, joined by a
// blank line: checkInputText(results) when non-empty, then "conflict
// markers remain in: <p1>, <p2>" when markers is non-empty, then "these
// paths are outside the merge; restore or delete them: <p1>, <p2>" when
// outside is non-empty.
func mergeCheckText(results []commandResult, markers, outside []string) string {
	var parts []string
	if text := checkInputText(results); text != "" {
		parts = append(parts, text)
	}
	if len(markers) > 0 {
		parts = append(parts, "conflict markers remain in: "+strings.Join(markers, ", "))
	}
	if len(outside) > 0 {
		parts = append(parts, "these paths are outside the merge; restore or delete them: "+strings.Join(outside, ", "))
	}
	return strings.Join(parts, "\n\n")
}

// mergeTitle is "Merge <branch> into the ticket branch".
func mergeTitle(req baseMergeRequest) string {
	return fmt.Sprintf("Merge %s into the ticket branch", req.BaseBranch)
}

// mergeFuncLines is the merge commit body: "Merges <branch> at <sha7>",
// then "Resolves <path>" for each of filesChanged, in order, skipping any
// path that is empty or holds "\n" or "\r".
func mergeFuncLines(req baseMergeRequest, filesChanged []string) []string {
	sha7 := req.BaseSHA
	if len(sha7) > 7 {
		sha7 = sha7[:7]
	}
	lines := []string{fmt.Sprintf("Merges %s at %s", req.BaseBranch, sha7)}
	for _, p := range filesChanged {
		if p == "" || strings.ContainsAny(p, "\n\r") {
			continue
		}
		lines = append(lines, "Resolves "+p)
	}
	return lines
}

// mergeQuestionText renders an agent's questions for Tried: per question
// "<title>\n<body>\nRecommended: <recommended>", joined by a blank line.
func mergeQuestionText(qs []response.Question) string {
	parts := make([]string, 0, len(qs))
	for _, q := range qs {
		parts = append(parts, fmt.Sprintf("%s\n%s\nRecommended: %s", q.Title, q.Body, q.Recommended))
	}
	return strings.Join(parts, "\n\n")
}

// reviewBotStep is reviewBotAction's own verdict (design "Shape", review bot
// clock markers).
type reviewBotStep string

const (
	reviewBotStart    reviewBotStep = "start"
	reviewBotWait     reviewBotStep = "wait"
	reviewBotNudge    reviewBotStep = "nudge"
	reviewBotEscalate reviewBotStep = "escalate"
)

// reviewBotSilentWhat is pollIdle's own escalation What, once a nudged check
// still has not reported after a second wait.
const reviewBotSilentWhat = "a required review check never reported"

// reviewBotMissingHead and reviewBotNudgedHead are the exact first lines
// Store.Marker matches for pollIdle's own review bot clock (design "Shape",
// "Markers"): the first poll that finds check missing on head, and the
// poll that posted check's trigger comment on head, respectively.
func reviewBotMissingHead(head, check string) string {
	return fmt.Sprintf("review bot missing %s %s", head, check)
}

func reviewBotNudgedHead(head, check string) string {
	return fmt.Sprintf("review bot nudged %s %s", head, check)
}

// reviewBotMarkerBody renders a review bot clock marker: headKey (one of
// reviewBotMissingHead or reviewBotNudgedHead) on its own first line, then
// at as RFC 3339 UTC on the second.
func reviewBotMarkerBody(headKey string, at time.Time) string {
	return fmt.Sprintf("%s\n%s", headKey, at.UTC().Format(time.RFC3339))
}

// reviewBotMarkerTime parses a review bot clock marker's own second line
// back into a time; ok is false when the body has no parseable second line
// (a malformed row never happens in practice, since only reviewBotMarkerBody
// ever writes one, but pollIdle treats it the same as no marker at all
// rather than erroring the ticket over it).
func reviewBotMarkerTime(body string) (at time.Time, ok bool) {
	_, rest, found := strings.Cut(body, "\n")
	if !found {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(rest))
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// reviewBotAction is pollIdle's own pure review-bot clock (design "Shape"):
// since nil (no missing marker yet) starts the clock. Once since is set, an
// elapsed time (now minus since) short of wait still waits; reached or past
// wait with no nudge yet (nudgedAt nil) nudges; reached or past wait, and
// nudgedAt's own elapsed time is also reached or past wait, escalates;
// otherwise (nudged, but not yet that long) waits.
func reviewBotAction(now time.Time, since, nudgedAt *time.Time, wait time.Duration) reviewBotStep {
	if since == nil {
		return reviewBotStart
	}
	if now.Sub(*since) < wait {
		return reviewBotWait
	}
	if nudgedAt == nil {
		return reviewBotNudge
	}
	if now.Sub(*nudgedAt) >= wait {
		return reviewBotEscalate
	}
	return reviewBotWait
}
