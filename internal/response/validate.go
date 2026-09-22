package response

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Kind is which shape of plan the checkers hold a ready plan to: "bug"
// turns on the bug-plan-shape rules (design section 6.6); "feature" or ""
// uses the ordinary rules. It is Package 2's own addition beside the
// section 9.1 wire types; Package 1 has no such type, and Kind never
// appears on the wire, only in ValidateContext.
type Kind string

const (
	KindBug     Kind = "bug"
	KindFeature Kind = "feature"
)

// PathError is one Validate failure, located at a specific element or
// attribute path (the grammar in epath.go).
type PathError struct {
	Path string
	Msg  string
}

func (e *PathError) Error() string {
	return e.Path + ": " + e.Msg
}

// ValidateContext carries what Validate needs beyond the document itself.
type ValidateContext struct {
	// Job is the running job, or "" for a standalone check. When set, it
	// must match the document's own job.
	Job Job
	// Kind selects bug or feature plan rules; "" behaves like "feature".
	Kind Kind
	// FS is the repo filesystem the code-claim path check reads against;
	// nil skips that check.
	FS fs.FS
}

// Validate runs Layer 1 (structure: presence, enums, patterns, bounds, job
// and outcome legality) and then Layer 2 (the type's own checks) over doc,
// returning every failure one per element path, in a deterministic order:
// Layer 1 by descriptor traversal (declaration order, depth-first,
// attributes before child elements, slices ascending by index), then the
// job/outcome legality checks, then Layer 2.
func Validate(doc *Document, ctx ValidateContext) []*PathError {
	t := reflect.TypeOf(doc.Response).Elem()
	root := shapeOf(t)
	val := reflect.ValueOf(doc.Response).Elem()

	present, err := presenceSet(doc.Elem, root)
	if err != nil {
		// doc.Elem already decoded successfully once (Parse produced it),
		// so a re-walk failure here is not expected; treat it as "nothing
		// found present" rather than panicking, so Validate stays total.
		present = map[string]bool{}
	}

	errs := make([]*PathError, 0, len(root.Children))
	errs = append(errs, checkChildren(root.Children, val, "", present)...)
	errs = append(errs, checkHeader(doc.Response.Header(), ctx)...)
	errs = append(errs, layer2(doc, ctx, present)...)
	return errs
}

// checkHeader adds the two checks that need the whole (job, outcome) pair
// rather than one field at a time: outcome legality for job, and, when the
// caller names a running job, that the document agrees with it.
func checkHeader(h Head, ctx ValidateContext) []*PathError {
	errs := make([]*PathError, 0, 2)
	if _, err := Lookup(h.Job, h.Outcome); err != nil {
		errs = append(errs, &PathError{
			Path: attrOutcome,
			Msg:  fmt.Sprintf("%s is not an outcome of %s", h.Outcome, h.Job),
		})
	}
	if ctx.Job != "" && ctx.Job != h.Job {
		errs = append(errs, &PathError{
			Path: attrJob,
			Msg:  fmt.Sprintf("document says %s, run is %s", h.Job, ctx.Job),
		})
	}
	return errs
}

// planChecklists is the plan checker's word lists, loaded once from the
// embedded, trust-root checklists.toml. A load failure means that trusted
// file itself is broken, which every caller needs to know about
// immediately rather than have Validate silently skip the plan checks.
var planChecklists = mustLoadChecklists()

func mustLoadChecklists() Checklists {
	lists, err := LoadChecklists()
	if err != nil {
		panic(err)
	}
	return lists
}

// layer2 dispatches to each response type's own checks: claims against
// injected observations, the plan checker, and the structural (none-union,
// children-DAG, option-cardinality) checks (design section 6.4). It runs a
// check only over a field present says Layer 1 actually found in the
// document, so a check never reads (or indexes into) a zero value that
// Layer 1 has already reported missing.
func layer2(doc *Document, ctx ValidateContext, present map[string]bool) []*PathError {
	switch r := doc.Response.(type) {
	case *ReadyResponse:
		return layer2Ready(r, ctx, present)
	case *NothingToDoResponse:
		return CheckNothingToDoClaims(r.Claims)
	case *ChildrenResponse:
		return checkChildrenDAG(r.Children)
	case *QuestionResponse:
		return checkQuestionCardinality(r.Questions)
	case *BuildResponse, *JudgeResponse:
		// Layer 1 only; their observation-dependent checks belong to the
		// orchestrator and the judge (Packages 8, 9).
		return nil
	default:
		return nil
	}
}

// layer2Ready runs a ReadyResponse's three Layer 2 checks: the code-claim
// path check (only when the caller supplied a filesystem), the plan
// checker, and the plan's two none-union checks. The plan checks all run
// only when present["plan"], since a missing <plan> element leaves
// r.Plan a zero value and Layer 1 already reports it missing.
func layer2Ready(r *ReadyResponse, ctx ValidateContext, present map[string]bool) []*PathError {
	var errs []*PathError

	if ctx.FS != nil && present["claims"] {
		errs = append(errs, CheckCodeClaims(r.Claims, ctx.FS)...)
	}

	if !present["plan"] {
		return errs
	}
	errs = append(errs, CheckPlan(r.Plan, r.Scenarios, ctx.Kind == KindBug, planChecklists)...)

	if present["plan/design/migrations"] {
		m := r.Plan.Design.Migrations
		if err := checkNoneUnion("plan/design/migrations", present["plan/design/migrations/none"], m.None, len(m.Items)); err != nil {
			errs = append(errs, err)
		}
	}
	if present["plan/delivery/deletions"] {
		d := r.Plan.Delivery.Deletions
		if err := checkNoneUnion("plan/delivery/deletions", present["plan/delivery/deletions/none"], d.None, len(d.Items)); err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}

// ---- Layer 1: the reflective pass ----------------------------------------

// checkChildren walks n's children in shape order and returns every
// failure, reading values out of containerVal (the reflect.Value of the
// struct that declares them) and building paths from containerPath.
func checkChildren(children []*node, containerVal reflect.Value, containerPath string, present map[string]bool) []*PathError {
	errs := make([]*PathError, 0, len(children))
	for _, c := range children {
		errs = append(errs, checkChild(c, containerVal, containerPath, present)...)
	}
	return errs
}

func checkChild(c *node, containerVal reflect.Value, containerPath string, present map[string]bool) []*PathError {
	switch c.Kind {
	case kindAttr:
		return checkAttr(c, containerVal, containerPath, present)
	case kindChardata:
		return checkConstraints(c, containerPath, c.Get(containerVal))
	case kindWrapper:
		return checkChild(c.Children[0], containerVal, joinPath(containerPath, c.Name), present)
	case kindElement:
		return checkElement(c, containerVal, containerPath, present)
	default:
		return nil
	}
}

func checkAttr(c *node, containerVal reflect.Value, containerPath string, present map[string]bool) []*PathError {
	path := joinPath(containerPath, c.Name)
	if !present[path] {
		if c.Required {
			return []*PathError{{Path: path, Msg: "missing required element"}}
		}
		return nil
	}
	return checkConstraints(c, path, c.Get(containerVal))
}

func checkElement(c *node, containerVal reflect.Value, containerPath string, present map[string]bool) []*PathError {
	v := c.Get(containerVal)
	if c.Slice {
		return checkSlice(c, v, containerPath, present)
	}

	path := joinPath(containerPath, c.Name)
	if !present[path] {
		if c.Required {
			return []*PathError{{Path: path, Msg: "missing required element"}}
		}
		return nil
	}

	ev, ok := deref(v)
	if !ok {
		return nil
	}
	errs := make([]*PathError, 0, len(c.Children))
	errs = append(errs, checkConstraints(c, path, ev)...)
	errs = append(errs, checkChildren(c.Children, ev, path, present)...)
	return errs
}

// checkSlice checks a repeated element's count against minItems/maxItems
// (reading the decoded length directly: a slice's absence is length zero,
// so minItems alone governs whether it is required) and then each item.
func checkSlice(c *node, v reflect.Value, containerPath string, present map[string]bool) []*PathError {
	aggPath := joinPath(containerPath, c.Name)
	n := v.Len()

	errs := make([]*PathError, 0, n)
	if c.HasMinItems && n < c.MinItems {
		errs = append(errs, &PathError{Path: aggPath, Msg: fmt.Sprintf("need at least %d", c.MinItems)})
	}
	if c.HasMaxItems && n > c.MaxItems {
		errs = append(errs, &PathError{Path: aggPath, Msg: fmt.Sprintf("at most %d allowed", c.MaxItems)})
	}

	for i := range n {
		itemPath := joinPath(containerPath, indexedName(c.Name, i))
		ev, ok := deref(v.Index(i))
		if !ok {
			continue
		}
		errs = append(errs, checkConstraints(c, itemPath, ev)...)
		errs = append(errs, checkChildren(c.Children, ev, itemPath, present)...)
	}
	return errs
}

// deref follows a pointer value (an optional element such as *Loop) to its
// target. It reports ok=false for a nil pointer, meaning "not present,
// nothing to check."
func deref(v reflect.Value) (reflect.Value, bool) {
	if v.Kind() != reflect.Pointer {
		return v, true
	}
	if v.IsNil() {
		return reflect.Value{}, false
	}
	return v.Elem(), true
}

// checkConstraints applies n's jsonschema constraints to v, using the
// exact messages design section 7.2 specifies.
func checkConstraints(n *node, path string, v reflect.Value) []*PathError {
	errs := make([]*PathError, 0, 2)
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		if n.HasMinLength && len([]rune(s)) < n.MinLength {
			errs = append(errs, &PathError{Path: path, Msg: "must not be empty"})
		}
		if n.HasPattern {
			if msg, bad := checkPattern(n.Pattern, s); bad {
				errs = append(errs, &PathError{Path: path, Msg: msg})
			}
		}
		// outcome's legality depends on job, not on the full cross-job
		// Outcome enum, so checkHeader's pair check replaces this generic
		// enum check for it; every other enum field uses it as-is.
		if n.Enum && n.Name != attrOutcome {
			if msg, bad := checkEnum(v); bad {
				errs = append(errs, &PathError{Path: path, Msg: msg})
			}
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		f := float64(v.Int())
		if n.HasMinimum && f < n.Minimum {
			errs = append(errs, &PathError{Path: path, Msg: "must be >= " + formatBound(n.Minimum)})
		}
		if n.HasMaximum && f > n.Maximum {
			errs = append(errs, &PathError{Path: path, Msg: "must be <= " + formatBound(n.Maximum)})
		}
	}
	return errs
}

// checkPattern reports whether s fails pattern, and the message to use.
// Fence.ExistedBecause is the one field whose pattern is a plain phrase
// rather than a regular expression proper; design section 7.2 gives it its
// own message instead of the general "must match <pattern>".
func checkPattern(pattern, s string) (msg string, bad bool) {
	re, err := regexp.Compile(pattern)
	if err != nil || re.MatchString(s) {
		return "", false
	}
	if pattern == "existed because" {
		return `must contain "existed because"`, true
	}
	return "must match " + pattern, true
}

// checkEnum reports whether v's string value is not one of its type's
// Values(), and the message to use.
func checkEnum(v reflect.Value) (msg string, bad bool) {
	values, ok := reflect.TypeAssert[interface{ Values() []string }](v)
	if !ok {
		return "", false
	}
	if slices.Contains(values.Values(), v.String()) {
		return "", false
	}
	return "must be one of " + strings.Join(values.Values(), "|"), true
}

func formatBound(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ---- Layer 1: the presence pass -------------------------------------------

// presenceSet walks elem's raw XML, guided by root, and returns the set of
// scalar (non-slice) attribute and element paths actually present in the
// document. A repeated element's count is read from the decoded value
// instead (checkSlice): its absence is length zero, so minItems alone
// governs it, and no presence tracking is needed for the count itself
// (design section 6.4). Presence still needs to descend into each item,
// because a required scalar field nested inside a repeated element can be
// missing from one item without being missing from the others.
func presenceSet(elem []byte, root *node) (map[string]bool, error) {
	dec := xml.NewDecoder(bytes.NewReader(elem))
	set := make(map[string]bool)

	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	start, ok := tok.(xml.StartElement)
	if !ok {
		return nil, fmt.Errorf("presence: expected a start element, got %T", tok)
	}

	recordAttrs(set, "", root, start.Attr)
	if err := walkContent(dec, "", root, set); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return set, nil
}

// walkContent reads tokens until n's own end element, recording presence
// for every attribute and scalar element among n's children, indexing a
// repeated child's nested paths as it goes.
func walkContent(dec *xml.Decoder, path string, n *node, set map[string]bool) error {
	counts := map[string]int{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := walkStart(dec, path, n, t, counts, set); err != nil {
				return err
			}
		case xml.EndElement:
			return nil
		}
	}
}

func walkStart(dec *xml.Decoder, path string, n *node, t xml.StartElement, counts map[string]int, set map[string]bool) error {
	child := matchChild(n, t.Name.Local)
	if child == nil {
		return dec.Skip()
	}

	var childPath string
	if child.Slice {
		i := counts[child.Name]
		counts[child.Name]++
		childPath = joinPath(path, indexedName(child.Name, i))
	} else {
		childPath = joinPath(path, child.Name)
		set[childPath] = true
	}

	recordAttrs(set, childPath, child, t.Attr)
	if !hasElementChildren(child) {
		return dec.Skip()
	}
	return walkContent(dec, childPath, child, set)
}

// matchChild returns n's element or wrapper child named localName, the
// only kinds a StartElement token can match (attributes and chardata never
// have their own start tag).
func matchChild(n *node, localName string) *node {
	for _, c := range n.Children {
		if (c.Kind == kindElement || c.Kind == kindWrapper) && c.Name == localName {
			return c
		}
	}
	return nil
}

// hasElementChildren reports whether n has a nested element or wrapper
// worth recursing for. A leaf whose only children are attributes and
// chardata has nothing further to discover: its own presence and value are
// already handled by its parent's dispatch and the reflective pass.
func hasElementChildren(n *node) bool {
	for _, c := range n.Children {
		if c.Kind == kindElement || c.Kind == kindWrapper {
			return true
		}
	}
	return false
}

// recordAttrs marks each of n's attribute children present in set when its
// name appears in attrs, regardless of value: an empty attribute value is
// still a present attribute.
func recordAttrs(set map[string]bool, path string, n *node, attrs []xml.Attr) {
	for _, c := range n.Children {
		if c.Kind != kindAttr {
			continue
		}
		for _, a := range attrs {
			if a.Name.Local == c.Name {
				set[joinPath(path, c.Name)] = true
				break
			}
		}
	}
}
