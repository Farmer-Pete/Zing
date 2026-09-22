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
