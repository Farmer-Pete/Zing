// conversation.go is D31's planning-conversation handler logic (design
// section 22.2, 22.3, 22.4, 22.5, 22.6): the handler-level Layer 2 checks a
// planning response's replies must pass beyond what the runtime's own parse
// already enforces (checkConversation), the commit-shape helper that turns
// a turn's replies into messages and settles (conversationEffects), and the
// two renderers that build a planning run's "conversation" prompt input
// (renderResumeConversation, renderFreshConversation).
package job

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/store"
)

// authorYou, msgTypeAnswer, msgTypeReply, and msgTypeResolved mirror
// internal/store's own unexported constants of the same values (design
// section 22.3): this package cannot reach those, and the values are part
// of the closed set migrations/0001_init.sql fixes, not private store
// detail, the same reasoning skeleton.go's own mirrors (authorZing,
// msgTypeQuestion, questionStateOpen, waitingFlagQuestions) already give.
const (
	authorYou       = "you"
	msgTypeAnswer   = "answer"
	msgTypeReply    = "reply"
	msgTypeResolved = "resolved"
	answerStateSent = "sent"

	// msgTypeFollowup is D32's own reopen turn (design section 22.12.2):
	// store's own unexported mirror of the same literal (msgTypeFollowup,
	// commit.go), mirrored here for the same reason as the rest of this
	// block.
	msgTypeFollowup = "followup"
)

// conversationDeliveredPrefix mirrors store's own unexported
// conversationDeliveredPrefix (internal/store/conversation_reads.go): the
// delivered-marker body prefix a planning commit writes once a turn has
// answered every owner message it was given (design section 22.3, 22.4).
// Store parses this same literal text back out (deliveredMarkers), so the
// two must never drift.
const conversationDeliveredPrefix = "conversation delivered run "

// replyLister is implemented by every planning response type that can
// carry <replies> (design section 22.2): PlanningQuestionsResponse,
// ReadyResponse, ChildrenResponse, and NothingToDoResponse each promote it
// from their embedded response.Conversation; RepliesResponse implements it
// directly. response.ErrorResponse carries no Conversation and so never
// satisfies this, exactly matching design section 22.2's "an error document
// carries none".
type replyLister interface {
	ReplyList() []response.Reply
}

// threadState is one planning turn's view of the ticket's own threads
// (design section 22.2): settled maps each known key to whether it is
// already settled, order recovers question id order for every error list
// this file renders, and received names the keys whose thread carried at
// least one of this run's own undelivered owner rows -- the "answer every
// owner message you receive" rule.
type threadState struct {
	settled  map[string]bool
	order    map[string]int64
	received map[string]bool
}

// buildThreadState reads conv into a threadState (design section 22.2): the
// "received" set is conv's own Undelivered() rows, bucketed back to their
// thread's wire key -- the same set runPlanningResume and runPlanningFirst
// use to decide whether a run carries any conversation input at all, so a
// response is only ever asked to answer what it was actually shown.
func buildThreadState(conv store.PlanningConversation) threadState {
	ts := threadState{
		settled:  make(map[string]bool, len(conv.Threads)),
		order:    make(map[string]int64, len(conv.Threads)),
		received: make(map[string]bool),
	}
	byQuestionID := make(map[int64]string, len(conv.Threads))
	for i := range conv.Threads {
		th := &conv.Threads[i]
		key := questionKey(th.Question)
		ts.settled[key] = th.Settled
		ts.order[key] = th.Question.ID
		byQuestionID[th.Question.ID] = key
	}
	undelivered := conv.Undelivered()
	for i := range undelivered {
		row := &undelivered[i]
		if row.ParentID == nil {
			continue
		}
		if key, ok := byQuestionID[*row.ParentID]; ok {
			ts.received[key] = true
		}
	}
	return ts
}

// pathOutcome is the fixed PathError.Path every outcome-level checkConversation
// rule (still settled, confirmed-only, confirming-turn-only, needs-everything-
// settled) shares.
const pathOutcome = "outcome"

// checkConversation runs design section 22.2's handler-level Layer 2 rules
// for a planning response that can carry replies (D32, design section
// 22.12.3 widens it with its own confirming-turn rules): these need the
// ticket's own threads, which the runtime's own parse (response.Validate)
// never sees, so they run here instead, after a valid document decodes. It
// is pure, given s. confirming is true only for the gate's own confirming
// turn (runGateConfirm): outcome confirmed is refused outside it, and
// replies, children, and nothing_to_do are refused inside it (the owner
// asked a direct question; those three change the subject).
func checkConversation(outcome response.Outcome, replies []response.Reply, s threadState, confirming bool) []*response.PathError {
	var errs []*response.PathError
	answeredThisTurn := make(map[string]bool, len(replies))
	settledThisTurn := make(map[string]bool, len(replies))

	for i, r := range replies {
		settled, known := s.settled[r.Question]
		switch {
		case !known:
			errs = append(errs, &response.PathError{
				Path: replyPath(i, "question"), Msg: "no planning question " + r.Question,
			})
			continue
		case settled:
			errs = append(errs, &response.PathError{
				Path: replyPath(i, "question"), Msg: r.Question + " is already settled and takes no more replies",
			})
			continue
		}
		answeredThisTurn[r.Question] = true
		if r.Settled {
			settledThisTurn[r.Question] = true
		}
	}

	var stillOpen []string
	for key, settled := range s.settled {
		if !settled && !settledThisTurn[key] {
			stillOpen = append(stillOpen, key)
		}
	}
	sortKeysByQuestionID(stillOpen, s.order)

	switch {
	case !confirming && outcome == response.OutcomeConfirmed:
		errs = append(errs, &response.PathError{
			Path: pathOutcome, Msg: "confirmed answers only the gate's confirming turn",
		})
	case confirming && (outcome == response.OutcomeReplies || outcome == response.OutcomeChildren || outcome == response.OutcomeNothingToDo):
		errs = append(errs, &response.PathError{
			Path: pathOutcome, Msg: "the owner asked to close the gate: return confirmed, questions, or ready",
		})
	case outcome == response.OutcomeReady, outcome == response.OutcomeChildren, outcome == response.OutcomeNothingToDo, outcome == response.OutcomeConfirmed:
		if len(stillOpen) > 0 {
			errs = append(errs, &response.PathError{
				Path: pathOutcome,
				Msg:  fmt.Sprintf("%s needs every question settled; still open: %s", outcome, strings.Join(stillOpen, ", ")),
			})
		}
	case outcome == response.OutcomeReplies:
		if len(stillOpen) == 0 {
			errs = append(errs, &response.PathError{
				Path: pathOutcome,
				Msg:  "replies needs a question left open; every question is settled, so return ready, children, or nothing_to_do",
			})
		}
	}

	var missing []string
	for key := range s.received {
		if !answeredThisTurn[key] {
			missing = append(missing, key)
		}
	}
	sortKeysByQuestionID(missing, s.order)
	if len(missing) > 0 {
		errs = append(errs, &response.PathError{
			Path: "replies",
			Msg:  "no reply to the owner on " + strings.Join(missing, ", ") + "; answer every owner message you receive",
		})
	}

	return errs
}

// sortKeysByQuestionID sorts keys by each one's question id (order), the
// id order every reply error list and every rendered "Still open" line
// uses (design section 22.2, 22.6).
func sortKeysByQuestionID(keys []string, order map[string]int64) {
	sort.Slice(keys, func(i, j int) bool { return order[keys[i]] < order[keys[j]] })
}

// replyPath builds one reply's own element path, mirroring
// internal/response's own unexported replyPath (semantics.go): this
// package cannot reach that one, and the grammar ("replies/reply[i]/field")
// is the closed path convention response.PathError documents already use,
// not private response detail.
func replyPath(i int, field string) string {
	return fmt.Sprintf("replies/reply[%d]/%s", i, field)
}

// conversationEffects builds the messages and ConversationCommit a
// planning turn's replies add to its success commit (design section 22.4):
// one "reply"/zing/sent message per reply, parented to its question
// (conv.ThreadByKey maps the wire key checkConversation already verified
// names a known thread of this ticket); the delivered marker, only when
// throughBatch is above zero (this run actually carried undelivered owner
// messages); and a store.SettleQuestion per reply marked settled, left for
// CommitHandlerResult's own applyConversationTx to apply (including its own
// late-message fence, design section 22.3). ConversationCommit.ThroughBatch
// is throughBatch when positive, else conv.Delivered (W): the newest batch
// this run received, or the watermark when it received none.
func conversationEffects(ticketID, runID, throughBatch int64, replies []response.Reply, conv store.PlanningConversation) ([]store.Message, store.ConversationCommit) {
	msgs := make([]store.Message, 0, len(replies)+1)
	settle := make([]store.SettleQuestion, 0, len(replies))

	for _, r := range replies {
		th, ok := conv.ThreadByKey(r.Question)
		if !ok {
			continue // unreachable: checkConversation already rejected an unknown key
		}
		qID := th.Question.ID
		sent := answerStateSent
		msgs = append(msgs, store.Message{
			TicketID: ticketID, ParentID: &qID, Type: msgTypeReply, Author: authorZing,
			State: &sent, RunID: &runID, Body: r.Text,
		})
		if r.Settled {
			settle = append(settle, store.SettleQuestion{QuestionID: qID, Decision: r.Decision})
		}
	}

	effectiveThrough := throughBatch
	if effectiveThrough == 0 {
		effectiveThrough = conv.Delivered
	}
	if throughBatch > 0 {
		msgs = append(msgs, store.Message{
			TicketID: ticketID, Type: msgTypeUpdate, Author: authorSystem,
			Body: fmt.Sprintf("%s%d batch %d", conversationDeliveredPrefix, runID, throughBatch),
		})
	}

	return msgs, store.ConversationCommit{ThroughBatch: effectiveThrough, Settle: settle}
}

// ---- the conversation prompt input (design section 22.5, 22.6) -----------

// questionKey decodes q's QuestionPayload and returns its Key, "" when it
// does not decode (unreachable for a real question row, every one
// validated against messages/question.json at insert) -- the same decode
// store's own unexported questionPayloadKey performs, mirrored here because
// this package cannot reach it.
func questionKey(q store.MessageRow) string {
	var p response.QuestionPayload
	if err := json.Unmarshal(q.Payload, &p); err != nil {
		return ""
	}
	return p.Key
}

// questionTitleAndBody splits a question row's stored Body (Title, blank
// line, then Body) back into its two parts, the same cut
// internal/console's own splitQuestionBody performs.
func questionTitleAndBody(q store.MessageRow) (title, body string) {
	title, rest, _ := strings.Cut(q.Body, "\n")
	return title, strings.TrimPrefix(rest, "\n")
}

// questionOptions decodes q's QuestionPayload.Options, nil when the
// payload does not decode (unreachable for a real question row).
func questionOptions(q store.MessageRow) []response.Option {
	var p response.QuestionPayload
	if err := json.Unmarshal(q.Payload, &p); err != nil {
		return nil
	}
	return p.Options
}

// indentLines prefixes every line of s with two spaces (design section
// 22.6's own continuation rule for a question body and a multi-line turn).
func indentLines(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n")
}

// bulletLine renders text as one "- " bulleted line, indenting every line
// after the first by two spaces (design section 22.6: "In any multi-line
// text, every line after the first is indented two spaces").
func bulletLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = "  " + lines[i]
	}
	return "- " + strings.Join(lines, "\n")
}

// newestAgentReply returns the body of th's newest agent reply turn (not
// its settling decision, a different message type), ok false when it has
// none yet (design section 22.6: "only when the agent has replied in that
// thread before").
func newestAgentReply(th *store.Thread) (text string, ok bool) {
	for i := range slices.Backward(th.Turns) {
		if th.Turns[i].Author == authorZing && th.Turns[i].Type == msgTypeReply {
			return th.Turns[i].Body, true
		}
	}
	return "", false
}

// renderOwnerRowText renders one undelivered owner row as the text after
// its "- " bullet (design section 22.5): a picked option names its key and
// text; a reply names its body. A pick whose key matches none of options
// (a stale or hand-edited key) falls back to naming the bare key, matching
// optionTextFor's own "" for an unknown key.
func renderOwnerRowText(row store.MessageRow, options []response.Option) string {
	if row.Type == msgTypeAnswer {
		var ap response.AnswerPayload
		if err := json.Unmarshal(row.Payload, &ap); err == nil && ap.Option != nil {
			if text := optionTextFor(options, *ap.Option); text != "" {
				return "picked option " + *ap.Option + ": " + text
			}
			return "picked option " + *ap.Option
		}
	}
	return "wrote: " + row.Body
}

// renderResumeConversation builds a resumed planning turn's "conversation"
// prompt input, byte for byte (design section 22.5, 22.6): one block per
// unsettled thread that has at least one of undelivered's rows, in question
// id order, blank-line separated; a trailing "Still open" line, only when
// some other unsettled thread has nothing undelivered, naming every one of
// those in question id order.
func renderResumeConversation(c store.PlanningConversation, undelivered []store.MessageRow) string {
	byQuestion := make(map[int64][]store.MessageRow)
	for i := range undelivered {
		if undelivered[i].ParentID == nil {
			continue
		}
		parentID := *undelivered[i].ParentID
		byQuestion[parentID] = append(byQuestion[parentID], undelivered[i])
	}

	var blocks []string
	var stillOpen []string
	for i := range c.Threads {
		th := &c.Threads[i]
		if th.Settled {
			continue
		}
		rows := byQuestion[th.Question.ID]
		if len(rows) == 0 {
			stillOpen = append(stillOpen, questionKey(th.Question))
			continue
		}
		blocks = append(blocks, renderResumeThreadBlock(c, th, rows))
	}

	var sb strings.Builder
	sb.WriteString(strings.Join(blocks, "\n\n"))
	if len(stillOpen) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("Still open, nothing new from the owner: " + strings.Join(stillOpen, ", ") + ".")
	}
	return sb.String()
}

// renderResumeThreadBlock renders one unsettled thread's resume block
// (design section 22.5, widened by D32, design section 22.12.2): its key
// and title, then, only when the owner just reopened it (c.ReopenedUndelivered),
// the two fixed lines naming the key and the decision it reopened past --
// a followup row itself prints no list item below, since these two lines
// already say it -- then its newest agent reply when it has one, then
// every one of rows (already in (batch_id, id) order, section 22.3) as one
// bullet each, oldest first.
func renderResumeThreadBlock(c store.PlanningConversation, th *store.Thread, rows []store.MessageRow) string {
	title, _ := questionTitleAndBody(th.Question)
	options := questionOptions(th.Question)
	key := questionKey(th.Question)

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: %s\n", key, title)
	if c.ReopenedUndelivered(th.Question.ID) {
		fmt.Fprintf(&sb, "The owner reopened %s.\nYour decision was: %s\n", key, th.Decision)
	}
	if last, ok := newestAgentReply(th); ok {
		fmt.Fprintf(&sb, "Your last reply: %s\n", last)
	}
	sb.WriteString("The owner, oldest first:")
	for i := range rows {
		sb.WriteString("\n")
		sb.WriteString(bulletLine(renderOwnerRowText(rows[i], options)))
	}
	return sb.String()
}

// renderFreshConversation builds a fresh planning session's own
// "conversation" prompt input, byte for byte (design section 22.5, 22.6):
// every planning thread, in question id order, blank-line separated. The
// undelivered rows it lists are part of this transcript, so the caller
// marks them delivered the same way a resume does (ThroughBatch from the
// same Undelivered() set).
func renderFreshConversation(c store.PlanningConversation) string {
	blocks := make([]string, 0, len(c.Threads))
	for i := range c.Threads {
		th := &c.Threads[i]
		if th.Settled {
			blocks = append(blocks, renderSettledThreadBlock(th))
		} else {
			blocks = append(blocks, renderUnsettledThreadBlock(th))
		}
	}
	return strings.Join(blocks, "\n\n")
}

// renderSettledThreadBlock renders one settled thread, two lines (design
// section 22.6): "<key> (settled): <title>", then its decision.
func renderSettledThreadBlock(th *store.Thread) string {
	title, _ := questionTitleAndBody(th.Question)
	return fmt.Sprintf("%s (settled): %s\nDecision: %s", questionKey(th.Question), title, th.Decision)
}

// renderUnsettledThreadBlock renders one unsettled thread for a fresh
// session's transcript (design section 22.6): key, title, the body
// indented, the options line (omitted when there are none), the
// recommendation, and every turn oldest first (omitted when there are
// none).
func renderUnsettledThreadBlock(th *store.Thread) string {
	title, body := questionTitleAndBody(th.Question)
	options := questionOptions(th.Question)

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: %s\n", questionKey(th.Question), title)
	sb.WriteString("Body:\n")
	sb.WriteString(indentLines(body))
	sb.WriteString("\n")
	if len(options) > 0 {
		parts := make([]string, len(options))
		for i, o := range options {
			parts[i] = o.Key + ": " + o.Text
		}
		fmt.Fprintf(&sb, "Options: %s\n", strings.Join(parts, "; "))
	}
	var qp response.QuestionPayload
	if err := json.Unmarshal(th.Question.Payload, &qp); err != nil {
		// Unreachable for a real question row (every one validated against
		// messages/question.json at insert): qp.Recommended stays "".
		qp = response.QuestionPayload{}
	}
	fmt.Fprintf(&sb, "Recommended: %s", qp.Recommended)
	if len(th.Turns) > 0 {
		sb.WriteString("\nThread, oldest first:")
		for i := range th.Turns {
			sb.WriteString("\n")
			sb.WriteString(bulletLine(renderFreshTurnText(th.Turns[i], options)))
		}
	}
	return sb.String()
}

// renderFreshTurnText renders one thread turn for a fresh session's
// transcript (design section 22.6, widened by D32, design section
// 22.12.2): "you: ..." for an agent reply, "you settled it: ..." for the
// agent's own settling decision, "the owner reopened the thread" for a
// followup row, "the owner picked option ..." for a sent answer, "the
// owner wrote: ..." for a sent reply.
func renderFreshTurnText(row store.MessageRow, options []response.Option) string {
	switch {
	case row.Author == authorZing && row.Type == msgTypeReply:
		return "you: " + row.Body
	case row.Author == authorZing && row.Type == msgTypeResolved:
		return "you settled it: " + row.Body
	case row.Author == authorYou && row.Type == msgTypeFollowup:
		return "the owner reopened the thread"
	case row.Author == authorYou && row.Type == msgTypeAnswer:
		var ap response.AnswerPayload
		if err := json.Unmarshal(row.Payload, &ap); err == nil && ap.Option != nil {
			if text := optionTextFor(options, *ap.Option); text != "" {
				return "the owner picked option " + *ap.Option + ": " + text
			}
			return "the owner picked option " + *ap.Option
		}
		return "the owner wrote: " + row.Body
	default:
		return "the owner wrote: " + row.Body
	}
}

// conversationResumeInput builds a resumed planning turn's own conversation
// extra input and the throughBatch Reserve's seed takes (design section
// 22.4): Undelivered() non-empty renders the resume block and reports its
// newest batch (already sorted (batch_id, id) ascending, so the last row
// carries it); empty reports no input and batch zero, so
// ConversationCommit falls back to the watermark.
func conversationResumeInput(conv store.PlanningConversation) (extra []prompt.NamedInput, throughBatch int64) {
	undelivered := conv.Undelivered()
	if len(undelivered) == 0 {
		return nil, 0
	}
	text := renderResumeConversation(conv, undelivered)
	throughBatch = *undelivered[len(undelivered)-1].BatchID
	return []prompt.NamedInput{prompt.Conversation(text)}, throughBatch
}

// conversationFreshInput builds a fresh planning turn's own conversation
// extra input (design section 22.6): nothing when the ticket has no
// planning thread yet (a brand new ticket's very first turn), else the
// whole transcript, with throughBatch set exactly as a resume's would be,
// since the transcript's own undelivered rows mark themselves delivered.
func conversationFreshInput(conv store.PlanningConversation) (extra []prompt.NamedInput, throughBatch int64) {
	if len(conv.Threads) == 0 {
		return nil, 0
	}
	text := renderFreshConversation(conv)
	extra = []prompt.NamedInput{prompt.Conversation(text)}
	if undelivered := conv.Undelivered(); len(undelivered) > 0 {
		throughBatch = *undelivered[len(undelivered)-1].BatchID
	}
	return extra, throughBatch
}
