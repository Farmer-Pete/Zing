package prompt

import (
	"fmt"
	"strings"
)

// ResumeHeader replaces the prompt file on a planning resume turn: there
// is no fresh job prompt to load, only this fixed instruction to continue
// the open session (plan section 6.3), byte-for-byte from the plan.
const ResumeHeader = "Continue this planning session. The owner's answers, the review findings, " +
	"or the errors follow. Recompute the frontier or revise the plan, and return the next document."

// labelTicket is the ticket input's label, shared by every constructor
// that carries one (ForClassify, ForPlanningFirst, ForPlanReview).
const labelTicket = "ticket"

// ForClassify builds the classify job's Input: the ticket fenced, then any
// carried inputs (an invalid-output reason, or an escalation's notes and
// error, or preserved answers), in that order. Called by internal/job's
// classify turn (plan section 6.1); calls Assemble once Schemas is set
// from response.RenderTemplate(JobClassify, ...) in classify schema order.
func ForClassify(jobPrompt, ticket string, extra []NamedInput) Input {
	inputs := make([]NamedInput, 0, 1+len(extra))
	inputs = append(inputs, NamedInput{Label: labelTicket, Text: ticket, Untrusted: true})
	inputs = append(inputs, extra...)
	return Input{JobPrompt: jobPrompt, Inputs: inputs}
}

// ForPlanningFirst builds the planning job's first-turn Input: the
// kind-specific prompt, the machine-configured styles, the ticket fenced,
// then any carried inputs (answers, notes, and an error, all fenced, when
// starting fresh from a resolution). Called by internal/job's planning
// first turn (plan section 6.2); calls Assemble once Schemas is set from
// response.RenderTemplate(JobPlanning, ...) in planning schema order.
func ForPlanningFirst(jobPrompt string, styles []string, ticket string, extra []NamedInput) Input {
	inputs := make([]NamedInput, 0, 1+len(extra))
	inputs = append(inputs, NamedInput{Label: labelTicket, Text: ticket, Untrusted: true})
	inputs = append(inputs, extra...)
	return Input{JobPrompt: jobPrompt, Styles: styles, Inputs: inputs}
}

// ForPlanningResume builds a planning resume turn's Input: ResumeHeader in
// place of a prompt file, no styles, and inputs passed through unchanged —
// built by the caller with Answer, Findings, Validation, Invalid, Notes,
// Answers, and Error so each carries the fencing plan section 4.2's table
// assigns it. Called by internal/job's planning resume turn (plan section
// 6.3, 6.4); calls Assemble once Schemas is set from
// response.RenderTemplate(JobPlanning, ...) in planning schema order.
func ForPlanningResume(inputs []NamedInput) Input {
	return Input{JobPrompt: ResumeHeader, Inputs: inputs}
}

// ForPlanReview builds the plan review job's Input: the job prompt with
// each lens's "## In a plan" section appended (blank line between), then
// the ticket, the scenario cohort, and the plan XML, all fenced (D15),
// then any carried inputs (answers, notes, and an error, fenced, or an
// invalid-output reason, raw). Called by internal/job's review tick (plan
// section 6.5); calls Assemble once Schemas is set from
// response.RenderTemplate(JobPlanreview, ...) in planreview schema order.
func ForPlanReview(jobPrompt string, lensSections []string, ticket, scenarios, plan string, extra []NamedInput) Input {
	blocks := make([]string, 0, 1+len(lensSections))
	blocks = append(blocks, strings.TrimRight(jobPrompt, "\n"))
	for _, section := range lensSections {
		blocks = append(blocks, strings.TrimRight(section, "\n"))
	}

	inputs := make([]NamedInput, 0, 3+len(extra))
	inputs = append(inputs,
		NamedInput{Label: labelTicket, Text: ticket, Untrusted: true},
		NamedInput{Label: "scenarios", Text: scenarios, Untrusted: true},
		NamedInput{Label: "plan", Text: plan, Untrusted: true},
	)
	inputs = append(inputs, extra...)

	return Input{JobPrompt: strings.Join(blocks, "\n\n"), Inputs: inputs}
}

// PlanLensSection extracts one lens file's "## In a plan" section (design
// section 6.5): the piece ForPlanReview's caller (internal/job's review
// tick) appends to the planreview prompt, not the whole lens file, which may
// also carry an "## In code" section for Package 8's review job. Moved here
// from golden_test.go's own copy (TASK 3), which now delegates to this one
// instead of keeping a duplicate.
func PlanLensSection(text string) (string, error) {
	const marker = "## In a plan"
	start := strings.Index(text, marker)
	if start < 0 {
		return "", fmt.Errorf("prompt: lens file has no %q section", marker)
	}
	rest := text[start:]
	if next := strings.Index(rest[len(marker):], "\n## "); next >= 0 {
		rest = rest[:len(marker)+next]
	}
	return strings.TrimRight(rest, "\n"), nil
}

// Answer returns the "answer" labeled input for one resumed question: the
// key, title, chosen option, and any typed reply text, fenced, since it
// carries whatever the owner typed (plan section 4.2, D15).
func Answer(text string) NamedInput { return NamedInput{Label: "answer", Text: text, Untrusted: true} }

// Findings returns the "findings" labeled input carrying a plan review's
// findings back into planning, fenced because it is model-written text
// re-entering a model (plan section 4.2, D15).
func Findings(text string) NamedInput {
	return NamedInput{Label: "findings", Text: text, Untrusted: true}
}

// Validation returns the "validation" labeled input carrying ready-check
// errors by path back into planning, fenced because CheckCodeClaims
// embeds model-supplied evidence paths (plan section 4.2, D15).
func Validation(text string) NamedInput {
	return NamedInput{Label: "validation", Text: text, Untrusted: true}
}

// Invalid returns the "invalid" labeled input carrying an invalid-output
// reason. It is raw, never fenced: its text is always one of the five
// closed reason sentences (design section 14), never owner- or
// model-supplied prose (plan section 4.2, D15).
func Invalid(reason string) NamedInput {
	return NamedInput{Label: "invalid", Text: reason, Untrusted: false}
}

// Notes returns the "notes" labeled input carrying an owner's free-reply
// text, fenced (plan section 4.2, D15).
func Notes(text string) NamedInput { return NamedInput{Label: "notes", Text: text, Untrusted: true} }

// Error returns the "error" labeled input carrying an escalation's what,
// why, and tried back into a resumed session, fenced because it is
// model-written text re-entering a model (plan section 4.2, D15).
func Error(text string) NamedInput { return NamedInput{Label: "error", Text: text, Untrusted: true} }

// Answers returns the "answers" labeled input carrying a cap_resumes
// resolution's preserved answers into a fresh session, fenced along with
// every other owner- or model-originated input (plan section 4.2, D15).
func Answers(text string) NamedInput {
	return NamedInput{Label: "answers", Text: text, Untrusted: true}
}
