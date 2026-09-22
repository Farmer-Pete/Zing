// Package lens loads the review lens files under prompts/lenses/.
package lens

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Lens is one review lens: what to check in a plan, and (except for
// "problem") what to check in code.
type Lens struct {
	Name, Plan, Code string
}

const (
	headingPlan = "## In a plan"
	headingCode = "## In code"
)

// Load reads every *.md file in dir within fsys and parses it into a Lens,
// returning the lenses sorted by name.
func Load(fsys fs.FS, dir string) ([]Lens, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read lens dir %s: %w", dir, err)
	}

	lenses := make([]Lens, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")

		b, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read lens %s: %w", name, err)
		}

		l, err := parseLens(name, string(b))
		if err != nil {
			return nil, err
		}
		lenses = append(lenses, l)
	}

	if len(lenses) == 0 {
		return nil, fmt.Errorf("lens: no lens files in %s", dir)
	}

	sort.Slice(lenses, func(i, j int) bool { return lenses[i].Name < lenses[j].Name })
	return lenses, nil
}

type section struct {
	heading string
	body    []string
}

// parseLens is a line scan (recursive-descent style, no parser generator):
// each line is either a heading, content before the first heading (an
// error), or a line belonging to the current section's body.
func parseLens(name, content string) (Lens, error) {
	var sections []section
	var cur *section

	for line := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed == headingPlan || trimmed == headingCode {
			for _, s := range sections {
				if s.heading == trimmed {
					return Lens{}, fmt.Errorf("lens %s: duplicate %q section", name, trimmed)
				}
			}
			sections = append(sections, section{heading: trimmed})
			cur = &sections[len(sections)-1]
			continue
		}
		if cur == nil {
			if strings.TrimSpace(line) != "" {
				return Lens{}, fmt.Errorf("lens %s: content before first section", name)
			}
			continue
		}
		cur.body = append(cur.body, line)
	}

	l := Lens{Name: name}
	for _, s := range sections {
		text := strings.Join(trimBlankLines(s.body), "\n")
		switch s.heading {
		case headingPlan:
			l.Plan = text
		case headingCode:
			l.Code = text
		}
	}

	if strings.TrimSpace(l.Plan) == "" {
		return Lens{}, fmt.Errorf("lens %s: missing %q section", name, headingPlan)
	}
	if name != "problem" && strings.TrimSpace(l.Code) == "" {
		return Lens{}, fmt.Errorf("lens %s: missing %q section", name, headingCode)
	}

	return l, nil
}

// trimBlankLines drops leading and trailing blank lines, keeping interior
// lines (including whitespace-only ones) unchanged.
func trimBlankLines(lines []string) []string {
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	end := len(lines)
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[start:end]
}
