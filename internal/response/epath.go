package response

import (
	"reflect"
	"strconv"
	"strings"
)

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

// planShape is Plan's own reflected XML shape (shape.go's shapeOf, cached
// under Plan's exact reflect.Type): ResolvesInPlan's one descriptor, shared
// across every call rather than rebuilt per finding.
var planShape = shapeOf(reflect.TypeFor[Plan]())

// ResolvesInPlan reports whether location -- a plan-review finding's own
// Location field, "an element path such as plan/delivery/tasks/task[3]"
// (types.go's own doc tag on Finding.Location) -- names an element or
// attribute that planXML, the stored plan re-rendered back to XML, actually
// carries. It walks planXML with Plan's reflected shape, the same presence
// pass Validate's Layer 1 runs over a whole response document
// (validate.go's presenceSet), so a finding invented against a plan the
// model never saw, or a hallucinated path, is dropped rather than
// re-entering planning as if it were real (design section 6.5, plan review
// task). A location outside the "plan" root (or the malformed empty
// string) never resolves.
func ResolvesInPlan(planXML []byte, location string) bool {
	if location == "plan" {
		return true
	}
	rest, ok := strings.CutPrefix(location, "plan/")
	if !ok {
		return false
	}
	set, err := presenceSet(planXML, planShape)
	if err != nil {
		return false
	}
	return set[rest]
}
