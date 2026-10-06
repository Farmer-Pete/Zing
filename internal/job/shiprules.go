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
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

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
// request: at most 3 failed runs, by name, each followed by its failed
// step's own log (checks.JobLogTail, up to 200 lines, from the step's
// "##[group]Run " header to its last "##[error]" line, falling back to
// the job log's plain last 200 lines when the log has no "##[error]")
// when the run's app is github-actions and its DetailsURL names a job
// id, a "the log could not be read" line when that fetch fails, or one
// line naming its DetailsURL otherwise; then every failed status, one
// line each.
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
	count := 0
	for i := range rows {
		if !isBaseMergeRequestRow(rows[i]) {
			continue
		}
		point, retry := requestRowFlags(rows[i])
		if point == "" && !retry {
			count++
		}
	}
	return count
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
