package response

import (
	"slices"
	"strings"
	"testing"
)

func TestLoadChecklists_MatchesEmbeddedTOML(t *testing.T) {
	t.Parallel()

	lists, err := LoadChecklists()
	if err != nil {
		t.Fatalf("LoadChecklists: %v", err)
	}

	wantPlaceholders := []string{"TODO", "TBD", "handle appropriately"}
	if !slices.Equal(lists.Placeholders, wantPlaceholders) {
		t.Errorf("Placeholders = %v, want %v", lists.Placeholders, wantPlaceholders)
	}

	// Vague is lowercased for case-insensitive matching; checklists.toml
	// itself carries these already lowercase, so this also pins that the
	// loader does not mangle already-lowercase entries.
	wantVague := []string{"large", "fast", "recent", "many", "often", "quickly", "several", "slow", "soon", "some"}
	if !slices.Equal(lists.Vague, wantVague) {
		t.Errorf("Vague = %v, want %v", lists.Vague, wantVague)
	}

	wantUnits := []string{"ns", "us", "ms", "s", "B", "KB", "MB", "GB", "%", "x", "rps", "qps", "Hz", "fps", "ops", "allocs"}
	if !slices.Equal(lists.Units, wantUnits) {
		t.Errorf("Units = %v, want %v", lists.Units, wantUnits)
	}
}

func TestParseChecklists_RejectsUnknownKey(t *testing.T) {
	t.Parallel()

	data := `placeholders = ["TODO"]
vague = ["slow"]
units = ["ms"]
extra = ["not a recognized key"]
`
	_, err := parseChecklists(data)
	if err == nil {
		t.Fatal("parseChecklists = nil error, want an error naming the unknown key")
	}
	if !strings.Contains(err.Error(), "extra") {
		t.Errorf("parseChecklists error = %q, want it to name the unknown key %q", err, "extra")
	}
}

func TestParseChecklists_RejectsMissingOrEmptyList(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data string
		want string
	}{
		{"missing units", "placeholders = [\"TODO\"]\nvague = [\"slow\"]\n", "units"},
		{"empty units", "placeholders = [\"TODO\"]\nvague = [\"slow\"]\nunits = []\n", "units"},
		{"empty placeholders", "placeholders = []\nvague = [\"slow\"]\nunits = [\"ms\"]\n", "placeholders"},
		{"empty vague", "placeholders = [\"TODO\"]\nvague = []\nunits = [\"ms\"]\n", "vague"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseChecklists(tc.data)
			if err == nil {
				t.Fatalf("parseChecklists = nil error, want an error naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseChecklists error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestPlanRules_NamesEveryChecklistEntry pins that the rendered block
// shares the validator's own word lists, so the planning prompt and the
// plan checker cannot disagree (plan section "Decisions and additions").
func TestPlanRules_NamesEveryChecklistEntry(t *testing.T) {
	t.Parallel()

	lists, err := LoadChecklists()
	if err != nil {
		t.Fatalf("LoadChecklists: %v", err)
	}
	block := renderPlanRules(lists)
	// Checked as the whole joined list, not entry by entry: a short entry
	// such as "s" or "ns" already occurs inside other words in the block
	// (ns inside "mentions"), so a per-entry strings.Contains would still
	// pass even if that entry were dropped or the list rendered wrongly.
	for name, joined := range map[string]string{
		"Vague":        strings.Join(lists.Vague, ", "),
		"Placeholders": strings.Join(lists.Placeholders, ", "),
		"Units":        strings.Join(lists.Units, ", "),
	} {
		if !strings.Contains(block, joined) {
			t.Errorf("renderPlanRules block missing the whole %s list %q:\n%s", name, joined, block)
		}
	}

	custom := Checklists{Placeholders: []string{"TODO"}, Vague: []string{"zork"}, Units: []string{"ms"}}
	if got := renderPlanRules(custom); !strings.Contains(got, "zork") {
		t.Errorf("renderPlanRules(custom) = %q, want it to contain %q", got, "zork")
	}
}

func TestParseChecklists_LowercasesVague(t *testing.T) {
	t.Parallel()

	data := `placeholders = ["TODO"]
vague = ["Large", "FAST"]
units = ["ms"]
`
	lists, err := parseChecklists(data)
	if err != nil {
		t.Fatalf("parseChecklists: %v", err)
	}
	want := []string{"large", "fast"}
	if !slices.Equal(lists.Vague, want) {
		t.Errorf("Vague = %v, want %v", lists.Vague, want)
	}
}
