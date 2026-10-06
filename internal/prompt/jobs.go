package prompt

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"zing/internal/response"
)

// ResumeHeader replaces the prompt file on a planning resume turn: there
// is no fresh job prompt to load, only this fixed instruction to continue
// the open session (plan section 6.3, design section 22.6), byte-for-byte
// from the plan.
const ResumeHeader = "Continue this planning session. The owner's messages, the review findings, " +
	"or the errors follow. Answer every owner message, recompute the frontier or revise the plan, " +
	"and return the next document."

// labelTicket is the ticket input's label, shared by every constructor
// that carries one (ForClassify, ForPlanningFirst, ForPlanReview,
// ForBuild, ForFix, ForMerge, ForJudge, ForReview).
const labelTicket = "ticket"

// labelPlan is the plan input's label, shared by every constructor that
// carries one (ForPlanReview, buildInputs, ForReview).
const labelPlan = "plan"

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

// placeholderBuildMinutes is the token ForPlanningFirst fills with the
// build job's timeout, so the planner can size tasks to fit one run (#53).
const placeholderBuildMinutes = "{build_minutes}"

// ForPlanningFirst builds the planning job's first-turn Input: the
// kind-specific prompt with {build_minutes} filled from buildMinutes, the
// machine-configured styles plus the plan checker's rendered rules block
// (response.PlanRules, so the validator and the prompt cannot disagree),
// the ticket fenced, then any carried inputs (answers, notes, and an
// error, all fenced, when starting fresh from a resolution). A jobPrompt
// missing {build_minutes} is the error "prompt: planning prompt lacks
// placeholder {build_minutes}". Called by internal/job's planning first
// turn (plan section 6.2); calls Assemble once Schemas is set from
// response.RenderTemplate(JobPlanning, ...) in planning schema order.
func ForPlanningFirst(jobPrompt string, styles []string, buildMinutes int, ticket string, extra []NamedInput) (Input, error) {
	filled, err := fillPlaceholder(jobPrompt, "planning", placeholderBuildMinutes, strconv.Itoa(buildMinutes))
	if err != nil {
		return Input{}, err
	}

	allStyles := make([]string, 0, len(styles)+1)
	allStyles = append(allStyles, styles...)
	allStyles = append(allStyles, response.PlanRules())

	inputs := make([]NamedInput, 0, 1+len(extra))
	inputs = append(inputs, NamedInput{Label: labelTicket, Text: ticket, Untrusted: true})
	inputs = append(inputs, extra...)
	return Input{JobPrompt: filled, Styles: allStyles, Inputs: inputs}, nil
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
		NamedInput{Label: labelPlan, Text: plan, Untrusted: true},
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

// Check returns the "check" labeled input carrying the output of the
// project's failing test or lint command back into the build session
// (#55), fenced because a command prints whatever the code under test
// makes it print.
func Check(text string) NamedInput { return NamedInput{Label: "check", Text: text, Untrusted: true} }

// Answers returns the "answers" labeled input carrying a cap_resumes
// resolution's preserved answers into a fresh session, fenced along with
// every other owner- or model-originated input (plan section 4.2, D15).
func Answers(text string) NamedInput {
	return NamedInput{Label: "answers", Text: text, Untrusted: true}
}

// Conversation returns the "conversation" labeled input (D31, design
// section 22.6): the owner's undelivered messages, or a fresh session's
// full transcript, fenced, since it carries owner-typed and model-written
// text alike.
func Conversation(text string) NamedInput {
	return NamedInput{Label: "conversation", Text: text, Untrusted: true}
}

// ConfirmHeader replaces the prompt file on the gate's confirming turn
// (D32, design section 22.12.3): the owner has approved, but Zing asks the
// planning session to say so itself before it seals, byte-for-byte from the
// plan.
const ConfirmHeader = "The owner wants to approve this plan and close the gate. Before Zing seals the scenarios, " +
	"say whether any question is still open: a decision the plan depends on that the owner has not made, " +
	"a thread you settled without the owner's word, or anything in the owner's notes below that changes the " +
	"plan. Do not guess an answer to fill a gap. If nothing is open, return confirmed. If something is open, " +
	"ask it as questions; the approval is then cancelled, and the owner gets a fresh gate after you return " +
	"ready. If the plan must change and you need nothing from the owner, return ready with the new plan. " +
	"If the owner's notes ask for any change to the plan, return ready with the revised plan, never confirmed; " +
	"it goes to plan review and a fresh gate. Return confirmed only when the notes change nothing in the " +
	"plan. The builder receives the owner's notes with every task."

// ForPlanningConfirm builds the gate's confirming turn Input: ConfirmHeader
// in place of a prompt file, inputs passed through unchanged -- built by the
// caller with Notes (when the approval carried reply text), Invalid,
// Validation, and Conversation, in that order (design section 22.12.3).
// Called by internal/job's confirming-turn runner; calls Assemble once
// Schemas is set from response.RenderTemplate(JobPlanning, ...) in the
// confirming turn's own schema order (confirmed, questions, ready, then
// question, error).
func ForPlanningConfirm(inputs []NamedInput) Input {
	return Input{JobPrompt: ConfirmHeader, Inputs: inputs}
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

// placeholderTestCmd and placeholderLintCmd are the two tokens ForBuild,
// ForFix, and ForMerge each fill, named once (goconst) since all three
// repeat them.
const (
	placeholderTestCmd = "{test_cmd}"
	placeholderLintCmd = "{lint_cmd}"
)

// placeholderPair pairs one token a job prompt must carry with the value
// fillPlaceholders fills it with: the five of prompts/build.md (plan
// section 12.1, section 9.1), or the two of prompts/review.md, {lens} and
// {sha} (plan section 12.1).
type placeholderPair struct {
	token, value string
}

// fillPlaceholder replaces the single occurrence of token in text with
// value. A job prompt missing the token is the fixed error the plan
// names, kind naming the prompt ("build", "review"): a caller-supplied
// job prompt is expected to name each placeholder exactly once, so
// fillPlaceholder does not check for a second occurrence.
func fillPlaceholder(text, kind, token, value string) (string, error) {
	if !strings.Contains(text, token) {
		return "", fmt.Errorf("prompt: %s prompt lacks placeholder %s", kind, token)
	}
	return strings.Replace(text, token, value, 1), nil
}

// fillPlaceholders applies fillPlaceholder for each pair in order,
// stopping at the first missing placeholder.
func fillPlaceholders(text, kind string, pairs []placeholderPair) (string, error) {
	var err error
	for _, p := range pairs {
		text, err = fillPlaceholder(text, kind, p.token, p.value)
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
// input as given, then extra (plan section 9.1). ticket may carry the
// owner's decisions after the ticket text (job.specFor), including the
// gate's approval notes, so no separate approval input is needed.
func buildInputs(ticket, planXML string, accepted []string, taskInput NamedInput, extra []NamedInput) []NamedInput {
	inputs := make([]NamedInput, 0, 3+len(extra))
	inputs = append(inputs,
		NamedInput{Label: labelTicket, Text: ticket, Untrusted: true},
		NamedInput{Label: labelPlan, Text: planXML},
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
	filled, err := fillPlaceholders(jobPrompt, "build", []placeholderPair{
		{"{n}", strconv.Itoa(task.N)},
		{"{total}", strconv.Itoa(task.Total)},
		{"{task title}", task.Title},
		{placeholderTestCmd, testCmd},
		{placeholderLintCmd, lintCmd},
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
// fixNoChangeLine tells a fix run what to do when the failure is not in
// the code (bug fix: a live fix builder escalated "could not reproduce"
// three times, since nothing told it a no-change outcome was allowed).
const fixNoChangeLine = "If the reported failure does not reproduce against the code, change nothing and return outcome ok with an empty files_changed and a report that says why."

func ForFix(jobPrompt, subject, label, text, testCmd, lintCmd, ticket, planXML string, accepted []string, extra []NamedInput) (Input, error) {
	if !strings.Contains(jobPrompt, buildTaskLine) {
		return Input{}, fmt.Errorf("prompt: build prompt lacks placeholder %s", buildTaskLine)
	}
	filled := strings.Replace(jobPrompt, buildTaskLine, "Fix run: "+subject+"\n"+fixNoChangeLine, 1)

	filled, err := fillPlaceholders(filled, "build", []placeholderPair{
		{placeholderTestCmd, testCmd},
		{placeholderLintCmd, lintCmd},
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

// ForMerge fills the merge job prompt's {test_cmd} and {lint_cmd} and
// lists the inputs: ticket (fenced), plan (raw), conflicts (fenced, one
// path per line, omitted when empty), base_log (fenced), then extra. A
// prompt missing either placeholder is the error
// "prompt: merge prompt lacks placeholder <name>". Called by
// internal/job's merge turn; Schemas is set by the caller from
// response.RenderTemplate(JobBuild, ...), since a merge answers in the
// build response shape.
func ForMerge(jobPrompt, testCmd, lintCmd, ticket, planXML, conflicts, baseLog string, extra []NamedInput) (Input, error) {
	filled, err := fillPlaceholders(jobPrompt, "merge", []placeholderPair{
		{placeholderTestCmd, testCmd},
		{placeholderLintCmd, lintCmd},
	})
	if err != nil {
		return Input{}, err
	}
	inputs := make([]NamedInput, 0, 4+len(extra))
	inputs = append(inputs,
		NamedInput{Label: labelTicket, Text: ticket, Untrusted: true},
		NamedInput{Label: labelPlan, Text: planXML},
	)
	if conflicts != "" {
		inputs = append(inputs, NamedInput{Label: "conflicts", Text: conflicts, Untrusted: true})
	}
	inputs = append(inputs, NamedInput{Label: "base_log", Text: baseLog, Untrusted: true})
	inputs = append(inputs, extra...)
	return Input{JobPrompt: filled, Inputs: inputs}, nil
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

// PerimeterResumeHeader replaces the prompt file on a perimeter resume
// turn: there is no fresh job prompt to load, only this fixed instruction
// to continue describing the file after the owner's answer (plan section
// 6.2), byte-for-byte from the plan.
const PerimeterResumeHeader = "Continue describing this file. The owner's answer follows. Return the next document."

// ForPerimeterResume builds a perimeter resume turn's Input:
// PerimeterResumeHeader in place of a prompt file, inputs passed through
// unchanged -- built by the caller with Answer, so it carries the fencing
// plan section 4.2's table assigns it. Called by internal/job's
// perimeter-question resume (plan section 6.2); calls Assemble once
// Schemas is set from response.RenderTemplate(JobPerimeter, ...) in
// perimeter schema order.
func ForPerimeterResume(inputs []NamedInput) Input {
	return Input{JobPrompt: PerimeterResumeHeader, Inputs: inputs}
}

// CodeLensSection extracts one lens file's "## In code" section (plan
// section 12.1): the piece ForReview's caller (internal/job's review
// round) appends to the review prompt for each lens. Not every lens file
// carries one; problem has no "## In code" section, since it applies only
// to a plan. Mirrors PlanLensSection's search, trimmed at the next "## "
// heading when one follows, though in practice "## In code" is always the
// last section of a lens file.
func CodeLensSection(text string) (string, error) {
	const marker = "## In code"
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

// ForReview builds one lens's review job Input: the job prompt with
// {lens} and {sha} filled, the lens file's "## In code" section appended
// (blank line between), then ticket, plan, and diff, all fenced, then
// extra (D15) — notes (fenced) among them on a retry that carries them
// (plan section 6.2). ticket may carry the owner's decisions after the
// ticket text (job.specFor); the ticket input comes first so a lens
// reads the owner's decisions before the plan and the diff. A jobPrompt
// missing either placeholder is the fixed error `prompt: review prompt
// lacks placeholder <name>`. Called once per lens by internal/job's
// review round (plan section 6.2); calls Assemble once Schemas is set
// from response.RenderTemplate(JobReview, ...) in review schema order.
func ForReview(jobPrompt, lensName, sha, codeSection, ticket, plan, diff string, extra []NamedInput) (Input, error) {
	filled, err := fillPlaceholders(jobPrompt, "review", []placeholderPair{
		{"{lens}", lensName},
		{"{sha}", sha},
	})
	if err != nil {
		return Input{}, err
	}
	filled = strings.TrimRight(filled, "\n") + "\n\n" + strings.TrimRight(codeSection, "\n")

	inputs := make([]NamedInput, 0, 3+len(extra))
	inputs = append(inputs,
		NamedInput{Label: labelTicket, Text: ticket, Untrusted: true},
		NamedInput{Label: labelPlan, Text: plan, Untrusted: true},
		NamedInput{Label: "diff", Text: diff, Untrusted: true},
	)
	inputs = append(inputs, extra...)

	return Input{JobPrompt: filled, Inputs: inputs}, nil
}

// ReviewResumeHeader replaces the prompt file on a review round's CONTINUE
// turn: there is no fresh job prompt to load, only this fixed instruction
// to continue the lens session that asked (plan section 6.2a),
// byte-for-byte from the plan.
const ReviewResumeHeader = "Continue this review. The owner's answers follow. Return the next document."

// ForReviewResume builds a review CONTINUE turn's Input: ReviewResumeHeader
// in place of a prompt file, inputs passed through unchanged -- built by
// the caller with Answers, carrying that lens's own answered round,
// fenced (plan section 6.2a). Called by internal/job's review round
// CONTINUE step; calls Assemble once Schemas is set from
// response.RenderTemplate(JobReview, ...) in review schema order.
func ForReviewResume(inputs []NamedInput) Input {
	return Input{JobPrompt: ReviewResumeHeader, Inputs: inputs}
}

// ReviewDiscussHeader replaces the prompt file on a review DISCUSS resume:
// there is no fresh job prompt to load, only this fixed instruction to
// revise or withdraw a finding from the owner's note (plan section 6.6),
// byte-for-byte from the plan.
const ReviewDiscussHeader = "The owner wants to discuss one of your findings. The finding and the owner's " +
	"note follow. Return ok with the finding revised, or with no finding if you withdraw it. " +
	"Return the next document."

// ForReviewDiscuss builds a review DISCUSS resume's Input:
// ReviewDiscussHeader in place of a prompt file, inputs passed through
// unchanged -- built by the caller with Findings (every finding of the
// group) and Notes (the owner's note per finding), both fenced (plan
// section 6.6). Called by internal/job's DISCUSS step; calls Assemble once
// Schemas is set from response.RenderTemplate(JobReview, ...) in review
// schema order.
func ForReviewDiscuss(inputs []NamedInput) Input {
	return Input{JobPrompt: ReviewDiscussHeader, Inputs: inputs}
}

// ForJudge builds the judge job's Input: the ticket fenced, then any
// carried inputs. Unlike ForReview and ForRespond, it takes no plan, diff,
// or thread parameter at all — the judge never receives the plan (N6) and
// reads its scenarios itself with `zing scenarios`, so neither is ever an
// input (plan section 7.2). Called by internal/job's judging START/RUN
// first turn; calls Assemble once Schemas is set from
// response.RenderTemplate(JobJudge, ...) in judge schema order.
func ForJudge(jobPrompt, ticket string, extra []NamedInput) Input {
	inputs := make([]NamedInput, 0, 1+len(extra))
	inputs = append(inputs, NamedInput{Label: labelTicket, Text: ticket, Untrusted: true})
	inputs = append(inputs, extra...)
	return Input{JobPrompt: jobPrompt, Inputs: inputs}
}

// JudgeResumeHeader replaces the prompt file on a judge resume turn: there
// is no fresh job prompt to load, only this fixed instruction to continue
// the open judge session (plan section 7.2), byte-for-byte from the plan.
const JudgeResumeHeader = "Continue judging. The input below says why you were resumed. Return the next document."

// ForJudgeResume builds a judge resume turn's Input: JudgeResumeHeader in
// place of a prompt file, inputs passed through unchanged -- built by the
// caller with Answers on an answered question, or a "coverage" input
// carrying the marker's error lines, fenced (plan section 7.2). Called by
// internal/job's judging resume turn; calls Assemble once Schemas is set
// from response.RenderTemplate(JobJudge, ...) in judge schema order.
func ForJudgeResume(inputs []NamedInput) Input {
	return Input{JobPrompt: JudgeResumeHeader, Inputs: inputs}
}

// ForRespond builds the respond job's Input: the job prompt, the
// machine-configured styles (D15's prompts/style/prose.md), then plan,
// diff, and threads, all fenced, then extra (plan section 9.2, D15).
// Called once per respond batch by internal/job's RESPOND step; calls
// Assemble once Schemas is set from response.RenderTemplate(JobRespond,
// ...) in respond schema order.
func ForRespond(jobPrompt string, styles []string, plan, diff, threads string, extra []NamedInput) Input {
	inputs := make([]NamedInput, 0, 3+len(extra))
	inputs = append(inputs,
		NamedInput{Label: labelPlan, Text: plan, Untrusted: true},
		NamedInput{Label: "diff", Text: diff, Untrusted: true},
		NamedInput{Label: "threads", Text: threads, Untrusted: true},
	)
	inputs = append(inputs, extra...)
	return Input{JobPrompt: jobPrompt, Styles: styles, Inputs: inputs}
}

// RespondResumeHeader replaces the prompt file on a respond resume turn:
// there is no fresh job prompt to load, only this fixed instruction to
// continue sorting the batch's threads (plan section 9.2), byte-for-byte
// from the plan.
const RespondResumeHeader = "Continue sorting these review threads. The input below says why you were resumed. " +
	"Return the next document."

// Deadline returns the raw "deadline" input telling a build run when it
// ends: "This run ends at 15:04 MST, in N minutes.", N rounded to the
// nearest minute and never below 0, end formatted in its own location.
func Deadline(now, end time.Time) NamedInput {
	minutes := max(int(math.Round(end.Sub(now).Minutes())), 0)
	text := fmt.Sprintf("This run ends at %s, in %d minutes.", end.Format("15:04 MST"), minutes)
	return NamedInput{Label: "deadline", Text: text}
}

// ForRespondResume builds a respond resume turn's Input: RespondResumeHeader
// in place of a prompt file, inputs passed through unchanged -- built by
// the caller with Answers on an answered question, or a "coverage" input
// carrying the marker's error lines, fenced (plan section 9.2). Called by
// internal/job's respond resume turn; calls Assemble once Schemas is set
// from response.RenderTemplate(JobRespond, ...) in respond schema order.
func ForRespondResume(inputs []NamedInput) Input {
	return Input{JobPrompt: RespondResumeHeader, Inputs: inputs}
}
