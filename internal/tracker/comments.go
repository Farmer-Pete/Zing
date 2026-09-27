package tracker

import "fmt"

// disclosureFmt is the shared disclosure line every comment builder ends
// with, naming the configured owner (Zing may run for more than one
// person, so the owner is a parameter, not a hardcoded name).
const disclosureFmt = "Posted automatically by Zing, an agent working on %s's behalf."

// disclosureNoOwner is disclosureFmt's fallback when owner is "", so the
// disclosure never renders the grammatically broken "on 's behalf.": an
// empty owner means Zing has no name to disclose, not no disclosure at all.
const disclosureNoOwner = "Posted automatically by Zing."

// disclosure renders the shared disclosure line, naming owner when it is
// set and omitting the possessive entirely when it is not.
func disclosure(owner string) string {
	if owner == "" {
		return disclosureNoOwner
	}
	return fmt.Sprintf(disclosureFmt, owner)
}

// PickupComment is posted when Zing picks up a ticket.
func PickupComment(owner string) string {
	return "Zing picked up this issue and started work.\n\n" + disclosure(owner)
}

// GateComment is posted when Zing has a plan ready for review at
// consoleURL.
func GateComment(owner, consoleURL string) string {
	return "Zing has a plan ready for review. Open the gate to approve or change it: " + consoleURL + "\n\n" + disclosure(owner)
}

// PRComment is posted when Zing opens a draft pull request at prURL.
func PRComment(owner, prURL string) string {
	return "Zing opened a draft pull request: " + prURL + "\n\n" + disclosure(owner)
}

// DoneComment is posted when Zing finishes a ticket, with its pull
// request at prURL ready for review.
func DoneComment(owner, prURL string) string {
	return "Zing finished this ticket. The pull request is ready for review: " + prURL + "\n\n" + disclosure(owner)
}

// NothingToDoComment is posted when planning's nothing_to_do outcome, with
// every code claim false, moves a ticket straight to done (design D12,
// section 6.8): notes is the agent's own explanation.
func NothingToDoComment(owner, notes string) string {
	return "Zing found nothing to do for this issue. " + notes + "\n\n" + disclosure(owner)
}
