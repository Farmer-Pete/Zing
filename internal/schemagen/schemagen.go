// Package schemagen reflects the stored response types into the committed
// JSON Schema files under internal/store/schemas.
package schemagen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"sort"

	"github.com/invopop/jsonschema"

	"zing/internal/response"
)

// Entry names one committed schema and the Go type reflected into it.
// Value is a zero value of the type to reflect.
type Entry struct {
	Table string
	Name  string
	Value any
}

// planReviewPayload is the stored shape of a planreview artifact: the
// findings only, not the FindingsResponse wrapper's job/outcome head.
type planReviewPayload struct {
	Findings []response.Finding `json:"findings"` // required array; a clean review stores []
}

// respondPayload is the stored shape of a respond artifact: the threads
// only, not the RespondResponse wrapper's job/outcome head.
type respondPayload struct {
	Threads []response.ThreadAction `json:"threads" jsonschema:"minItems=1"`
}

// claimsPayload is the stored shape of a claims artifact: []Claim at the
// document root. JSONSchemaExtend sets the root-array minItems the bare
// slice reflection cannot carry, preserving ReadyResponse.Claims' constraint.
type claimsPayload []response.Claim

func (claimsPayload) JSONSchemaExtend(s *jsonschema.Schema) {
	one := uint64(1)
	s.MinItems = &one
}

// childrenPayload is the stored shape of a children artifact: []Child at the
// document root, preserving ChildrenResponse.Children's minItems=2.
type childrenPayload []response.Child

func (childrenPayload) JSONSchemaExtend(s *jsonschema.Schema) {
	two := uint64(2)
	s.MinItems = &two
}

// The two artifact tables a stored type belongs to.
const (
	tableMessages  = "messages"
	tableArtifacts = "artifacts"
)

// Registry lists the 16 stored types, each mapped to its committed schema path.
func Registry() []Entry {
	return []Entry{
		{tableMessages, "question", response.QuestionPayload{}},
		{tableMessages, "answer", response.AnswerPayload{}},
		{tableMessages, "escalation", response.EscalationPayload{}},
		{tableMessages, "state", response.StatePayload{}},
		{tableArtifacts, "claims", claimsPayload{}},
		{tableArtifacts, "scenario", response.Scenario{}},
		{tableArtifacts, "plan", response.Plan{}},
		{tableArtifacts, "planreview", planReviewPayload{}},
		{tableArtifacts, "children", childrenPayload{}},
		{tableArtifacts, "file", response.FileArtifact{}},
		{tableArtifacts, "fence", response.Fence{}},
		{tableArtifacts, "task", response.TaskArtifact{}},
		{tableArtifacts, "build_report", response.BuildReport{}},
		{tableArtifacts, "finding", response.Finding{}},
		{tableArtifacts, "verdict", response.Verdict{}},
		{tableArtifacts, "respond", respondPayload{}},
	}
}

// valueser is implemented by every response enum type.
type valueser interface{ Values() []string }

var valueserType = reflect.TypeFor[valueser]()

// enumMapper gives every response enum field an explicit `enum` array. The
// jsonschema reflector does not read a Values() method on its own, so
// without this mapping an enum field would come out as an unconstrained
// string. Reflector.reflectTypeToSchema dereferences pointer field types
// before calling Mapper, so t here is never a pointer.
func enumMapper(t reflect.Type) *jsonschema.Schema {
	if !t.Implements(valueserType) {
		return nil
	}
	v, ok := reflect.TypeAssert[valueser](reflect.Zero(t))
	if !ok {
		return nil // unreachable: t.Implements(valueserType) already guarantees this
	}
	values := v.Values()
	enum := make([]any, len(values))
	for i, val := range values {
		enum[i] = val
	}
	return &jsonschema.Schema{Type: "string", Enum: enum}
}

func newReflector() *jsonschema.Reflector {
	return &jsonschema.Reflector{
		DoNotReference: true, // inline, no $defs
		Mapper:         enumMapper,
		// RequiredFromJSONSchemaTags stays false: a field is required unless
		// its json tag has omitempty, matching section 9.1.
	}
}

// Generate reflects every registered type into pretty JSON Schema, each with
// a trailing newline, keyed by its relative path ("messages/state.json").
func Generate() (map[string][]byte, error) {
	reflector := newReflector()
	out := make(map[string][]byte, len(Registry()))
	for _, e := range Registry() {
		schema := reflector.Reflect(e.Value)
		b, err := json.MarshalIndent(schema, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal schema for %s/%s: %w", e.Table, e.Name, err)
		}
		b = append(b, '\n')
		out[e.Table+"/"+e.Name+".json"] = b
	}
	return out, nil
}

// Diff returns the relative paths whose committed bytes in committed differ
// from Generate()'s output, sorted. An unreadable or missing committed file
// counts as a difference.
func Diff(committed fs.FS) ([]string, error) {
	generated, err := Generate()
	if err != nil {
		return nil, err
	}

	var diffs []string
	for relpath, want := range generated {
		got, readErr := fs.ReadFile(committed, relpath)
		if readErr != nil || !bytes.Equal(got, want) {
			diffs = append(diffs, relpath)
		}
	}
	sort.Strings(diffs)
	return diffs, nil
}
