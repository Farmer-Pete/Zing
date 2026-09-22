package response

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed checklists.toml
var checklistsTOML string

// Checklists holds the plan-checker's tunable word lists (design section
// 6.7): placeholder tokens, vague qualifiers, and measurement units.
type Checklists struct {
	Placeholders []string
	Vague        []string
	Units        []string
}

// checklistsFile is checklists.toml's exact shape. Its field names are the
// toml keys the trust root is allowed to set; BurntSushi/toml's
// MetaData.Undecoded rejects anything else.
type checklistsFile struct {
	Placeholders []string `toml:"placeholders"`
	Vague        []string `toml:"vague"`
	Units        []string `toml:"units"`
}

// LoadChecklists parses the embedded checklists.toml. checklists.toml is a
// trust root (design section 9): an unknown key is a build-time mistake in
// that trusted file, not untrusted input, so LoadChecklists rejects it
// rather than ignoring it silently.
func LoadChecklists() (Checklists, error) {
	return parseChecklists(checklistsTOML)
}

// parseChecklists is LoadChecklists' decode step, split out so a test can
// exercise the unknown-key rejection and the vague-lowercasing without
// editing the embedded trust-root file.
func parseChecklists(data string) (Checklists, error) {
	var f checklistsFile
	meta, err := toml.Decode(data, &f)
	if err != nil {
		return Checklists{}, fmt.Errorf("checklists: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return Checklists{}, fmt.Errorf("checklists: unknown key %q", undecoded[0].String())
	}

	// Every list gates a plan-checker rule, so a missing or empty one in the
	// trust-root file would silently disable that rule. Fail fast instead.
	if len(f.Placeholders) == 0 {
		return Checklists{}, errors.New("checklists: placeholders must be a non-empty list")
	}
	if len(f.Vague) == 0 {
		return Checklists{}, errors.New("checklists: vague must be a non-empty list")
	}
	if len(f.Units) == 0 {
		return Checklists{}, errors.New("checklists: units must be a non-empty list")
	}

	vague := make([]string, len(f.Vague))
	for i, w := range f.Vague {
		vague[i] = strings.ToLower(w)
	}

	return Checklists{Placeholders: f.Placeholders, Vague: vague, Units: f.Units}, nil
}
