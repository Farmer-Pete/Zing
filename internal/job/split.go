// split.go builds a planner's children outcome into the split gate (design
// section 6.6's own split variant, plan #74): childrenCommit stores the
// proposed children as the children artifact and posts the split question,
// exactly as postGateCommit posts the plan gate.
package job

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"zing/internal/response"
	"zing/internal/store"
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
