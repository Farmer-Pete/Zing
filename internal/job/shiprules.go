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
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"zing/internal/orchestrator"
	"zing/internal/response"
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

	if ciPending(reduced, statuses, missing) {
		return CIResult{State: CIPending, Missing: missing}
	}
	return CIResult{State: CIGreen, Missing: missing}
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

	result := EvaluateCI(runs, statuses, required)
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

// actionsJobIDPattern pulls the Actions job id out of a check run's
// DetailsURL (design section 8.5's log tails).
var actionsJobIDPattern = regexp.MustCompile(`/actions/runs/\d+/job/(\d+)`)

func jobIDFromDetailsURL(url string) (int64, bool) {
	m := actionsJobIDPattern.FindStringSubmatch(url)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// ciLogText is design section 8.5's log-tail text for a ci_log fix
// request: at most 3 failed runs, by name, each followed by its Actions
// job log tail (checks.JobLogTail, 200 lines) when the run's app is
// github-actions and its DetailsURL names a job id, a "the log could not
// be read" line when that fetch fails, or one line naming its DetailsURL
// otherwise; then every failed status, one line each.
func ciLogText(ctx context.Context, checks Checks, owner, repo string, failedRuns []orchestrator.CheckRun, failedStatuses []orchestrator.CommitStatus) string {
	runs := append([]orchestrator.CheckRun(nil), failedRuns...)
	sort.Slice(runs, func(i, j int) bool { return runs[i].Name < runs[j].Name })
	if len(runs) > 3 {
		runs = runs[:3]
	}

	var parts []string
	for _, r := range runs {
		jobID, ok := jobIDFromDetailsURL(r.DetailsURL)
		if r.AppSlug == ghGitHubActions && ok {
			tail, err := checks.JobLogTail(ctx, owner, repo, jobID, 200)
			if err != nil {
				parts = append(parts, fmt.Sprintf("check %s (%s): the log could not be read: %s", r.Name, r.Conclusion, err))
				continue
			}
			parts = append(parts, fmt.Sprintf("check %s (%s)\n%s", r.Name, r.Conclusion, tail))
			continue
		}
		parts = append(parts, fmt.Sprintf("check %s (%s) %s", r.Name, r.Conclusion, r.DetailsURL))
	}

	for _, s := range failedStatuses {
		parts = append(parts, fmt.Sprintf("status %s (%s) %s", s.Context, s.State, s.TargetURL))
	}

	return strings.Join(parts, "\n\n")
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
