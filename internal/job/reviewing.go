// reviewing.go is the real "reviewing" state handler (design section 6): the
// post-build prelude (postbuild.go) runs first; ROUND (6.2) runs the
// ticket's review lenses in parallel, one commit per tick, bounded by
// Deps.LensesParallel; a lens that asks a question holds every other lens's
// own findings and the round waits (6.2a, ASKED); once the owner has
// answered every asking lens, CONTINUE resumes them (6.2a); a clean round
// with only at-or-below-floor findings requests a fix (FIXREQ, 6.8); one
// with findings above the floor posts the review question (6.4), which
// TRIAGE (6.5) resolves into one decision per item, defaulting an
// undecided or out-of-set one to accept; a discussed item with no "review
// discussed <id>" marker of its own runs DISCUSS (6.6), batching every
// pending item of its own lens session into one resume, routing its
// survivors back through FilterFindings and DedupFindings (6.3) the same
// way a fresh round does. It replaces the skeleton's reviewingHandler,
// which advanced straight to judging with no review job at all.
//
// Re-review's own lens selection (6.7) and the loop gate escalations are
// wired (selectLenses already exists, task 8), but the two-in-a-row and
// loop-exhaustion escalations (6.8, 6.9) this task's own named tests do not
// exercise are exactly as the plan's text describes; task 12 adds its own
// live pass and the remaining escalation resumes.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"zing/internal/orchestrator"
	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/runtime"
	"zing/internal/store"
)

// artifactTypeFinding is the artifacts.type value every stored review
// finding carries (design section 4.2; migrations/0001_init.sql's own
// artifacts.type CHECK is the source of truth, matched by
// internal/store/postbuild_reads.go's own private constant of the same
// name and value).
const artifactTypeFinding = "finding"

// jobReviewName is the job.go/machine.toml key ROUND's own sessions and job
// config are keyed under, "review" (design section 4.3, 6.2).
const jobReviewName = string(response.JobReview)

// The section 6.2/6.4 escalation What/Why texts this file writes that
// building.go has no equivalent constant for (a matching text, such as "no
// stored plan for this ticket" or "the worktree could not be prepared", is
// reused directly from building.go: the message is the same regardless of
// which state's own step hit it).
const (
	branchUnrecordedWhat = "the ticket branch holds commits Zing did not record"
	branchUnrecordedWhy  = "the branch does not match every commit Zing has recorded for this ticket, in order; with no fix open, an unrecorded commit is not Zing's"

	treeDirtyBeforeReviewWhat = "the worktree has uncommitted changes before review"
	treeDirtyBeforeReviewWhy  = "ChangedPaths found paths still dirty in the ticket's own worktree"

	noDiffWhat = "the branch has no diff against the base branch"
	noDiffWhy  = "Diff returned no text between the base branch and the branch head"

	lensAgentErrorWhy = "lens %s returned an error"

	lensFailedTwiceWhat = "two review rounds in a row failed"
	headMovedTwiceWhat  = "the ticket branch moved during two review rounds"

	lensInvalidTwiceWhat = "lens %s returned an invalid document twice in a row"
)

// reviewInvalidRetryHeader replaces the prompt file on a lens's same-tick
// retry after an invalid document (design section 6.2 step 7, issue #32):
// the retry carries no plan, diff, or code section again, only the
// validator's own error.
const reviewInvalidRetryHeader = "Your last document did not parse. The validator's errors follow. Return the next document."

// errLensFailed is ROUND and CONTINUE's own roundCtx cancellation cause
// (design section 6.2 step 7): the first lens result that is neither a
// parsed ok nor a parsed question document cancels every lens still
// waiting on the semaphore, so a goroutine that never acquired it reserves
// nothing.
var errLensFailed = errors.New("job: reviewing: a lens returned a non-ok, non-question result")

// reviewEscalation is escalationCommit (planning.go) plus this file's own
// "escalation written" log (design section 11), for every environment
// escalation ROUND writes before or during a round: origin is always
// "review" (a fix's own escalations, origin "fix", are the fix driver's,
// entered through the post-build prelude before reviewingHandler.Run ever
// reaches ROUND).
func reviewEscalation(t store.Ticket, d Deps, what, why, tried string) store.HandlerCommit {
	code := string(response.EscalationCodeEnvironment)
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginReview))
	return escalationCommit(t, d, nil, nil, code, what, why, tried, response.EscalationOriginReview)
}

// reviewLenses returns machine.toml's jobs.review.lenses, in file order, as
// response.Lens values (design section 6.2 step 6, 6.7): round 1 (or any
// round after "failed" or "void") runs every one of them.
func reviewLenses(d Deps) []response.Lens {
	cfg := d.Machine.Jobs[jobReviewName]
	out := make([]response.Lens, len(cfg.Lenses))
	for i, l := range cfg.Lenses {
		out[i] = response.Lens(l)
	}
	return out
}

// reviewingHandler runs the real reviewing state (design section 6.2, 6.2a).
type reviewingHandler struct{}

func (h reviewingHandler) Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	c, handled, err := postBuildPrelude(ctx, t, d, response.EscalationOriginReview)
	if handled || err != nil {
		return c, err
	}

	rounds, err := d.Store.AnsweredRounds(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: answered rounds: %w", err)
	}
	if len(rounds) > 0 {
		round := rounds[0]
		if round.Job != jobReviewName {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: unexpected answered round job %q", round.Job)
		}
		kind, kindErr := newestQuestionKind(round)
		if kindErr != nil {
			return store.HandlerCommit{}, kindErr
		}
		switch kind {
		case response.QuestionKindReview:
			return h.triage(ctx, t, d, round)
		case response.QuestionKindQuestion:
			// A plain agent question answered on this round is either a
			// discuss resume's own generic question (6.6 step 5: "the answer
			// comes back through step (1) and resumes this session with
			// answers"), when the round's session is the one a pending
			// discuss group is still waiting on, or an asking lens's own
			// question from the round itself (6.2a CONTINUE). The two never
			// overlap: a lens session belongs to exactly one of ROUND's own
			// asking set or a discuss group at a time.
			if round.SessionID != nil {
				group, found, groupErr := h.nextPendingDiscussGroup(ctx, t, d)
				if groupErr != nil {
					return store.HandlerCommit{}, groupErr
				}
				if found && group.sessionID == *round.SessionID {
					return h.discuss(ctx, t, d, group, &round)
				}
			}
			return h.continueRound(ctx, t, d, rounds)
		default:
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: unexpected question kind %q", kind)
		}
	}

	// Decision tree step (2): a finding with decision discuss and no "review
	// discussed <id>" marker runs DISCUSS directly, ahead of step (3)'s own
	// round-marker read, whenever no answered round is waiting on a route of
	// its own.
	group, found, err := h.nextPendingDiscussGroup(ctx, t, d)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if found {
		return h.discuss(ctx, t, d, group, nil)
	}

	return h.enterRound(ctx, t, d)
}

// ---- decision tree step 3: no round marker, or the newest one -------------

// reviewRoundMarkerPrefix is every "review round " marker's shared prefix
// (design section 5.1): done, failed, void, and asked markers all share it,
// so Store.MarkersWithPrefix returns every one of them, oldest first.
const reviewRoundMarkerPrefix = "review round "

// The four "review round " marker first-line shapes (design section 5.1).
var (
	reviewRoundDoneLine   = regexp.MustCompile(`^review round ([1-9]\d*) done sha ([0-9a-f]{40}) lenses (.+)$`)
	reviewRoundFailedLine = regexp.MustCompile(`^review round ([1-9]\d*) failed$`)
	reviewRoundVoidLine   = regexp.MustCompile(`^review round ([1-9]\d*) void$`)
	reviewRoundAskedLine  = regexp.MustCompile(`^review round ([1-9]\d*) asked$`)
)

// reviewRoundDoneCount reports how many "review round <k> done ..." markers
// the ticket carries: the decision tree's own "n := 1 + count(...)" (design
// section 6.1 step 3). A round's own number persists across a "failed" or
// "void" retry; it only advances once that round actually completes as
// "done".
func reviewRoundDoneCount(markers []store.MessageRow) int {
	n := 0
	for i := range markers {
		firstLine, _, _ := strings.Cut(markers[i].Body, "\n")
		if reviewRoundDoneLine.MatchString(firstLine) {
			n++
		}
	}
	return n
}

// enterRound is decision tree step 3 (design section 6.1): no round marker,
// or the newest one is "failed" or "void", enters ROUND n; the newest is
// "asked" with its questions still open (no answered round reached
// reviewingHandler.Run's own AnsweredRounds check, or it would have routed
// to continueRound instead) waits; the newest is "done" for round n-1
// enters enterFromDone. On the "failed" or "void" branch, an owner Retry
// (markerRetryRequested, written by resolvePostBuildEscalation's review
// row) newer than that round marker resets the two-in-a-row count: round
// is called with priorFailedOrVoid false, so a fresh failure only marks
// instead of escalating at once (issue #32).
func (h reviewingHandler) enterRound(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error) {
	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, reviewRoundMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: review round markers: %w", err)
	}
	n := reviewRoundDoneCount(markers) + 1

	if len(markers) == 0 {
		return h.round(ctx, t, d, n, "", false)
	}
	newest := markers[len(markers)-1]
	firstLine, _, _ := strings.Cut(newest.Body, "\n")

	switch {
	case reviewRoundDoneLine.MatchString(firstLine):
		return h.enterFromDone(ctx, t, d, n)
	case reviewRoundAskedLine.MatchString(firstLine):
		return store.HandlerCommit{}, ErrNoAction
	case reviewRoundFailedLine.MatchString(firstLine), reviewRoundVoidLine.MatchString(firstLine):
		retries, retryErr := d.Store.MarkersWithPrefix(ctx, t.ID, markerRetryRequested)
		if retryErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: retry requested markers: %w", retryErr)
		}
		priorFailedOrVoid := true
		for i := range retries {
			if retries[i].ID > newest.ID && retries[i].Body == markerRetryRequested {
				priorFailedOrVoid = false
				break
			}
		}
		return h.round(ctx, t, d, n, "", priorFailedOrVoid)
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: unrecognized review round marker %q", firstLine)
	}
}

// enterFromDone is decision tree step 3's "newest round marker is done for
// round n-1" branch (design section 6.1): an undecided finding of round n-1
// waits; with no fix-requested-findings marker after the done marker, an
// empty fix list moves to judging and a non-empty one calls fixreq (FIXREQ,
// 6.8); otherwise a fix has already landed (or the post-build prelude would
// have run it), so round n runs again with 6.7's lens selection.
//
// The undecided check reads the newest row per finding id (newestFindingRow
// PerID), not every row raw: TRIAGE (6.5) and DISCUSS (6.6) both append a
// new row for an id that already has one (the original above-floor row, a
// discuss survivor's own supersede), so an id's own first, Decision-nil row
// can outlive its own decision being taken; skipping that dedup here would
// see it forever and never leave ErrNoAction.
func (h reviewingHandler) enterFromDone(ctx context.Context, t store.Ticket, d Deps, n int) (store.HandlerCommit, error) {
	prevRound := n - 1

	findings, err := d.Store.Findings(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: findings: %w", err)
	}
	for _, row := range newestFindingRowPerID(findings) {
		f := row.Finding
		if f.Round == prevRound && !f.Held && f.Decision == nil {
			return store.HandlerCommit{}, ErrNoAction
		}
	}

	// Marker's own exact-first-line match cannot find this one: the "done"
	// marker's first line carries the round's own sha and lens list after
	// "review round <n> done" (successCommit), not that text alone, so this
	// reads it the same way reviewRoundDoneCount does, by prefix.
	doneMarkers, err := d.Store.MarkersWithPrefix(ctx, t.ID, fmt.Sprintf("review round %d done", prevRound))
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: done marker: %w", err)
	}
	if len(doneMarkers) == 0 {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: round %d has no done marker", prevRound)
	}
	doneMarker := doneMarkers[len(doneMarkers)-1]

	fixReqs, err := d.Store.MarkersWithPrefix(ctx, t.ID, fixRequestedFindingsPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: fix requested findings markers: %w", err)
	}
	hasReqAfter := false
	for i := range fixReqs {
		if fixReqs[i].ID > doneMarker.ID {
			hasReqAfter = true
			break
		}
	}

	if !hasReqAfter {
		accepted := acceptedRoundFindings(findings, prevRound)
		if len(accepted) == 0 {
			c := baseCommit(t, d)
			c.Next, c.Reason = stateJudging, reasonReviewClean
			return c, nil
		}
		return h.fixreq(ctx, t, d, accepted)
	}

	return h.round(ctx, t, d, n, "", false)
}

// newestFindingRowPerID reduces findings (Findings' own artifact-id order,
// every row ever written) to the newest row per finding id, the same
// newestFileEventPerPath pattern building.go already uses for a path:
// findings are append-only, so a later row for the same id always
// supersedes an earlier one (TRIAGE's own decided row over ROUND's
// undecided one; a discuss revision keeps its own id distinct instead, so
// it never collides with the row it supersedes). The map holds pointers
// into findings itself, never copied again, since FindingRow is large
// enough that every other range over it would otherwise copy it uselessly
// (gocritic rangeValCopy).
func newestFindingRowPerID(findings []store.FindingRow) map[string]*store.FindingRow {
	out := make(map[string]*store.FindingRow, len(findings))
	for i := range findings {
		out[findings[i].Finding.ID] = &findings[i]
	}
	return out
}

// acceptedRoundFindings returns every finding of round with Decision accept,
// unheld, in Findings' own order (artifact id order).
func acceptedRoundFindings(findings []store.FindingRow, round int) []response.FindingArtifact {
	var out []response.FindingArtifact
	for i := range findings {
		f := findings[i].Finding
		if f.Round == round && !f.Held && f.Decision != nil && *f.Decision == response.FindingAccept {
			out = append(out, f)
		}
	}
	return out
}

// fixRequestedFindingsPrefix is the "fix requested findings after run "
// marker family FIXREQ writes (fixRequestMessage's own format, fix.go):
// enterFromDone reads it to tell whether round n-1's own findings already
// opened a fix request.
const fixRequestedFindingsPrefix = "fix requested findings after run "

// fixreq is FIXREQ (design section 6.8) entered from enterFromDone: under
// jobs.review.max_loops it opens a fix request with accepted's own fix
// text. At the gate, an accepted list wholly at or below the floor moves on
// to judging (acceptAtCap, issue #68); otherwise an accepted row the owner
// picked (OwnerPicked) opens the fix request anyway, bypassing the gate,
// because the review question already asked the owner this once; otherwise
// the gate escalates loops_exhausted as before.
func (h reviewingHandler) fixreq(ctx context.Context, t store.Ticket, d Deps, accepted []response.FindingArtifact) (store.HandlerCommit, error) {
	msg, k, maxLoops, err := fixRequestOrLoopsExhausted(ctx, t, d, accepted)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if msg == nil {
		switch {
		case allAtOrBelowFloor(accepted, d.Floor):
			return acceptAtCap(baseCommit(t, d), t, d, k, maxLoops, accepted), nil
		case slices.ContainsFunc(accepted, func(f response.FindingArtifact) bool { return f.OwnerPicked }):
			return ownerAcceptAtCap(ctx, t, d, k, maxLoops, accepted)
		default:
			what := fmt.Sprintf("review findings remain after %d fix runs", k)
			why := fmt.Sprintf("max_loops for review is %d", maxLoops)
			return reviewLoopsExhausted(t, d, what, why, renderFixFindings(accepted)), nil
		}
	}
	c := baseCommit(t, d)
	c.Messages = []store.Message{*msg}
	return c, nil
}

// reviewLoopsExhausted is FIXREQ's own loops_exhausted escalation (design
// section 6.8): no run caused it, so RunID and SessionID are both nil.
func reviewLoopsExhausted(t store.Ticket, d Deps, what, why, tried string) store.HandlerCommit {
	code := string(response.EscalationCodeLoopsExhausted)
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginReview))
	return escalationCommit(t, d, nil, nil, code, what, why, tried, response.EscalationOriginReview)
}

// ---- TRIAGE (design section 6.5) ------------------------------------------

// reviewNoteNone is TRIAGE's own text for a discussed item whose answer
// carried no free reply (design section 6.5 step 4, D24).
const reviewNoteNone = "(the owner gave no note)"

// reviewFindingDecision maps one item's answered response.Decision to its
// FindingDecision counterpart (design section 6.5 step 2): review items
// take accept, drop, or discuss only -- reject is perimeter's own decision
// -- so ok is false for reject, an unanswered item's zero Decision, or any
// other value; the caller then applies the safe default (accept).
func reviewFindingDecision(d response.Decision) (response.FindingDecision, bool) {
	switch d {
	case response.DecisionAccept:
		return response.FindingAccept, true
	case response.DecisionDrop:
		return response.FindingDrop, true
	case response.DecisionDiscuss:
		return response.FindingDiscuss, true
	default:
		return "", false
	}
}

// replyTexts returns each reply's own Body, in replies' own order
// (AnsweredRounds' own ascending id order).
func replyTexts(replies []store.MessageRow) []string {
	out := make([]string, len(replies))
	for i := range replies {
		out[i] = replies[i].Body
	}
	return out
}

// triage is TRIAGE (design section 6.5): entry is decision tree step (1)'s
// own "newest question kind review" branch, once the owner has answered the
// review question (6.4). It stores one new finding row per item, carrying
// the owner's own decision or the safe default accept (a missing or
// out-of-set decision, logged at warn); the row's own OwnerPicked is true
// only for the owner's own valid decision, never for the defaulted accept.
// It also writes one "review note <id>" marker per discussed item (D24: the
// owner's reply on the question applies to every finding discussed in that
// answer), and resolves the round's own question. No state transition and
// no new Waiting (baseCommit's own nil clears it): the next tick's decision
// tree step (2) or (3) routes the ticket on from the decisions this commit
// just stored.
func (h reviewingHandler) triage(ctx context.Context, t store.Ticket, d Deps, round store.Round) (store.HandlerCommit, error) {
	if len(round.Questions) != 1 {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: triage: review round carries %d questions, want 1", len(round.Questions))
	}
	q := round.Questions[0]
	var qp response.QuestionPayload
	if err := json.Unmarshal(q.Payload, &qp); err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: triage: unmarshal question %d payload: %w", q.ID, err)
	}

	findings, err := d.Store.Findings(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: triage: findings: %w", err)
	}
	newest := newestFindingRowPerID(findings)
	decisions := mergedItemDecisions(round.Answers)
	note := strings.Join(replyTexts(round.Replies), "\n")

	c := baseCommit(t, d)
	c.ResolveQuestions = questionIDs(round)

	for _, item := range qp.Items {
		row, ok := newest[item.Ref]
		if !ok {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: triage: no stored finding for item %q", item.Ref)
		}
		fd, validDecision := reviewFindingDecision(decisions[item.Ref])
		if !validDecision {
			slog.Warn("review decision defaulted to accept", "ticket_id", t.ID, "finding_id", item.Ref)
			fd = response.FindingAccept
		}

		finding := row.Finding
		finding.Decision = &fd
		finding.OwnerPicked = validDecision
		payload, marshalErr := json.Marshal(finding)
		if marshalErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: triage: marshal finding %s: %w", finding.ID, marshalErr)
		}
		c.Artifacts = append(c.Artifacts, store.Artifact{Type: artifactTypeFinding, RunID: row.RunID, Payload: payload})

		if fd == response.FindingDiscuss {
			body := "review note " + finding.ID + "\n"
			if note == "" {
				body += reviewNoteNone
			} else {
				body += note
			}
			c.Messages = append(c.Messages, store.Message{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: body})
		}
	}
	return c, nil
}

// ---- DISCUSS (design section 6.6) -----------------------------------------

// pendingDiscussGroup is P's own batch for one lens session (design section
// 6.6): every still-pending discussed finding of that session, ID order,
// plus the session's own lens, round, and frozen sha (every row of one
// session's own group shares them, since a lens session belongs to exactly
// one round: round() always reserves a fresh session per lens, never
// resuming a prior round's).
type pendingDiscussGroup struct {
	sessionID int64
	lens      response.Lens
	round     int
	sha       string
	findings  []response.FindingArtifact
}

// reviewDiscussedMarker and reviewNoteMarker are the "review discussed <id>"
// and "review note <id>" marker heads (design section 6.5 step 4, 6.6 step
// 4): Marker's own exact-first-line match, keyed by id.
func reviewDiscussedMarker(id string) string { return "review discussed " + id }
func reviewNoteMarker(id string) string      { return "review note " + id }

// nextPendingDiscussGroup finds P, the ticket's own still-pending discussed
// findings (design section 6.6: decision discuss, no "review discussed
// <id>" marker), grouped by the lens session that produced them, and
// returns the group whose lowest finding id is lowest. found is false when
// the ticket carries no pending discuss at all.
func (h reviewingHandler) nextPendingDiscussGroup(ctx context.Context, t store.Ticket, d Deps) (pendingDiscussGroup, bool, error) {
	findings, err := d.Store.Findings(ctx, t.ID)
	if err != nil {
		return pendingDiscussGroup{}, false, fmt.Errorf("job: reviewing: discuss: findings: %w", err)
	}

	var pendingIDs []string
	pendingRows := make(map[string]*store.FindingRow)
	for id, row := range newestFindingRowPerID(findings) {
		f := row.Finding
		if f.Held || f.Decision == nil || *f.Decision != response.FindingDiscuss {
			continue
		}
		_, marked, markerErr := d.Store.Marker(ctx, t.ID, reviewDiscussedMarker(id))
		if markerErr != nil {
			return pendingDiscussGroup{}, false, fmt.Errorf("job: reviewing: discuss: discussed marker %s: %w", id, markerErr)
		}
		if marked {
			continue
		}
		pendingIDs = append(pendingIDs, id)
		pendingRows[id] = row
	}
	if len(pendingIDs) == 0 {
		return pendingDiscussGroup{}, false, nil
	}

	sort.Slice(pendingIDs, func(i, j int) bool {
		ri, ki, _ := findingIDKey(pendingIDs[i])
		rj, kj, _ := findingIDKey(pendingIDs[j])
		if ri != rj {
			return ri < rj
		}
		return ki < kj
	})

	groups := make(map[int64]*pendingDiscussGroup)
	var order []int64
	for _, id := range pendingIDs {
		row := pendingRows[id]
		if row.RunID == nil {
			return pendingDiscussGroup{}, false, fmt.Errorf("job: reviewing: discuss: finding %s carries no run id", id)
		}
		run, runErr := d.Store.RunByID(ctx, *row.RunID)
		if runErr != nil {
			return pendingDiscussGroup{}, false, fmt.Errorf("job: reviewing: discuss: run by id: %w", runErr)
		}
		if run.Lens == nil {
			return pendingDiscussGroup{}, false, fmt.Errorf("job: reviewing: discuss: run %d carries no lens", run.ID)
		}
		g, ok := groups[run.SessionID]
		if !ok {
			g = &pendingDiscussGroup{sessionID: run.SessionID, lens: response.Lens(*run.Lens), round: row.Finding.Round, sha: row.Finding.SHA}
			groups[run.SessionID] = g
			order = append(order, run.SessionID)
		}
		g.findings = append(g.findings, row.Finding)
	}

	// pendingIDs is sorted ascending, so the first session order names owns
	// the globally lowest pending finding id: "the group whose lowest
	// finding id is lowest" (design section 6.6).
	return *groups[order[0]], true, nil
}

// renderDiscussFindings renders every finding of rows, in ID order, each as
// 6.3's own fix-text block (findingBlock, reviewrules.go) with no decision
// filter: every finding DISCUSS resumes with carries decision discuss, so
// renderFixFindings' own accept-only filter would drop them all.
func renderDiscussFindings(rows []response.FindingArtifact) string {
	sorted := sortByID(rows)
	blocks := make([]string, len(sorted))
	for i := range sorted {
		blocks[i] = findingBlock(sorted[i])
	}
	return strings.Join(blocks, "\n\n")
}

// discussNotesInput renders design section 6.6 step 3's own "notes" input:
// one "<id>: <review note text>" line per finding of rows, in ID order,
// reviewNoteNone when that finding's own "review note <id>" marker carries
// none (TRIAGE always writes one, so its absence names a bug, not a valid
// state this falls back on quietly).
func discussNotesInput(ctx context.Context, d Deps, ticketID int64, rows []response.FindingArtifact) (string, error) {
	sorted := sortByID(rows)
	lines := make([]string, len(sorted))
	for i := range sorted {
		m, ok, err := d.Store.Marker(ctx, ticketID, reviewNoteMarker(sorted[i].ID))
		if err != nil {
			return "", fmt.Errorf("job: reviewing: discuss: review note marker %s: %w", sorted[i].ID, err)
		}
		text := reviewNoteNone
		if ok {
			_, rest, hasRest := strings.Cut(m.Body, "\n")
			if hasRest {
				text = rest
			}
		}
		lines[i] = sorted[i].ID + ": " + text
	}
	return strings.Join(lines, "\n"), nil
}

// nextFindingK returns the next free per-round sequence number for a new
// "r<round>f<k>" id (design section 6.2, 6.6): one past the highest k any
// unheld finding of round already carries. Held rows (r<round>h<k>) keep
// their own separate counter (askedCommit's heldSoFar) and are excluded.
func nextFindingK(findings []store.FindingRow, round int) int {
	maxK := 0
	for i := range findings {
		f := findings[i].Finding
		if f.Held || f.Round != round {
			continue
		}
		if _, k, ok := findingIDKey(f.ID); ok && k > maxK {
			maxK = k
		}
	}
	return maxK + 1
}

// discussHeadMovedWhat and discussHeadMovedWhy are DISCUSS's own step 1
// environment escalation (design section 6.6): "the ticket branch moved
// during triage", byte for byte from the plan.
const (
	discussHeadMovedWhat = "the ticket branch moved during triage"
	discussHeadMovedWhy  = "HeadSHA no longer matches the sha the discussed finding's own round was read at"
)

// discuss is DISCUSS (design section 6.6): group is nextPendingDiscussGroup's
// own pick (P's lowest-id group). round is nil for a fresh discuss resume,
// entered from decision tree step (2) (inputs: every finding of the group,
// and the owner's notes); non-nil when the discuss session's own generic
// question has just been answered (step (1)'s "session of a pending
// discuss"), which resumes with the owner's answers like any other agent
// question (7.2, N1) instead of starting a new discuss turn, and whose
// question ids this commit also resolves.
func (h reviewingHandler) discuss(ctx context.Context, t store.Ticket, d Deps, group pendingDiscussGroup, round *store.Round) (store.HandlerCommit, error) {
	maxResumes := d.Machine.Jobs[jobReviewName].MaxResumes
	sess, state, err := d.Store.SessionByID(ctx, group.sessionID, maxResumes)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: discuss: session by id: %w", err)
	}

	proj, wt, escalation, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return reviewEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	headSHA, err := proj.Orch.HeadSHA(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: discuss: head sha: %w", err)
	}
	if headSHA != group.sha {
		return reviewEscalation(t, d, discussHeadMovedWhat, discussHeadMovedWhy, ""), nil
	}

	var resolveIDs []int64
	if round != nil {
		resolveIDs = questionIDs(*round)
	}

	// resumeCharge (job.go, design D5, section 7.4): an interrupted latest
	// run resumes this discuss turn free and bypasses the exhausted-cap
	// escalation below, even on a session already at max_resumes.
	newestRun, foundRun, newestErr := d.Store.SessionNewestRun(ctx, sess.ID)
	if newestErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: discuss: newest run: %w", newestErr)
	}
	bump, gate := true, true
	if foundRun {
		bump, gate = resumeCharge(newestRun)
	}

	if state == store.SessionExhausted && gate {
		has, hasErr := d.Store.HasEscalation(ctx, t.ID, string(response.EscalationOriginCapResumes), sess.ID)
		if hasErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: discuss: has escalation: %w", hasErr)
		}
		if has {
			return store.HandlerCommit{}, ErrNoAction
		}
		c := reviewCapResumesEscalation(t, d, sess.ID)
		c.ResolveQuestions = resolveIDs
		return c, nil
	}

	schemas, err := renderSchemas(response.JobReview, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: discuss: %w", err)
	}

	var inputs []prompt.NamedInput
	if round != nil {
		answers, ansErr := renderRoundAnswers(*round)
		if ansErr != nil {
			return store.HandlerCommit{}, ansErr
		}
		inputs = []prompt.NamedInput{prompt.Answers(answers)}
	} else {
		notesText, notesErr := discussNotesInput(ctx, d, t.ID, group.findings)
		if notesErr != nil {
			return store.HandlerCommit{}, notesErr
		}
		inputs = []prompt.NamedInput{
			prompt.Findings(renderDiscussFindings(group.findings)),
			prompt.Notes(notesText),
		}
	}
	if foundRun && newestRun.Interrupted {
		inputs = append(inputs, prompt.NamedInput{Label: labelInterrupted, Text: interruptedResumeText, Untrusted: false})
	}
	var in prompt.Input
	if round != nil {
		in = prompt.ForReviewResume(inputs)
	} else {
		in = prompt.ForReviewDiscuss(inputs)
	}
	in.Schemas = schemas

	req := runtime.RunRequest{
		Job: response.JobReview, Label: fmt.Sprintf("%d-%s", group.round, group.lens), WorkDir: wt.Dir(),
		SessionID: derefString(sess.ExternalID), Prompt: prompt.Assemble(in),
	}
	su := store.SessionUpsert{ID: &sess.ID, BumpResumes: bump}

	priorInvalid, _, invErr := d.Store.ConsecutiveInvalidOutputs(ctx, t.ID, jobReviewName, &sess.ID)
	if invErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: discuss: consecutive invalid outputs: %w", invErr)
	}

	return h.discussRunAndRoute(ctx, t, d, su, req, group.lens, priorInvalid, sess.ID, resolveIDs,
		func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error) {
			return h.discussOkCommit(ctx, t, d, proj, wt, group, rr, sessionCommit)
		})
}

// discussRunAndRoute runs one discuss resume (runJob) and routes its result
// exactly as every other job's universal outcome table does (design section
// 6.8), with review's own readonly sandbox reason in place of build's:
// onOk handles the review job's own ok outcome (a *response.FindingsResponse),
// a plain agent question is questionOutcomeCommit, an agent error is
// errorOutcomeCommit (origin review), and every runtime failure short of
// that -- budget, sandbox, config, claim lost, canceled, invalid output, or
// exec failure -- is the same universal handling runAndRoute (planning.go)
// gives classify and planning; this file does not call runAndRoute directly
// because that seam passes runJob a nil lens, and a discuss resume's own run
// must carry its lens the same way every other review run does.
func (h reviewingHandler) discussRunAndRoute(
	ctx context.Context, t store.Ticket, d Deps, su store.SessionUpsert, req runtime.RunRequest,
	lens response.Lens, priorInvalid int, sessionID int64, resolveIDs []int64,
	onOk func(rr runResult, sessionCommit *store.SessionUpsert) (store.HandlerCommit, error),
) (store.HandlerCommit, error) {
	lensStr := string(lens)
	rr, runErr := runJob(ctx, d, t, jobReviewName, su, req, nil, &lensStr, 0)
	sessionCommit := resumeSessionRecord(sessionID, rr)

	if runErr != nil {
		switch {
		case errors.Is(runErr, runtime.ErrCanceled), errors.Is(runErr, ErrConfig), errors.Is(runErr, store.ErrClaimLost):
			return store.HandlerCommit{}, runErr
		case errors.Is(runErr, ErrBudget):
			return budgetEscalationCommit(t, d, resolveIDs), nil
		case errors.Is(runErr, ErrSandbox):
			return sandboxEscalationCommit(t, d, resolveIDs, response.EscalationOriginReview, d.Sandboxes.ReadOnly.Reason()), nil
		}
		var invErr *runtime.InvalidOutputError
		if errors.As(runErr, &invErr) { //nolint:modernize // errors.AsType discards its bool via _, which errcheck flags
			return invalidOutputCommit(t, d, rr, invErr, priorInvalid, sessionCommit, resolveIDs, response.EscalationOriginReview), nil
		}
		if isExecFailure(runErr) {
			return execFailureCommit(t, d, rr, sessionCommit, resolveIDs, response.EscalationOriginReview), nil
		}
		wrapped := fmt.Errorf("job: reviewing: discuss: unrecognized runJob error: %w", runErr)
		if rr.Reserved.RunID != 0 {
			return postRunFailure(t, d, rr, sessionCommit, resolveIDs, response.EscalationOriginReview, wrapped), nil
		}
		return store.HandlerCommit{}, wrapped
	}

	switch resp := rr.Res.Response.(type) {
	case *response.FindingsResponse:
		return onOk(rr, sessionCommit)
	case *response.QuestionResponse:
		return questionOutcomeCommit(t, d, rr, resp.Questions, sessionCommit, resolveIDs)
	case *response.ErrorResponse:
		return errorOutcomeCommit(t, d, rr, resp, sessionCommit, resolveIDs, response.EscalationOriginReview), nil
	default:
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: discuss: outcome %s not handled", rr.Res.Response.Header().Outcome)
	}
}

// discussOkCommit is DISCUSS's own ok outcome (design section 6.6 step 4):
// survivors := FilterFindings(fr.Findings, idx) against the round's own
// frozen diff, then DedupFindings; each survivor becomes a new row at the
// round's own next free k, Supersedes every id of the group, SHA and Round
// the group's own. One "review discussed <id>" marker per finding of the
// group (design section 6.6: "One marker ... per finding of the group"),
// each carrying the same run id, batch, and kept count. Survivors above the
// floor post a new review question (6.4); zero survivors means the lens
// withdrew every finding of the group, and no question follows.
func (h reviewingHandler) discussOkCommit(
	ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree,
	group pendingDiscussGroup, rr runResult, sessionCommit *store.SessionUpsert,
) (store.HandlerCommit, error) {
	fr, ok := rr.Res.Response.(*response.FindingsResponse)
	if !ok {
		return store.HandlerCommit{}, errors.New("job: reviewing: discuss: expected a findings document")
	}

	diff, err := proj.Orch.Diff(ctx, wt, group.sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: discuss: diff: %w", err)
	}
	idx := orchestrator.ParseDiff(diff)

	survivors := FilterFindings(fr.Findings, idx)
	merged := DedupFindings(survivors, reviewLenses(d))

	findings, err := d.Store.Findings(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: discuss: findings: %w", err)
	}
	k := nextFindingK(findings, group.round)

	groupSorted := sortByID(group.findings)
	groupIDs := make([]string, len(groupSorted))
	for i := range groupSorted {
		groupIDs[i] = groupSorted[i].ID
	}

	for i := range merged {
		merged[i].ID = fmt.Sprintf("r%df%d", group.round, k+i)
		merged[i].Round = group.round
		merged[i].SHA = group.sha
		merged[i].Supersedes = groupIDs
	}
	atOrBelow, above := splitByFloor(merged, d.Floor)
	stored := sortByID(append(append([]response.FindingArtifact{}, atOrBelow...), above...))

	c := baseCommit(t, d)
	c.Runs = terminalRuns(rr, string(response.OutcomeOk))
	c.Session = sessionCommit

	runIDStr := strconv.FormatInt(rr.Reserved.RunID, 10)
	batchStr := strings.Join(groupIDs, ",")
	kept := len(merged)
	for _, id := range groupIDs {
		c.Messages = append(c.Messages, store.Message{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("%s\nrun %s batch %s kept %d", reviewDiscussedMarker(id), runIDStr, batchStr, kept),
		})
	}

	if len(stored) > 0 {
		artifacts := make([]store.Artifact, len(stored))
		for i := range stored {
			payload, marshalErr := json.Marshal(stored[i])
			if marshalErr != nil {
				return store.HandlerCommit{}, fmt.Errorf("job: reviewing: discuss: marshal finding %s: %w", stored[i].ID, marshalErr)
			}
			runID := rr.Reserved.RunID
			artifacts[i] = store.Artifact{Type: artifactTypeFinding, RunID: &runID, Payload: payload}
		}
		c.Artifacts = artifacts
	}

	if len(above) > 0 {
		qMsg, qErr := reviewQuestionMessage(t, d, group.round, rr.Reserved.RunID, above)
		if qErr != nil {
			return store.HandlerCommit{}, qErr
		}
		c.Messages = append(c.Messages, qMsg)
		waiting := waitingFlagReview
		c.Waiting = &waiting
	}
	return c, nil
}

// ---- ROUND (design section 6.2) -------------------------------------------

// lensFirstTry is a lens's discarded first turn: the run that returned an
// invalid document before its same-tick retry (runLensesParallel, design
// section 6.2 step 7, issue #32).
type lensFirstTry struct {
	rr     runResult
	invErr *runtime.InvalidOutputError
}

// lensAttempt is one lens's own runJob result, plus its position in the
// lens set that round (or continueRound) launched it from: idx breaks a tie
// among several lenses that failed the same way, in lens order, exactly as
// every other "in lens order" rule in section 6 does. rr and err are the
// lens's own final turn: the retry's own result when firstTry is set,
// otherwise its only turn. sessionRecord builds that attempt's own
// terminalizing Session/Sessions entry: freshSessionRecord for a first-turn
// ROUND lens, a resumeSessionRecord closure bound to its own session id for
// a CONTINUE resume.
type lensAttempt struct {
	idx           int
	lens          response.Lens
	rr            runResult
	err           error
	sessionRecord func(runResult) *store.SessionUpsert
	// firstTry is set only when this lens's first turn returned an invalid
	// document with a reserved run and a session id, roundCtx was not
	// already canceled, and the retry ran (design section 6.2 step 7, issue
	// #32).
	firstTry *lensFirstTry
}

// outcomeString classifies a's own parsed document into the terminal
// outcome string every row but the first four uses (design section 6.2:
// "ok for a run that returned a parsed ok document, question or error for
// those outcomes, error for everything else").
func (a lensAttempt) outcomeString() string {
	if a.err == nil {
		switch a.rr.Res.Response.(type) {
		case *response.FindingsResponse:
			return string(response.OutcomeOk)
		case *response.QuestionResponse:
			return string(response.OutcomeQuestion)
		}
	}
	return string(response.OutcomeError)
}

// isGood reports whether a's own result is a parsed ok or question document
// -- the only two outcomes that do not cancel the round or continuation
// (design section 6.2 step 7).
func (a lensAttempt) isGood() bool {
	if a.err != nil {
		return false
	}
	switch a.rr.Res.Response.(type) {
	case *response.FindingsResponse, *response.QuestionResponse:
		return true
	default:
		return false
	}
}

// errLensesParallelRange is runLensesParallel's own config error (a
// Package 9 handoff bug fix): d.LensesParallel sizes the semaphore channel
// below, and a value less than 1 -- the zero value a test helper or a caller
// forgot to set -- makes that channel capacity 0, which every goroutine's
// own "case sem <- struct{}{}" then blocks on forever, since nothing ever
// receives from a channel no send has yet completed on. review.max_lenses_
// parallel's own range (design D3) is 1 to 7.
var errLensesParallelRange = fmt.Errorf("%w: lenses parallel must be 1 to 7", ErrConfig)

// runLensesParallel runs one runtime turn per lens, bounded by
// d.LensesParallel in flight at once (design section 6.2 step 7): a
// semaphore channel sized to it gates each goroutine's own runJob call. An
// invalid document with a reserved run and a session id is retried once, in
// place, on that same session, carrying the validator's error (issue #32),
// unless roundCtx is already canceled; the first final result that is
// still not a parsed ok or question document cancels roundCtx, so a
// goroutine still waiting on the semaphore reserves nothing. build is
// called once per lens, inside its own goroutine, to assemble that lens's
// own SessionUpsert and RunRequest; it must not block. schemas is the
// retry's own schema list (the same one build's own closure already
// rendered for the lens's first turn). The returned attempts are sorted by
// idx (lens order), regardless of the order they actually finished in.
// d.LensesParallel outside [1,7] is refused before any goroutine starts and
// before any run reserves (errLensesParallelRange).
func runLensesParallel(
	ctx context.Context, d Deps, t store.Ticket,
	lenses []response.Lens, schemas []string,
	build func(lens response.Lens) (store.SessionUpsert, runtime.RunRequest, func(runResult) *store.SessionUpsert),
) ([]lensAttempt, error) {
	if d.LensesParallel < 1 || d.LensesParallel > 7 {
		return nil, errLensesParallelRange
	}

	roundCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	sem := make(chan struct{}, d.LensesParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var attempts []lensAttempt

	for i, lens := range lenses {
		wg.Add(1)
		go func(idx int, lens response.Lens) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-roundCtx.Done():
				return
			}
			defer func() { <-sem }()

			su, req, sessionRecord := build(lens)
			lensStr := string(lens)
			rr, err := runJob(roundCtx, d, t, jobReviewName, su, req, nil, &lensStr, 0)

			at := lensAttempt{idx: idx, lens: lens, rr: rr, err: err, sessionRecord: sessionRecord}
			var invErr *runtime.InvalidOutputError
			if errors.As(err, &invErr) && rr.Reserved.RunID != 0 { //nolint:modernize // see execFailureKind
				switch {
				case rr.Res.SessionID == "":
					slog.Info("review lens retry skipped", "ticket_id", t.ID, "lens", lensStr, "run_id", rr.Reserved.RunID, "cause", "no session id")
				case roundCtx.Err() != nil:
					slog.Info("review lens retry skipped", "ticket_id", t.ID, "lens", lensStr, "run_id", rr.Reserved.RunID, "cause", "round canceled")
				default:
					reason := invErr.Reason
					if invErr.Detail != "" {
						reason += "\n" + invErr.Detail
					}
					in := prompt.Input{
						JobPrompt: reviewInvalidRetryHeader,
						Inputs:    []prompt.NamedInput{prompt.Invalid(invalidRetryText(reason))},
						Schemas:   schemas,
					}
					retryReq := runtime.RunRequest{
						Job: response.JobReview, Label: req.Label, WorkDir: req.WorkDir,
						SessionID: rr.Res.SessionID, Prompt: prompt.Assemble(in),
					}
					sessionID := rr.Reserved.SessionID
					slog.Info("review lens retry started", "ticket_id", t.ID, "lens", lensStr, "session_id", sessionID,
						"run_id", rr.Reserved.RunID, "reason", invErr.Reason)
					rr2, err2 := runJob(roundCtx, d, t, jobReviewName, store.SessionUpsert{ID: &sessionID}, retryReq, nil, &lensStr, 0)
					at.firstTry = &lensFirstTry{rr: rr, invErr: invErr}
					at.rr, at.err = rr2, err2
					slog.Info("review lens retry finished", "ticket_id", t.ID, "lens", lensStr, "session_id", sessionID,
						"run_id", rr2.Reserved.RunID, "first_run_id", rr.Reserved.RunID,
						"outcome", at.outcomeString(), "err_kind", errKind(err2))
				}
			}
			if !at.isGood() {
				cancel(errLensFailed)
			}

			mu.Lock()
			attempts = append(attempts, at)
			mu.Unlock()
		}(i, lens)
	}
	wg.Wait()

	slices.SortFunc(attempts, func(a, b lensAttempt) int { return a.idx - b.idx })
	return attempts, nil
}

// terminalizeAttempts builds the Runs, Session, and Sessions a commit
// carries for every lens that actually reserved a run (design section 6.2:
// "Session holds the first reserved run's session record in lens order and
// Sessions holds the rest; a nil record (only on runtime.ErrStart) is
// skipped"), in idx (lens) order. A retried lens (firstTry set, issue #32)
// terminalizes both of its own runs: the first turn as error, the retry as
// its own outcome; its session record is built from the first turn, whose
// own run carries the session's external id on creation, so the retry's own
// run (a resume, D13) is the one skipped rather than double-recorded.
func terminalizeAttempts(attempts []lensAttempt) (runs []store.Run, session *store.SessionUpsert, sessions []store.SessionUpsert) {
	for i := range attempts {
		a := &attempts[i]
		recFrom := a.rr
		if a.firstTry != nil {
			runs = append(runs, terminalRuns(a.firstTry.rr, string(response.OutcomeError))...)
			recFrom = a.firstTry.rr
		}
		if a.rr.Reserved.RunID != 0 {
			runs = append(runs, terminalRuns(a.rr, a.outcomeString())...)
		}
		if a.firstTry == nil && a.rr.Reserved.RunID == 0 {
			continue
		}

		rec := a.sessionRecord(recFrom)
		if rec == nil {
			continue
		}
		if session == nil {
			session = rec
			continue
		}
		sessions = append(sessions, *rec)
	}
	return runs, session, sessions
}

// applyAttempts terminalizes every lens run in attempts onto c (Runs,
// Session, Sessions) and appends one "response invalid run N" marker per
// run that returned an invalid document, in lens order, first turn before
// retry, so the console shows each run's own validator error (design
// section 6.2 step 7, issue #32).
func applyAttempts(c *store.HandlerCommit, ticketID int64, attempts []lensAttempt) {
	c.Runs, c.Session, c.Sessions = terminalizeAttempts(attempts)
	for i := range attempts {
		a := &attempts[i]
		if a.firstTry != nil {
			c.Messages = append(c.Messages, store.Message{
				TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem,
				Body: invalidMarkerBody(a.firstTry.rr.Reserved.RunID, a.firstTry.invErr),
			})
		}
		var invErr *runtime.InvalidOutputError
		if a.rr.Reserved.RunID != 0 && errors.As(a.err, &invErr) { //nolint:modernize // see execFailureKind
			c.Messages = append(c.Messages, store.Message{
				TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem,
				Body: invalidMarkerBody(a.rr.Reserved.RunID, invErr),
			})
		}
	}
}

// firstBadAttempt returns the first (lens order) attempt matching pred, in
// idx order, so the table's several "any lens X" rows and their own "lens
// <l>: <reason>" text all name the same lens deterministically when more
// than one matches in the same round.
func firstBadAttempt(attempts []lensAttempt, pred func(lensAttempt) bool) (lensAttempt, bool) {
	for i := range attempts {
		if pred(attempts[i]) {
			return attempts[i], true
		}
	}
	return lensAttempt{}, false
}

// execFailureKind names one lens's own non-agent-authored failure for the
// "review round <n> failed" marker's reason (design section 6.2 row 7):
// "invalid output", "exec failed", "timed out", or "configuration error".
// ok is false when a's own error is none of these (an agent-authored error
// document routes through the agent-error row instead, checked first).
func execFailureKind(a lensAttempt) (string, bool) {
	var invErr *runtime.InvalidOutputError
	switch {
	case errors.As(a.err, &invErr): //nolint:modernize // errors.AsType discards its bool via _, which errcheck flags
		return "invalid output", true
	case errors.Is(a.err, runtime.ErrTimeout):
		return "timed out", true
	case isExecFailure(a.err):
		return "exec failed", true
	case errors.Is(a.err, ErrConfig):
		return "configuration error", true
	default:
		return "", false
	}
}

// round is ROUND (design section 6.2): the plan, worktree, branch, and
// clean-tree checks, the diff, the lens set (6.7), the parallel run, and
// the commit-selection table (step 9). n is the round number the decision
// tree already computed; notes carries a retry's own owner notes (fenced),
// empty outside a retry (task 12); priorFailedOrVoid is true when the
// newest "review round <n>" marker already read "failed" or "void" before
// this attempt, gating the two-in-a-row escalations of row 7 and row 8.
// enterRound passes false instead when an owner Retry (markerRetryRequested)
// is newer than that round marker, so a Retry always buys one fresh round
// before the count can escalate again (issue #32).
func (h reviewingHandler) round(ctx context.Context, t store.Ticket, d Deps, n int, notes string, priorFailedOrVoid bool) (store.HandlerCommit, error) {
	plan, _, havePlan, err := d.Store.StoredPlan(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: stored plan: %w", err)
	}
	if !havePlan {
		return reviewEscalation(t, d, noStoredPlanWhat, noStoredPlanWhy, ""), nil
	}

	proj, wt, escalation, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return reviewEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	reports, err := d.Store.BuildReports(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: build reports: %w", err)
	}
	branchShas, err := proj.Orch.BranchCommits(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: branch commits: %w", err)
	}
	if !slices.Equal(recordedShas(reports), branchShas) {
		return withBranch(reviewEscalation(t, d, branchUnrecordedWhat, branchUnrecordedWhy, ""), wt), nil
	}

	changed, err := proj.Orch.ChangedPaths(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: changed paths: %w", err)
	}
	if len(changed) > 0 {
		quoted := make([]string, len(changed))
		for i, c := range changed {
			quoted[i] = strconv.Quote(c.Path)
		}
		tried := strings.Join(quoted, ", ")
		return withBranch(reviewEscalation(t, d, treeDirtyBeforeReviewWhat, treeDirtyBeforeReviewWhy, tried), wt), nil
	}

	sha, err := proj.Orch.HeadSHA(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: head sha: %w", err)
	}
	diff, err := proj.Orch.Diff(ctx, wt, sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: diff: %w", err)
	}
	if strings.TrimSpace(diff) == "" {
		return withBranch(reviewEscalation(t, d, noDiffWhat, noDiffWhy, ""), wt), nil
	}
	idx := orchestrator.ParseDiff(diff)

	lenses, err := h.lensesForRound(ctx, t, d, wt, n, sha)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	planXML, err := planXMLFor(plan)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: plan xml: %w", err)
	}
	jobCfg := d.Machine.Jobs[jobReviewName]
	jobPromptText, err := readAsset(jobCfg.Prompt.Single)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: %w", err)
	}
	schemas, err := renderSchemas(response.JobReview, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: %w", err)
	}
	ticketText, err := specFor(ctx, d, t)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: %w", err)
	}
	var extra []prompt.NamedInput
	if notes != "" {
		extra = append(extra, prompt.Notes(notes))
	}

	attempts, lensesErr := runLensesParallel(ctx, d, t, lenses, schemas, func(lens response.Lens) (store.SessionUpsert, runtime.RunRequest, func(runResult) *store.SessionUpsert) {
		codeSection, csErr := lensCodeSection(lens)
		if csErr != nil {
			return store.SessionUpsert{Job: jobReviewName, Runtime: jobCfg.Runtime}, runtime.RunRequest{}, freshSessionRecord
		}
		in, buildErr := prompt.ForReview(jobPromptText, string(lens), sha, codeSection, ticketText, planXML, diff, extra)
		if buildErr != nil {
			return store.SessionUpsert{Job: jobReviewName, Runtime: jobCfg.Runtime}, runtime.RunRequest{}, freshSessionRecord
		}
		in.Schemas = schemas
		req := runtime.RunRequest{
			Job: response.JobReview, Label: fmt.Sprintf("%d-%s", n, lens), WorkDir: wt.Dir(),
			Prompt: prompt.Assemble(in),
		}
		return store.SessionUpsert{Job: jobReviewName, Runtime: jobCfg.Runtime}, req, freshSessionRecord
	})
	if lensesErr != nil {
		return store.HandlerCommit{}, lensesErr
	}

	return h.roundCommit(ctx, t, d, proj, wt, n, sha, idx, attempts, priorFailedOrVoid)
}

// lensCodeSection reads lens's own prompt file and returns its "## In code"
// section (design section 6.2 step 6, 12.1).
func lensCodeSection(lens response.Lens) (string, error) {
	text, err := readAsset("prompts/lenses/" + string(lens) + ".md")
	if err != nil {
		return "", fmt.Errorf("job: reviewing: %w", err)
	}
	return prompt.CodeLensSection(text)
}

// lensesForRound is design section 6.7's own lens set: every lens for round
// 1; for round n >= 2, selectLenses over round n-1's own rows (newest per
// id, as selectLenses' own doc comment requires) and the paths that changed
// between its own frozen sha and the current one. Round n-1's own frozen
// sha is read from any one of its own finding rows (kept or held: every row
// of a round carries that round's own SHA); a round with none at all (every
// lens clean with zero findings) has nothing to compare against, so the
// full lens set runs again rather than guessing. The dedup matters once a
// round's own id has more than one row: TRIAGE (6.5) appends a decided row
// over ROUND's own undecided one, and a withdrawn discuss leaves the
// original "discuss" row as an id's own newest with no accept ever
// following it. Without the dedup, every one of an id's own rows is read
// independently, which happens to land on the same lens set today (an id
// never carries two rows both decided accept), but that is selectLenses'
// own invariant to rely on, not lensesForRound's to assume twice.
func (h reviewingHandler) lensesForRound(ctx context.Context, t store.Ticket, d Deps, wt orchestrator.Worktree, n int, sha string) ([]response.Lens, error) {
	all := reviewLenses(d)
	if n <= 1 {
		return all, nil
	}

	findings, err := d.Store.Findings(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("job: reviewing: findings: %w", err)
	}
	prevRound := n - 1
	var prevRows []response.FindingArtifact
	prevSHA := ""
	for _, row := range newestFindingRowPerID(findings) {
		f := row.Finding
		if f.Round != prevRound {
			continue
		}
		prevSHA = f.SHA
		if !f.Held {
			prevRows = append(prevRows, f)
		}
	}
	if prevSHA == "" {
		return all, nil
	}

	proj, ok := d.Projects[t.ProjectID]
	if !ok {
		return nil, ErrConfig
	}
	changedFiles, err := proj.Orch.ChangedFilesBetween(ctx, wt, prevSHA, sha)
	if err != nil {
		return nil, fmt.Errorf("job: reviewing: changed files between: %w", err)
	}
	return selectLenses(n, all, prevRows, changedFiles), nil
}

// roundCommit is ROUND's own commit-selection table (design section 6.2
// step 9): the first matching row wins. resolveIDs is always nil here: a
// fresh ROUND entry (enterRound) or a same-round retry after "failed" or
// "void" never carries an answered round of its own to resolve; CONTINUE's
// own commit is continueCommit, below, which does carry one.
func (h reviewingHandler) roundCommit(
	ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree,
	n int, sha string, idx orchestrator.DiffIndex, attempts []lensAttempt, priorFailedOrVoid bool,
) (store.HandlerCommit, error) {
	return h.tableCommit(ctx, t, d, proj, wt, n, sha, idx, attempts, priorFailedOrVoid, nil, nil, nil)
}

// tableCommit is roundCommit and continueRound's own shared commit-selection
// table (design section 6.2 step 9, 6.2a step 3): CONTINUE feeds it the
// resumed attempts, resolveIDs (every consumed round's own question ids),
// its own held rows folded into a clean success (heldFindings), and the
// lenses the original round already found done (priorDone, merged into a
// re-ask's own "done" list); ROUND feeds it a fresh attempt set, a nil
// resolveIDs, no held findings, and no prior-done lenses.
func (h reviewingHandler) tableCommit(
	ctx context.Context, t store.Ticket, d Deps, proj Project, wt orchestrator.Worktree,
	n int, sha string, idx orchestrator.DiffIndex, attempts []lensAttempt, priorFailedOrVoid bool,
	resolveIDs []int64, heldFindings []response.Finding, priorDone []response.Lens,
) (store.HandlerCommit, error) {
	if ctx.Err() != nil {
		if at, ok := firstBadAttempt(attempts, func(a lensAttempt) bool { return errors.Is(a.err, runtime.ErrCanceled) }); ok {
			return store.HandlerCommit{}, at.err
		}
		return store.HandlerCommit{}, ctx.Err()
	}

	if _, ok := firstBadAttempt(attempts, func(a lensAttempt) bool { return errors.Is(a.err, ErrBudget) }); ok {
		c := budgetEscalationCommit(t, d, resolveIDs)
		applyAttempts(&c, t.ID, attempts)
		return c, nil
	}

	if _, ok := firstBadAttempt(attempts, func(a lensAttempt) bool { return errors.Is(a.err, ErrSandbox) }); ok {
		return sandboxEscalationCommit(t, d, resolveIDs, response.EscalationOriginReview, d.Sandboxes.ReadOnly.Reason()), nil
	}

	if at, ok := firstBadAttempt(attempts, func(a lensAttempt) bool { return errors.Is(a.err, store.ErrClaimLost) }); ok {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: lens %s: %w", at.lens, at.err)
	}

	if at, ok := firstBadAttempt(attempts, func(a lensAttempt) bool { return errors.Is(a.err, ErrConfig) }); ok {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: lens %s: %w", at.lens, at.err)
	}

	if at, ok := firstBadAttempt(attempts, func(a lensAttempt) bool {
		if a.err != nil {
			return false
		}
		_, isErr := a.rr.Res.Response.(*response.ErrorResponse)
		return isErr
	}); ok {
		errResp, ok := at.rr.Res.Response.(*response.ErrorResponse)
		if !ok {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: lens %s: expected an error document", at.lens)
		}
		c := escalationCommit(t, d, &at.rr.Reserved.RunID, &at.rr.Reserved.SessionID,
			string(errResp.Error.Code), errResp.Error.What, errResp.Error.Why, errResp.Error.Tried, response.EscalationOriginReview)
		applyAttempts(&c, t.ID, attempts)
		c.ResolveQuestions = resolveIDs
		c.Messages = append(c.Messages, reviewRoundFailedMarker(t.ID, n, fmt.Sprintf(lensAgentErrorWhy, at.lens)))
		slog.Warn("escalation written", "ticket_id", t.ID, "session_id", at.rr.Reserved.SessionID, "run_id", at.rr.Reserved.RunID,
			"code", errResp.Error.Code, "origin", string(response.EscalationOriginReview))
		return c, nil
	}

	// A lens whose first turn was invalid and whose same-tick retry (issue
	// #32, runLensesParallel) is itself invalid escalates response_invalid
	// now, naming the lens and the retry's own validator error: the
	// response_invalid: two-in-a-row rule below (firstExecFailure's row)
	// only ever sees one invalid turn per lens per round, since a lens's
	// second consecutive invalid turn is now always this row instead.
	if at, ok := firstBadAttempt(attempts, func(a lensAttempt) bool {
		var invErr *runtime.InvalidOutputError
		return a.firstTry != nil && errors.As(a.err, &invErr) //nolint:modernize // see execFailureKind
	}); ok {
		var invErr *runtime.InvalidOutputError
		errors.As(at.err, &invErr) //nolint:errcheck,modernize // the predicate above already matched
		// Why carries only the closed reason; the validator's own detail can
		// quote model text and stays on the run's own invalid marker.
		why := fmt.Sprintf("%s; validator errors in response invalid run %d", invErr.Reason, at.rr.Reserved.RunID)
		code := string(response.EscalationCodeResponseInvalid)
		c := escalationCommit(t, d, &at.rr.Reserved.RunID, &at.rr.Reserved.SessionID,
			code, fmt.Sprintf(lensInvalidTwiceWhat, at.lens), why, "", response.EscalationOriginReview)
		applyAttempts(&c, t.ID, attempts)
		c.ResolveQuestions = resolveIDs
		c.Messages = append(c.Messages, reviewRoundFailedMarker(t.ID, n, fmt.Sprintf("lens %s: invalid output", at.lens)))
		slog.Warn("escalation written", "ticket_id", t.ID, "session_id", at.rr.Reserved.SessionID, "run_id", at.rr.Reserved.RunID,
			"code", code, "origin", string(response.EscalationOriginReview))
		return c, nil
	}

	if at, kind, ok := firstExecFailure(attempts); ok {
		reason := fmt.Sprintf("lens %s: %s", at.lens, kind)
		c := baseCommit(t, d)
		applyAttempts(&c, t.ID, attempts)
		c.ResolveQuestions = resolveIDs
		c.Messages = append(c.Messages, reviewRoundFailedMarker(t.ID, n, reason))
		if priorFailedOrVoid {
			code := string(response.EscalationCodeRuntimeExecFailed)
			if kind == "invalid output" {
				code = string(response.EscalationCodeResponseInvalid)
			}
			slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil, "code", code, "origin", string(response.EscalationOriginReview))
			c.Escalation = &store.EscalationCommit{
				Body: code + ": " + lensFailedTwiceWhat,
				Payload: response.EscalationPayload{
					Code: code, What: lensFailedTwiceWhat, Why: reason,
					Options: escalationOptions, Origin: string(response.EscalationOriginReview),
				},
			}
			waiting := waitingFlagQuestions
			c.Waiting = &waiting
		}
		return c, nil
	}

	sha2, err := proj.Orch.HeadSHA(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: head sha: %w", err)
	}
	if sha2 != sha {
		reason := fmt.Sprintf("head moved from %s to %s", sha, sha2)
		c := baseCommit(t, d)
		applyAttempts(&c, t.ID, attempts)
		c.ResolveQuestions = resolveIDs
		c.Messages = append(c.Messages, store.Message{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("review round %d void\n%s", n, reason),
		})
		if priorFailedOrVoid {
			slog.Warn("escalation written", "ticket_id", t.ID, "session_id", nil, "run_id", nil,
				"code", string(response.EscalationCodeEnvironment), "origin", string(response.EscalationOriginReview))
			c.Escalation = &store.EscalationCommit{
				Body: string(response.EscalationCodeEnvironment) + ": " + headMovedTwiceWhat,
				Payload: response.EscalationPayload{
					Code: string(response.EscalationCodeEnvironment), What: headMovedTwiceWhat, Why: reason,
					Options: escalationOptions, Origin: string(response.EscalationOriginReview),
				},
			}
			waiting := waitingFlagQuestions
			c.Waiting = &waiting
		}
		return c, nil
	}

	anyQuestion := false
	for i := range attempts {
		if _, ok := attempts[i].rr.Res.Response.(*response.QuestionResponse); ok {
			anyQuestion = true
			break
		}
	}
	if anyQuestion {
		return h.askedCommit(t, d, n, sha, attempts, resolveIDs, priorDone, len(heldFindings))
	}

	return h.successCommit(ctx, t, d, n, sha, idx, attempts, resolveIDs, heldFindings)
}

// firstExecFailure returns the first (lens order) attempt whose own error
// is an invalid output, an exec failure, a timeout, or a configuration
// error -- row 7 of design section 6.2's own table, checked after the
// agent-authored error row so a parsed *response.ErrorResponse never
// double-matches here.
func firstExecFailure(attempts []lensAttempt) (lensAttempt, string, bool) {
	for i := range attempts {
		if attempts[i].err == nil {
			continue
		}
		if kind, ok := execFailureKind(attempts[i]); ok {
			return attempts[i], kind, true
		}
	}
	return lensAttempt{}, "", false
}

// reviewRoundFailedMarker is the "review round <n> failed" marker every
// failure row of design section 6.2's table writes, reason on its own
// second line.
func reviewRoundFailedMarker(ticketID int64, n int, reason string) store.Message {
	return store.Message{
		TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("review round %d failed\n%s", n, reason),
	}
}

// fixRequestOrLoopsExhausted is FIXREQ's own body (design section 6.8),
// shared by fixreq and successCommit's inline FIXREQ: under
// jobs.review.max_loops, it returns a "fix requested findings" marker (msg
// non-nil) the caller adds to its own commit; at or past the gate, msg is
// nil and the caller decides between acceptAtCap and a loops_exhausted
// escalation. k and maxLoops are returned either way.
func fixRequestOrLoopsExhausted(ctx context.Context, t store.Ticket, d Deps, accepted []response.FindingArtifact) (msg *store.Message, k, maxLoops int, err error) {
	allReqs, err := d.Store.MarkersWithPrefix(ctx, t.ID, "fix requested findings")
	if err != nil {
		return nil, 0, 0, fmt.Errorf("job: reviewing: fix requested findings markers: %w", err)
	}
	k = len(allReqs)
	maxLoops = d.Machine.Jobs[jobReviewName].MaxLoops
	if k >= maxLoops {
		return nil, k, maxLoops, nil
	}

	m, _, err := findingsFixRequest(ctx, t, d, accepted)
	if err != nil {
		return nil, 0, 0, err
	}
	return &m, k, maxLoops, nil
}

// findingsFixRequest builds the one fix request message FIXREQ sends for
// accepted, against the ticket's latest run (design section 6.8): both the
// under-cap path (fixRequestOrLoopsExhausted) and the owner-pick path past
// the cap (ownerAcceptAtCap) open the same kind of fix request, so they
// share this build.
func findingsFixRequest(ctx context.Context, t store.Ticket, d Deps, accepted []response.FindingArtifact) (store.Message, int64, error) {
	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.Message{}, 0, fmt.Errorf("job: reviewing: max run id: %w", err)
	}
	m, err := fixRequestMessage(t, FixKindFindings, renderFixFindings(accepted), maxRunID)
	if err != nil {
		return store.Message{}, 0, fmt.Errorf("job: reviewing: fix request message: %w", err)
	}
	return m, maxRunID, nil
}

// reasonReviewAcceptedAtCap is the reviewing → judging state reason when
// the FIXREQ gate is reached with only at-or-below-floor findings left
// (issue #68), distinct from reasonReviewClean so the state history shows
// that findings were let through unfixed.
const reasonReviewAcceptedAtCap = "review findings accepted at loop cap"

// acceptedAtCapTextRunes caps each finding's one-line text in acceptAtCap's
// message, "..." appended when cut.
const acceptedAtCapTextRunes = 200

// allAtOrBelowFloor reports whether every row's severity is at or below
// floor, by the same Rank comparison splitByFloor uses. An empty rows is
// true.
func allAtOrBelowFloor(rows []response.FindingArtifact, floor response.Severity) bool {
	for i := range rows {
		if rows[i].Severity.Rank() > floor.Rank() {
			return false
		}
	}
	return true
}

// acceptAtCap is the FIXREQ gate's own outcome when every accepted finding
// is at or below the floor (issue #68, mirroring maybeResumeFloorFindings'
// cap gate in planning.go): c, the caller's own commit so far, moves to
// judging with one message for the owner listing, in id order, the findings
// let through unfixed, one "- <id> <severity> <location> <text>" line
// each. Location and Text are lens output, so both are whitespace-collapsed:
// no finding can break its own line or forge another. No escalation, no
// Waiting.
func acceptAtCap(c store.HandlerCommit, t store.Ticket, d Deps, k, maxLoops int, accepted []response.FindingArtifact) store.HandlerCommit {
	lines := make([]string, 0, 2+len(accepted))
	lines = append(lines,
		"Zing accepted "+orchestrator.CountNoun(len(accepted), "finding", "findings")+" at the review fix loop cap",
		fmt.Sprintf("Review reached max_loops (%d) after %d fix runs, and every finding left is at or below the floor (%s). "+
			"Zing moved the ticket to judging without fixing these:", maxLoops, k, d.Floor),
	)
	sorted := sortByID(accepted)
	for i := range sorted {
		f := &sorted[i]
		location := collapseWhitespace(f.Location)
		text := collapseWhitespace(f.Text)
		if cut := cutRunes(text, acceptedAtCapTextRunes); cut != text {
			text = cut + "..."
		}
		lines = append(lines, fmt.Sprintf("- %s %s %s %s", f.ID, f.Severity, location, text))
	}
	slog.Info("review findings accepted at loop cap", "ticket_id", t.ID, "fix_runs", k, "findings", len(accepted))
	c.Messages = append(c.Messages, store.Message{TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem, Body: strings.Join(lines, "\n")})
	c.Next, c.Reason = stateJudging, reasonReviewAcceptedAtCap
	return c
}

// ownerAcceptAtCap is the FIXREQ gate's own outcome when the gate is at or
// past max_loops, the accepted list has an above-floor finding, and at
// least one accepted row's OwnerPicked is true: the owner already decided
// to fix it on the review question, so this opens the fix request anyway,
// bypassing the gate, instead of asking loops_exhausted the same thing
// again (ticket 55).
func ownerAcceptAtCap(ctx context.Context, t store.Ticket, d Deps, k, maxLoops int, accepted []response.FindingArtifact) (store.HandlerCommit, error) {
	m, maxRunID, err := findingsFixRequest(ctx, t, d, accepted)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: owner accept at cap: %w", err)
	}
	sorted := sortByID(accepted)
	ids := make([]string, len(sorted))
	for i := range sorted {
		ids[i] = sorted[i].ID
	}
	slog.Info("review owner accept past loop cap starts fix", "ticket_id", t.ID, "fix_runs", k, "max_loops", maxLoops,
		"after_run_id", maxRunID, "finding_ids", strings.Join(ids, ","))
	c := baseCommit(t, d)
	c.Messages = []store.Message{m}
	return c, nil
}

// waitingFlagReview is the review question's own waiting_on value (design
// section 6.4), distinct from waitingFlagQuestions: a review question is
// triaged (task 11), not answered the generic way.
const waitingFlagReview = "review"

// reviewItemText is Item.Text's own format for a review question (design
// section 6.4): "[<severity>] <lenses joined by +> <path>:<line> <text>
// Fix: <fix>", whitespace collapsed, cut to 600 runes with "..." appended
// when cut.
func reviewItemText(row response.FindingArtifact) string {
	lensNames := make([]string, len(row.Lenses))
	for i, l := range row.Lenses {
		lensNames[i] = string(l)
	}
	text := fmt.Sprintf("[%s] %s %s %s Fix: %s", row.Severity, strings.Join(lensNames, "+"), row.Location, row.Text, row.Fix)
	text = collapseWhitespace(text)
	if cut := cutRunes(text, 600); cut != text {
		return cut + "..."
	}
	return text
}

// reviewQuestionMessage is the review question (design section 6.4): runID
// is the round's first run in lens order (or the discuss run, task 11).
func reviewQuestionMessage(t store.Ticket, d Deps, n int, runID int64, above []response.FindingArtifact) (store.Message, error) {
	items := make([]response.Item, len(above))
	for i := range above {
		items[i] = response.Item{Ref: above[i].ID, Text: reviewItemText(above[i])}
	}
	payload, err := json.Marshal(response.QuestionPayload{
		Kind: response.QuestionKindReview, State: response.QuestionStateOpen,
		Recommended: "Accept each finding unless it is wrong",
		// Options is a bare JSON array in the schema (no omitempty), so a nil
		// slice -- Go's zero value, and every review question's own value:
		// section 6.4 gives it none -- must still marshal as [], not null.
		Options: []response.Option{},
		Items:   items,
	})
	if err != nil {
		return store.Message{}, fmt.Errorf("job: reviewing: marshal review question payload: %w", err)
	}
	body := fmt.Sprintf(
		"Triage review findings\n\nRound %d of the code review found %s above the floor (%s). "+
			"Accept a finding to fix it. Drop it to leave the code as it is. Discuss it to send your reply on this question to the lens that raised it.",
		n, orchestrator.CountNoun(len(above), "finding", "findings"), string(d.Floor),
	)
	state := questionStateOpen
	return store.Message{
		TicketID: t.ID, RunID: &runID, Type: msgTypeQuestion, Author: authorZing,
		State: &state, Body: body, Payload: payload,
	}, nil
}

// successCommit is ROUND's own success commit (design section 6.2 step 9's
// last row) and CONTINUE's own "all ok" outcome (6.2a step 3): extra is the
// round's own held findings (nil for a fresh ROUND; CONTINUE's held rows,
// converted back to response.Finding, for a completed continuation).
// Findings: survivors := FilterFindings(all, idx), then DedupFindings, then
// ids r<n>f<k>. At-or-below d.Floor get Decision accept. Then the first
// matching row: findings above the floor post the review question (6.4);
// else one or more survivors call FIXREQ (6.8); else Next = judging.
func (h reviewingHandler) successCommit(
	ctx context.Context, t store.Ticket, d Deps, n int, sha string, idx orchestrator.DiffIndex,
	attempts []lensAttempt, resolveIDs []int64, extra []response.Finding,
) (store.HandlerCommit, error) {
	runIDByLens := make(map[response.Lens]int64, len(attempts))
	all := append([]response.Finding(nil), extra...)
	for i := range attempts {
		fr, ok := attempts[i].rr.Res.Response.(*response.FindingsResponse)
		if !ok {
			continue
		}
		runIDByLens[attempts[i].lens] = attempts[i].rr.Reserved.RunID
		all = append(all, fr.Findings...)
	}

	survivors := FilterFindings(all, idx)
	merged := DedupFindings(survivors, reviewLenses(d))
	for i := range merged {
		merged[i].ID = fmt.Sprintf("r%df%d", n, i+1)
		merged[i].Round = n
		merged[i].SHA = sha
	}
	atOrBelow, above := splitByFloor(merged, d.Floor)
	stored := sortByID(append(append([]response.FindingArtifact{}, atOrBelow...), above...))

	c := baseCommit(t, d)
	applyAttempts(&c, t.ID, attempts)
	c.ResolveQuestions = resolveIDs

	lensList := make([]string, len(attempts))
	for i := range attempts {
		lensList[i] = string(attempts[i].lens)
	}
	c.Messages = append(c.Messages, store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("review round %d done sha %s lenses %s\nkept %d dropped %d merged %d",
			n, sha, strings.Join(lensList, ","), len(merged), len(all)-len(survivors), len(survivors)-len(merged)),
	})

	artifacts := make([]store.Artifact, len(stored))
	for i := range stored {
		payload, err := json.Marshal(stored[i])
		if err != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: marshal finding artifact %s: %w", stored[i].ID, err)
		}
		runID := runIDByLens[stored[i].Lens]
		artifacts[i] = store.Artifact{Type: artifactTypeFinding, RunID: &runID, Payload: payload}
	}
	c.Artifacts = artifacts

	switch {
	case len(above) > 0:
		runID := attempts[0].rr.Reserved.RunID
		qMsg, qErr := reviewQuestionMessage(t, d, n, runID, above)
		if qErr != nil {
			return store.HandlerCommit{}, qErr
		}
		c.Messages = append(c.Messages, qMsg)
		waiting := waitingFlagReview
		c.Waiting = &waiting
		return c, nil

	case len(atOrBelow) > 0:
		msg, k, maxLoops, fixErr := fixRequestOrLoopsExhausted(ctx, t, d, atOrBelow)
		if fixErr != nil {
			return store.HandlerCommit{}, fixErr
		}
		if msg == nil {
			// Every row here is at or below the floor by construction
			// (splitByFloor), so the gate always accepts them (issue #68).
			return acceptAtCap(c, t, d, k, maxLoops, atOrBelow), nil
		}
		c.Messages = append(c.Messages, *msg)
		return c, nil

	default:
		c.Next, c.Reason = stateJudging, reasonReviewClean
		return c, nil
	}
}

// askedCommit is ASKED (design section 6.2a): one or more lenses asked and
// every other lens returned ok. priorDone is empty for a fresh round's own
// first ask; a re-ask (CONTINUE, when a resumed lens asks again) passes the
// lenses the original round already found done, merged into this commit's
// own "done" list, in lens order; heldSoFar is the count of held rows the
// round already carries, so a re-ask's own new held rows continue that
// same per-round k sequence rather than restarting it.
func (h reviewingHandler) askedCommit(
	t store.Ticket, d Deps, n int, sha string, attempts []lensAttempt, resolveIDs []int64,
	priorDone []response.Lens, heldSoFar int,
) (store.HandlerCommit, error) {
	c := baseCommit(t, d)
	applyAttempts(&c, t.ID, attempts)
	c.ResolveQuestions = resolveIDs

	var askingRunIDs []int64
	doneSet := make(map[response.Lens]bool, len(attempts)+len(priorDone))
	for _, l := range priorDone {
		doneSet[l] = true
	}
	var artifacts []store.Artifact
	k := heldSoFar

	for i := range attempts {
		a := &attempts[i]
		switch resp := a.rr.Res.Response.(type) {
		case *response.QuestionResponse:
			runID := a.rr.Reserved.RunID
			askingRunIDs = append(askingRunIDs, runID)
			msgs, err := questionMessagesFor(t.ID, resp.Questions)
			if err != nil {
				return store.HandlerCommit{}, err
			}
			for j := range msgs {
				msgs[j].RunID = &runID
			}
			c.Messages = append(c.Messages, msgs...)

		case *response.FindingsResponse:
			runID := a.rr.Reserved.RunID
			doneSet[a.lens] = true
			for _, f := range resp.Findings {
				k++
				row := response.FindingArtifact{
					Lens: f.Lens, Severity: f.Severity, Location: f.Location, Text: f.Text, Fix: f.Fix, PlanRef: f.PlanRef,
					Held: true, ID: fmt.Sprintf("r%dh%d", n, k), Round: n, SHA: sha, Lenses: []response.Lens{f.Lens},
				}
				payload, err := json.Marshal(row)
				if err != nil {
					return store.HandlerCommit{}, fmt.Errorf("job: reviewing: marshal held finding %s: %w", row.ID, err)
				}
				artifacts = append(artifacts, store.Artifact{Type: artifactTypeFinding, RunID: &runID, Payload: payload})
			}
		}
	}
	c.Artifacts = artifacts

	slices.Sort(askingRunIDs)
	runsStrs := make([]string, len(askingRunIDs))
	for i, id := range askingRunIDs {
		runsStrs[i] = strconv.FormatInt(id, 10)
	}
	var doneLensNames []string
	for _, l := range reviewLenses(d) {
		if doneSet[l] {
			doneLensNames = append(doneLensNames, string(l))
		}
	}
	doneStr := "-"
	if len(doneLensNames) > 0 {
		doneStr = strings.Join(doneLensNames, ",")
	}
	c.Messages = append(c.Messages, store.Message{
		TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
		Body: fmt.Sprintf("review round %d asked\nruns %s\ndone %s", n, strings.Join(runsStrs, ","), doneStr),
	})

	waiting := waitingFlagQuestions
	c.Waiting = &waiting
	return c, nil
}

// ---- CONTINUE (design section 6.2a) ---------------------------------------

// parseAskedMarker parses M, the newest "review round <n> asked" marker
// (design section 5.1): line 2 "runs <rid1,...>" (ascending) and line 3
// "done <lens1,...>" ("-" for none).
func parseAskedMarker(m store.MessageRow) (n int, runIDs []int64, doneLenses []response.Lens, err error) {
	lines := strings.Split(m.Body, "\n")
	if len(lines) < 3 {
		return 0, nil, nil, fmt.Errorf("job: reviewing: asked marker %d: malformed (fewer than 3 lines)", m.ID)
	}
	match := reviewRoundAskedLine.FindStringSubmatch(lines[0])
	if match == nil {
		return 0, nil, nil, fmt.Errorf("job: reviewing: asked marker %d: malformed first line %q", m.ID, lines[0])
	}
	n, err = strconv.Atoi(match[1])
	if err != nil {
		return 0, nil, nil, fmt.Errorf("job: reviewing: asked marker %d: round number: %w", m.ID, err)
	}

	runsField, ok := strings.CutPrefix(lines[1], "runs ")
	if !ok {
		return 0, nil, nil, fmt.Errorf("job: reviewing: asked marker %d: malformed line 2 %q", m.ID, lines[1])
	}
	for s := range strings.SplitSeq(runsField, ",") {
		id, idErr := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if idErr != nil {
			return 0, nil, nil, fmt.Errorf("job: reviewing: asked marker %d: run id %q: %w", m.ID, s, idErr)
		}
		runIDs = append(runIDs, id)
	}

	doneField, ok := strings.CutPrefix(lines[2], "done ")
	if !ok {
		return 0, nil, nil, fmt.Errorf("job: reviewing: asked marker %d: malformed line 3 %q", m.ID, lines[2])
	}
	if doneField != "-" {
		for s := range strings.SplitSeq(doneField, ",") {
			doneLenses = append(doneLenses, response.Lens(strings.TrimSpace(s)))
		}
	}
	return n, runIDs, doneLenses, nil
}

// heldFindingsForRound returns round n's own held findings, converted back
// to response.Finding (the shape a fresh lens result carries), and the
// frozen sha every row of the round shares. ok is false when the round
// carries no held row at all (every "done" lens returned zero findings):
// heldFindingsForRound has nothing to compare, so continueRound treats the
// round as unchanged rather than guessing.
func heldFindingsForRound(ctx context.Context, d Deps, ticketID int64, n int) (findings []response.Finding, sha string, ok bool, err error) {
	rows, err := d.Store.Findings(ctx, ticketID)
	if err != nil {
		return nil, "", false, fmt.Errorf("job: reviewing: findings: %w", err)
	}
	for i := range rows {
		f := rows[i].Finding
		if f.Round != n || !f.Held {
			continue
		}
		sha = f.SHA
		ok = true
		findings = append(findings, response.Finding{
			Lens: f.Lens, Severity: f.Severity, Location: f.Location, Text: f.Text, Fix: f.Fix, PlanRef: f.PlanRef,
		})
	}
	return findings, sha, ok, nil
}

// reviewCapResumesEscalation is capResumesEscalation (planning.go) plus
// this file's own "escalation written" log (design section 6.2a step 2):
// an exhausted asking lens's own session, escalated once; the round it
// belongs to stays answered (no ResolveQuestions), so a retry (task 12, 5.6)
// can still read it.
func reviewCapResumesEscalation(t store.Ticket, d Deps, sessionID int64) store.HandlerCommit {
	slog.Warn("escalation written", "ticket_id", t.ID, "session_id", sessionID, "run_id", nil,
		"code", string(response.EscalationCodeResumesExhausted), "origin", string(response.EscalationOriginCapResumes))
	return capResumesEscalation(t, d, sessionID)
}

// continueRound is CONTINUE (design section 6.2a): once every question of
// the newest "review round <n> asked" marker M is answered, resume each
// asking lens's own session in parallel, under the same semaphore ROUND
// uses. rounds is reviewingHandler.Run's own AnsweredRounds read, reused
// here rather than read twice.
func (h reviewingHandler) continueRound(ctx context.Context, t store.Ticket, d Deps, rounds []store.Round) (store.HandlerCommit, error) {
	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, reviewRoundMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: review round markers: %w", err)
	}
	if len(markers) == 0 {
		return store.HandlerCommit{}, errors.New("job: reviewing: continue: no review round marker")
	}
	newest := markers[len(markers)-1]
	if !reviewRoundAskedLine.MatchString(strings.SplitN(newest.Body, "\n", 2)[0]) {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: newest review round marker is not asked: %q", newest.Body)
	}
	n, runIDs, priorDone, err := parseAskedMarker(newest)
	if err != nil {
		return store.HandlerCommit{}, err
	}

	byRunID := make(map[int64]store.Round, len(rounds))
	for _, r := range rounds {
		if r.RunID != nil {
			byRunID[*r.RunID] = r
		}
	}
	answered := make([]store.Round, 0, len(runIDs))
	var resolveIDs []int64
	for _, rid := range runIDs {
		r, ok := byRunID[rid]
		if !ok {
			return store.HandlerCommit{}, ErrNoAction
		}
		answered = append(answered, r)
		resolveIDs = append(resolveIDs, questionIDs(r)...)
	}

	// Step 1 (design section 6.2a): HeadSHA(wt) must equal the round's own
	// sha before the cap gate ever runs, else the round is void regardless
	// of any asking session's own resume state.
	proj, wt, escalation, err := ensureWorktreeOrEscalate(ctx, t, d, func(errText string) store.HandlerCommit {
		return reviewEscalation(t, d, worktreeNotPreparedWhat, worktreeNotPreparedWhy, errText)
	})
	if err != nil {
		return store.HandlerCommit{}, err
	}
	if escalation != nil {
		return *escalation, nil
	}

	headSHA, err := proj.Orch.HeadSHA(ctx, wt)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: head sha: %w", err)
	}
	heldFindings, roundSHA, haveHeld, err := heldFindingsForRound(ctx, d, t.ID, n)
	if err != nil {
		return store.HandlerCommit{}, err
	}
	sha := headSHA
	if haveHeld {
		sha = roundSHA
	}
	if haveHeld && headSHA != roundSHA {
		c := baseCommit(t, d)
		c.ResolveQuestions = resolveIDs
		c.Messages = []store.Message{{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("review round %d void\nhead moved from %s to %s", n, roundSHA, headSHA),
		}}
		return c, nil
	}

	// Step 2: for each asking run, the cap gate; an exhausted session
	// escalates once with no run started for any lens, unless its newest
	// run was itself cut short (resumeCharge, design D5, section 7.4),
	// which bypasses the cap for that one asker and resumes it free.
	maxResumes := d.Machine.Jobs[jobReviewName].MaxResumes
	type asker struct {
		lens        response.Lens
		sess        store.Session
		answers     string
		bump        bool
		interrupted bool
	}
	askers := make([]asker, 0, len(answered))
	for _, r := range answered {
		run, runErr := d.Store.RunByID(ctx, *r.RunID)
		if runErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: run by id: %w", runErr)
		}
		if run.Lens == nil {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: run %d carries no lens", *r.RunID)
		}
		sess, state, sessErr := d.Store.SessionByID(ctx, *r.SessionID, maxResumes)
		if sessErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: session by id: %w", sessErr)
		}
		newestRun, foundRun, newestErr := d.Store.SessionNewestRun(ctx, sess.ID)
		if newestErr != nil {
			return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: newest run: %w", newestErr)
		}
		bump, gate := true, true
		if foundRun {
			bump, gate = resumeCharge(newestRun)
		}
		if state == store.SessionExhausted && gate {
			has, hasErr := d.Store.HasEscalation(ctx, t.ID, string(response.EscalationOriginCapResumes), sess.ID)
			if hasErr != nil {
				return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: has escalation: %w", hasErr)
			}
			if has {
				return store.HandlerCommit{}, ErrNoAction
			}
			return reviewCapResumesEscalation(t, d, sess.ID), nil
		}
		answers, ansErr := renderRoundAnswers(r)
		if ansErr != nil {
			return store.HandlerCommit{}, ansErr
		}
		askers = append(askers, asker{
			lens: response.Lens(*run.Lens), sess: sess, answers: answers,
			bump: bump, interrupted: foundRun && newestRun.Interrupted,
		})
	}

	diff, err := proj.Orch.Diff(ctx, wt, sha)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: continue: diff: %w", err)
	}
	idx := orchestrator.ParseDiff(diff)

	schemas, err := renderSchemas(response.JobReview, response.OutcomeOk)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: %w", err)
	}

	lenses := make([]response.Lens, len(askers))
	byLens := make(map[response.Lens]asker, len(askers))
	for i := range askers {
		lenses[i] = askers[i].lens
		byLens[askers[i].lens] = askers[i]
	}

	attempts, lensesErr := runLensesParallel(ctx, d, t, lenses, schemas, func(lens response.Lens) (store.SessionUpsert, runtime.RunRequest, func(runResult) *store.SessionUpsert) {
		a := byLens[lens]
		inputs := []prompt.NamedInput{prompt.Answers(a.answers)}
		if a.interrupted {
			inputs = append(inputs, prompt.NamedInput{Label: labelInterrupted, Text: interruptedResumeText, Untrusted: false})
		}
		in := prompt.ForReviewResume(inputs)
		in.Schemas = schemas
		req := runtime.RunRequest{
			Job: response.JobReview, Label: fmt.Sprintf("%d-%s", n, lens), WorkDir: wt.Dir(),
			SessionID: derefString(a.sess.ExternalID), Prompt: prompt.Assemble(in),
		}
		su := store.SessionUpsert{ID: &a.sess.ID, BumpResumes: a.bump}
		sessionID := a.sess.ID
		return su, req, func(rr runResult) *store.SessionUpsert { return resumeSessionRecord(sessionID, rr) }
	})
	if lensesErr != nil {
		return store.HandlerCommit{}, lensesErr
	}

	return h.tableCommit(ctx, t, d, proj, wt, n, sha, idx, attempts, false, resolveIDs, heldFindings, priorDone)
}

// ---- review escalation retries (design section 5.6) -----------------------

// reviewCapResumesContinueReason is CONTINUE's own cap_resumes retry marker
// reason (design section 5.6's "cap_resumes, exhausted session of job
// review, from CONTINUE" row), byte for byte from the plan.
const reviewCapResumesContinueReason = "a lens question could not be answered"

// retryReviewLoopsExhausted is design section 5.6's "review, loops_exhausted"
// retry row: a fix request of kind findings, built from the escalation's own
// Tried text (the fix text FIXREQ had already rendered, 6.3) with the
// owner's own notes appended, written straight through fixRequestMessage --
// bypassing fixreq's own max_loops gate entirely, the one request 5.6 says
// to skip it for.
func (h reviewingHandler) retryReviewLoopsExhausted(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, notes, tried string) (store.HandlerCommit, error) {
	text := tried
	if notes != "" {
		text = strings.TrimRight(text, "\n") + "\n\n" + notes
	}
	maxRunID, err := d.Store.MaxRunID(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: loops_exhausted retry: max run id: %w", err)
	}
	msg, msgErr := fixRequestMessage(t, FixKindFindings, text, maxRunID)
	if msgErr != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: reviewing: loops_exhausted retry: fix request message: %w", msgErr)
	}
	c := baseCommit(t, d)
	c.ResolveQuestions = resolveIDs
	c.Messages = []store.Message{msg}
	return c, nil
}

// retryCapResumesReview is design section 5.6's own cap_resumes recovery for
// a job "review" session: the only two places a review session ever
// resumes are DISCUSS (6.6) and CONTINUE (6.2a), so the exhausted session
// belongs to exactly one of them. A session some finding's decision
// "discuss" is still waiting on (no "review discussed <id>" marker of its
// own) is DISCUSS's; otherwise it is CONTINUE's own asking session.
func (h reviewingHandler) retryCapResumesReview(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, notes string, sessionID int64) (store.HandlerCommit, int, error) {
	pending, err := h.sessionPendingDiscuss(ctx, t, d, sessionID)
	if err != nil {
		return store.HandlerCommit{}, 0, err
	}
	if len(pending) > 0 {
		return h.retryCapResumesDiscuss(t, d, resolveIDs, notes, pending)
	}
	return h.retryCapResumesContinue(ctx, t, d, resolveIDs, sessionID)
}

// sessionPendingDiscuss returns sessionID's own still-pending discussed
// findings (design section 6.6's own entry condition: decision discuss, no
// "review discussed <id>" marker of its own), newest row per id, in id
// order: empty when sessionID belongs to no open discuss group, the signal
// retryCapResumesReview reads to tell a DISCUSS session from a CONTINUE one.
func (h reviewingHandler) sessionPendingDiscuss(ctx context.Context, t store.Ticket, d Deps, sessionID int64) ([]store.FindingRow, error) {
	findings, err := d.Store.Findings(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("job: reviewing: cap_resumes retry: findings: %w", err)
	}
	var out []store.FindingRow
	for id, row := range newestFindingRowPerID(findings) {
		f := row.Finding
		if f.Held || f.Decision == nil || *f.Decision != response.FindingDiscuss || row.RunID == nil {
			continue
		}
		_, marked, markErr := d.Store.Marker(ctx, t.ID, reviewDiscussedMarker(id))
		if markErr != nil {
			return nil, fmt.Errorf("job: reviewing: cap_resumes retry: discussed marker %s: %w", id, markErr)
		}
		if marked {
			continue
		}
		run, runErr := d.Store.RunByID(ctx, *row.RunID)
		if runErr != nil {
			return nil, fmt.Errorf("job: reviewing: cap_resumes retry: run by id: %w", runErr)
		}
		if run.SessionID == sessionID {
			out = append(out, *row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Finding.ID < out[j].Finding.ID })
	return out, nil
}

// retryCapResumesDiscuss is design section 5.6's own "cap_resumes, exhausted
// session of job review, from DISCUSS" row: every pending finding of the
// session becomes decision accept, one new row per id (same id, TRIAGE's
// own nil-to-decided shape), the owner's own notes appended to its fix
// text, and the escalation's own round resolves.
func (h reviewingHandler) retryCapResumesDiscuss(t store.Ticket, d Deps, resolveIDs []int64, notes string, pending []store.FindingRow) (store.HandlerCommit, int, error) {
	accept := response.FindingAccept
	artifacts := make([]store.Artifact, len(pending))
	for i := range pending {
		row := &pending[i]
		finding := row.Finding
		finding.Decision = &accept
		finding.OwnerPicked = false // Zing's recovery accept, not the owner's pick on the review question
		if notes != "" {
			finding.Fix = strings.TrimRight(finding.Fix, "\n") + "\n\n" + notes
		}
		payload, marshalErr := json.Marshal(finding)
		if marshalErr != nil {
			return store.HandlerCommit{}, 0, fmt.Errorf("job: reviewing: cap_resumes retry: marshal finding %s: %w", finding.ID, marshalErr)
		}
		artifacts[i] = store.Artifact{Type: artifactTypeFinding, RunID: row.RunID, Payload: payload}
	}
	c := baseCommit(t, d)
	c.ResolveQuestions = resolveIDs
	c.Artifacts = artifacts
	return c, len(pending), nil
}

// retryCapResumesContinue is design section 5.6's own "cap_resumes,
// exhausted session of job review, from CONTINUE" row: the newest "review
// round <n> asked" marker names the round; this writes "review round <n>
// failed" with CONTINUE's own cap reason (the held rows go unread from then
// on, same as any other failed round) and resolves every answered question
// of the asked set. The next tick's decision tree reads the "failed" marker
// and starts a fresh ROUND n (6.1), the same lens set it would have had
// (6.7's own repeat rule).
func (h reviewingHandler) retryCapResumesContinue(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64, sessionID int64) (store.HandlerCommit, int, error) {
	markers, err := d.Store.MarkersWithPrefix(ctx, t.ID, reviewRoundMarkerPrefix)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: reviewing: cap_resumes retry: review round markers: %w", err)
	}
	if len(markers) == 0 {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: reviewing: cap_resumes retry: ticket %d: no review round marker", t.ID)
	}
	newest := markers[len(markers)-1]
	if !reviewRoundAskedLine.MatchString(strings.SplitN(newest.Body, "\n", 2)[0]) {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: reviewing: cap_resumes retry: newest review round marker is not asked: %q", newest.Body)
	}
	n, runIDs, _, err := parseAskedMarker(newest)
	if err != nil {
		return store.HandlerCommit{}, 0, err
	}

	rounds, err := d.Store.AnsweredRounds(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: reviewing: cap_resumes retry: answered rounds: %w", err)
	}
	byRunID := make(map[int64]store.Round, len(rounds))
	for _, r := range rounds {
		if r.RunID != nil {
			byRunID[*r.RunID] = r
		}
	}

	allResolveIDs := append([]int64{}, resolveIDs...)
	preserved := 0
	sessionFound := false
	for _, rid := range runIDs {
		r, ok := byRunID[rid]
		if !ok {
			continue
		}
		allResolveIDs = append(allResolveIDs, questionIDs(r)...)
		preserved++
		if r.SessionID != nil && *r.SessionID == sessionID {
			sessionFound = true
		}
	}
	if !sessionFound {
		return store.HandlerCommit{}, 0, fmt.Errorf("job: reviewing: cap_resumes retry: session %d is not an asker of the newest asked round", sessionID)
	}

	c := baseCommit(t, d)
	c.ResolveQuestions = allResolveIDs
	c.Messages = []store.Message{reviewRoundFailedMarker(t.ID, n, reviewCapResumesContinueReason)}
	return c, preserved, nil
}

// derefString returns *s, or "" for a nil s: a resumed session's own
// external id is always set by the time it can be resumed (D13), but the
// zero value is the safe, obviously-wrong fallback rather than a panic.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
