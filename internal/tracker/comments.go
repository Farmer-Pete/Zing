package tracker

import (
	"fmt"
	"strings"
)

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

// DoneComment is posted when Zing finishes a ticket. With mergeSHA set, the
// pull request at prURL was merged, and the comment names that merge
// commit; mergeSHA "" means the caller has none to report, and the comment
// keeps its older "ready for review" wording.
func DoneComment(owner, prURL, mergeSHA string) string {
	if mergeSHA == "" {
		return "Zing finished this ticket. The pull request is ready for review: " + prURL + "\n\n" + disclosure(owner)
	}
	return "Zing finished this ticket. The pull request was merged: " + prURL + " (merge commit " + mergeSHA + ")\n\n" + disclosure(owner)
}

// NothingToDoComment is posted when planning's nothing_to_do outcome, with
// every code claim false, moves a ticket straight to done (design D12,
// section 6.8): notes is the agent's own explanation.
func NothingToDoComment(owner, notes string) string {
	return "Zing found nothing to do for this issue. " + notes + "\n\n" + disclosure(owner)
}

// replyPrefixFmt and replyPrefixNoOwner are ReplyPrefix's two forms
// (design D10, section 9.3): the GitHub login of the token's owner, read
// once with GitHub.Viewer and cached, because zing.toml's "user" need not
// be a login. The empty-login fallback follows disclosure's own rule, so
// an unknown login never renders the grammatically broken "on 's behalf:".
const (
	replyPrefixFmt     = "Zing (an AI agent) replying on behalf of @%s:"
	replyPrefixNoOwner = "Zing (an AI agent) replying:"
)

// ReplyPrefix is the first line of every reply Zing posts to a GitHub
// review thread (design D10, section 9.3): disclosure scoped to one
// reply, distinct from disclosure's own whole-comment line because a
// reply lives inside someone else's review thread, not a standalone
// comment Zing posts on its own behalf.
func ReplyPrefix(login string) string {
	if login == "" {
		return replyPrefixNoOwner
	}
	return fmt.Sprintf(replyPrefixFmt, login)
}

// IssueRef renders ref for a human: "#" + ref for a GitHub issue number
// (every character ASCII digits), ref unchanged otherwise (plan #74, the
// planner's split).
func IssueRef(ref string) string {
	if ref == "" {
		return ref
	}
	for _, r := range ref {
		if r < '0' || r > '9' {
			return ref
		}
	}
	return "#" + ref
}

// zingMarker opens every hidden marker Zing posts; replyPrefixLead opens
// every review-thread reply; disclosureLead starts both disclosure forms.
const (
	zingMarker      = "<!-- zing:"
	replyPrefixLead = "Zing (an AI agent)"
	disclosureLead  = "Posted automatically by Zing"
)

// OwnerComments keeps the comments owner wrote, in order: author equal to
// owner ignoring case, body not blank, and not one of Zing's own posts
// (holding disclosureLead, opening with replyPrefixLead, or holding
// zingMarker in any case). An empty owner keeps nothing.
func OwnerComments(cs []Comment, owner string) []Comment {
	if owner == "" {
		return nil
	}
	var out []Comment
	for _, c := range cs {
		body := strings.TrimSpace(c.Body)
		byOwner := strings.EqualFold(c.Author, owner)
		zingPost := strings.Contains(body, disclosureLead) ||
			strings.HasPrefix(body, replyPrefixLead) ||
			strings.Contains(strings.ToLower(body), zingMarker)
		if byOwner && body != "" && !zingPost {
			out = append(out, c)
		}
	}
	return out
}

// RenderComments renders cs as the text stored in tickets.owner_comments:
// one block per comment, "Comment by AUTHOR:" then its trimmed body,
// blocks joined by one blank line. No comments renders "".
func RenderComments(cs []Comment) string {
	blocks := make([]string, len(cs))
	for i, c := range cs {
		blocks[i] = "Comment by " + c.Author + ":\n" + strings.TrimSpace(c.Body)
	}
	return strings.Join(blocks, "\n\n")
}
