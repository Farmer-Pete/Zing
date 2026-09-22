package response

import (
	"fmt"
	"slices"
	"strings"
)

// registryKey is a (job, outcome) pair, the lookup key design section 7.1
// maps to a concrete response type.
type registryKey struct {
	Job     Job
	Outcome Outcome
}

// registry maps every (job, outcome) pair in design section 7.1 to a
// constructor for its concrete response type. Built once at package init.
var registry = buildRegistry()

//nolint:ireturn // each constructor must hand back the dynamic response type the (job, outcome) pair names.
func buildRegistry() map[registryKey]func() Response {
	m := map[registryKey]func() Response{
		{JobClassify, OutcomeBug}:         func() Response { return &ClassifyResponse{} },
		{JobClassify, OutcomeFeature}:     func() Response { return &ClassifyResponse{} },
		{JobPlanning, OutcomeQuestions}:   func() Response { return &QuestionResponse{} },
		{JobPlanning, OutcomeReady}:       func() Response { return &ReadyResponse{} },
		{JobPlanning, OutcomeChildren}:    func() Response { return &ChildrenResponse{} },
		{JobPlanning, OutcomeNothingToDo}: func() Response { return &NothingToDoResponse{} },
		{JobPlanreview, OutcomeOk}:        func() Response { return &FindingsResponse{} },
		{JobBuild, OutcomeOk}:             func() Response { return &BuildResponse{} },
		{JobPerimeter, OutcomeOk}:         func() Response { return &PerimeterResponse{} },
		{JobReview, OutcomeOk}:            func() Response { return &FindingsResponse{} },
		{JobJudge, OutcomeOk}:             func() Response { return &JudgeResponse{} },
		{JobRespond, OutcomeOk}:           func() Response { return &RespondResponse{} },
		{JobSide, OutcomeOk}:              func() Response { return &SideResponse{} },
	}
	for _, j := range Job("").Values() {
		job := Job(j)
		m[registryKey{job, OutcomeQuestion}] = func() Response { return &QuestionResponse{} }
		m[registryKey{job, OutcomeError}] = func() Response { return &ErrorResponse{} }
	}
	return m
}

// RegisteredPair is one (job, outcome) pair the registry recognizes.
type RegisteredPair struct {
	Job     Job
	Outcome Outcome
}

// RegisteredPairs returns every (job, outcome) pair the registry
// recognizes, derived straight from the registry map so it can never drift
// from what Lookup actually accepts, sorted for a deterministic result.
// This is the single source of truth for enumerating the registry;
// cmd/zing's selftest uses it instead of keeping its own copy.
func RegisteredPairs() []RegisteredPair {
	pairs := make([]RegisteredPair, 0, len(registry))
	for k := range registry {
		pairs = append(pairs, RegisteredPair(k))
	}
	slices.SortFunc(pairs, func(a, b RegisteredPair) int {
		if c := strings.Compare(string(a.Job), string(b.Job)); c != 0 {
			return c
		}
		return strings.Compare(string(a.Outcome), string(b.Outcome))
	})
	return pairs
}

// Lookup returns a fresh pointer for the response type registered to
// (job, outcome). An unregistered pair returns an error naming both.
//
//nolint:ireturn // Lookup's whole job is to dispatch to the dynamic type the (job, outcome) pair names.
func Lookup(job Job, outcome Outcome) (Response, error) {
	ctor, ok := registry[registryKey{job, outcome}]
	if !ok {
		return nil, fmt.Errorf("no response for job %s outcome %s", job, outcome)
	}
	return ctor(), nil
}
