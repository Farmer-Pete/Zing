package response

import "encoding/xml"

// ---- every response starts with this ------------------------------------

type Head struct {
	XMLName xml.Name `xml:"zing"          json:"-"`
	Job     Job      `xml:"job,attr"      json:"job"     doc:"the running job; must match"`
	Outcome Outcome  `xml:"outcome,attr"  json:"outcome" doc:"one of this job's outcomes, or question, or error"`
}

// ---- universal outcomes, any job ---------------------------------------

type QuestionResponse struct {
	Head
	Questions []Question `xml:"question"  json:"questions" jsonschema:"minItems=1" doc:"one per decision you need"`
	Progress  string     `xml:"progress"  json:"progress"  doc:"what is done so far, so the resumed run can continue"`
}

type Question struct {
	Key         string   `xml:"key,attr"    json:"key"         jsonschema:"pattern=^[qQ][0-9]+$"`
	Title       string   `xml:"title"       json:"title"       jsonschema:"minLength=1" doc:"short"`
	Body        string   `xml:"body"        json:"body"        jsonschema:"minLength=1" doc:"what you need to know and why, markdown"`
	Options     []Option `xml:"option"      json:"options"     jsonschema:"maxItems=4"  doc:"none, or two to four"`
	Recommended string   `xml:"recommended" json:"recommended" jsonschema:"minLength=1" doc:"an option key or free text, with the reason"`
}

type Option struct {
	Key  string `xml:"key,attr"  json:"key"  jsonschema:"pattern=^[a-z]$"`
	Text string `xml:",chardata" json:"text" jsonschema:"minLength=1"`
}

type ErrorResponse struct {
	Head
	Error RunError `xml:"error" json:"error"`
}

type RunError struct {
	Code  ErrorCode `xml:"code,attr" json:"code"`
	What  string    `xml:"what"      json:"what"  jsonschema:"minLength=1" doc:"what could not be done"`
	Why   string    `xml:"why"       json:"why"   jsonschema:"minLength=1" doc:"why, with evidence"`
	Tried string    `xml:"tried"     json:"tried" doc:"what was tried"`
}

// JudgeRunError is the judge's own error outcome: RunError plus an optional
// amendment, allowed only with code cannot_run (checkJudgeAmendment).
type JudgeRunError struct {
	RunError
	Amendment *Amendment `xml:"amendment,omitempty" json:"amendment,omitempty" doc:"cannot_run only: your rewrite of the one sealed scenario whose check is wrong as written"`
}

// JudgeErrorResponse is the judge's own error response, registered for the
// pair job judge and outcome error. Every other job keeps ErrorResponse.
type JudgeErrorResponse struct {
	Head
	Error JudgeRunError `xml:"error" json:"error"`
}

// Amendment is the judge's proposed rewrite of one sealed scenario whose
// check is wrong as written (JudgeRunError.Amendment).
type Amendment struct {
	Scenario string       `xml:"scenario,attr" json:"scenario" jsonschema:"pattern=^s[0-9]+$"`
	Kind     ScenarioKind `xml:"kind,attr,omitempty" json:"kind,omitempty" doc:"omit to keep the scenario's current kind"`
	Given    string       `xml:"given"  json:"given"  jsonschema:"minLength=1" doc:"the starting state"`
	When     string       `xml:"when"   json:"when"   jsonschema:"minLength=1" doc:"the action"`
	Then     string       `xml:"then"   json:"then"   jsonschema:"minLength=1" doc:"what a stranger would observe, from outside the code"`
	Check    string       `xml:"check"  json:"check"  jsonschema:"minLength=1" doc:"one command whose exit code decides it"`
	Reason   string       `xml:"reason" json:"reason" jsonschema:"minLength=1" doc:"why the sealed check is wrong as written"`
}

// ---- classify ------------------------------------------------------------

type ClassifyResponse struct { // outcome: bug | feature
	Head
	Reason string `xml:"reason" json:"reason" jsonschema:"minLength=1" doc:"one sentence: the fact that decided it"`
}

// ---- planning -----------------------------------------------------------

type Claim struct {
	Kind     ClaimKind    `xml:"kind,attr"     json:"kind"`
	Verdict  ClaimVerdict `xml:"verdict,attr"  json:"verdict"`
	Evidence string       `xml:"evidence,attr" json:"evidence" doc:"for a code claim, a path you read, with a line where one applies"`
	Text     string       `xml:",chardata"     json:"text"     jsonschema:"minLength=1"`
}

type Scenario struct {
	ID    string       `xml:"id,attr"              json:"id"              jsonschema:"pattern=^s[0-9]+$"`
	Kind  ScenarioKind `xml:"kind,attr"            json:"kind"`
	Check string       `xml:"check,attr,omitempty" json:"check_cmd"       doc:"one command whose exit code decides it"`
	Given string       `xml:"given"                json:"given"           jsonschema:"minLength=1" doc:"the starting state"`
	When  string       `xml:"when"                 json:"when"            jsonschema:"minLength=1" doc:"the action"`
	Then  string       `xml:"then"                 json:"then"            jsonschema:"minLength=1" doc:"what a stranger would observe, from outside the code"`
}

type NothingToDoResponse struct {
	Head
	Claims []Claim `xml:"claims>claim" json:"claims" jsonschema:"minItems=1" doc:"every code claim is false"`
	Notes  string  `xml:"notes"        json:"notes"  doc:"why there is nothing to build"`
	Conversation
}

type ChildrenResponse struct {
	Head
	Children []Child `xml:"child" json:"children" jsonschema:"minItems=2" doc:"tickets that can each be built and verified alone"`
	Notes    string  `xml:"notes" json:"notes"    doc:"the shared architecture, markdown"`
	Conversation
}

type Child struct {
	Key       string   `xml:"key,attr"   json:"key"        jsonschema:"pattern=^c[0-9]+$"`
	Title     string   `xml:"title"      json:"title"      jsonschema:"minLength=1"`
	Body      string   `xml:"body"       json:"body"       jsonschema:"minLength=1" doc:"specific enough to build alone, markdown"`
	DependsOn []string `xml:"depends_on" json:"depends_on" doc:"keys of other children; no cycles"`
}

type ReadyResponse struct {
	Head
	Claims    []Claim    `xml:"claims>claim"       json:"claims"    jsonschema:"minItems=1"`
	Scenarios []Scenario `xml:"scenarios>scenario" json:"scenarios" jsonschema:"minItems=2,maxItems=30"`
	Plan      Plan       `xml:"plan"               json:"plan"`
	Conversation
}

// The plan. Four parts. The console shows each part as a heading and each child as a sub-heading.
type Plan struct {
	Overview     Overview      `xml:"overview"                json:"overview" doc:"for the owner first; a reader with no prior context must follow it"`
	Design       Design        `xml:"design"                  json:"design"`
	Delivery     Delivery      `xml:"delivery"                json:"delivery"`
	Review       Review        `xml:"review"                  json:"review"`
	Dispositions []Disposition `xml:"dispositions>disposition" json:"dispositions,omitempty" doc:"one per finding in the needs_disposition input, or named in a validation message; leave out when there is none"`
}

// Disposition is the planner's fixed-or-disputed call on one finding the
// planning resume's needs_disposition input required an answer for.
type Disposition struct {
	Finding string          `xml:"finding,attr" json:"finding" jsonschema:"pattern=^p[0-9]+-f[0-9]+$" doc:"the id of a finding in the needs_disposition input, such as p2-f1"`
	Kind    DispositionKind `xml:"kind,attr"    json:"kind"`
	Path    string          `xml:"path,attr,omitempty" json:"path,omitempty" doc:"fixed only: the plan element path you changed, such as plan/delivery/tasks/task[2]"`
	Reason  string          `xml:",chardata"    json:"reason,omitempty" doc:"disputed only: why the finding is wrong"`
}

type Overview struct {
	Objective string   `xml:"objective"         json:"objective" jsonschema:"minLength=1" doc:"one sentence a non-engineer could read"`
	Context   string   `xml:"context"           json:"context"   jsonschema:"minLength=1" doc:"the part of the system this change lands in, markdown"`
	Problem   Problem  `xml:"problem"           json:"problem"`
	Goals     []string `xml:"goals>goal"        json:"goals"     jsonschema:"minItems=1" doc:"specific and testable"`
	NonGoals  []string `xml:"nongoals>nongoal"  json:"nongoals"  jsonschema:"minItems=1" doc:"what the 80/20 version leaves out, and what this change will never do"`
}

type Problem struct {
	Text       string       `xml:",chardata"              json:"text"       jsonschema:"minLength=1" doc:"what is broken or missing, and why it matters, markdown"`
	Loop       *Loop        `xml:"loop,omitempty"         json:"loop,omitempty"       doc:"bug only: one command that goes red on this bug"`
	Repro      string       `xml:"repro,omitempty"        json:"repro,omitempty"      doc:"bug only: the minimised reproduction, every element load bearing"`
	Hypotheses []Hypothesis `xml:"hypotheses>hypothesis"  json:"hypotheses,omitempty" jsonschema:"maxItems=5" doc:"bug only: three to five, ranked"`
}

type Loop struct {
	Cmd  string `xml:"cmd,attr"  json:"cmd"  jsonschema:"minLength=1"`
	Text string `xml:",chardata" json:"text" doc:"what it asserts, and its output when run"`
}

type Hypothesis struct {
	Rank       int    `xml:"rank,attr"  json:"rank"       jsonschema:"minimum=1,maximum=5"`
	Cause      string `xml:"cause"      json:"cause"      jsonschema:"minLength=1" doc:"what is wrong"`
	Prediction string `xml:"prediction" json:"prediction" jsonschema:"minLength=1" doc:"if this is the cause, then changing X makes the bug disappear"`
}

type Design struct {
	Demo       Demo       `xml:"demo"            json:"demo"`
	Shape      string     `xml:"shape"           json:"shape"      jsonschema:"minLength=1" doc:"components, interfaces, data flow, lifecycles; markdown with at least one mermaid block"`
	Changes    []Change   `xml:"changes>change"  json:"changes"    doc:"one per new or changed function or type, so the reader sees how it fits with the code around it"`
	Types      []TypeDef  `xml:"types>type"      json:"types"      doc:"every new or changed type"`
	Migrations Migrations `xml:"migrations"      json:"migrations"`
}

type Demo struct {
	Cmd  string `xml:"cmd,attr"  json:"cmd"  jsonschema:"minLength=1" doc:"the command that shows it"`
	Text string `xml:",chardata" json:"text" jsonschema:"minLength=1" doc:"the smallest slice that runs end to end and shows the value; what the owner will see"`
}

type Change struct {
	Path       string     `xml:"path,attr"              json:"path"`
	Symbol     string     `xml:"symbol,attr"            json:"symbol"`
	Kind       ChangeKind `xml:"kind,attr"              json:"kind"`
	Callers    string     `xml:"callers"                json:"callers"     jsonschema:"minLength=1" doc:"what calls it, by name"`
	Callees    string     `xml:"callees"                json:"callees"     jsonschema:"minLength=1" doc:"what it calls, by name"`
	Before     string     `xml:"before"                 json:"before"      doc:"the code today, or none"`
	After      string     `xml:"after"                  json:"after"       jsonschema:"minLength=1" doc:"the code after"`
	SimpleCall string     `xml:"simple_call,omitempty"  json:"simple_call,omitempty" doc:"one line showing the simple case; required for a public function"`
}

type TypeDef struct {
	Name        string       `xml:"name,attr"  json:"name"`
	File        string       `xml:"file,attr"  json:"file"`
	Kind        ChangeKind   `xml:"kind,attr"  json:"kind"`
	Fields      []Field      `xml:"field"      json:"fields"      jsonschema:"minItems=1" doc:"every field with exact type, format, and constraints"`
	Transitions []Transition `xml:"transition" json:"transitions" doc:"every valid transition, for any state field"`
}

type Field struct {
	Name        string `xml:"name,attr"        json:"name"`
	Type        string `xml:"type,attr"        json:"type"`
	Required    bool   `xml:"required,attr"    json:"required"`
	Default     string `xml:"default,attr"     json:"default"     doc:"empty when none"`
	Constraints string `xml:"constraints,attr" json:"constraints" doc:"allowed values, min, max, format"`
}

type Transition struct {
	From string `xml:"from,attr" json:"from"`
	To   string `xml:"to,attr"   json:"to"`
	When string `xml:"when,attr" json:"when" doc:"the event that causes it"`
}

type Migrations struct {
	None  bool        `xml:"none,attr,omitempty" json:"-"          doc:"write none=\"true\" when there are no migrations"`
	Items []Migration `xml:"migration"           json:"migrations" doc:"one per migration, each in full"`
}

type Migration struct {
	File     string `xml:"file,attr" json:"file"`
	Schema   string `xml:"schema"    json:"schema"   jsonschema:"minLength=1" doc:"the DDL: tables, columns, indexes, constraints, defaults, enums"`
	Backfill string `xml:"backfill"  json:"backfill" jsonschema:"minLength=1" doc:"any data transform and the rows it touches, or none"`
	Locks    string `xml:"locks"     json:"locks"    jsonschema:"minLength=1" doc:"lock scope, long index builds, hot tables, rollout effects"`
	Compat   string `xml:"compat"    json:"compat"   jsonschema:"minLength=1" doc:"forward and backward compatibility during a rolling deploy"`
	Rollback string `xml:"rollback"  json:"rollback" jsonschema:"minLength=1" doc:"how to undo it"`
}

type Delivery struct {
	Files     []FileChange `xml:"files>file"  json:"files" jsonschema:"minItems=1" doc:"every path the build will create, modify, or delete; this is the perimeter"`
	Deletions Deletions    `xml:"deletions"   json:"deletions"`
	Tests     []TestCase   `xml:"tests>test"  json:"tests" jsonschema:"minItems=1" doc:"integration tests at every new or changed cut point first; unit tests only for parsers and pure functions; for a bug, the regression test first"`
	Tasks     []Task       `xml:"tasks>task"  json:"tasks" jsonschema:"minItems=1,maxItems=12" doc:"in build order; the working demo's tasks first"`
}

type FileChange struct {
	Path   string     `xml:"path,attr"   json:"path"`
	Action FileAction `xml:"action,attr" json:"action"`
	Task   string     `xml:"task,attr"   json:"task,omitempty" jsonschema:"pattern=^[1-9][0-9]?( [1-9][0-9]?)*$" doc:"the n of every task that changes this file, space-separated, such as 1 or 1 3"`
	Reason string     `xml:",chardata"   json:"reason" jsonschema:"minLength=1" doc:"one line"`
}

type Deletions struct {
	None  bool    `xml:"none,attr,omitempty" json:"-"         doc:"write none=\"true\" when nothing is removed"`
	Items []Fence `xml:"fence"               json:"deletions" doc:"one per removed function, file, flag, or behavior; Chesterton's fence"`
}

type Fence struct {
	Path           string `xml:"path,attr"   json:"path"`
	Symbol         string `xml:"symbol,attr" json:"symbol"`
	ExistedBecause string `xml:",chardata"   json:"existed_because" jsonschema:"pattern=existed because" doc:"why it existed, in the words existed because"`
}

type TestCase struct {
	Name    string   `xml:"name,attr"  json:"name"    jsonschema:"minLength=1"`
	Seam    string   `xml:"seam,attr"  json:"seam"    jsonschema:"minLength=1" doc:"the cut point or unit it sits at"`
	Kind    TestKind `xml:"kind,attr"  json:"kind"`
	Mocks   string   `xml:"mocks,attr" json:"mocks"   doc:"cut points mocked, by name, or empty"`
	Asserts string   `xml:",chardata"  json:"asserts" jsonschema:"minLength=1" doc:"what it asserts"`
}

type Task struct {
	N    int    `xml:"n,attr"    json:"n"    jsonschema:"minimum=1,maximum=12"`
	Test string `xml:"test,attr" json:"test" jsonschema:"minLength=1" doc:"the test written before this task"`
	Demo bool   `xml:"demo,attr" json:"demo" doc:"true for the working demo's tasks"`
	Text string `xml:",chardata" json:"text" jsonschema:"minLength=1" doc:"what to build, exactly, markdown"`
}

type Review struct {
	TrustRoot    string   `xml:"trust_root"                json:"trust_root"   jsonschema:"minLength=1" doc:"which trust-root or style-guide paths are in files, and why, or none"`
	Alternatives []string `xml:"alternatives>alternative"  json:"alternatives" jsonschema:"minItems=1" doc:"an approach that was on the table, and why the plan does not use it"`
	Risks        []string `xml:"risks>risk"                json:"risks"        jsonschema:"minItems=1" doc:"a risk or open question"`
}

// ---- planning conversation (D31) -------------------------------------

// Conversation is embedded in every planning outcome that can answer the
// owner: questions (and planning's universal question), ready, children,
// and nothing_to_do. error carries none.
type Conversation struct {
	Replies []Reply `xml:"replies>reply" json:"replies" doc:"none, or one per question thread you answer this turn"`
}

// ReplyList returns the replies, the one accessor internal/job reads.
func (c Conversation) ReplyList() []Reply { return c.Replies }

type Reply struct {
	Question string `xml:"question,attr"           json:"question"           jsonschema:"pattern=^Q[0-9]+$"            doc:"the question's key as Zing showed it, such as Q7, never your own q1"`
	Settled  bool   `xml:"settled,attr,omitempty"  json:"settled,omitempty"  doc:"write settled=\"true\" to close the thread with decision"`
	Decision string `xml:"decision,attr,omitempty" json:"decision,omitempty" jsonschema:"minLength=1,maxLength=500" doc:"required when settled: the decision taken, one sentence"`
	Text     string `xml:",chardata"               json:"text"               jsonschema:"minLength=1"                  doc:"your reply to the owner, markdown"`
}

type PlanningQuestionsResponse struct { // outcome: questions | question (planning only)
	Head
	Questions []Question `xml:"question" json:"questions" jsonschema:"minItems=1" doc:"one per decision you need"`
	Progress  string     `xml:"progress" json:"progress"  doc:"what is done so far, so the resumed run can continue"`
	Conversation
}

type RepliesResponse struct { // outcome: replies (planning only)
	Head
	Replies  []Reply `xml:"replies>reply" json:"replies"  jsonschema:"minItems=1" doc:"one per question thread you answer this turn"`
	Progress string  `xml:"progress"      json:"progress" doc:"what is done so far, so the resumed run can continue"`
}

func (r *RepliesResponse) ReplyList() []Reply { return r.Replies }

// ConfirmedResponse is the confirming turn's own clean answer (D32, design
// section 22.12.3): the gate's approval resumes the planning session with a
// fixed question asking whether anything is still open, and this outcome
// says no. Notes is the agent's own one-or-two-sentence reason; Conversation
// rides along like every other planning outcome's (a confirming turn can
// still receive an owner message, on the rare resume after a crash).
type ConfirmedResponse struct { // outcome: confirmed (planning only, the confirming turn)
	Head
	Notes string `xml:"notes" json:"notes" jsonschema:"minLength=1" doc:"why no question is open, one or two sentences"`
	Conversation
}

// ---- plan review and code review -----------------------------------------

type FindingsResponse struct { // planreview and review, outcome ok
	Head
	Findings []Finding `xml:"finding" json:"findings"`
}

type Finding struct {
	Lens         Lens      `xml:"lens,attr"           json:"lens"`
	Severity     Severity  `xml:"severity,attr"       json:"severity"`
	Location     string    `xml:"location,attr"       json:"location" jsonschema:"minLength=1" doc:"plan: an element path such as plan/delivery/tasks/task[3]; code: path:line inside the diff"`
	Text         string    `xml:"text"                json:"text"     jsonschema:"minLength=1" doc:"what is wrong or unanswered"`
	Fix          string    `xml:"fix"                 json:"fix"      jsonschema:"minLength=1" doc:"what to change"`
	PlanRef      string    `xml:"plan_ref,omitempty"  json:"plan_ref,omitempty" doc:"code review, fidelity lens only: the plan element"`
	Decision     *Decision `xml:"-"                   json:"decision,omitempty"  doc:"set by the owner at triage"`
	ID           string    `xml:"-"                   json:"id,omitempty"        jsonschema:"pattern=^p[0-9]+-f[0-9]+$" doc:"plan review only, set by Zing when it stores the artifact"`
	Reopens      string    `xml:"-" json:"reopens,omitempty"       jsonschema:"pattern=^p[0-9]+-f[0-9]+$" doc:"plan review only, the id of the previous review's finding that this one raises again at the same location"`
	ReopensAfter string    `xml:"-" json:"reopens_after,omitempty" jsonschema:"enum=fixed,enum=no_disposition" doc:"plan review only, set exactly when reopens is set: what the plan did with the earlier finding"`
}

// ---- build ----------------------------------------------------------------

type BuildResponse struct { // outcome ok
	Head
	Claims BuildClaims  `xml:"claims" json:"claims"`
	Extras []ExtraClaim `xml:"extra"  json:"extras" doc:"one per changed path the plan does not declare"`
	Fences []Fence      `xml:"fence"  json:"fences" doc:"one per deletion this task made"`
	Report string       `xml:"report" json:"report" jsonschema:"minLength=1" doc:"what was built, what was decided, markdown"`
	Notes  string       `xml:"notes"  json:"notes"  doc:"anything the owner must know"`
}

// ExtraClaim is the builder's own justification for one undeclared path.
type ExtraClaim struct {
	Path   string `xml:"path,attr" json:"path"   jsonschema:"minLength=1"`
	Reason string `xml:",chardata" json:"reason" jsonschema:"minLength=1" doc:"why this task cannot be done without this path"`
}

type BuildClaims struct {
	FilesChanged []string `xml:"files_changed>path" json:"files_changed" doc:"every changed path, or none for a fix run whose failure is not in the code; the program diffs the tree"`
	// Fence: TestExit and LintExit existed because the builder ran the
	// full suite and CHECK compared its claimed exits with a re-run; Zing
	// now measures them itself and sends the output back (#55).
}

// ---- perimeter, judge, side ----------------------------------------------

type PerimeterResponse struct { // outcome ok
	Head
	Reason string `xml:"reason" json:"reason" jsonschema:"minLength=1" doc:"one sentence: what the change does"`
}

type JudgeResponse struct { // outcome ok
	Head
	Verdicts []Verdict `xml:"verdict" json:"verdicts" jsonschema:"minItems=1" doc:"exactly one per scenario id"`
}

type Verdict struct {
	Scenario string `xml:"scenario,attr" json:"scenario_id" jsonschema:"pattern=^s[0-9]+$"`
	Result   Result `xml:"result,attr"   json:"result"`
	Evidence string `xml:"evidence"      json:"evidence" jsonschema:"minLength=1" doc:"the command you ran and what you observed"`
}

type RespondResponse struct { // outcome ok
	Head
	Threads []ThreadAction `xml:"thread" json:"threads" jsonschema:"minItems=1,maxItems=1000" doc:"exactly one per unresolved review thread"`
}

type ThreadAction struct {
	ID     string     `xml:"id,attr"     json:"id"     doc:"the review thread id from the inputs"`
	Action ThreadVerb `xml:"action,attr" json:"action"`
	Text   string     `xml:",chardata"   json:"text"   jsonschema:"minLength=1" doc:"fix: what to change; reply: the reply; nit: the reply; addressed: the commit that covers it"`
}

type SideResponse struct { // outcome ok
	Head
	Answer string `xml:"answer" json:"answer" jsonschema:"minLength=1" doc:"markdown, with the evidence cited"`
}

// ---- stored only: message payloads (no xml) --------------------------------

type QuestionPayload struct {
	Key         string        `json:"key"         jsonschema:"pattern=^Q[0-9]+$"`
	Kind        QuestionKind  `json:"kind"`
	State       QuestionState `json:"state"`
	Recommended string        `json:"recommended" jsonschema:"minLength=1"`
	Options     []Option      `json:"options"     jsonschema:"maxItems=4"`
	Items       []Item        `json:"items,omitempty" doc:"perimeter and review: one per file or finding"`
	Amendment   *Amendment    `json:"amendment,omitempty" doc:"copied by escalateTx from the escalation payload; the console reads it to prefill the edit box"`
}

type Item struct {
	Ref      string    `json:"ref"  doc:"a path, or a finding id"`
	Text     string    `json:"text"`
	Decision *Decision `json:"decision,omitempty"`
}

type AnswerPayload struct {
	Option *string             `json:"option,omitempty" jsonschema:"pattern=^[a-z]$"`
	Items  map[string]Decision `json:"items,omitempty"  doc:"ref to decision, for perimeter and review"`
}

type EscalationPayload struct {
	Code      string     `json:"code"    jsonschema:"enum=resumes_exhausted,enum=loops_exhausted,enum=wall_clock,enum=usage_hold,enum=plan_gap,enum=cannot_run,enum=environment,enum=other,enum=split_unsupported,enum=nothing_to_do_with_true_claims,enum=runtime_exec_failed,enum=response_invalid,enum=seal_failed,enum=post_run_failed,enum=sandbox_unavailable,enum=replan_unsupported,enum=pr_closed"`
	What      string     `json:"what"    jsonschema:"minLength=1"`
	Why       string     `json:"why"     jsonschema:"minLength=1"`
	Tried     string     `json:"tried"`
	Options   []string   `json:"options" jsonschema:"enum=retry,enum=planning,enum=abandon"`
	SessionID *int64     `json:"session_id,omitempty" doc:"the session the escalated run belongs to, when a run or session caused it"`
	Origin    string     `json:"origin"  jsonschema:"enum=classify,enum=planning_first,enum=planning_resume,enum=planreview,enum=gate_approve,enum=seal,enum=cap_resumes,enum=cap_loops,enum=cap_budget,enum=split,enum=nothing_to_do_claims,enum=build,enum=perimeter,enum=fix,enum=review,enum=judge,enum=shipping,enum=respond" doc:"the step that produced this escalation (design section 6.7)"`
	Grant     *FileGrant `json:"grant,omitempty" doc:"set only by attachFileGrant; when set, escalateTx adds option d"`
	Amendment *Amendment `json:"amendment,omitempty" doc:"set only by judgeErrorCommit for a usable amendment, with Kind already resolved; stored escalations without it stay valid"`
}

// FileGrant is one task number plus the delivery file paths another task's
// CHECK rejected it for: escalateTx's option d, and grantPlanFilesTx's and
// GrantFileTasks' input, for letting Task also change Paths (plan #51).
type FileGrant struct {
	Task  int      `json:"task"  jsonschema:"minimum=1,maximum=12" doc:"the unit number the files are granted to"`
	Paths []string `json:"paths" jsonschema:"minItems=1" doc:"a repository-relative path as it appears in delivery.files; unique; marker order"`
}

type StatePayload struct {
	From   TicketState `json:"from"`
	To     TicketState `json:"to"`
	Reason string      `json:"reason" jsonschema:"minLength=1"`
}

// ---- stored only: artifact payloads that add to a response type -----------

type FileArtifact struct {
	FileChange
	TrustRoot   bool               `json:"trust_root"`
	StyleGuide  bool               `json:"style_guide"`
	TaskN       int                `json:"task_n"                jsonschema:"minimum=0,maximum=12" doc:"0 for a fix run"`
	Description string             `json:"description,omitempty" doc:"the perimeter run's sentence"`
	Decision    *PerimeterDecision `json:"decision,omitempty"`
}

type TaskArtifact struct {
	Task
	Title     string    `json:"title"`
	State     TaskState `json:"state"`
	CommitSHA *string   `json:"commit_sha,omitempty" jsonschema:"pattern=^[0-9a-f]{40}$"`
}

// BuildReport is the stored form of one build run that returned ok, and,
// with CommitSHA set, the record that the unit landed.
type BuildReport struct {
	TaskN int `json:"task_n" jsonschema:"minimum=0,maximum=12" doc:"0 for a fix run"`
	BuildClaims
	Extras    []ExtraClaim `json:"extras"`
	Fences    []Fence      `json:"fences"`
	Report    string       `json:"report"`
	Title     string       `json:"title"                jsonschema:"minLength=1" doc:"the commit subject"`
	CommitSHA *string      `json:"commit_sha,omitempty" jsonschema:"pattern=^[0-9a-f]{40}$"`
	// LegacyTestExit and LegacyLintExit keep a report stored before #55
	// valid: the builder claimed both exits then. Zing never writes them
	// now; a stored value is carried through unchanged.
	LegacyTestExit *int `json:"test_exit,omitempty" doc:"legacy, before #55; never written now"`
	LegacyLintExit *int `json:"lint_exit,omitempty" doc:"legacy, before #55; never written now"`
}

// FindingArtifact is the stored form of one code-review finding (artifact
// type "finding"). Rows are append-only; the newest row per ID wins.
type FindingArtifact struct {
	// The six fields of Finding, copied rather than embedded, so Decision can
	// have its own narrower type:
	Lens        Lens             `json:"lens"`
	Severity    Severity         `json:"severity"`
	Location    string           `json:"location" jsonschema:"minLength=1"`
	Text        string           `json:"text"     jsonschema:"minLength=1"`
	Fix         string           `json:"fix"      jsonschema:"minLength=1"`
	PlanRef     string           `json:"plan_ref,omitempty"`
	Decision    *FindingDecision `json:"decision,omitempty" doc:"set when routed: accept at or below the floor, else the owner's triage"`
	OwnerPicked bool             `json:"owner_picked,omitempty" doc:"true when TRIAGE stored the owner's own item decision; absent for a default accept, a floor accept, a lens row, or a row stored before this field"`

	Held       bool     `json:"held,omitempty"       doc:"a lens output kept while the round waits on a lens question (6.2a); not yet filtered, never routed"`
	ID         string   `json:"id"                   jsonschema:"pattern=^r[1-9][0-9]*[fh][1-9][0-9]*$" doc:"r<round>f<k>, or r<round>h<k> for a held row"`
	Round      int      `json:"round"                jsonschema:"minimum=1" doc:"no maximum: the agent budget bounds rounds"`
	SHA        string   `json:"sha"                  jsonschema:"pattern=^[0-9a-f]{40}$" doc:"the frozen head the lenses read"`
	Lenses     []Lens   `json:"lenses"               jsonschema:"minItems=1,maxItems=7" doc:"every lens that reported this location, in lens order"`
	Supersedes []string `json:"supersedes,omitempty" doc:"the discussed findings of the batch this one revises (6.6); each matches ^r[1-9][0-9]*f[1-9][0-9]*$"`
}

// VerdictArtifact is the stored form of one judge verdict (artifact type
// "verdict"). Rows are append-only; the newest row per (Round, Scenario) wins.
type VerdictArtifact struct {
	Verdict                // scenario_id, result, evidence
	Kind      ScenarioKind `json:"kind"`
	Round     int          `json:"round"                jsonschema:"minimum=1" doc:"no maximum: the agent budget bounds rounds"`
	SHA       string       `json:"sha"                  jsonschema:"pattern=^[0-9a-f]{40}$"`
	CheckExit *int         `json:"check_exit,omitempty" jsonschema:"minimum=-1,maximum=255" doc:"set on the row a check re-run wrote; -1 means timed out"`
}

// RespondArtifact is the stored form of one respond run (artifact type
// "respond"): one action per thread of the batch.
type RespondArtifact struct {
	Threads []ThreadAction `json:"threads" jsonschema:"minItems=1,maxItems=1000"`
	Batch   int            `json:"batch"   jsonschema:"minimum=1" doc:"no maximum: the number of batches is bounded by the agent budget (section 21)"`
	SHA     string         `json:"sha"     jsonschema:"pattern=^[0-9a-f]{40}$" doc:"the PR head the batch was sorted against"`
	Seen    []ThreadSeen   `json:"seen"    jsonschema:"minItems=1,maxItems=1000" doc:"code-read, never model output: each thread's last human comment when the batch started"`
}

// ThreadSeen is what code read of one thread when its batch started (9.3).
// commentDigest(c) is the lowercase hex SHA-256 of
// c.ID + "\n" + c.UpdatedAt.UTC().Format(time.RFC3339Nano) + "\n" +
// hex(SHA-256(c.Body)), so an edit changes it even when the id does not.
type ThreadSeen struct {
	TID         string `json:"tid"          jsonschema:"pattern=^t[0-9a-f]{16}$"`
	LastComment string `json:"last_comment" jsonschema:"pattern=^[0-9a-f]{64}$" doc:"commentDigest of the last comment that is not a Zing reply"`
}
