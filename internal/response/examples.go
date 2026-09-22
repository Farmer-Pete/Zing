package response

import (
	"embed"
	"io/fs"
	"sort"
)

// ExampleFS embeds one valid example document per registered response type
// (design section 6.9), so cmd/zing's selftest and this package's own test
// can Parse and Validate every one of them without a filesystem read
// outside the module. internal/response, not cmd/zing, embeds them: an
// embed directive can only reach files under its own package directory.
//
//go:embed examples/*.xml
var ExampleFS embed.FS

// ExampleFiles returns the sorted path of every embedded example
// (examples/<name>.xml), so a caller that walks ExampleFS - this package's
// own test and cmd/zing's selftest - iterates them in one deterministic
// order.
func ExampleFiles() ([]string, error) {
	entries, err := fs.ReadDir(ExampleFS, "examples")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, "examples/"+e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
