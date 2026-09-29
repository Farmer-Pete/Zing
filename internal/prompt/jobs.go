package prompt

import (
	"fmt"
	"strconv"
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

// BuildResumeHeader replaces the prompt file on a build resume turn: there
// is no fresh job prompt to load, only this fixed instruction to continue
// the open worktree session (plan section 6.3), byte-for-byte from the
// plan.
const BuildResumeHeader = "Continue this build task in the same worktree. The input below says why " +
	"you were resumed. Finish the task and return the next document."

// buildTaskLine is the literal line prompts/build.md carries for a first
// build turn. ForFix replaces the whole line at once, rather than filling
// its three placeholders individually, since a fix run has no task number
// or title to fill them with (plan section 9.1).
const buildTaskLine = "Task {n} of {total}: {task title}"

// buildPlaceholder pairs one of the five tokens prompts/build.md must
// carry with the value ForBuild fills it with (plan section 12.1,
// section 9.1).
type buildPlaceholder struct {
	token, value string
}

// fillPlaceholder replaces the single occurrence of token in text with
// value. A job prompt missing the token is the fixed error the plan
// names: a caller-supplied build prompt is expected to name each
// placeholder exactly once, so fillPlaceholder does not check for a
// second occurrence.
func fillPlaceholder(text, token, value string) (string, error) {
	if !strings.Contains(text, token) {
		return "", fmt.Errorf("prompt: build prompt lacks placeholder %s", token)
	}
	return strings.Replace(text, token, value, 1), nil
}

// fillPlaceholders applies fillPlaceholder for each pair in order,
// stopping at the first missing placeholder.
func fillPlaceholders(text string, pairs []buildPlaceholder) (string, error) {
	var err error
	for _, p := range pairs {
		text, err = fillPlaceholder(text, p.token, p.value)
		if err != nil {
			return "", err
		}
	}
	return text, nil
}

// BuildTask is one task of an approved plan: the shape ForBuild needs to
// fill the build job prompt's placeholders and compose the task input
// (plan section 9.1).
type BuildTask struct {
	N, Total int
	Title    string // unit.Title without the "Task <n>: " prefix
	Text     string // the task text, raw
	Test     string // the task's named test, raw
}

// buildInputs assembles the inputs shared by ForBuild and ForFix: ticket
// fenced, plan raw, accepted raw (omitted when empty), the task or fix
// input as given, then extra (plan section 9.1).
func buildInputs(ticket, planXML string, accepted []string, taskInput NamedInput, extra []NamedInput) []NamedInput {
	inputs := make([]NamedInput, 0, 3+len(extra))
	inputs = append(inputs,
		NamedInput{Label: labelTicket, Text: ticket, Untrusted: true},
		NamedInput{Label: "plan", Text: planXML},
	)
	if len(accepted) > 0 {
		inputs = append(inputs, NamedInput{Label: "accepted", Text: strings.Join(accepted, "\n")})
	}
	inputs = append(inputs, taskInput)
	inputs = append(inputs, extra...)
	return inputs
}

// ForBuild fills the build job prompt's five placeholders (`{n}`,
// `{total}`, `{task title}`, `{test_cmd}`, `{lint_cmd}`) and lists the
// inputs: ticket (fenced), plan (raw), accepted (raw, omitted when none),
// task (raw, "Task <n> of <total>\nTest: <test>\n\n<text>"), then extra.
// A jobPrompt missing one of the five placeholders is the error
// `prompt: build prompt lacks placeholder <name>`. Called by
// internal/job's build turn (plan section 6.3); calls Assemble once
// Schemas is set from response.RenderTemplate(JobBuild, ...) in build
// schema order.
func ForBuild(jobPrompt string, task BuildTask, testCmd, lintCmd, ticket, planXML string, accepted []string, extra []NamedInput) (Input, error) {
	filled, err := fillPlaceholders(jobPrompt, []buildPlaceholder{
		{"{n}", strconv.Itoa(task.N)},
		{"{total}", strconv.Itoa(task.Total)},
		{"{task title}", task.Title},
		{"{test_cmd}", testCmd},
		{"{lint_cmd}", lintCmd},
	})
	if err != nil {
		return Input{}, err
	}

	taskText := fmt.Sprintf("Task %d of %d\nTest: %s\n\n%s", task.N, task.Total, task.Test, task.Text)
	inputs := buildInputs(ticket, planXML, accepted, NamedInput{Label: "task", Text: taskText}, extra)

	return Input{JobPrompt: filled, Inputs: inputs}, nil
}

// ForFix is ForBuild with the fix input in place of the task: the line
// "Task {n} of {total}: {task title}" becomes "Fix run: <subject>", and
// the task input is replaced by {label, text, fenced}. A jobPrompt
// missing that line, or missing `{test_cmd}` or `{lint_cmd}`, is the same
// fixed error ForBuild returns. Called by internal/job's fix turn (plan
// section 8); calls Assemble once Schemas is set from
// response.RenderTemplate(JobBuild, ...) in build schema order.
func ForFix(jobPrompt, subject, label, text, testCmd, lintCmd, ticket, planXML string, accepted []string, extra []NamedInput) (Input, error) {
	if !strings.Contains(jobPrompt, buildTaskLine) {
		return Input{}, fmt.Errorf("prompt: build prompt lacks placeholder %s", buildTaskLine)
	}
	filled := strings.Replace(jobPrompt, buildTaskLine, "Fix run: "+subject, 1)

	filled, err := fillPlaceholders(filled, []buildPlaceholder{
		{"{test_cmd}", testCmd},
		{"{lint_cmd}", lintCmd},
	})
	if err != nil {
		return Input{}, err
	}

	inputs := buildInputs(ticket, planXML, accepted, NamedInput{Label: label, Text: text, Untrusted: true}, extra)

	return Input{JobPrompt: filled, Inputs: inputs}, nil
}

// ForBuildResume builds a build resume turn's Input: BuildResumeHeader in
// place of a prompt file, inputs passed through unchanged — built by the
// caller with the labeled input the resume reason names (plan section
// 6.3). Called by internal/job's build resume turn; calls Assemble once
// Schemas is set from response.RenderTemplate(JobBuild, ...) in build
// schema order.
func ForBuildResume(inputs []NamedInput) Input {
	return Input{JobPrompt: BuildResumeHeader, Inputs: inputs}
}

// ForPerimeter builds the perimeter job's Input: the job prompt, then the
// path and the hunk, both fenced, then any carried inputs. Called by
// internal/job's perimeter turn (plan section 12.2); calls Assemble once
// Schemas is set from response.RenderTemplate(JobPerimeter, ...) in
// perimeter schema order.
func ForPerimeter(jobPrompt, path, hunk string, extra []NamedInput) Input {
	inputs := make([]NamedInput, 0, 2+len(extra))
	inputs = append(inputs,
		NamedInput{Label: "path", Text: path, Untrusted: true},
		NamedInput{Label: "hunk", Text: hunk, Untrusted: true},
	)
	inputs = append(inputs, extra...)
	return Input{JobPrompt: jobPrompt, Inputs: inputs}
}
