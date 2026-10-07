package job

// SeedOwnerDecision re-exports spec_internal_test.go's own unexported
// seedOwnerDecision (r2f1, r2f2): a _test.go file in package job compiles
// into the same test binary as package job_test, so this alias is how
// escalation_test.go's own tests (package job_test) reach the one fixture,
// instead of keeping a second copy that can drift from it.
var SeedOwnerDecision = seedOwnerDecision

// StageSnap, StageBuild, StageCopy, NewStageSnap, and UseStage re-export
// stagesnap_test.go's own stage-snapshot primitive (ticket #102, owner
// decision Q2), so job_test's own buildTicketInBuilding shares the one
// copy of the VACUUM, repo copy, and local_path rewrite steps instead of
// keeping a second copy of them.
type (
	StageSnap  = stageSnap
	StageBuild = stageBuild
	StageCopy  = stageCopy
)

func NewStageSnap(name string) *StageSnap {
	return &StageSnap{name: name}
}

var UseStage = useStage
