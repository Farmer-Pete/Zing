package tracker

import "fmt"

// disclosureFmt is the shared disclosure line every comment builder ends
// with, naming the configured owner (Zing may run for more than one
// person, so the owner is a parameter, not a hardcoded name).
const disclosureFmt = "Posted automatically by Zing, an agent working on %s's behalf."

// PickupComment is posted when Zing picks up a ticket.
func PickupComment(owner string) string {
	return "Zing picked up this issue and started work.\n\n" + fmt.Sprintf(disclosureFmt, owner)
}

// GateComment is posted when Zing has a plan ready for review at
// consoleURL.
func GateComment(owner, consoleURL string) string {
	return "Zing has a plan ready for review. Open the gate to approve or change it: " + consoleURL + "\n\n" + fmt.Sprintf(disclosureFmt, owner)
}

// PRComment is posted when Zing opens a draft pull request at prURL.
func PRComment(owner, prURL string) string {
	return "Zing opened a draft pull request: " + prURL + "\n\n" + fmt.Sprintf(disclosureFmt, owner)
}

// DoneComment is posted when Zing finishes a ticket, with its pull
// request at prURL ready for review.
func DoneComment(owner, prURL string) string {
	return "Zing finished this ticket. The pull request is ready for review: " + prURL + "\n\n" + fmt.Sprintf(disclosureFmt, owner)
}
