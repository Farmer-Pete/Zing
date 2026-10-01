// threadrules.go holds the respond job's pure thread rules (design section
// 9, M4 task 2): the tid GitHub review threads are named by everywhere
// outside a GraphQL call, the classes design section 9.1 sorts threads
// into, the digest that detects an edited or new human comment, the
// disclosed reply body every Zing reply is built from, and the coverage
// check a respond run's raw output must pass.
package job

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"zing/internal/orchestrator"
	"zing/internal/response"
	"zing/internal/tracker"
)

// tid names a review thread everywhere outside a GraphQL call -- markers,
// reply markers, prompts, the respond job's own thread ids, logs, and the
// poll fingerprint (design section 9.1): "t" plus the first 16 characters
// of the lowercase hex SHA-256 of the raw GraphQL id's bytes. The raw id
// itself is kept only for GraphQL calls, passed as a JSON variable, never
// logged or stored.
func tid(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "t" + hex.EncodeToString(sum[:])[:16]
}

// commentDigest is design sections 4.1 and 9.1's digest of one thread
// comment: the lowercase hex SHA-256 of the comment's id, its UpdatedAt
// (UTC, RFC 3339 nano, so an edit changes the digest even when the id does
// not), and the hex SHA-256 of its body.
func commentDigest(c orchestrator.ThreadComment) string {
	bodySum := sha256.Sum256([]byte(c.Body))
	joined := c.ID + "\n" + c.UpdatedAt.UTC().Format(time.RFC3339Nano) + "\n" + hex.EncodeToString(bodySum[:])
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:])
}

// isZingReply reports whether c is one of Zing's own disclosed replies
// (design section 9.1): true only when c.Author is exactly login (the
// authenticated viewer, GitHub.Viewer), the body's first line equals
// tracker.ReplyPrefix(login), and the body contains the reserved
// "<!-- zing:" sequence. A comment by anyone else that copies the prefix
// and a marker fails the author check, so it is a human comment: its
// thread stays actionable rather than becoming a leftover Zing would
// auto-resolve (9.5).
func isZingReply(c orchestrator.ThreadComment, login string) bool {
	if c.Author != login {
		return false
	}
	firstLine, _, _ := strings.Cut(c.Body, "\n")
	if firstLine != tracker.ReplyPrefix(login) {
		return false
	}
	return strings.Contains(c.Body, "<!-- zing:")
}

// maxThreadRawIDBytes is design section 9.1's unclassified bound: a raw
// GraphQL id longer than this is a shape code cannot sort.
const maxThreadRawIDBytes = 256

// classifyThreads sorts threads into design section 9.1's four classes,
// each slice keeping threads' own order, with no thread ever dropped:
//
//   - resolved: IsResolved.
//   - actionable: unresolved, at least one comment, last comment not a
//     Zing reply (isZingReply).
//   - leftover: unresolved, last comment a Zing reply.
//   - unclassified: unresolved and anything else -- zero comments, an
//     empty raw id, or a raw id longer than maxThreadRawIDBytes bytes. An
//     unclassified thread is never sent to the respond job; it blocks the
//     ready flip and the merge gate until a human comment makes it
//     actionable or someone resolves it.
func classifyThreads(threads []orchestrator.Thread, login string) (resolved, actionable, leftover, unclassified []orchestrator.Thread) {
	for _, t := range threads {
		switch {
		case t.IsResolved:
			resolved = append(resolved, t)
		case t.ID == "" || len(t.ID) > maxThreadRawIDBytes || len(t.Comments) == 0:
			unclassified = append(unclassified, t)
		case isZingReply(t.Comments[len(t.Comments)-1], login):
			leftover = append(leftover, t)
		default:
			actionable = append(actionable, t)
		}
	}
	return resolved, actionable, leftover, unclassified
}

// renderThreads renders threads, in the order given, as design section
// 9.2's threadsText: for each thread, "thread <tid>", the file location
// ("file <path>:<line>", or "file <path>" when Line is 0, with
// " (outdated)" appended when IsOutdated), then every comment as
// "comment by @<login> at <RFC 3339>:" followed by its body, comments
// separated by a blank line. Threads are separated by a line of three
// dashes.
func renderThreads(threads []orchestrator.Thread) string {
	blocks := make([]string, 0, len(threads))
	for _, t := range threads {
		var b strings.Builder
		fmt.Fprintf(&b, "thread %s\n", tid(t.ID))
		if t.Line == 0 {
			fmt.Fprintf(&b, "file %s", t.Path)
		} else {
			fmt.Fprintf(&b, "file %s:%d", t.Path, t.Line)
		}
		if t.IsOutdated {
			b.WriteString(" (outdated)")
		}
		b.WriteString("\n")

		comments := make([]string, 0, len(t.Comments))
		for _, c := range t.Comments {
			comments = append(comments, fmt.Sprintf("comment by @%s at %s:\n%s", c.Author, c.CreatedAt.UTC().Format(time.RFC3339), c.Body))
		}
		b.WriteString(strings.Join(comments, "\n\n"))

		blocks = append(blocks, b.String())
	}
	return strings.Join(blocks, "\n---\n")
}

// errReservedMarkerText is replyBody's second-layer refusal (design
// section 9.3): Layer 2 (4.1) already rejects a respond document whose
// thread text holds the reserved "<!-- zing:" sequence, so this only
// fires on text that reached replyBody another way (a direct caller, or a
// future response shape Layer 2 does not cover); it stays as the second
// layer, the same defense-in-depth shape as checkRespondThreadsShape and
// its job-package counterpart.
var errReservedMarkerText = errors.New("job: reply text contains the reserved marker sequence")

// replyBody builds the body of every reply Zing posts to a GitHub review
// thread (design section 9.3): when text holds no "<!-- zing:" sequence
// in any letter case, it returns tracker.ReplyPrefix(login), a blank
// line, text trimmed, a blank line, then marker. Every caller -- APPLY's
// reply and addressed actions, FIX-REPLIES' fixed-thread note -- builds
// its body through replyBody before the first write, so no posted
// comment ever carries a marker other than the one appended here.
func replyBody(login, text, marker string) (string, error) {
	if strings.Contains(strings.ToLower(text), "<!-- zing:") {
		return "", errReservedMarkerText
	}
	return tracker.ReplyPrefix(login) + "\n\n" + strings.TrimSpace(text) + "\n\n" + marker, nil
}

// hexCommitTokenPattern is CheckRespondCoverage's "addressed" rule: a
// standalone run of 7 to 40 lowercase hex characters, word-bounded so a
// longer run of hex-looking characters (not itself a valid commit-length
// token) never matches a truncated slice of itself.
var hexCommitTokenPattern = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)

// addressesBranchCommit reports whether text holds a token
// hexCommitTokenPattern matches that is a prefix of one of branchShas
// (design section 9.2's "addressed" rule).
func addressesBranchCommit(text string, branchShas []string) bool {
	for _, tok := range hexCommitTokenPattern.FindAllString(text, -1) {
		for _, sha := range branchShas {
			if strings.HasPrefix(sha, tok) {
				return true
			}
		}
	}
	return false
}

// CheckRespondCoverage is design section 9.2's respond coverage check,
// applied to one respond run's raw thread actions against the batch's own
// thread ids (ids, the tids of 9.1) and the shas BranchCommits(wt)
// returns. It walks resp.Threads in document order: an id not in ids
// gives "thread <id> is not in this batch"; an addressed action whose
// text holds no token that prefixes a branch commit (addressesBranchCommit)
// gives "thread <id>: addressed must name a commit on this branch". It
// then walks ids, in order, giving "missing action for thread <id>" for
// every one no thread of resp.Threads answered. Layer 1 already refused a
// duplicate or empty thread id (4.1), so this never checks for either.
func CheckRespondCoverage(ids []string, resp response.RespondResponse, branchShas []string) []string {
	batch := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		batch[id] = struct{}{}
	}

	var errs []string
	answered := make(map[string]struct{}, len(resp.Threads))
	for _, th := range resp.Threads {
		if _, ok := batch[th.ID]; !ok {
			errs = append(errs, "thread "+th.ID+" is not in this batch")
			continue
		}
		answered[th.ID] = struct{}{}
		if th.Action == response.ThreadVerbAddressed && !addressesBranchCommit(th.Text, branchShas) {
			errs = append(errs, "thread "+th.ID+": addressed must name a commit on this branch")
		}
	}

	for _, id := range ids {
		if _, ok := answered[id]; !ok {
			errs = append(errs, "missing action for thread "+id)
		}
	}
	return errs
}
