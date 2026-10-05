// spec.go gives every stage that judges against the ticket the owner's
// decisions since pickup, so no stage sees a staler spec than another
// (design "Decisions and additions", goal "specFor"): specFor reads the
// three sources (the planning conversation, the owner's answers outside
// it, and the gate's approval notes) and renderSpec, its pure renderer,
// turns them into the one ticket text every stage uses in place of the
// hand-built t.Title+"\n\n"+t.Body.
package job

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"zing/internal/response"
	"zing/internal/store"
)

// specDecisionsHeader and specApprovalHeader are renderSpec's own fixed
// lines (design shape): the first introduces the decisions section, only
// when there is one; the second always precedes the gate's approval notes,
// the last block when one is present.
const (
	specDecisionsHeader = "Owner decisions, oldest first. They amend the ticket text above."
	specApprovalHeader  = "At the gate, the owner approved the plan and wrote:"
)

// specFor is the ticket text every stage judges against (design goal 1):
// the title and body, then the owner's decisions since pickup -- settled
// planning threads, answered questions outside the planning conversation
// and the gate, and the gate's own approval notes. It reads the store
// fresh every call (design nongoal: no snapshot).
func specFor(ctx context.Context, d Deps, t store.Ticket) (string, error) {
	conv, err := d.Store.PlanningConversation(ctx, t.ID)
	if err != nil {
		return "", fmt.Errorf("job: spec: %w", err)
	}
	answers, err := d.Store.OwnerAnswers(ctx, t.ID)
	if err != nil {
		return "", fmt.Errorf("job: spec: %w", err)
	}
	notes, err := d.Store.ApprovalNotes(ctx, t.ID)
	if err != nil {
		return "", fmt.Errorf("job: spec: %w", err)
	}
	return renderSpec(t, conv, answers, notes), nil
}

// renderSpec builds the spec text, byte for byte (design shape, "Spec text
// format"): base (t.Title+"\n\n"+t.Body) alone when there are no decisions,
// else base, a blank line, specDecisionsHeader, a blank line, then every
// question block (settled planning threads and qualifying OwnerAnswers,
// sorted by question id) and, last, the approval block, blank-line
// separated.
func renderSpec(t store.Ticket, conv store.PlanningConversation, answers []store.OwnerAnswer, approvalNotes string) string {
	type block struct {
		qid  int64
		text string
	}
	var blocks []block
	for i := range conv.Threads {
		th := conv.Threads[i]
		if !th.Settled {
			continue
		}
		lines := ownerLines(th.Turns, questionOptions(th.Question), false)
		blocks = append(blocks, block{th.Question.ID, renderDecisionBlock(th.Question, lines, th.Decision)})
	}
	for i := range answers {
		a := answers[i]
		lines := ownerLines(a.Owner, questionOptions(a.Question), a.Escalation)
		if len(lines) == 0 {
			continue
		}
		blocks = append(blocks, block{a.Question.ID, renderDecisionBlock(a.Question, lines, "")})
	}
	slices.SortFunc(blocks, func(x, y block) int { return cmp.Compare(x.qid, y.qid) })

	parts := make([]string, 0, len(blocks)+1)
	for _, b := range blocks {
		parts = append(parts, b.text)
	}
	if strings.TrimSpace(approvalNotes) != "" {
		parts = append(parts, specApprovalHeader+"\n"+bulletLine(approvalNotes))
	}

	base := t.Title + "\n\n" + t.Body
	if len(parts) == 0 {
		return base
	}
	return base + "\n\n" + specDecisionsHeader + "\n\n" + strings.Join(parts, "\n\n")
}

// ownerLines applies the owner line rules (design shape, "Owner line
// rules") to rows, one question's worth of answer and reply rows: a row
// whose author is not the owner is skipped; an answer with no picked
// option, or one belonging to an escalation, is skipped, otherwise it
// renders as renderOwnerRowText does; a reply with an empty trimmed body
// is skipped, otherwise it renders the same way (its body, not an option,
// since it is not of type answer).
func ownerLines(rows []store.MessageRow, options []response.Option, escalation bool) []string {
	var lines []string
	for i := range rows {
		row := rows[i]
		if row.Author != authorYou {
			continue
		}
		switch row.Type {
		case msgTypeAnswer:
			var ap response.AnswerPayload
			if err := json.Unmarshal(row.Payload, &ap); err != nil || ap.Option == nil || escalation {
				continue
			}
			lines = append(lines, renderOwnerRowText(row, options))
		case msgTypeReply:
			if strings.TrimSpace(row.Body) == "" {
				continue
			}
			lines = append(lines, renderOwnerRowText(row, options))
		}
	}
	return lines
}

// renderDecisionBlock renders one question's block (design shape, "A
// question block has up to three parts"): the head line (KEY: TITLE, or
// just TITLE when the key is empty), then, when lines is non-empty, "The
// owner, oldest first:" and one bulleted line per entry, then, when
// decision is non-empty, "Decision: DECISION".
func renderDecisionBlock(q store.MessageRow, lines []string, decision string) string {
	title, _ := questionTitleAndBody(q)
	head := title
	if key := questionKey(q); key != "" {
		head = key + ": " + title
	}

	var sb strings.Builder
	sb.WriteString(head)
	if len(lines) > 0 {
		sb.WriteString("\nThe owner, oldest first:")
		for _, line := range lines {
			sb.WriteString("\n")
			sb.WriteString(bulletLine(line))
		}
	}
	if decision != "" {
		sb.WriteString("\nDecision: " + decision)
	}
	return sb.String()
}
