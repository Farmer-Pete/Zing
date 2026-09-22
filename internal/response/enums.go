package response

// Job identifies which job produced a response.
type Job string

const (
	JobClassify   Job = "classify"
	JobPlanning   Job = "planning"
	JobPlanreview Job = "planreview"
	JobBuild      Job = "build"
	JobPerimeter  Job = "perimeter"
	JobReview     Job = "review"
	JobJudge      Job = "judge"
	JobRespond    Job = "respond"
	JobSide       Job = "side"
)

// Values returns every valid Job.
func (Job) Values() []string {
	return []string{
		string(JobClassify), string(JobPlanning), string(JobPlanreview), string(JobBuild),
		string(JobPerimeter), string(JobReview), string(JobJudge), string(JobRespond), string(JobSide),
	}
}

// TicketState is a ticket's position in the pipeline.
type TicketState string

const (
	TicketStateQueued    TicketState = "queued"
	TicketStatePlanning  TicketState = "planning"
	TicketStateBuilding  TicketState = "building"
	TicketStateReviewing TicketState = "reviewing"
	TicketStateJudging   TicketState = "judging"
	TicketStateShipping  TicketState = "shipping"
	TicketStateDone      TicketState = "done"
	TicketStateEscalated TicketState = "escalated"
	TicketStateAbandoned TicketState = "abandoned"
)

// Values returns every valid TicketState.
func (TicketState) Values() []string {
	return []string{
		string(TicketStateQueued), string(TicketStatePlanning), string(TicketStateBuilding),
		string(TicketStateReviewing), string(TicketStateJudging), string(TicketStateShipping),
		string(TicketStateDone), string(TicketStateEscalated), string(TicketStateAbandoned),
	}
}

// Outcome is the closed union of every job outcome plus the two universal outcomes.
type Outcome string

const (
	OutcomeBug         Outcome = "bug"
	OutcomeFeature     Outcome = "feature"
	OutcomeQuestions   Outcome = "questions"
	OutcomeReady       Outcome = "ready"
	OutcomeChildren    Outcome = "children"
	OutcomeNothingToDo Outcome = "nothing_to_do"
	OutcomeOk          Outcome = "ok"
	OutcomeQuestion    Outcome = "question"
	OutcomeError       Outcome = "error"
)

// Values returns every valid Outcome.
func (Outcome) Values() []string {
	return []string{
		string(OutcomeBug), string(OutcomeFeature), string(OutcomeQuestions), string(OutcomeReady),
		string(OutcomeChildren), string(OutcomeNothingToDo), string(OutcomeOk), string(OutcomeQuestion),
		string(OutcomeError),
	}
}

// ClaimKind is what a planning claim is about.
type ClaimKind string

const (
	ClaimKindCode ClaimKind = "code"
	ClaimKindEnv  ClaimKind = "env"
)

// Values returns every valid ClaimKind.
func (ClaimKind) Values() []string {
	return []string{string(ClaimKindCode), string(ClaimKindEnv)}
}

// ClaimVerdict is whether a claim checked out.
type ClaimVerdict string

const (
	ClaimVerdictTrue      ClaimVerdict = "true"
	ClaimVerdictFalse     ClaimVerdict = "false"
	ClaimVerdictUnchecked ClaimVerdict = "unchecked"
)

// Values returns every valid ClaimVerdict.
func (ClaimVerdict) Values() []string {
	return []string{string(ClaimVerdictTrue), string(ClaimVerdictFalse), string(ClaimVerdictUnchecked)}
}

// ScenarioKind categorizes a plan scenario.
type ScenarioKind string

const (
	ScenarioKindBehavior    ScenarioKind = "behavior"
	ScenarioKindNegative    ScenarioKind = "negative"
	ScenarioKindPerformance ScenarioKind = "performance"
)

// Values returns every valid ScenarioKind.
func (ScenarioKind) Values() []string {
	return []string{
		string(ScenarioKindBehavior), string(ScenarioKindNegative), string(ScenarioKindPerformance),
	}
}

// ChangeKind is whether a plan change is new or modified.
type ChangeKind string

const (
	ChangeKindNew      ChangeKind = "new"
	ChangeKindModified ChangeKind = "modified"
)

// Values returns every valid ChangeKind.
func (ChangeKind) Values() []string {
	return []string{string(ChangeKindNew), string(ChangeKindModified)}
}

// FileAction is what a delivered file change does.
type FileAction string

const (
	FileActionCreate FileAction = "create"
	FileActionModify FileAction = "modify"
	FileActionDelete FileAction = "delete"
)

// Values returns every valid FileAction.
func (FileAction) Values() []string {
	return []string{string(FileActionCreate), string(FileActionModify), string(FileActionDelete)}
}

// TestKind categorizes a delivery test case.
type TestKind string

const (
	TestKindIntegration TestKind = "integration"
	TestKindUnit        TestKind = "unit"
	TestKindRegression  TestKind = "regression"
	TestKindE2e         TestKind = "e2e"
)

// Values returns every valid TestKind.
func (TestKind) Values() []string {
	return []string{
		string(TestKindIntegration), string(TestKindUnit), string(TestKindRegression), string(TestKindE2e),
	}
}

// Lens is a review lens name.
type Lens string

const (
	LensProblem        Lens = "problem"
	LensSimplification Lens = "simplification"
	LensCorrectness    Lens = "correctness"
	LensSecurity       Lens = "security"
	LensFidelity       Lens = "fidelity"
	LensTests          Lens = "tests"
	LensQuality        Lens = "quality"
	LensObservability  Lens = "observability"
)

// Values returns every valid Lens.
func (Lens) Values() []string {
	return []string{
		string(LensProblem), string(LensSimplification), string(LensCorrectness), string(LensSecurity),
		string(LensFidelity), string(LensTests), string(LensQuality), string(LensObservability),
	}
}

// Severity is a review finding's severity.
type Severity string

const (
	SeverityBlocker Severity = "blocker"
	SeverityMajor   Severity = "major"
	SeverityMinor   Severity = "minor"
	SeverityNit     Severity = "nit"
)

// Values returns every valid Severity.
func (Severity) Values() []string {
	return []string{string(SeverityBlocker), string(SeverityMajor), string(SeverityMinor), string(SeverityNit)}
}

// ErrorCode categorizes a run error.
type ErrorCode string

const (
	ErrorCodePlanGap     ErrorCode = "plan_gap"
	ErrorCodeCannotRun   ErrorCode = "cannot_run"
	ErrorCodeEnvironment ErrorCode = "environment"
	ErrorCodeOther       ErrorCode = "other"
)

// Values returns every valid ErrorCode.
func (ErrorCode) Values() []string {
	return []string{
		string(ErrorCodePlanGap), string(ErrorCodeCannotRun), string(ErrorCodeEnvironment), string(ErrorCodeOther),
	}
}

// QuestionKind categorizes a stored question.
type QuestionKind string

const (
	QuestionKindQuestion  QuestionKind = "question"
	QuestionKindGate      QuestionKind = "gate"
	QuestionKindSplit     QuestionKind = "split"
	QuestionKindPerimeter QuestionKind = "perimeter"
	QuestionKindReview    QuestionKind = "review"
	QuestionKindMerge     QuestionKind = "merge"
)

// Values returns every valid QuestionKind.
func (QuestionKind) Values() []string {
	return []string{
		string(QuestionKindQuestion), string(QuestionKindGate), string(QuestionKindSplit),
		string(QuestionKindPerimeter), string(QuestionKindReview), string(QuestionKindMerge),
	}
}

// QuestionState is a stored question's lifecycle state.
type QuestionState string

const (
	QuestionStateOpen     QuestionState = "open"
	QuestionStateAnswered QuestionState = "answered"
	QuestionStateResolved QuestionState = "resolved"
)

// Values returns every valid QuestionState.
func (QuestionState) Values() []string {
	return []string{string(QuestionStateOpen), string(QuestionStateAnswered), string(QuestionStateResolved)}
}

// TaskState is a build task's lifecycle state.
type TaskState string

const (
	TaskStatePending TaskState = "pending"
	TaskStateRunning TaskState = "running"
	TaskStateDone    TaskState = "done"
	TaskStateFailed  TaskState = "failed"
)

// Values returns every valid TaskState.
func (TaskState) Values() []string {
	return []string{
		string(TaskStatePending), string(TaskStateRunning), string(TaskStateDone), string(TaskStateFailed),
	}
}

// Decision is an owner's triage call on a finding or item.
type Decision string

const (
	DecisionAccept  Decision = "accept"
	DecisionReject  Decision = "reject"
	DecisionDrop    Decision = "drop"
	DecisionDiscuss Decision = "discuss"
)

// Values returns every valid Decision.
func (Decision) Values() []string {
	return []string{string(DecisionAccept), string(DecisionReject), string(DecisionDrop), string(DecisionDiscuss)}
}

// Result is a scenario verdict's pass/fail result.
type Result string

const (
	ResultPass Result = "pass"
	ResultFail Result = "fail"
)

// Values returns every valid Result.
func (Result) Values() []string {
	return []string{string(ResultPass), string(ResultFail)}
}

// ThreadVerb is the action taken on a review thread.
type ThreadVerb string

const (
	ThreadVerbFix       ThreadVerb = "fix"
	ThreadVerbReply     ThreadVerb = "reply"
	ThreadVerbAddressed ThreadVerb = "addressed"
)

// Values returns every valid ThreadVerb.
func (ThreadVerb) Values() []string {
	return []string{string(ThreadVerbFix), string(ThreadVerbReply), string(ThreadVerbAddressed)}
}
