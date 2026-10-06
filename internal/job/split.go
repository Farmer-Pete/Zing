// split.go builds a planner's children outcome into the split gate (design
// section 6.6's own split variant, plan #74): childrenCommit stores the
// proposed children as the children artifact and posts the split question,
// exactly as postGateCommit posts the plan gate. enterFromSplitRound reads
// the owner's answered split round, mirroring enterFromGateRound: reject
// resumes or restarts planning with the owner's note, approve files each
// child as a tracker issue (task 6).
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/store"
	"zing/internal/tracker"
)

const (
	// artifactTypeChildren names the stored children artifact (design
	// section 6.6's split variant; the schema already lists this type).
	artifactTypeChildren = "children"

	// waitingFlagSplit is the split question's own waiting_on value
	// (distinct from waitingFlagGate), so SendBatch clears it only when the
	// ticket's open split question is answered.
	waitingFlagSplit = string(response.QuestionKindSplit)

	// splitOptionApprove and splitOptionReject are the two option keys the
	// split question ever offers, mirroring gateOptionApprove/Reject: "a"
	// recommended.
	splitOptionApprove = "a"
	splitOptionReject  = "b"

	splitApproveExplains = "Approve files each child below as a tracker issue that links back to this one, queues the children in dependency order (a child waits until every issue it depends on is done), and closes this ticket as split. Reject sends this ticket back to planning with your note."

	// splitRejectedNote prefixes a rejected split's resume notes (design
	// section 6.7, mirroring the gate's own rejection note), followed by the
	// owner's joined replies.
	splitRejectedNote = "The owner rejected your proposed split into child tickets. Plan this ticket again. The owner's note follows.\n\n"
)

// childrenCommit is the children outcome's own commit (design section 6.6's
// split variant): it stores the children artifact, deduplicating a repeated
// depends_on key inside one child (semantics.go's checkChildrenDAG rejects a
// duplicate child key, an unknown key, self-dependency, and a cycle, but not
// a key repeated inside one child's own depends_on), posts the split
// question, and sets waiting_on to split.
func childrenCommit(t store.Ticket, d Deps, rr runResult, r *response.ChildrenResponse, sessionCommit *store.SessionUpsert, resolveIDs []int64) (store.HandlerCommit, error) {
	children := make([]response.Child, len(r.Children))
	for i, c := range r.Children {
		seen := make(map[string]bool, len(c.DependsOn))
		deps := []string{}
		for _, k := range c.DependsOn {
			if !seen[k] {
				seen[k] = true
				deps = append(deps, k)
			}
		}
		c.DependsOn = deps
		children[i] = c
	}
	payload, err := json.Marshal(response.ChildrenArtifact{Children: children, Notes: r.Notes})
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: split: marshal children artifact: %w", err)
	}
	msg, err := splitQuestionMessage(t.ID, len(children))
	if err != nil {
		return store.HandlerCommit{}, err
	}
	runID := rr.Reserved.RunID
	c := baseCommit(t, d)
	c.Runs = terminalRuns(rr, string(response.OutcomeChildren))
	c.Session = sessionCommit
	c.ResolveQuestions = resolveIDs
	c.Artifacts = []store.Artifact{{Type: artifactTypeChildren, RunID: &runID, Payload: payload}}
	c.Messages = []store.Message{msg}
	waiting := waitingFlagSplit
	c.Waiting = &waiting
	slog.Info("split proposed", "ticket_id", t.ID, "run_id", runID, "children", len(children))
	return c, nil
}

// splitQuestionMessage builds the split gate's "Post" message: kind split,
// recommended "a", the fixed Approve/Reject chip pair, Key left empty for
// CommitHandlerResult's own fillQuestionKeyTx to allocate -- the same shape
// as gateQuestionMessage, with no run attached.
func splitQuestionMessage(ticketID int64, n int) (store.Message, error) {
	payload, err := json.Marshal(response.QuestionPayload{
		Kind:        response.QuestionKindSplit,
		State:       response.QuestionStateOpen,
		Recommended: splitOptionApprove,
		Options: []response.Option{
			{Key: splitOptionApprove, Text: "Approve"},
			{Key: splitOptionReject, Text: "Reject"},
		},
	})
	if err != nil {
		return store.Message{}, fmt.Errorf("job: split: marshal question payload: %w", err)
	}
	body := fmt.Sprintf("Split this ticket into %d tickets?", n) + "\n\n" + splitApproveExplains
	return store.Message{
		TicketID: ticketID, Type: msgTypeQuestion, Author: authorZing,
		State: new(questionStateOpen), Body: body, Payload: payload,
	}, nil
}

// splitOrder returns children in an order where every child comes after
// every child it depends on (Kahn's algorithm), always taking the
// earliest-listed child among those currently ready, so filing proceeds
// in a stable, predictable order.
func splitOrder(children []response.Child) ([]response.Child, error) {
	byKey := make(map[string]response.Child, len(children))
	for _, c := range children {
		byKey[c.Key] = c
	}
	for _, c := range children {
		for _, dep := range c.DependsOn {
			if _, ok := byKey[dep]; !ok {
				return nil, fmt.Errorf("job: split: child %s depends on unknown key %s", c.Key, dep)
			}
		}
	}

	taken := make(map[string]bool, len(children))
	order := make([]response.Child, 0, len(children))
	for len(order) < len(children) {
		progressed := false
		for _, c := range children {
			if taken[c.Key] {
				continue
			}
			ready := true
			for _, dep := range c.DependsOn {
				if !taken[dep] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			taken[c.Key] = true
			order = append(order, c)
			progressed = true
			break
		}
		if !progressed {
			remaining := make([]string, 0, len(children)-len(order))
			for _, c := range children {
				if !taken[c.Key] {
					remaining = append(remaining, c.Key)
				}
			}
			return nil, fmt.Errorf("job: split: dependency cycle among %s", strings.Join(remaining, ", "))
		}
	}
	return order, nil
}

// enterFromSplitRound is enterFromRound's own split branch (design section
// 6.6's split variant, mirroring enterFromGateRound): round is the answered
// split round enterFromRound just identified by its newest question's kind.
// Option b, or a round carrying replies and no option at all, is a reject --
// "resume or fresh" with the owner's joined replies as notes, prefixed by
// splitRejectedNote, the same as the gate's own rejection (design section
// 6.7). Option a (approve) files the children, one per tick
// (fileNextSplitChild).
func (h planningHandler) enterFromSplitRound(ctx context.Context, t store.Ticket, d Deps, round store.Round) (store.HandlerCommit, error) {
	resolveIDs := questionIDs(round)
	if newestChosenOption(round.Answers) != splitOptionApprove {
		slog.Info("split rejected", "ticket_id", t.ID)
		notes := splitRejectedNote + joinReplies(round.Replies)
		return resumeOrFresh(ctx, t, d, []prompt.NamedInput{prompt.Notes(notes)}, resolveIDs)
	}
	return fileNextSplitChild(ctx, t, d, resolveIDs)
}

// SplitTracker is what an approved split needs from the tracker (design
// section 6.6's split variant): FileSplitChild files one child as a new
// tracker issue under projectID, returning its ref; CloseSplitParent posts
// comment on ref once (idempotent, like postMarkedOnce) and closes it. The
// dispatcher implements it over its own tracker and bindings and passes
// itself as Deps.Splitter (internal/dispatch/split.go).
type SplitTracker interface {
	FileSplitChild(ctx context.Context, projectID int64, title, body string) (ref string, err error)
	CloseSplitParent(ctx context.Context, projectID int64, ref, body string) error
}

// fileNextSplitChild files the next unfiled child, in dependency order, one
// per tick (design section 6.6's split variant): it reads the stored
// children artifact and the children already filed under t
// (Store.SplitChildren), computes splitOrder, and files the first child not
// yet in that set. A tracker failure logs a warning and returns
// ErrNoAction, so the next tick resumes from the store's filed children
// without filing a duplicate. Once every child is filed, it closes the
// parent (closeSplitParent).
func fileNextSplitChild(ctx context.Context, t store.Ticket, d Deps, resolveIDs []int64) (store.HandlerCommit, error) {
	if d.Splitter == nil {
		return store.HandlerCommit{}, errors.New("job: split: no split tracker wired")
	}
	art, found, err := d.Store.GetArtifact(ctx, t.ID, artifactTypeChildren)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: split: get children artifact: %w", err)
	}
	if !found {
		return store.HandlerCommit{}, fmt.Errorf("job: split: ticket %d has a split round but no children artifact", t.ID)
	}
	var ca response.ChildrenArtifact
	if err = json.Unmarshal(art.Payload, &ca); err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: split: unmarshal children artifact: %w", err)
	}
	order, err := splitOrder(ca.Children)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: split: %w", err)
	}
	filed, err := d.Store.SplitChildren(ctx, t.ID)
	if err != nil {
		return store.HandlerCommit{}, fmt.Errorf("job: split: split children: %w", err)
	}
	refByKey := make(map[string]string, len(filed))
	for _, f := range filed {
		refByKey[f.Key] = f.Ref
	}
	for _, c := range order {
		if _, ok := refByKey[c.Key]; ok {
			continue
		}
		depRefs := make([]string, len(c.DependsOn))
		for i, k := range c.DependsOn {
			depRefs[i] = refByKey[k]
		}
		body := splitChildBody(c.Body, ca.Notes, t.TrackerRef, depRefs)
		ref, fileErr := d.Splitter.FileSplitChild(ctx, t.ProjectID, c.Title, body)
		if fileErr != nil {
			slog.Warn("split child filing failed", "ticket_id", t.ID, "child_key", c.Key, "err", fileErr)
			return store.HandlerCommit{}, ErrNoAction
		}
		slog.Info("split child filed", "ticket_id", t.ID, "child_key", c.Key, "ref", ref)
		commit := baseCommit(t, d)
		commit.SplitChild = &store.SplitChild{Key: c.Key, Ref: ref, Title: c.Title, Body: body, DependsOn: c.DependsOn}
		commit.Messages = []store.Message{{
			TicketID: t.ID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("Filed %s as %s: %s", c.Key, tracker.IssueRef(ref), c.Title),
		}}
		return commit, nil
	}
	return closeSplitParent(ctx, t, d, order, refByKey, resolveIDs)
}

// closeSplitParent runs once every child is filed (design section 6.6's
// split variant, owner decision Q1): it posts one comment on the parent's
// issue listing every child's human-readable ref, closes the issue, and
// moves the parent ticket to done with reason "split into #A, #B". A
// tracker failure logs a warning and returns ErrNoAction, so the next tick
// retries; CloseSplitParent is idempotent, so a retried tick never posts
// the comment twice.
func closeSplitParent(ctx context.Context, t store.Ticket, d Deps, order []response.Child, refByKey map[string]string, resolveIDs []int64) (store.HandlerCommit, error) {
	refs := make([]string, len(order))
	for i, c := range order {
		refs[i] = tracker.IssueRef(refByKey[c.Key])
	}
	list := strings.Join(refs, ", ")
	comment := "Zing split this ticket into " + list + ". Each child is queued in dependency order."
	if err := d.Splitter.CloseSplitParent(ctx, t.ProjectID, t.TrackerRef, comment); err != nil {
		slog.Warn("split parent close failed", "ticket_id", t.ID, "err", err)
		return store.HandlerCommit{}, ErrNoAction
	}
	c := baseCommit(t, d)
	c.Next = stateDone
	c.Reason = "split into " + list
	c.ResolveQuestions = resolveIDs
	slog.Info("split parent closed", "ticket_id", t.ID, "children", len(order))
	return c, nil
}

// splitChildBody renders one split child's issue body (design section
// 6.6's split variant): body trimmed, then, only when notes is non-blank, a
// "Shared notes from the split" heading followed by notes trimmed (owner
// decision Q4), then "Split from" the parent's human-readable ref, then,
// only when depRefs is non-empty, "Depends on" each ref joined by ", ".
// Parts are joined by a blank line.
func splitChildBody(body, notes, parentRef string, depRefs []string) string {
	parts := []string{strings.TrimSpace(body)}
	if strings.TrimSpace(notes) != "" {
		parts = append(parts, "## Shared notes from the split\n\n"+strings.TrimSpace(notes))
	}
	parts = append(parts, fmt.Sprintf("Split from %s.", tracker.IssueRef(parentRef)))
	if len(depRefs) > 0 {
		refs := make([]string, len(depRefs))
		for i, ref := range depRefs {
			refs[i] = tracker.IssueRef(ref)
		}
		parts = append(parts, fmt.Sprintf("Depends on %s.", strings.Join(refs, ", ")))
	}
	return strings.Join(parts, "\n\n")
}
