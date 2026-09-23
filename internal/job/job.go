// Package job is the dispatcher-to-job seam (design section 6.5): a
// Handler runs one pipeline state's work and returns a store.HandlerCommit
// describing what to persist and where the ticket goes next. A handler
// performs no write of its own; the dispatcher validates the commit with
// ValidateCommit and applies it through store.CommitHandlerResult under the
// claim fence.
package job

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"zing/internal/machine"
	"zing/internal/runtime"
	"zing/internal/store"
)

// The pipeline state names, spelled once so job.go and skeleton.go never
// repeat the literal (design section 7.1).
const (
	stateQueued    = "queued"
	statePlanning  = "planning"
	stateBuilding  = "building"
	stateReviewing = "reviewing"
	stateJudging   = "judging"
	stateShipping  = "shipping"
	stateDone      = "done"
)

// Deps is what a handler needs to do its work and build a commit: read-only
// store access, the runtime that serves the job's fake or real turns, and
// the claim this run holds (Owner, Expires), which every commit must carry
// back unchanged as its fence.
type Deps struct {
	Store   *store.Store    // reads only inside a handler
	Runtime runtime.Runtime // the fake; carries its own script fs
	Owner   string
	Expires time.Time // the claim lease; the commit fence
}

// Handler runs one pipeline state's job for ticket t and returns the commit
// the dispatcher should validate and apply. It writes nothing itself.
type Handler interface {
	Run(ctx context.Context, t store.Ticket, d Deps) (store.HandlerCommit, error)
}

// Registry returns the six skeleton handlers (design section 6.5), keyed by
// the pipeline state each drives. done is terminal and carries no handler.
func Registry() map[string]Handler {
	return map[string]Handler{
		stateQueued:    queuedHandler{},
		statePlanning:  planningHandler{},
		stateBuilding:  buildingHandler{},
		stateReviewing: reviewingHandler{},
		stateJudging:   judgingHandler{},
		stateShipping:  shippingHandler{},
	}
}

// Validate confirms every non-terminal state m.States.Order names has a
// handler in reg, so a missing handler fails at startup, never at a nil map
// read mid-tick.
func Validate(m *machine.Machine, reg map[string]Handler) error {
	terminal := make(map[string]bool, len(m.States.Terminal))
	for _, s := range m.States.Terminal {
		terminal[s] = true
	}
	for _, s := range m.States.Order {
		if terminal[s] {
			continue
		}
		h, ok := reg[s]
		if !ok || h == nil {
			return fmt.Errorf("job: state %s has no handler", s)
		}
	}
	return nil
}

// legalEdges is the section 7.1 state table: for each From state, the To
// states a transition (a commit with Next set) may legally target.
var legalEdges = map[string][]string{
	stateQueued:    {statePlanning},
	statePlanning:  {statePlanning, stateBuilding},
	stateBuilding:  {stateReviewing},
	stateReviewing: {stateJudging},
	stateJudging:   {stateShipping},
	stateShipping:  {stateDone},
}

// legalWaiting is the eight waiting_on flags migrations/0001_init.sql
// allows (design section 7.1 and 8).
var legalWaiting = map[string]bool{
	"questions": true, "split": true, "gate": true, "perimeter": true,
	"review": true, "merge": true, "children": true, "error": true,
}

// ValidateCommit checks c against the section 7.1 state table and the
// commit shape rules (design section 6.5): c.TicketID must name the ticket
// it was built against, the commit must do something (it is never wholly
// empty: at least one of Next, Waiting, Messages, Runs, ResolveQuestions, or
// Session must be set), when Next is set it names a legal successor of
// t.State and carries a non-empty Reason, and it does not also set a
// non-error Waiting; any set Waiting is one of the eight closed-set flags.
func ValidateCommit(t store.Ticket, c store.HandlerCommit) error {
	if c.TicketID != t.ID {
		return fmt.Errorf("job: commit is for ticket %d, not ticket %d", c.TicketID, t.ID)
	}
	if c.Next == "" && c.Waiting == nil && len(c.Messages) == 0 && len(c.Runs) == 0 &&
		len(c.ResolveQuestions) == 0 && c.Session == nil {
		return fmt.Errorf("job: commit for ticket %d carries no Next, Waiting, Messages, Runs, ResolveQuestions, or Session", t.ID)
	}
	if c.Next != "" {
		if c.Reason == "" {
			return fmt.Errorf("job: transition to %s carries no reason", c.Next)
		}
		if !legalEdge(t.State, c.Next) {
			return fmt.Errorf("job: %s -> %s is not a legal edge", t.State, c.Next)
		}
		if c.Waiting != nil && *c.Waiting != "error" {
			return fmt.Errorf("job: commit transitions to %s and also waits on %s", c.Next, *c.Waiting)
		}
	}
	if c.Waiting != nil && !legalWaiting[*c.Waiting] {
		return fmt.Errorf("job: waiting_on %q is not one of the eight waiting flags", *c.Waiting)
	}
	return nil
}

func legalEdge(from, to string) bool {
	return slices.Contains(legalEdges[from], to)
}

// OrderCandidates returns a new slice holding candidates ordered by reverse
// pipeline position (the state's index in order, largest first), then by
// the numeric external id parsed from TrackerRef (ascending), then by row
// id (ascending). A TrackerRef with no numeric suffix sorts after every
// candidate that has one (design section 6.2, 6.8).
func OrderCandidates(candidates []store.Ticket, order []string) []store.Ticket {
	pos := make(map[string]int, len(order))
	for i, s := range order {
		pos[s] = i
	}

	out := make([]store.Ticket, len(candidates))
	copy(out, candidates)
	slices.SortFunc(out, func(a, b store.Ticket) int {
		if c := cmp.Compare(pos[b.State], pos[a.State]); c != 0 { // reverse: larger index first
			return c
		}
		aNum, aOK := numericSuffix(a.TrackerRef)
		bNum, bOK := numericSuffix(b.TrackerRef)
		switch {
		case aOK && bOK:
			if c := cmp.Compare(aNum, bNum); c != 0 {
				return c
			}
		case aOK && !bOK:
			return -1
		case !aOK && bOK:
			return 1
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

// numericSuffix parses the trailing run of ASCII digits in ref (for
// example "fake#2" -> 2), reporting ok=false when ref carries no trailing
// digit at all.
func numericSuffix(ref string) (n int64, ok bool) {
	i := len(ref)
	for i > 0 && ref[i-1] >= '0' && ref[i-1] <= '9' {
		i--
	}
	if i == len(ref) {
		return 0, false
	}
	v, err := strconv.ParseInt(ref[i:], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
