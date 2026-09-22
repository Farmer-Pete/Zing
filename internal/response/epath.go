package response

import "strconv"

// Attribute names shared by parse.go's header extraction and validate.go's
// job/outcome legality checks.
const (
	attrJob     = "job"
	attrOutcome = "outcome"
)

// joinPath appends name as the next segment of parent, following the
// element-path grammar in design section 6.4: segments join with "/", and
// the root zing element is omitted (parent is "" at the top level).
func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

// indexedName formats a repeated element's name with its 0-based index, as
// in claim[0].
func indexedName(name string, i int) string {
	return name + "[" + strconv.Itoa(i) + "]"
}
