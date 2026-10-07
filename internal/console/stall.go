// stall.go decides, for a single non-terminal ticket, why it is not moving
// right now (ticket "Say on each ticket why it is not moving", split from
// #79). decideStall is pure over its stallInput so the decision is testable
// without a store, a dispatcher, or a running serve; rail.go's
// buildStallRail (Task 3) loads that input and renders the result.
package console

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"zing/internal/dispatch"
	"zing/internal/response"
	"zing/internal/store"
)

// stallReason names which of the five live reasons, or none, decideStall
// picked. Rendered verbatim as the rail's data-stall-reason attribute.
type stallReason string

const (
	stallNone      stallReason = ""
	stallRunning   stallReason = "running"
	stallOwner     stallReason = "owner"
	stallClaimDead stallReason = "claim_dead"
	stallCI        stallReason = "ci"
	stallSlot      stallReason = "slot"
)

// stallInput is decideStall's whole input: the ticket row plus the facts
// buildStallRail gathered from the dispatcher's slot snapshot, the store,
// and the wall clock. A nil field means its source was unavailable (no
// SlotSource) or not applicable (not shipping, no CI marker, no runs).
type stallInput struct {
	Ticket store.Ticket

	// Slots is nil when the console has no SlotSource (selftest, most tests).
	Slots *dispatch.SlotSnapshot

	// Candidate is true only when Ticket.ID is among the store's
	// ListReadyCandidates at Now. Meaningless when Slots is nil.
	Candidate bool

	// ClaimAlive is dispatch.ClaimProcessesAlive for this ticket's foreign
	// claim; false when no claim applies or no entry matched.
	ClaimAlive bool

	// CIMarker is the newest "ci waiting" system update for this ticket;
	// nil when the ticket is not in shipping or there is no such marker.
	CIMarker *store.MessageRow

	// LastRan is the greatest non-nil Run.StartedAt for this ticket; nil
	// when it never ran.
	LastRan *time.Time

	// Now is the wall clock at render.
	Now time.Time
}

// decideStall picks the one reason the ticket is not moving, in this
// precedence (owner decision Q4): running now, then waiting on the owner,
// then a dead claim, then CI waiting, then a full run-slot table. It
// returns stallNone and "" when none applies (owner decision Q3).
func decideStall(in stallInput) (reason stallReason, text string) {
	t := in.Ticket
	if in.Slots != nil && slices.Contains(in.Slots.Inflight, t.ID) {
		return stallRunning, "running now"
	}
	if t.WaitingOn != nil {
		return stallOwner, "waiting on the owner (" + *t.WaitingOn + ")"
	}
	if in.Slots != nil && t.ClaimOwner != nil && *t.ClaimOwner != in.Slots.Owner {
		tail := "the next dispatch pass reclaims it"
		if in.ClaimAlive {
			tail = "its leftover agent process is still running"
		}
		return stallClaimDead, "claim held by a process that is no longer alive (" + *t.ClaimOwner + "); " + tail
	}
	if t.State == string(response.TicketStateShipping) && in.CIMarker != nil && in.CIMarker.CreatedAt != nil &&
		(in.LastRan == nil || in.CIMarker.CreatedAt.After(*in.LastRan)) {
		first, _, _ := strings.Cut(in.CIMarker.Body, "\n")
		var names []string
		for n := range strings.SplitSeq(strings.TrimPrefix(first, updateMarkerCIWaitingPrefix), ",") {
			if n != "" {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			mins := max(int(in.Now.Sub(*in.CIMarker.CreatedAt)/time.Minute), 0)
			unit := "minutes"
			if mins == 1 {
				unit = "minute"
			}
			return stallCI, fmt.Sprintf("CI waiting %d %s for %s", mins, unit, strings.Join(names, ", "))
		}
	}
	if in.Slots != nil && in.Candidate && len(in.Slots.Inflight) >= in.Slots.MaxParallel {
		ids := make([]string, len(in.Slots.Inflight))
		for i, id := range in.Slots.Inflight {
			ids[i] = strconv.FormatInt(id, 10)
		}
		return stallSlot, "waiting for a free run slot; slots held by tickets " + strings.Join(ids, ", ")
	}
	return stallNone, ""
}

// lastRanText renders the rail's "last ran" line: t is the ticket's newest
// run's StartedAt, or nil when it never ran.
func lastRanText(t *time.Time) string {
	if t == nil {
		return "never ran"
	}
	return "last ran " + t.UTC().Format("2006-01-02 15:04") + " UTC"
}
