package schemagen

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"zing/internal/response"
)

var update = flag.Bool("update", false, "update the committed schema files in internal/store/schemas")

const schemaDir = "../store/schemas"

func TestRegistry_Has16Entries(t *testing.T) {
	t.Parallel()

	entries := Registry()
	if len(entries) != 16 {
		t.Fatalf("len(Registry()) = %d, want 16", len(entries))
	}

	var messages, artifacts int
	for _, e := range entries {
		switch e.Table {
		case "messages":
			messages++
		case "artifacts":
			artifacts++
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
