package schemagen

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"zing/internal/response"
)

var update = flag.Bool("update", false, "update the committed schema files in internal/store/schemas")

const schemaDir = "../store/schemas"

func TestRegistry_Has17Entries(t *testing.T) {
	t.Parallel()

	entries := Registry()
	if len(entries) != 17 {
		t.Fatalf("len(Registry()) = %d, want 17", len(entries))
	}

	var messages, artifacts, pushSubscriptions int
	for _, e := range entries {
		switch e.Table {
		case "messages":
			messages++
		case "artifacts":
			artifacts++
		case "push_subscriptions":
			pushSubscriptions++
		default:
			t.Errorf("unexpected table %q for %s", e.Table, e.Name)
		}
	}
	if messages != 4 {
		t.Errorf("messages entries = %d, want 4", messages)
	}
	if artifacts != 12 {
		t.Errorf("artifacts entries = %d, want 12", artifacts)
	}
	if pushSubscriptions != 1 {
		t.Errorf("push_subscriptions entries = %d, want 1", pushSubscriptions)
	}
}

// TestGenerate_MatchesCommitted is the golden test: run with -update to
// (re)write internal/store/schemas from Generate(); without it, every
// committed file's bytes must equal Generate()'s output exactly.
func TestGenerate_MatchesCommitted(t *testing.T) {
	generated, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if *update {
		for relpath, b := range generated {
			path := filepath.Join(schemaDir, filepath.FromSlash(relpath))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir for %s: %v", path, err)
			}
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
	}

	for relpath, want := range generated {
		path := filepath.Join(schemaDir, filepath.FromSlash(relpath))
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read committed %s: %v", relpath, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: committed schema differs from Generate() output; run with -update", relpath)
		}
	}

	// Bidirectional: fail if a committed schema file exists that Generate()
	// did not produce (an orphan with no Registry() source).
	walkErr := filepath.WalkDir(schemaDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(schemaDir, path)
		if err != nil {
			return err
		}
		relpath := filepath.ToSlash(rel)
		if _, ok := generated[relpath]; !ok {
			t.Errorf("%s: committed schema file has no Registry() entry to generate it", relpath)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", schemaDir, walkErr)
	}
}

// TestDiff_DetectsOrphanFile proves Diff reports a committed schema file that
// exists on disk but has no Registry() entry to generate it, in addition to
// the byte-mismatch case TestGenerate_MatchesCommitted already covers.
func TestDiff_DetectsOrphanFile(t *testing.T) {
	t.Parallel()

	generated, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	committed := fstest.MapFS{}
	for relpath, b := range generated {
		committed[relpath] = &fstest.MapFile{Data: b}
	}
	const orphan = "artifacts/orphan.json"
	committed[orphan] = &fstest.MapFile{Data: []byte("{}\n")}

	diffs, err := Diff(committed)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	found := false
	for _, d := range diffs {
		if d == orphan {
			found = true
		}
	}
	if !found {
		t.Errorf("Diff(committed with orphan file) = %v, want it to include %q", diffs, orphan)
	}
}

// TestEnumMapper_EmitsExactEnum asserts every response enum type reflects to
// an explicit `enum` array, since invopop does not read Values() on its own.
func TestEnumMapper_EmitsExactEnum(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		zero valueser
	}{
		{"Job", response.Job("")},
		{"TicketState", response.TicketState("")},
		{"Outcome", response.Outcome("")},
		{"ClaimKind", response.ClaimKind("")},
		{"ClaimVerdict", response.ClaimVerdict("")},
		{"ScenarioKind", response.ScenarioKind("")},
		{"ChangeKind", response.ChangeKind("")},
		{"FileAction", response.FileAction("")},
		{"TestKind", response.TestKind("")},
		{"Lens", response.Lens("")},
		{"Severity", response.Severity("")},
		{"ErrorCode", response.ErrorCode("")},
		{"QuestionKind", response.QuestionKind("")},
		{"QuestionState", response.QuestionState("")},
		{"TaskState", response.TaskState("")},
		{"Decision", response.Decision("")},
		{"Result", response.Result("")},
		{"ThreadVerb", response.ThreadVerb("")},
	}
	if len(tests) != 18 {
		t.Fatalf("18 enums documented in the plan, got %d test cases", len(tests))
	}

	reflector := newReflector()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			schema := reflector.Reflect(tt.zero)
			if schema.Type != "string" {
				t.Errorf("%s: schema.Type = %q, want %q", tt.name, schema.Type, "string")
			}

			want := tt.zero.Values()
			if len(schema.Enum) != len(want) {
				t.Fatalf("%s: enum = %v, want %v", tt.name, schema.Enum, want)
			}
			for i, v := range want {
				if schema.Enum[i] != v {
					t.Errorf("%s: enum[%d] = %v, want %v", tt.name, i, schema.Enum[i], v)
				}
			}
		})
	}
}

// enumFieldTests names one representative field, inside one of the 17
// generated STORED schemas, for every enum type that actually appears as a
// field in a stored payload. TestEnumMapper_EmitsExactEnum above reflects
// each enum type at the schema root; this test instead inspects the real
// generated documents, so a Mapper regression that only shows up once a
// type is embedded (nesting, array wrapping, an enum used by more than one
// property) cannot slip past the root-only check.
//
// ErrorCode, Job, and Outcome are Go enum types with a Values() method, but
// none of them is a field on any of the 17 stored types: ErrorCode and Job
// only appear on RunError and Head, and Outcome only appears on Head, and
// none of the response-wrapper types that embed Head or RunError are
// stored types (see the schemagen.Registry doc comment). So those three
// have no representative field here; their own schema is still asserted by
// TestEnumMapper_EmitsExactEnum.
// JSON Schema keyword literals repeated across enumFieldTests' navigation
// paths, and the one schema file three different enum fields share.
const (
	schemaItems      = "items"
	schemaProperties = "properties"
	schemaKind       = "kind"

	questionSchemaFile = "messages/question.json"
)

var enumFieldTests = []struct {
	name string
	zero valueser
	file string
	path []string
}{
	{"ClaimKind", response.ClaimKind(""), "artifacts/claims.json", []string{schemaItems, schemaProperties, schemaKind}},
	{"ClaimVerdict", response.ClaimVerdict(""), "artifacts/claims.json", []string{schemaItems, schemaProperties, "verdict"}},
	{"ScenarioKind", response.ScenarioKind(""), "artifacts/scenario.json", []string{schemaProperties, schemaKind}},
	{
		"ChangeKind", response.ChangeKind(""), "artifacts/plan.json",
		[]string{schemaProperties, "design", schemaProperties, "changes", schemaItems, schemaProperties, schemaKind},
	},
	{"FileAction", response.FileAction(""), "artifacts/file.json", []string{schemaProperties, "action"}},
	{
		"TestKind", response.TestKind(""), "artifacts/plan.json",
		[]string{schemaProperties, "delivery", schemaProperties, "tests", schemaItems, schemaProperties, schemaKind},
	},
	{"Lens", response.Lens(""), "artifacts/finding.json", []string{schemaProperties, "lens"}},
	{"Severity", response.Severity(""), "artifacts/finding.json", []string{schemaProperties, "severity"}},
	{"TaskState", response.TaskState(""), "artifacts/task.json", []string{schemaProperties, "state"}},
	{
		"Decision", response.Decision(""), questionSchemaFile,
		[]string{schemaProperties, schemaItems, schemaItems, schemaProperties, "decision"},
	},
	{"Result", response.Result(""), "artifacts/verdict.json", []string{schemaProperties, "result"}},
	{
		"ThreadVerb", response.ThreadVerb(""), "artifacts/respond.json",
		[]string{schemaProperties, "threads", schemaItems, schemaProperties, "action"},
	},
	{"QuestionKind", response.QuestionKind(""), questionSchemaFile, []string{schemaProperties, schemaKind}},
	{"QuestionState", response.QuestionState(""), questionSchemaFile, []string{schemaProperties, "state"}},
	{"TicketState", response.TicketState(""), "messages/state.json", []string{schemaProperties, "from"}},
}

func TestEnumMapper_StoredSchemaFieldsCarryExactEnum(t *testing.T) {
	t.Parallel()

	if len(enumFieldTests) != 15 {
		t.Fatalf("15 of the 18 enum types appear in a stored schema field; got %d test cases", len(enumFieldTests))
	}

	generated, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	for _, tt := range enumFieldTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, ok := generated[tt.file]
			if !ok {
				t.Fatalf("no generated schema for %s", tt.file)
			}
			var doc map[string]any
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatalf("unmarshal %s: %v", tt.file, err)
			}

			var node any = doc
			for _, key := range tt.path {
				m, isObj := node.(map[string]any)
				if !isObj {
					t.Fatalf("%s: %v is not an object navigating to %q", tt.file, node, key)
				}
				next, present := m[key]
				if !present {
					t.Fatalf("%s: missing key %q under path %v", tt.file, key, tt.path)
				}
				node = next
			}

			field, ok := node.(map[string]any)
			if !ok {
				t.Fatalf("%s: field at %v is not an object", tt.file, tt.path)
			}
			gotEnum, ok := field["enum"].([]any)
			if !ok {
				t.Fatalf("%s: field at %v has no enum array", tt.file, tt.path)
			}

			want := tt.zero.Values()
			if len(gotEnum) != len(want) {
				t.Fatalf("%s: enum at %v = %v, want %v", tt.file, tt.path, gotEnum, want)
			}
			for i, v := range want {
				if gotEnum[i] != v {
					t.Errorf("%s: enum[%d] at %v = %v, want %v", tt.file, i, tt.path, gotEnum[i], v)
				}
			}
		})
	}
}
