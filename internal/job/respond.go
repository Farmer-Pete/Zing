// respond.go declares the consumer-side interface the respond job needs
// from GitHub's GraphQL and REST surface (PKG9-PLAN.md section 9, 10.3),
// and, as of M4 task 4, holds the respond job itself: RESPOND's first turn
// and every resume (design section 9.2), the shipping decision tree's own
// steps (1)'s "job respond" branch and (2) (section 8.1), POLL's own row 5
// (section 8.5, starting a batch), and the respond rows of
// resolvePostBuildEscalation (section 5.6, postbuild.go). M4 task 5 adds
// APPLY (9.3): the decision tree's own step (3), applyArtifact's selection
// rule, and shipHandler.apply. M4 task 6 adds FIX-REPLIES and RE-REQUEST
// (9.4), POLL's own rows 1 and 2 (section 8.5), wired in from shipping.go's
// own poll.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/google/go-github/v92/github"

	"zing/internal/orchestrator"
	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// ReviewThreads is what the respond handler needs to read and act on a
// pull request's review threads (PKG9-PLAN.md section 9, 10.3, M4 task 1
// onward).
type ReviewThreads interface {
	// ListThreads lists every review thread of the pull request, each with
	// its last 100 comments (orchestrator.GitHubClient.ListThreads, GraphQL,
	// every page).
	ListThreads(ctx context.Context, owner, repo string, number int) ([]orchestrator.Thread, error)
	// ThreadCommentsContain pages every comment of the thread, not only the
	// last 100, and reports whether one authored by author contains needle
	// (orchestrator.GitHubClient.ThreadCommentsContain, GraphQL).
	ThreadCommentsContain(ctx context.Context, threadID, needle, author string) (bool, error)
	// ReplyToThread posts body as a reply in the thread
	// (orchestrator.GitHubClient.ReplyToThread, GraphQL).
	ReplyToThread(ctx context.Context, threadID, body string) error
	// ResolveThread resolves the thread
	// (orchestrator.GitHubClient.ResolveThread, GraphQL).
	ResolveThread(ctx context.Context, threadID string) error
	// ListReviews lists every review of the pull request
	// (orchestrator.GitHubClient.ListReviews, REST, every page).
	ListReviews(ctx context.Context, owner, repo string, number int) ([]orchestrator.Review, error)
	// RequestReviewers requests one reviewer by login
	// (orchestrator.GitHubClient.RequestReviewers, REST).
	RequestReviewers(ctx context.Context, owner, repo string, number int, login string) error
	// Viewer returns the authenticated login, the token's own owner
	// (orchestrator.GitHubClient.Viewer, REST, D10), cached by callers that
	// need it more than once.
	Viewer(ctx context.Context) (string, error)
}

// artifactTypeRespond is the artifacts.type value every stored respond
// batch carries (design section 4.2; internal/store/postbuild_reads.go's
// own private constant of the same name and value is unreachable from this
// package).
const artifactTypeRespond = "respond"

// respondBatchMarkerPrefix groups every "respond batch " marker (design
// section 5.1): started, retry, skipped, and stale all share it, so
// Store.MarkersWithPrefix returns every one, oldest first.
const respondBatchMarkerPrefix = "respond batch "

// The "respond batch " marker first-line shapes this file reads (design
// section 5.1, 8.1, 9.2).
var (
	respondBatchNumberLine  = regexp.MustCompile(`^respond batch ([1-9]\d*)`)
	respondBatchStartedLine = regexp.MustCompile(`^respond batch ([1-9]\d*) started sha ([0-9a-f]{40}) after run (\d+)$`)
	respondBatchRetryLine   = regexp.MustCompile(`^respond batch ([1-9]\d*) retry sha ([0-9a-f]{40}) after run (\d+)$`)
	respondBatchSkippedLine = regexp.MustCompile(`^respond batch ([1-9]\d*) skipped$`)
	respondBatchStaleLine   = regexp.MustCompile(`^respond batch ([1-9]\d*) stale$`)
)

// respondCoverageFailedFmt and respondCoverageDeliveredFmt are the coverage
// marker heads RESPOND's own ok outcome and its coverage resume write
// (design section 5.1, 9.2), the same shape judging.go's own
// judgeCoverageFailedFmt/judgeCoverageDeliveredFmt give the judge.
const (
	respondCoverageFailedFmt    = "respond coverage failed run %d"
	respondCoverageDeliveredFmt = "respond coverage delivered run %d"
)

// respondCoverageTwiceWhat is RESPOND's own "incomplete twice in a row"
// escalation text (design section 9.2, mirroring judgeCoverageTwiceWhat).
const respondCoverageTwiceWhat = "the respond batch's thread actions are incomplete twice in a row"

// respondStaleHeadMovedReason is the one "respond batch <n> stale" reason
// whose text design section 9.2 fixes exactly; the other, "thread <tid> has
// a new comment", is built per thread.
const respondStaleHeadMovedReason = "the pull request head moved"

// ---- pure helpers: thread rendering, digests, and marker (de)serialization

// threadsByTID indexes threads by tid(th.ID) (design section 9.1).
func threadsByTID(threads []orchestrator.Thread) map[string]orchestrator.Thread {
	out := make(map[string]orchestrator.Thread, len(threads))
	for _, th := range threads {
		out[tid(th.ID)] = th
	}
	return out
}

// lastHumanCommentDigest returns commentDigest of th's own newest comment
// that is not one of Zing's own disclosed replies (design section 8.5 row
// 5, 9.2, 9.3): the digest a batch's own "seen" snapshot stores, and every
// later freshness check compares against. ok is false when th carries no
// such comment (every comment is a Zing reply, or it has none).
func lastHumanCommentDigest(th orchestrator.Thread, login string) (digest string, ok bool) {
	for i := range slices.Backward(th.Comments) {
		if isZingReply(th.Comments[i], login) {
			continue
		}
		return commentDigest(th.Comments[i]), true
	}
	return "", false
}

// pollThreadsFrom converts threads into the plain value pollFingerprint
// needs (design section 8.3): tid(th.ID), th.IsResolved, and commentDigest
// of th's own last comment ("" when it has none).
func pollThreadsFrom(threads []orchestrator.Thread) []PollThread {
	out := make([]PollThread, 0, len(threads))
	for _, th := range threads {
		digest := ""
		if len(th.Comments) > 0 {
			digest = commentDigest(th.Comments[len(th.Comments)-1])
		}
		out = append(out, PollThread{TID: tid(th.ID), IsResolved: th.IsResolved, LastCommentDigest: digest})
	}
	return out
}

// renderRespondBatch renders threadsText for a respond run (design section
// 9.2): per tid of ids, in order, the matching thread of threads
// (renderThreads, threadrules.go); a tid matching none renders as "thread
// <tid>" then "(this thread no longer exists)".
func renderRespondBatch(ids []string, threads []orchestrator.Thread) string {
	byTID := threadsByTID(threads)
	blocks := make([]string, 0, len(ids))
	for _, id := range ids {
		th, ok := byTID[id]
		if !ok {
			blocks = append(blocks, fmt.Sprintf("thread %s\n(this thread no longer exists)", id))
			continue
		}
		blocks = append(blocks, renderThreads([]orchestrator.Thread{th}))
	}
	return strings.Join(blocks, "\n---\n")
}

// respondStaleReason is design section 9.2's own thread-comment half of the
// freshness check: for each id of ids whose thread still exists in byTID
// and is unresolved (the same "still exists and is unresolved" qualifier
// 9.3's own freshness check states explicitly), a last-human-comment digest
// that disagrees with seen[id] is a new comment. A missing thread, a
// resolved one, or one with no human comment at all is not a mismatch: 9.2
// renders a missing thread as "no longer exists" instead, and APPLY (9.3)
// treats an owner-resolved thread as simply done.
func respondStaleReason(ids []string, seen map[string]string, byTID map[string]orchestrator.Thread, login string) (stale bool, reason string) {
	for _, id := range ids {
		th, ok := byTID[id]
		if !ok || th.IsResolved {
			continue
		}
		digest, hasDigest := lastHumanCommentDigest(th, login)
		if !hasDigest {
			continue
		}
		if seen[id] != digest {
			return true, fmt.Sprintf("thread %s has a new comment", id)
		}
	}
	return false, ""
}

// respondBatchRawLines splits a "respond batch " marker's own body into its
// second and third lines, unparsed (design section 5.1): the tids
// (comma-separated) and the "seen <tid>=<digest>,..." snapshot, exactly as
// written -- the shape a cap_resumes retry copies byte for byte (section
// 5.6).
func respondBatchRawLines(body string) (line2, line3 string, err error) {
	lines := strings.SplitN(body, "\n", 3)
	if len(lines) < 3 {
		return "", "", fmt.Errorf("job: shipping: respond batch marker has fewer than 3 lines: %q", body)
	}
	return lines[1], lines[2], nil
}

// parseRespondBatchIDsSeen parses a "respond batch " marker's own tids and
// seen snapshot (design section 5.1, 9.2) out of its second and third
// lines.
func parseRespondBatchIDsSeen(body string) (ids []string, seen map[string]string, err error) {
	line2, line3, err := respondBatchRawLines(body)
	if err != nil {
		return nil, nil, err
	}
	ids = strings.Split(line2, ",")
	seenLine := strings.TrimPrefix(line3, "seen ")
	seen = make(map[string]string, len(ids))
	for pair := range strings.SplitSeq(seenLine, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if ok {
			seen[k] = v
		}
	}
	return ids, seen, nil
}

// respondBatchLines renders a fresh "respond batch " marker's own second
// and third lines from tids (already sorted by the caller) and seen (design
// section 8.5 row 5, 9.2).
func respondBatchLines(tids []string, seen map[string]string) (line2, line3 string) {
	line2 = strings.Join(tids, ",")
	pairs := make([]string, len(tids))
	for i, id := range tids {
		pairs[i] = id + "=" + seen[id]
	}
	line3 = "seen " + strings.Join(pairs, ",")
	return line2, line3
}

// respondNewestBatchN returns the highest batch number any "respond batch "
// marker of markers names, 0 when none exists.
func respondNewestBatchN(markers []store.MessageRow) (int, error) {
	maxN := 0
	for i := range markers {
		firstLine, _, _ := strings.Cut(markers[i].Body, "\n")
		sub := respondBatchNumberLine.FindStringSubmatch(firstLine)
		if sub == nil {
			continue
		}
		n, err := strconv.Atoi(sub[1])
		if err != nil {
			return 0, fmt.Errorf("job: shipping: parse respond batch number %q: %w", firstLine, err)
		}
		if n > maxN {
			maxN = n
		}
	}
	return maxN, nil
}

// findUniqueRespondStarted returns the one "respond batch <n> started ..."
// marker of markers, the error design section 5.6 names ("respond batch
// <n> has no unique started marker") when there is zero or more than one.
func findUniqueRespondStarted(markers []store.MessageRow, n int) (store.MessageRow, error) {
	var found []store.MessageRow
	for i := range markers {
		firstLine, _, _ := strings.Cut(markers[i].Body, "\n")
		sub := respondBatchStartedLine.FindStringSubmatch(firstLine)
		if sub == nil {
			continue
		}
		mn, err := strconv.Atoi(sub[1])
		if err != nil {
			return store.MessageRow{}, fmt.Errorf("job: shipping: parse respond batch started %q: %w", firstLine, err)
		}
		if mn == n {
			found = append(found, markers[i])
		}
	}
	if len(found) != 1 {
		return store.MessageRow{}, fmt.Errorf("job: respond batch %d has no unique started marker", n)
	}
	return found[0], nil
}

// respondStartedSHA reads the sha a "respond batch <n> started ..."
// marker's own first line names.
func respondStartedSHA(body string) (string, error) {
	firstLine, _, _ := strings.Cut(body, "\n")
	sub := respondBatchStartedLine.FindStringSubmatch(firstLine)
	if sub == nil {
		return "", fmt.Errorf("job: shipping: not a respond batch started marker: %q", firstLine)
	}
	return sub[2], nil
}

// respondBatchOwning finds the respond batch (n, sha, ids, seen) whose own
// session is sessionID (design section 5.1's watermark rule, D18): it walks
// markers newest first, and for each "started" or "retry" line, asks
// Store.SessionAfter at that line's own watermark, the same identity check
// judgeRoundOwning (judging.go) makes for a judge round.
func respondBatchOwning(ctx context.Context, t store.Ticket, d Deps, markers []store.MessageRow, sessionID int64) (n int, sha string, ids []string, seen map[string]string, err error) {
	maxResumes := d.Machine.Jobs[jobRespondName].MaxResumes
	for i := range slices.Backward(markers) {
		firstLine, _, _ := strings.Cut(markers[i].Body, "\n")
		var roundN int
		var roundSHA string
		var afterRunID int64

		switch {
		case respondBatchStartedLine.MatchString(firstLine):
			sub := respondBatchStartedLine.FindStringSubmatch(firstLine)
			roundN, err = strconv.Atoi(sub[1])
			if err != nil {
				return 0, "", nil, nil, fmt.Errorf("job: shipping: parse respond batch started %q: %w", firstLine, err)
			}
			roundSHA = sub[2]
			afterRunID, err = strconv.ParseInt(sub[3], 10, 64)
			if err != nil {
				return 0, "", nil, nil, fmt.Errorf("job: shipping: parse respond batch started %q: %w", firstLine, err)
			}
		case respondBatchRetryLine.MatchString(firstLine):
			sub := respondBatchRetryLine.FindStringSubmatch(firstLine)
			roundN, err = strconv.Atoi(sub[1])
			if err != nil {
				return 0, "", nil, nil, fmt.Errorf("job: shipping: parse respond batch retry %q: %w", firstLine, err)
			}
			roundSHA = sub[2]
			afterRunID, err = strconv.ParseInt(sub[3], 10, 64)
			if err != nil {
				return 0, "", nil, nil, fmt.Errorf("job: shipping: parse respond batch retry %q: %w", firstLine, err)
			}
		default:
			continue
		}

		sess, _, _, ok, sessErr := d.Store.SessionAfter(ctx, t.ID, jobRespondName, afterRunID, maxResumes)
		if sessErr != nil {
			return 0, "", nil, nil, fmt.Errorf("job: shipping: session after: %w", sessErr)
		}
		if ok && sess.ID == sessionID {
			batchIDs, batchSeen, parseErr := parseRespondBatchIDsSeen(markers[i].Body)
			if parseErr != nil {
				return 0, "", nil, nil, parseErr
			}
			return roundN, roundSHA, batchIDs, batchSeen, nil
		}
	}
	return 0, "", nil, nil, fmt.Errorf("job: shipping: session %d owns no respond batch", sessionID)
}

// respondCoverageInput is the "coverage" input a coverage resume carries
// (design section 9.2, mirroring judgeCoverageInput): the marker's own
// error lines, fenced.
func respondCoverageInput(text string) prompt.NamedInput {
	return prompt.NamedInput{Label: "coverage", Text: text, Untrusted: true}
}

// respondStaleCommit is design section 9.2's "respond batch <n> stale"
// commit: no run, ClearPoll, so the next poll starts a fresh batch.
func respondStaleCommit(t store.Ticket, d Deps, n int, reason string) store.HandlerCommit {
	c := baseCommit(t, d)
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("respond batch %d stale\n%s", n, reason),
	}}
	c.ClearPoll = true
	return c
}

// respondSkippedCommit is design section 9.2's "respond batch <n> skipped"
// commit: none of the batch's own tids still match a thread GitHub
// returns, so no run starts and the batch closes.
func respondSkippedCommit(t store.Ticket, d Deps, n int) store.HandlerCommit {
	c := baseCommit(t, d)
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("respond batch %d skipped", n),
	}}
	c.ClearPoll = true
	return c
}

// ---- POLL row 5: starting a batch (design section 8.5) --------------------

// startRespondBatch is design section 8.5 row 5: the actionable tids,
// sorted, and their own last-human-comment digests become the batch's own
// "started" marker; no run starts in this commit (design section 8.1: the
// next tick's own row (2) does that).
func (h shipHandler) startRespondBatch(ctx context.Context, t store.Ticket, d Deps, sha string, actionable []orchestrator.Thread, login string) (store.HandlerCommit, error) {
	tids := make([]string, len(actionable))
	seen := make(map[string]string, len(actionable))
	for i, th := range actionable {
		id := tid(th.ID)
		tids[i] = id
		if digest, ok := lastHumanCommentDigest(th, login); ok {
			seen[id] = digest
		}
	}
	sort.Strings(tids)

	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: max run id: %w", err)
	}
	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, respondBatchMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: poll: respond batch markers: %w", err)
	}
	prevMax, err := respondNewestBatchN(markers)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	n := prevMax + 1

	line2, line3 := respondBatchLines(tids, seen)
	c := baseCommit(t, d)
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("respond batch %d started sha %s after run %d\n%s\n%s", n, sha, maxRunID, line2, line3),
	}}
	c.ClearPoll = true
	return c, nil
}

// ---- decision tree step (2): entering an open batch (design section 8.1) --

// enterRespondBatch is the shipping decision tree's own step (2) (design
// section 8.1): the newest "respond batch " marker, when it is a "started"
// or "retry" line with no respond artifact of that batch yet, routes to
// RESPOND's first turn or the right resume; a "skipped" or "stale" newest
// marker, or a batch that already has an artifact, leaves handled false for
// step (3) or (4)/(5) to read instead.
func (h shipHandler) enterRespondBatch(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, bool, error) {
	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, respondBatchMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: shipping: respond batch markers: %w", err)
	}
	if len(markers) == 0 {
		return store.HandlerCommit{}, false, nil
	}
	newest := markers[len(markers)-1]
	firstLine, _, _ := strings.Cut(newest.Body, "\n")

	var n int
	var sha string
	var afterRunID int64
	switch {
	case respondBatchStartedLine.MatchString(firstLine):
		sub := respondBatchStartedLine.FindStringSubmatch(firstLine)
		n, err = strconv.Atoi(sub[1])
		if err != nil {
			return store.HandlerCommit{}, false, fmt.Errorf("job: shipping: parse respond batch started %q: %w", firstLine, err)
		}
		sha = sub[2]
		afterRunID, err = strconv.ParseInt(sub[3], 10, 64)
		if err != nil {
			return store.HandlerCommit{}, false, fmt.Errorf("job: shipping: parse respond batch started %q: %w", firstLine, err)
		}
	case respondBatchRetryLine.MatchString(firstLine):
		sub := respondBatchRetryLine.FindStringSubmatch(firstLine)
		n, err = strconv.Atoi(sub[1])
		if err != nil {
			return store.HandlerCommit{}, false, fmt.Errorf("job: shipping: parse respond batch retry %q: %w", firstLine, err)
		}
		sha = sub[2]
		afterRunID, err = strconv.ParseInt(sub[3], 10, 64)
		if err != nil {
			return store.HandlerCommit{}, false, fmt.Errorf("job: shipping: parse respond batch retry %q: %w", firstLine, err)
		}
	case respondBatchSkippedLine.MatchString(firstLine), respondBatchStaleLine.MatchString(firstLine):
		return store.HandlerCommit{}, false, nil
	default:
		return store.HandlerCommit{}, false, fmt.Errorf("job: shipping: unrecognized respond batch marker %q", firstLine)
	}

	rows, err := d.Store.RespondBatches(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: shipping: respond batches: %w", err)
	}
	for _, r := range rows {
		if r.Respond.Batch == n {
			return store.HandlerCommit{}, false, nil
		}
	}

	ids, seen, err := parseRespondBatchIDsSeen(newest.Body)
	if err != nil {
		return store.HandlerCommit{}, false, err
	}

	maxResumes := d.Machine.Jobs[jobRespondName].MaxResumes
	sess, state, newestRun, found, err := d.Store.SessionAfter(ctx, t.ID, jobRespondName, afterRunID, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: shipping: session after: %w", err)
	}
	if !found {
		commit, runErr := h.respondRunFirst(ctx, t, d, n, sha, ids, seen, nil)
		return commit, true, runErr
	}

	outcome := ""
	if newestRun.Outcome != nil {
		outcome = *newestRun.Outcome
	}

	switch outcome {
	case string(response.OutcomeOk):
		commit, runErr := h.respondEnterCoverageResume(ctx, t, d, n, sha, ids, seen, sess, state, newestRun)
		return commit, true, runErr
	case string(response.OutcomeError):
		commit, runErr := h.respondEnterErrorResume(ctx, t, d, n, sha, ids, seen, sess, state)
		return commit, true, runErr
	default:
		return store.HandlerCommit{}, true, ErrNoAction
	}
}

// ---- RESPOND: first turn and resumes (design section 9.2) -----------------

// respondRunFirst is RESPOND's first turn (design section 9.2): the
// freshness check (the PR head against sha, each batch thread's own last
// human comment digest against seen), the "none of ids returned" skip
// check, then the prompt and the runtime call. extra is nil for an
// ordinary first turn; a cap_resumes-exhausted-with-a-run retry
// (resolvePostBuildEscalation, postbuild.go) passes notes and error
// (fenced) instead.
func (h shipHandler) respondRunFirst(ctx context.Context, t store.Ticket, d Deps, n int, sha string, ids []string, seen map[string]string, extra []prompt.NamedInput) (store.HandlerCommit, error) {
	proj, wt, escCommit, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return shipEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escCommit != nil {
		return *escCommit, nil
	}

	number, err := parsePRNumber(*t.PRURL)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond: %w", err)
	}
	// The freshness check compares GitHub's own current pull request head
	// (design section 9.2: "the PR head equals the marker's sha"), not the
	// ticket's own local worktree: a direct push to the PR bypassing Zing
	// moves the former without ever touching the latter, and that is
	// exactly the case this check exists to catch.
	pr, err := proj.PullRequests.GetPR(ctx, proj.Owner, proj.Repo, number)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond: get pr: %w", err)
	}
	if pr.HeadSHA != sha {
		return respondStaleCommit(t, d, n, respondStaleHeadMovedReason), nil
	}
	local := pr.HeadSHA

	threads, err := proj.Threads.ListThreads(ctx, proj.Owner, proj.Repo, number)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond: list threads: %w", err)
	}
	login, err := proj.Threads.Viewer(ctx)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond: viewer: %w", err)
	}

	byTID := threadsByTID(threads)
	if stale, reason := respondStaleReason(ids, seen, byTID, login); stale {
		return respondStaleCommit(t, d, n, reason), nil
	}

	present := 0
	for _, id := range ids {
		if _, ok := byTID[id]; ok {
			present++
		}
	}
	if present == 0 {
		return respondSkippedCommit(t, d, n), nil
	}

	plan, _, havePlan, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond: stored plan: %w", err)
	}
	if !havePlan {
		return shipEscalation(t, d, noStoredPlanWhat, noStoredPlanWhy, ""), nil
	}
	planXML, err := planXMLFor(plan)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond: plan xml: %w", err)
	}
	diff, err := proj.Orch.Diff(ctx, wt, local)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond: diff: %w", err)
	}
	threadsText := renderRespondBatch(ids, threads)

	jobCfg := d.Machine.Jobs[jobRespondName]
	jobPromptText, err := readAsset(jobCfg.Prompt.Single)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: %w", err)
	}
	schemas, err := renderSchemas(response.JobRespond, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: %w", err)
	}

	in := prompt.ForRespond(jobPromptText, jobCfg.Style, planXML, diff, threadsText, extra)
	in.Schemas = schemas

	req := runtime.RunRequest{
		Job: response.JobRespond, Label: strconv.Itoa(n), WorkDir: wt.Dir(),
		Prompt: prompt.Assemble(in),
	}
	su := store.SessionUpsert{Job: jobRespondName, Runtime: jobCfg.Runtime}

	return h.respondRunAndRoute(ctx, d, t, su, req, 0, freshSessionRecord, nil,
		func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error) {
			return respondOkCommit(ctx, proj, wt, t, d, n, local, ids, seen, rr, sessionCommit, nil, false)
		})
}

// respondResumeTurn runs one respond resume -- a coverage failure, an
// invalid output, an interrupted run, or an answered question -- each
// resuming the session in the ticket's own worktree (unlike the judge,
// respond needs no detached checkout of its own) through
// prompt.ForRespondResume. secondFailure is true only for the coverage
// resume's own ok outcome (judgeResumeTurn's priorInvalid is this file's
// nearest analogue).
func (h shipHandler) respondResumeTurn(
	ctx context.Context, t store.Ticket, d Deps, n int, sha string, ids []string, seen map[string]string,
	sess store.Session, priorInvalid int, resolveIDs []int64, inputs []prompt.NamedInput, secondFailure bool,
) (store.HandlerCommit, error) {
	if sess.ExternalID == nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond resume: session %d has no external id", sess.ID)
	}
	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return store.HandlerCommit{}, ErrConfig
	}
	wt, _, err := proj.Orch.EnsureWorktree(ctx, t.ID, t.Title)
	if err != nil {
		return shipEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, err.Error()), nil
	}

	schemas, err := renderSchemas(response.JobRespond, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: %w", err)
	}
	in := prompt.ForRespondResume(inputs)
	in.Schemas = schemas

	req := runtime.RunRequest{
		Job: response.JobRespond, Label: strconv.Itoa(n), WorkDir: wt.Dir(),
		SessionID: *sess.ExternalID, Prompt: prompt.Assemble(in),
	}
	su := store.SessionUpsert{ID: &sess.ID, BumpResumes: true}
	sessionRecord := func(r runResult) *store.SessionUpsert { return resumeSessionRecord(sess.ID, r) }

	return h.respondRunAndRoute(ctx, d, t, su, req, priorInvalid, sessionRecord, resolveIDs,
		func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error) {
			return respondOkCommit(ctx, proj, wt, t, d, n, sha, ids, seen, rr, sessionCommit, resolveIDs, secondFailure)
		})
}

// respondEnterCoverageResume is decision tree step (2)'s "newest run ok,
// 'respond coverage failed run <rid>' undelivered" branch (design section
// 9.2), mirroring judgeHandler.enterCoverageResume.
func (h shipHandler) respondEnterCoverageResume(ctx context.Context, t store.Ticket, d Deps, n int, sha string, ids []string, seen map[string]string, sess store.Session, state store.SessionState, newestRun store.Run) (store.HandlerCommit, error) {
	pendingRow, pending, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf(respondCoverageFailedFmt, newestRun.ID))
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond coverage failed marker: %w", err)
	}
	if !pending {
		return store.HandlerCommit{}, ErrNoAction
	}

	capCommit, mayResume, capErr := h.respondCapGate(ctx, t, d, sess, state)
	if !mayResume {
		return capCommit, capErr
	}

	_, errsText, _ := strings.Cut(pendingRow.Body, "\n")
	priorRunID := newestRun.ID

	commit, resumeErr := h.respondResumeTurn(ctx, t, d, n, sha, ids, seen, sess, 0, nil, []prompt.NamedInput{respondCoverageInput(errsText)}, true)
	if resumeErr == nil && len(commit.Runs) > 0 {
		commit.Messages = append(commit.Messages, store.Message{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf(respondCoverageDeliveredFmt, priorRunID),
		})
	}
	return commit, resumeErr
}

// respondEnterErrorResume is decision tree step (2)'s two "newest run
// error" branches (design section 9.2), mirroring judgeHandler.enterErrorResume.
func (h shipHandler) respondEnterErrorResume(ctx context.Context, t store.Ticket, d Deps, n int, sha string, ids []string, seen map[string]string, sess store.Session, state store.SessionState) (store.HandlerCommit, error) {
	nInvalid, reason, err := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobRespondName, &sess.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: consecutive invalid outputs: %w", err)
	}

	var input prompt.NamedInput
	switch nInvalid {
	case 1:
		input = prompt.Invalid(invalidRetryText(reason))
	case 0:
		input = prompt.NamedInput{Label: labelInterrupted, Text: interruptedResumeText, Untrusted: false}
	default:
		return store.HandlerCommit{}, ErrNoAction
	}

	capCommit, mayResume, capErr := h.respondCapGate(ctx, t, d, sess, state)
	if !mayResume {
		return capCommit, capErr
	}

	return h.respondResumeTurn(ctx, t, d, n, sha, ids, seen, sess, nInvalid, nil, []prompt.NamedInput{input}, false)
}

// resumeRespondAnswered is the shipping decision tree's own step (1) "job
// respond" branch (design section 8.1, 9.2): the owner has answered a plain
// agent question from a respond run; this resumes the same session with an
// "answers" input, the shape every other job's answered-question resume
// takes.
func (h shipHandler) resumeRespondAnswered(ctx context.Context, t store.Ticket, d Deps, round store.Round) (store.HandlerCommit, error) {
	if round.SessionID == nil {
		return store.HandlerCommit{}, errors.New("job: shipping: answered round has no session id")
	}
	maxResumes := d.Machine.Jobs[jobRespondName].MaxResumes
	sess, state, err := d.Store.SessionByID(ctx, *round.SessionID, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: session by id: %w", err)
	}
	resolveIDs := questionIDs(round)

	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, respondBatchMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond batch markers: %w", err)
	}
	n, sha, ids, seen, err := respondBatchOwning(ctx, t, d, markers, *round.SessionID)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	capCommit, mayResume, capErr := h.respondCapGate(ctx, t, d, sess, state)
	if !mayResume {
		capCommit.ResolveQuestions = resolveIDs
		return capCommit, capErr
	}

	answers, ansErr := renderRoundAnswers(round)
	if ansErr != nil {
		return store.HandlerCommit{}, ansErr
	}

	return h.respondResumeTurn(ctx, t, d, n, sha, ids, seen, sess, 0, resolveIDs, []prompt.NamedInput{prompt.Answers(answers)}, false)
}

// respondCapGate is the decision tree's own shared "needs a resume" gate
// (design section 6.9), mirroring judgeHandler.resumeCapGate: an open
// session leaves the resume itself to the caller; an exhausted one either
// escalates resumes_exhausted once or, when that escalation already exists
// for this session, returns ErrNoAction.
func (h shipHandler) respondCapGate(ctx context.Context, t store.Ticket, d Deps, sess store.Session, state store.SessionState) (store.HandlerCommit, bool, error) {
	if state != store.SessionExhausted {
		return store.HandlerCommit{}, true, nil
	}
	has, err := d.Store.HasEscalation(ctx, t.ID, string(response.EscalationOriginCapResumes), sess.ID)
	if err != nil {
		return store.HandlerCommit{}, false, fmt.Errorf("job: shipping: has escalation: %w", err)
	}
	if has {
		return store.HandlerCommit{}, false, ErrNoAction
	}
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", sess.ID, "run_id", nil,
		"code", string(response.EscalationCodeResumesExhausted), "origin", string(response.EscalationOriginCapResumes))
	c := capResumesEscalation(t, d, sess.ID)
	c.ClearPoll = true
	return c, false, nil
}

// respondRunAndRoute runs one respond turn (runJob) and routes its result
// exactly as every other job's universal outcome table does (design
// section 6.8), with respond's own readonly sandbox reason in place of
// build's or the judge's (routeFailure, planning.go, hardcodes
// d.Sandboxes.Build.Reason(), which is wrong here; judgeRunAndRoute,
// judging.go, is the model this mirrors). Every commit it returns also sets
// ClearPoll, design section 8.1's own closing note ("every commit in
// shipping sets ClearPoll, except POLL's idle commits and the merge
// question commit") -- centralized here, once, rather than at each of this
// file's own return points.
func (h shipHandler) respondRunAndRoute(
	ctx context.Context, d Deps, t store.Ticket, su store.SessionUpsert, req runtime.RunRequest,
	priorInvalid int, sessionRecord func(runResult) *store.SessionUpsert, resolveIDs []int64,
	onOk func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error),
) (store.HandlerCommit, error) {
	commit, err := h.respondRunAndRouteRaw(ctx, d, t, su, req, priorInvalid, sessionRecord, resolveIDs, onOk)
	if err == nil {
		commit.ClearPoll = true
	}
	return commit, err
}

func (h shipHandler) respondRunAndRouteRaw(
	ctx context.Context, d Deps, t store.Ticket, su store.SessionUpsert, req runtime.RunRequest,
	priorInvalid int, sessionRecord func(runResult) *store.SessionUpsert, resolveIDs []int64,
	onOk func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error),
) (store.HandlerCommit, error) {
	rr, runErr := runJob(ctx, d, t, jobRespondName, su, req, nil, nil)
	sessionCommit := sessionRecord(rr)

	if runErr != nil {
		switch {
		case errors.Is(runErr, runtime.ErrCanceled), errors.Is(runErr, ErrConfig), errors.Is(runErr, store.ErrClaimLost):
			return store.HandlerCommit{}, runErr
		case errors.Is(runErr, ErrBudget):
			return budgetEscalationCommit(t, d, resolveIDs), nil
		case errors.Is(runErr, ErrSandbox):
			return sandboxEscalationCommit(t, d, resolveIDs, response.EscalationOriginRespond, d.Sandboxes.ReadOnly.Reason()), nil
		}
		var invErr *runtime.InvalidOutputError
		if errors.As(runErr, &invErr) { //nolint:modernize // errors.AsType discards its bool via _, which errcheck flags
			return invalidOutputCommit(t, d, rr, invErr, priorInvalid, sessionCommit, resolveIDs, response.EscalationOriginRespond), nil
		}
		if isExecFailure(runErr) {
			return execFailureCommit(t, d, rr, sessionCommit, resolveIDs, response.EscalationOriginRespond), nil
		}
		wrapped := fmt.Errorf("job: shipping: unrecognized runJob error: %w", runErr)
		if rr.Reserved.RunID != 0 {
			return postRunFailure(t, d, rr, sessionCommit, resolveIDs, response.EscalationOriginRespond, wrapped), nil
		}
		return store.HandlerCommit{}, wrapped
	}

	switch resp := rr.Res.Response.(type) {
	case *response.RespondResponse:
		c, err := onOk(rr, sessionCommit)
		return c, err
	case *response.QuestionResponse:
		return questionOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs)
	case *response.ErrorResponse:
		return errorOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs, response.EscalationOriginRespond), nil
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}

// respondOkCommit is RESPOND's own "ok" outcome (design section 9.2):
// CheckRespondCoverage(ids, resp, BranchCommits(wt)). No errors
// terminalizes ok and stores one "respond" artifact {threads, batch n, sha,
// seen}, seen copied from the batch marker's own line 3, never from the
// model. Errors on this batch's first coverage attempt (secondFailure
// false) terminalize ok and write "respond coverage failed run <rid>" with
// one error per line. Errors on a coverage resume's own ok outcome
// (secondFailure true) terminalize ok and escalate response_invalid
// instead: the batch's thread actions are incomplete twice in a row.
func respondOkCommit(ctx context.Context, proj Project, wt orchestrator.Worktree, t store.Ticket, d Deps, n int, sha string, ids []string, seen map[string]string, rr runResult, sessionCommit *store.SessionUpsert, resolveIDs []int64, secondFailure bool) (store.HandlerCommit, error) {
	resp, ok := rr.Res.Response.(*response.RespondResponse)
	if !ok {
		return store.HandlerCommit{}, errors.New("job: shipping: expected a respond document")
	}

	branchShas, err := proj.Orch.BranchCommits(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond: branch commits: %w", err)
	}
	errs := CheckRespondCoverage(ids, *resp, branchShas)

	c := baseCommit(t, d)
	c.Runs = terminalRuns(rr, string(response.OutcomeOk))
	c.Session = sessionCommit
	c.ResolveQuestions = resolveIDs

	if len(errs) == 0 {
		seenArr := make([]response.ThreadSeen, 0, len(ids))
		for _, id := range ids {
			if digest, ok := seen[id]; ok {
				seenArr = append(seenArr, response.ThreadSeen{TID: id, LastComment: digest})
			}
		}
		artifact := response.RespondArtifact{Threads: resp.Threads, Batch: n, SHA: sha, Seen: seenArr}
		payload, marshalErr := json.Marshal(artifact)
		if marshalErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond: marshal artifact: %w", marshalErr)
		}
		runID := rr.Reserved.RunID
		c.Artifacts = []store.Artifact{{Type: artifactTypeRespond, RunID: &runID, Payload: payload}}
		return c, nil
	}

	if !secondFailure {
		c.Messages = []store.Message{{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf(respondCoverageFailedFmt, rr.Reserved.RunID) + "\n" + strings.Join(errs, "\n"),
		}}
		return c, nil
	}

	code := string(response.EscalationCodeResponseInvalid)
	runID, sessionID := rr.Reserved.RunID, rr.Reserved.SessionID
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", sessionID, "run_id", runID, "code", code, "origin", string(response.EscalationOriginRespond))
	c.Escalation = &store.EscalationCommit{
		RunID: &runID,
		Body:  code + ": " + respondCoverageTwiceWhat,
		Payload: response.EscalationPayload{
			Code: code, What: respondCoverageTwiceWhat, Why: strings.Join(errs, "; "),
			Options: escalationOptions, SessionID: &sessionID, Origin: string(response.EscalationOriginRespond),
		},
	}
	waiting := waitingFlagQuestions
	c.Waiting = &waiting
	return c, nil
}

// ---- design section 5.6's respond rows (postbuild.go) ----------------------

// retryCapResumesRespond is design section 5.6's "cap_resumes, exhausted
// session of job respond" row: the one "respond batch <n> started ..."
// marker of the exhausted session's own batch n (findUniqueRespondStarted;
// zero or more than one is the loud error the plan names), copied byte for
// byte into a fresh "respond batch <n> retry sha <sha> after run <R>"
// marker. The shipping tree reads it like a "started" marker of batch n
// (design section 8.1): the first-turn freshness check (respondRunFirst)
// compares the current head and each thread against that same snapshot, so
// a batch that went stale while the owner decided is marked stale and never
// runs.
func (h shipHandler) retryCapResumesRespond(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, sessionID int64, preservedRounds []store.Round) (store.HandlerCommit, int, error) {
	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, respondBatchMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: shipping: respond cap_resumes retry: batch markers: %w", err)
	}
	n, _, _, _, err := respondBatchOwning(ctx, t, d, markers, sessionID)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: shipping: respond cap_resumes retry: %w", err)
	}

	started, err := findUniqueRespondStarted(markers, n)
	if err != nil {
		return store.HandlerCommit{}, 0, err
	}
	sha, err := respondStartedSHA(started.Body)
	if err != nil {
		return store.HandlerCommit{}, 0, err
	}
	line2, line3, err := respondBatchRawLines(started.Body)
	if err != nil {
		return store.HandlerCommit{}, 0, err
	}

	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: shipping: respond cap_resumes retry: max run id: %w", err)
	}

	allResolveIDs := append([]int64{}, resolveIDs...)
	for _, r := range preservedRounds {
		allResolveIDs = append(allResolveIDs, questionIDs(r)...)
	}

	c := baseCommit(t, d)
	c.ResolveQuestions = allResolveIDs
	c.ClearPoll = true
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("respond batch %d retry sha %s after run %d\n%s\n%s", n, sha, maxRunID, line2, line3),
	}}
	return c, len(preservedRounds), nil
}

// retryRespondWithRun is design section 5.6's "respond, with a run" row: a
// fresh respond batch (a new, higher batch number; its own tids and seen
// snapshot read fresh, exactly as POLL's own startRespondBatch would) run
// immediately with notes and error (fenced) -- the same "write the started
// marker, then run its first turn, in one commit" shape judgeHandler.retryFreshRound
// gives a fresh judge round. No actionable thread remains: the generic
// "retry requested" marker (retryRespondNoRun) stands in, since there is
// nothing left to batch.
func (h shipHandler) retryRespondWithRun(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, notes, errorText string) (store.HandlerCommit, error) {
	// Only proj is needed here: respondRunFirst (below) re-derives its own
	// worktree, since the fresh batch it runs may land on a later tick than
	// this one.
	proj, _, escCommit, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return shipEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escCommit != nil {
		return *escCommit, nil
	}

	number, err := parsePRNumber(*t.PRURL)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond retry: %w", err)
	}
	pr, err := proj.PullRequests.GetPR(ctx, proj.Owner, proj.Repo, number)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond retry: get pr: %w", err)
	}
	local := pr.HeadSHA
	threads, err := proj.Threads.ListThreads(ctx, proj.Owner, proj.Repo, number)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond retry: list threads: %w", err)
	}
	login, err := proj.Threads.Viewer(ctx)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond retry: viewer: %w", err)
	}
	_, actionable, _, _ := classifyThreads(threads, login)
	if len(actionable) == 0 {
		return h.retryRespondNoRun(t, d, resolveIDs), nil
	}

	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, respondBatchMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond retry: batch markers: %w", err)
	}
	prevMax, err := respondNewestBatchN(markers)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	n := prevMax + 1

	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: respond retry: max run id: %w", err)
	}

	tids := make([]string, len(actionable))
	seen := make(map[string]string, len(actionable))
	for i, th := range actionable {
		id := tid(th.ID)
		tids[i] = id
		if digest, ok := lastHumanCommentDigest(th, login); ok {
			seen[id] = digest
		}
	}
	sort.Strings(tids)
	line2, line3 := respondBatchLines(tids, seen)
	startMsg := store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("respond batch %d started sha %s after run %d\n%s\n%s", n, local, maxRunID, line2, line3),
	}

	extra := []prompt.NamedInput{prompt.Notes(notes), prompt.Error(errorText)}
	commit, runErr := h.respondRunFirst(ctx, t, d, n, local, tids, seen, extra)
	if runErr != nil {
		return store.HandlerCommit{}, runErr
	}
	commit.Messages = append([]store.Message{startMsg}, commit.Messages...)
	commit.ResolveQuestions = resolveIDs
	return commit, nil
}

// retryRespondNoRun is design section 5.6's "respond, with no run" row: no
// run was ever reserved for the escalated attempt, so the generic "retry
// requested" marker is enough -- the next tick re-reads the same "respond
// batch " marker family and tries again from wherever it left off.
func (h shipHandler) retryRespondNoRun(t store.Ticket, d Deps, resolveIDs []int64) store.HandlerCommit {
	return shipRetryMarkerCommit(t, d, resolveIDs)
}

// ---- decision tree step (3): APPLY (design section 9.3) -------------------

// applyArtifact is APPLY's own selection rule (design section 9.3 step 1):
// the newest respond artifact with no "respond applied <ArtifactID>" marker
// and no "respond batch <n> stale" marker for its own batch n. ok is false
// when there is no respond artifact at all, the newest one is already
// applied, or its own batch already went stale -- 9.2's "the next poll
// starts a fresh batch" means a stale batch is abandoned, never retried
// through APPLY, so step (3) has nothing to do and the decision tree moves
// on to PUBLISH or POLL. A new batch never starts (startRespondBatch, POLL
// row 5) while an unresolved respond artifact is still pending apply, so
// the newest row is always the one, if any, this rule needs to find.
func applyArtifact(ctx context.Context, t store.Ticket, d Deps, rows []store.RespondRow) (store.RespondRow, bool, error) {
	if len(rows) == 0 {
		return store.RespondRow{}, false, nil
	}
	newest := rows[len(rows)-1]

	_, appliedMarked, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf("respond applied %d", newest.ArtifactID))
	if err != nil {
		return store.RespondRow{}, false, fmt.Errorf("job: shipping: respond applied marker: %w", err)
	}
	if appliedMarked {
		return store.RespondRow{}, false, nil
	}

	_, staleMarked, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf("respond batch %d stale", newest.Respond.Batch))
	if err != nil {
		return store.RespondRow{}, false, fmt.Errorf("job: shipping: respond batch stale marker: %w", err)
	}
	if staleMarked {
		return store.RespondRow{}, false, nil
	}

	return newest, true, nil
}

// respondFixRequestText is design section 9.3 step 3's own text for a
// collected batch of "fix" thread actions: "Review threads from respond
// batch <n>:", then one line per action, in artifact order, "thread <tid>
// (<path>:<line>): <action text>" -- "(<path>)" when the thread's own Line
// is 0, the same fallback renderThreads (threadrules.go) gives a thread
// with no line (design section 9.1, 9.2).
func respondFixRequestText(batch int, actions []response.ThreadAction, byTID map[string]orchestrator.Thread) string {
	lines := make([]string, 0, len(actions)+1)
	lines = append(lines, fmt.Sprintf("Review threads from respond batch %d:", batch))
	for _, action := range actions {
		th := byTID[action.ID]
		loc := th.Path
		if th.Line != 0 {
			loc = fmt.Sprintf("%s:%d", th.Path, th.Line)
		}
		lines = append(lines, fmt.Sprintf("thread %s (%s): %s", action.ID, loc, action.Text))
	}
	return strings.Join(lines, "\n")
}

// applyPendingReply is one reply or addressed action APPLY has decided to
// post, its body already built by replyBody before any GitHub write (design
// section 9.3 step 1b).
type applyPendingReply struct {
	rawID, marker, body string
}

// apply is the shipping decision tree's own step (3) (design section 9.3):
// the freshness check against a's own SHA and seen digests (step 1a, before
// any write); every reply body built with replyBody before the first write
// (step 1b); for each action, in artifact order, a missing or resolved
// thread is skipped and logged, a reply or addressed action posts (guarded
// by ThreadCommentsContain's own idempotent marker check) and resolves, and
// a fix action is collected (step 2); the collected fix actions, if any, go
// through the shared shipping gate of 8.7 before one consolidated fix
// request is written (step 3); and the closing "respond applied <aid>"
// marker (step 4). A GitHub write error returns with no commit: the next
// tick runs APPLY again, and the per-reply marker (ThreadCommentsContain)
// skips what this attempt already posted.
func (h shipHandler) apply(ctx context.Context, t store.Ticket, d Deps, a store.RespondRow) (store.HandlerCommit, error) {
	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return store.HandlerCommit{}, ErrConfig
	}
	number, err := parsePRNumber(*t.PRURL)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: %w", err)
	}
	pr, err := proj.PullRequests.GetPR(ctx, proj.Owner, proj.Repo, number)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: get pr: %w", err)
	}
	threads, err := proj.Threads.ListThreads(ctx, proj.Owner, proj.Repo, number)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: list threads: %w", err)
	}
	login, err := proj.Threads.Viewer(ctx)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: viewer: %w", err)
	}

	// 1a. Freshness, before any write.
	if pr.HeadSHA != a.Respond.SHA {
		return respondStaleCommit(t, d, a.Respond.Batch, respondStaleHeadMovedReason), nil
	}
	seenIDs := make([]string, len(a.Respond.Seen))
	seen := make(map[string]string, len(a.Respond.Seen))
	for i, s := range a.Respond.Seen {
		seenIDs[i] = s.TID
		seen[s.TID] = s.LastComment
	}
	byTID := threadsByTID(threads)
	if stale, reason := respondStaleReason(seenIDs, seen, byTID, login); stale {
		return respondStaleCommit(t, d, a.Respond.Batch, reason), nil
	}

	// 1b. Every reply body built before the first write; step 2's own
	// missing/resolved skip and fix collection are pure too, so this whole
	// pass makes no GitHub call.
	var replies []applyPendingReply
	var fixActions []response.ThreadAction
	skipped := 0

	for _, action := range a.Respond.Threads {
		th, exists := byTID[action.ID]
		switch {
		case !exists:
			skipped++
			slog.Info("thread skipped", "ticket_id", t.ID, "tid", action.ID, "reason", "missing")
			continue
		case th.IsResolved:
			skipped++
			slog.Info("thread skipped", "ticket_id", t.ID, "tid", action.ID, "reason", "resolved")
			continue
		}

		switch action.Action {
		case response.ThreadVerbFix:
			fixActions = append(fixActions, action)
		case response.ThreadVerbReply, response.ThreadVerbAddressed:
			marker := fmt.Sprintf("<!-- zing:reply a%d %s -->", a.ArtifactID, action.ID)
			body, bodyErr := replyBody(login, action.Text, marker)
			if bodyErr != nil {
				return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: reply body: %w", bodyErr)
			}
			replies = append(replies, applyPendingReply{rawID: th.ID, marker: marker, body: body})
		default:
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: thread %s: unrecognized action %q", action.ID, action.Action)
		}
	}

	// Step 2's own writes: ReplyToThread guarded by ThreadCommentsContain's
	// own idempotent marker check, then ResolveThread -- always, since every
	// thread reaching this loop was already read unresolved above.
	for _, rep := range replies {
		contains, containsErr := proj.Threads.ThreadCommentsContain(ctx, rep.rawID, rep.marker, login)
		if containsErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: thread comments contain: %w", containsErr)
		}
		if !contains {
			if replyErr := proj.Threads.ReplyToThread(ctx, rep.rawID, rep.body); replyErr != nil {
				return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: reply to thread: %w", replyErr)
			}
		}
		if resolveErr := proj.Threads.ResolveThread(ctx, rep.rawID); resolveErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: resolve thread: %w", resolveErr)
		}
	}

	// Step 3: the collected fix actions, gated by the shared shipping
	// counter of 8.7. A gate hit still keeps every reply and resolve this
	// attempt already made; it only withholds the "respond applied <aid>"
	// marker, so the escalation holds the artifact id instead (8.7).
	fixed := 0
	var fixMsg *store.Message
	var requestAfterRunID int64
	if len(fixActions) > 0 {
		text := respondFixRequestText(a.Respond.Batch, fixActions, byTID)

		ciReqs, ciErr := d.Store.MarkersWithPrefix(ctx, t.ID, fixRequestedCILogPrefix)
		if ciErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: ci_log fix requests: %w", ciErr)
		}
		threadReqs, threadErr := d.Store.MarkersWithPrefix(ctx, t.ID, fixRequestedThreadsPrefix)
		if threadErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: threads fix requests: %w", threadErr)
		}
		k := len(ciReqs) + len(threadReqs)
		maxLoops := d.Machine.Jobs[jobRespondName].MaxLoops

		gateOK, what, why, tried := shippingGate(k, maxLoops, string(FixKindThreads), a.ArtifactID, text)
		if !gateOK {
			c := shipLoopsExhausted(t, d, what, why, tried)
			c.ClearPoll = true
			return c, nil
		}

		maxRunID, runErr := d.Store.MaxRunID(ctx, t.ID)
		if runErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: max run id: %w", runErr)
		}
		msg, msgErr := fixRequestMessage(t, FixKindThreads, text, maxRunID)
		if msgErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: apply: fix request message: %w", msgErr)
		}
		fixMsg = &msg
		requestAfterRunID = maxRunID
		fixed = 1
	}

	// Step 4: the closing marker. Its own line 2 reports what this commit
	// itself did (replies posted or skipped this attempt, and whether a fix
	// request was written -- 0 or 1, never a thread count: a retried gate
	// (shipping.go's retryShippingLoopsExhausted) rebuilds this same line
	// knowing only whether one consolidated request exists, not how many
	// threads fed it).
	body := fmt.Sprintf("respond applied %d\nreplied %d fixing %d skipped %d", a.ArtifactID, len(replies), fixed, skipped)
	if fixMsg != nil {
		body += fmt.Sprintf("\nfix request after run %d", requestAfterRunID)
	}
	appliedMsg := store.Message{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: body}

	c := baseCommit(t, d)
	if fixMsg != nil {
		c.Messages = []store.Message{*fixMsg, appliedMsg}
	} else {
		c.Messages = []store.Message{appliedMsg}
	}
	c.ClearPoll = true
	return c, nil
}

// ---- FIX-REPLIES and RE-REQUEST (design section 9.4, POLL rows 1 and 2 of
// 8.5, wired in from shipping.go's own poll) ---------------------------------

// respondAppliedFixRequestLine matches "fix request after run <R>", the
// third line a "respond applied <aid>" marker carries only when APPLY's own
// step 3 (9.3, apply above) or 5.6's own "shipping, loops_exhausted" retry
// row (shipping.go's retryShippingLoopsExhausted) wrote a consolidated fix
// request for that batch's own collected fix actions.
var respondAppliedFixRequestLine = regexp.MustCompile(`^fix request after run (\d+)$`)

// fixLandedLine matches "fix landed <mid> sha <sha>" (fix.go's own marker
// shape, design section 5.1, D22): fix.go itself only ever needs the
// message id back (parseFixLandedMessageID), since DriveFix never reads the
// landed sha; FIX-REPLIES does, so this file parses both out of the same
// marker family.
var fixLandedLine = regexp.MustCompile(`^fix landed (\d+) sha ([0-9a-f]{40})$`)

// fixRepliesPostedMarkerFor is "fix replies posted <aid>", FIX-REPLIES' own
// closing marker (design section 9.4): once written, that respond
// artifact's own fix threads are never revisited.
func fixRepliesPostedMarkerFor(aid int64) string {
	return fmt.Sprintf("fix replies posted %d", aid)
}

// respondFixLandedSHA is design section 9.4's own chain from a respond
// artifact's own "respond applied <aid>" marker to the sha its collected
// fix actions landed at: line 3 "fix request after run <R>" names the
// request's own watermark; "fix requested threads after run <R>" is that
// request's own marker, whose message id is the request's own identity
// (fix.go's openFixRequest gives the same identity rule); "fix landed <mid>
// sha <S>" is that request's own landing marker. ok is false when the
// applied marker carries no such line 3 (this batch collected no fix
// action), the request marker cannot be found, or the request has not
// landed yet -- none of these are errors, only "not yet".
func respondFixLandedSHA(ctx context.Context, t store.Ticket, d Deps, aid int64) (sha string, ok bool, err error) {
	applied, found, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf("respond applied %d", aid))
	if err != nil {
		return "", false, fmt.Errorf("job: shipping: respond applied marker: %w", err)
	}
	if !found {
		return "", false, nil
	}
	lines := strings.SplitN(applied.Body, "\n", 3)
	if len(lines) < 3 {
		return "", false, nil
	}
	sub := respondAppliedFixRequestLine.FindStringSubmatch(lines[2])
	if sub == nil {
		return "", false, nil
	}
	r, err := strconv.ParseInt(sub[1], 10, 64)
	if err != nil {
		return "", false, fmt.Errorf("job: shipping: parse %q: %w", lines[2], err)
	}

	reqMarker, found, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf("fix requested threads after run %d", r))
	if err != nil {
		return "", false, fmt.Errorf("job: shipping: fix requested threads marker: %w", err)
	}
	if !found {
		return "", false, nil
	}

	landed, err := d.Store.MarkersWithPrefix(ctx, t.ID, fixLandedPrefix)
	if err != nil {
		return "", false, fmt.Errorf("job: shipping: fix landed markers: %w", err)
	}
	for i := range landed {
		firstLine, _, _ := strings.Cut(landed[i].Body, "\n")
		landedSub := fixLandedLine.FindStringSubmatch(firstLine)
		if landedSub == nil {
			continue
		}
		mid, convErr := strconv.ParseInt(landedSub[1], 10, 64)
		if convErr != nil {
			continue
		}
		if mid == reqMarker.ID {
			return landedSub[2], true, nil
		}
	}
	return "", false, nil
}

// fixRepliesPending is design section 8.5 row 1's own entry condition: the
// oldest respond artifact (store order, ascending by artifact id --
// Store.RespondBatches' own ORDER BY artifacts.id) with no
// "fix replies posted <aid>" marker and no "respond batch <n> stale" marker
// for its own batch (applyArtifact's own exact rule: a batch that has gone
// stale is abandoned, never retried, through APPLY or FIX-REPLIES alike),
// whose collected fix actions landed and are now an ancestor of the pull
// request's own head. ok is false when no artifact qualifies, so POLL's own
// row 2 (RE-REQUEST) or a later row reads this tick's own state instead.
func (h shipHandler) fixRepliesPending(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, rows []store.RespondRow, prHeadSHA string) (a store.RespondRow, sha string, ok bool, err error) {
	for _, row := range rows {
		_, posted, postedErr := d.Store.Marker(ctx, t.ID, fixRepliesPostedMarkerFor(row.ArtifactID))
		if postedErr != nil {
			return store.RespondRow{}, "", false, fmt.Errorf("job: shipping: fix replies posted marker: %w", postedErr)
		}
		if posted {
			continue
		}
		_, staleMarked, staleErr := d.Store.Marker(ctx, t.ID, fmt.Sprintf("respond batch %d stale", row.Respond.Batch))
		if staleErr != nil {
			return store.RespondRow{}, "", false, fmt.Errorf("job: shipping: respond batch stale marker: %w", staleErr)
		}
		if staleMarked {
			continue
		}

		s, landed, lerr := respondFixLandedSHA(ctx, t, d, row.ArtifactID)
		if lerr != nil {
			return store.RespondRow{}, "", false, lerr
		}
		if !landed {
			continue
		}
		anc, ancErr := proj.Orch.IsAncestor(ctx, wt, s, prHeadSHA)
		if ancErr != nil {
			return store.RespondRow{}, "", false, fmt.Errorf("job: shipping: fix replies pending: is ancestor: %w", ancErr)
		}
		if !anc {
			continue
		}
		return row, s, true, nil
	}
	return store.RespondRow{}, "", false, nil
}

// fixReplies is FIX-REPLIES (design section 9.4, POLL row 1 of 8.5): a's own
// freshness check against the landed fix -- a.Respond.SHA must be an
// ancestor of sha, and every one of a's own fix threads that still exists
// in threads and is unresolved must keep its own last human comment digest
// (respondStaleReason, the same shared helper APPLY's own freshness fence,
// 9.3 step 1a, reuses) -- then, for each fix thread not resolved, with no
// "<!-- zing:fixed a<aid> <tid> -->" marker from login yet
// (ThreadCommentsContain pages every comment), replyBody(login, "Fixed in
// <sha7>.", marker) -- every body built before the first write, 9.3 step
// 1b's own rule, reused here too -- then resolve. Marker
// "fix replies posted <aid>". A mismatch writes "respond batch <n> stale"
// instead and posts nothing, same as 9.3 step 1a: the closing marker is
// never written, so fixRepliesPending's own stale check retires this batch
// for good, and its threads are answered by a fresh batch instead (design
// section 9.2, 9.4).
func (h shipHandler) fixReplies(ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree, threads []orchestrator.Thread, login string, a store.RespondRow, sha string) (store.HandlerCommit, error) {
	anc, ancErr := proj.Orch.IsAncestor(ctx, wt, a.Respond.SHA, sha)
	if ancErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: fix replies: is ancestor: %w", ancErr)
	}
	if !anc {
		return respondStaleCommit(t, d, a.Respond.Batch, respondStaleHeadMovedReason), nil
	}

	var fixTIDs []string
	for _, action := range a.Respond.Threads {
		if action.Action == response.ThreadVerbFix {
			fixTIDs = append(fixTIDs, action.ID)
		}
	}
	seen := make(map[string]string, len(a.Respond.Seen))
	for _, s := range a.Respond.Seen {
		seen[s.TID] = s.LastComment
	}
	byTID := threadsByTID(threads)
	if stale, reason := respondStaleReason(fixTIDs, seen, byTID, login); stale {
		return respondStaleCommit(t, d, a.Respond.Batch, reason), nil
	}

	var replies []applyPendingReply
	for _, id := range fixTIDs {
		th, exists := byTID[id]
		if !exists || th.IsResolved {
			continue
		}
		marker := fmt.Sprintf("<!-- zing:fixed a%d %s -->", a.ArtifactID, id)
		body, bodyErr := replyBody(login, fmt.Sprintf("Fixed in %s.", sha[:7]), marker)
		if bodyErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: fix replies: reply body: %w", bodyErr)
		}
		replies = append(replies, applyPendingReply{rawID: th.ID, marker: marker, body: body})
	}

	for _, rep := range replies {
		contains, containsErr := proj.Threads.ThreadCommentsContain(ctx, rep.rawID, rep.marker, login)
		if containsErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: fix replies: thread comments contain: %w", containsErr)
		}
		if !contains {
			if replyErr := proj.Threads.ReplyToThread(ctx, rep.rawID, rep.body); replyErr != nil {
				return store.HandlerCommit{}, fmt.Errorf("job: shipping: fix replies: reply to thread: %w", replyErr)
			}
		}
		if resolveErr := proj.Threads.ResolveThread(ctx, rep.rawID); resolveErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: fix replies: resolve thread: %w", resolveErr)
		}
	}

	c := baseCommit(t, d)
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fixRepliesPostedMarkerFor(a.ArtifactID),
	}}
	c.ClearPoll = true
	return c, nil
}

// reRequestedMarkerFor is "reviewers re-requested <headSHA>", the marker
// RE-REQUEST writes once per head (design section 9.4): Store.Marker's own
// exact first-line match means a different head's own marker never matches
// this one, so each head is handled once.
func reRequestedMarkerFor(headSHA string) string {
	return "reviewers re-requested " + headSHA
}

// fixLandedAfterPROpened is design section 8.5 row 2's own first clause: has
// any "fix landed " marker (fix.go's own family, any FixKind) landed after
// "pr opened <number>" (design section 8.2 step 6, shipping.go's own
// publish) -- i.e. its own message id is newer, messages being an
// append-only, strictly increasing log. ok is false, with no error, when
// "pr opened" itself cannot be found, which never happens once t.PRURL is
// non-nil (PUBLISH always writes it in the same commit as pr_url).
func fixLandedAfterPROpened(ctx context.Context, t store.Ticket, d Deps, number int) (bool, error) {
	opened, found, err := d.Store.Marker(ctx, t.ID, fmt.Sprintf("pr opened %d", number))
	if err != nil {
		return false, fmt.Errorf("job: shipping: pr opened marker: %w", err)
	}
	if !found {
		return false, nil
	}
	landed, err := d.Store.MarkersWithPrefix(ctx, t.ID, fixLandedPrefix)
	if err != nil {
		return false, fmt.Errorf("job: shipping: fix landed markers: %w", err)
	}
	for i := range landed {
		if landed[i].ID > opened.ID {
			return true, nil
		}
	}
	return false, nil
}

// staleReviewers is design section 9.4's own RE-REQUEST selection: distinct
// logins, in first-seen order, whose newest review (the last one reviews
// carries for that login -- ListReviews is paginated to completion in
// GitHub's own submission order, oldest first) has state APPROVED,
// CHANGES_REQUESTED, or COMMENTED, a commit id other than headSHA, user
// type "User" (never a bot), and a login other than viewer's own.
func staleReviewers(reviews []orchestrator.Review, headSHA, viewer string) []string {
	var order []string
	newest := make(map[string]orchestrator.Review, len(reviews))
	for _, r := range reviews {
		if _, seen := newest[r.Login]; !seen {
			order = append(order, r.Login)
		}
		newest[r.Login] = r
	}

	var stale []string
	for _, login := range order {
		r := newest[login]
		if r.UserType != "User" || login == viewer || r.CommitID == headSHA {
			continue
		}
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED", "COMMENTED":
			stale = append(stale, login)
		}
	}
	return stale
}

// is422 reports whether err is the one GitHub validation refusal
// RequestReviewers can carry (classifyGitHubErr,
// internal/orchestrator/githuberr.go, leaves 405/409/422 unclassified, each
// caller's own to interpret): a 422 means GitHub refused that one login --
// not a collaborator, already a requested reviewer, or the pull request's
// own author -- and RE-REQUEST logs it and moves on to the next login
// (design section 9.4) instead of failing the whole commit.
func is422(err error) bool {
	ere, ok := errors.AsType[*github.ErrorResponse](err)
	return ok && ere.Response != nil && ere.Response.StatusCode == http.StatusUnprocessableEntity
}

// reRequest is RE-REQUEST (design section 9.4, POLL row 2 of 8.5): headSHA's
// own stale reviewers (staleReviewers), RequestReviewers one login per call
// -- GitHub ignores an already-requested reviewer (design section 11), so a
// repeated request after a crash is harmless, and a 422 (is422) is logged
// and skipped rather than failing the commit -- then the closing
// "reviewers re-requested <headSHA>" marker naming every login this commit
// actually requested (possibly none), so this exact head is only ever
// handled once.
func (h shipHandler) reRequest(ctx context.Context, t store.Ticket, d Deps, proj Project, number int, headSHA, login string) (store.HandlerCommit, error) {
	reviews, err := proj.Threads.ListReviews(ctx, proj.Owner, proj.Repo, number)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: shipping: re-request: list reviews: %w", err)
	}
	stale := staleReviewers(reviews, headSHA, login)

	requested := make([]string, 0, len(stale))
	for _, l := range stale {
		if reqErr := proj.Threads.RequestReviewers(ctx, proj.Owner, proj.Repo, number, l); reqErr != nil {
			if is422(reqErr) {
				slog.Info("reviewer request skipped", "ticket_id", t.ID, "login", l, "error", reqErr)
				continue
			}
			return store.HandlerCommit{}, fmt.Errorf("job: shipping: re-request: request reviewers: %w", reqErr)
		}
		requested = append(requested, l)
	}

	c := baseCommit(t, d)
	c.Messages = []store.Message{{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: reRequestedMarkerFor(headSHA) + "\n" + strings.Join(requested, ","),
	}}
	c.ClearPoll = true
	return c, nil
}
