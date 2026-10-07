package templates

import "zing/internal/response"

// DisplayDecision is the word the console shows for a stored item decision.
// A perimeter answer stores reject (internal/job/building.go reads it) but
// every item row shows drop for "do not apply", so reject shows as drop.
// Reject is only ever stored on a perimeter item, so no kind is needed.
func DisplayDecision(d response.Decision) response.Decision {
	if d == response.DecisionReject {
		return response.DecisionDrop
	}
	return d
}
