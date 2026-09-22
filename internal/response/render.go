package response

import (
	"reflect"
	"strconv"
	"strings"
)

// RenderTemplate looks up the concrete type registered to (job, outcome)
// and prints the annotated template a run's prompt closes with: the type's
// shape, walked from the same descriptor shape.go builds for parsing and
// validation, with concrete root attributes job="<job>" outcome="<outcome>"
// (design section 6.8). An unregistered pair returns Lookup's own error
// unchanged, so callers (and selftest) see the one canonical string.
func RenderTemplate(job Job, outcome Outcome) (string, error) {
	r, err := Lookup(job, outcome)
	if err != nil {
		return "", err
	}
	root := shapeOf(reflect.TypeOf(r).Elem())
	override := map[string]string{attrJob: string(job), attrOutcome: string(outcome)}

	attrs, chardata, children := splitChildren(root.Children)

	var buf strings.Builder
	tag, comment := startTagAndComment(&node{Name: "zing"}, attrs, override, true)
	buf.WriteString(tag + ">")
	writeComment(&buf, comment)
	buf.WriteByte('\n')
	if chardata != nil {
		buf.WriteString("  ...\n")
	}
	for _, c := range children {
		renderChild(&buf, c, 1, override)
	}
	buf.WriteString("</zing>\n")

	return buf.String(), nil
}

// ---- one XML line per shape node ------------------------------------------

// renderChild renders one child of a struct (an element or wrapper; attrs
// and chardata are handled by the struct's own renderElement call, never
// passed here). It intercepts the two none-union types (Migrations,
// Deletions) before the generic path, since they render two example lines
// instead of one (design section 6.8).
func renderChild(buf *strings.Builder, n *node, depth int, override map[string]string) {
	switch n.Kind {
	case kindWrapper:
		indent := strings.Repeat("  ", depth)
		buf.WriteString(indent + "<" + n.Name + ">\n")
		renderChild(buf, n.Children[0], depth+1, override)
		buf.WriteString(indent + "</" + n.Name + ">\n")
	case kindElement:
		if isNoneUnion(n) {
			renderNoneUnion(buf, n, depth)
			return
		}
		renderElement(buf, n, depth, override, false)
	case kindAttr, kindChardata:
		// unreachable: a struct's own attrs and chardata are consumed by
		// splitChildren in renderElement/RenderTemplate before reaching
		// renderChild; only element and wrapper kinds appear as a child
		// of another node's "rest" slice.
	}
}

// renderElement renders one element line (or block, for a struct-typed
// element): a scalar leaf as "<name>...</name>", an attrs-only struct
// (no chardata, no nested elements) self-closing, and everything else as
// an open tag, its content, and a close tag. skipOwnNote is true only for
// the synthetic root call, whose node carries no field of its own to
// describe.
func renderElement(buf *strings.Builder, n *node, depth int, override map[string]string, skipOwnNote bool) {
	indent := strings.Repeat("  ", depth)

	if !isStructType(n) {
		tag, comment := startTagAndComment(n, nil, override, skipOwnNote)
		buf.WriteString(indent + tag + ">...</" + n.Name + ">")
		writeComment(buf, comment)
		buf.WriteByte('\n')
		return
	}

	attrs, chardata, children := splitChildren(n.Children)
	tag, comment := startTagAndComment(n, attrs, override, skipOwnNote)

	switch {
	case chardata == nil && len(children) == 0:
		buf.WriteString(indent + tag + "/>")
		writeComment(buf, comment)
		buf.WriteByte('\n')
	case chardata != nil && len(children) == 0:
		buf.WriteString(indent + tag + ">...</" + n.Name + ">")
		writeComment(buf, comment)
		buf.WriteByte('\n')
	default:
		buf.WriteString(indent + tag + ">")
		writeComment(buf, comment)
		buf.WriteByte('\n')
		if chardata != nil {
			buf.WriteString(indent + "  ...\n")
		}
		for _, c := range children {
			renderChild(buf, c, depth+1, override)
		}
		buf.WriteString(indent + "</" + n.Name + ">\n")
	}
}

// isNoneUnion reports whether n is the Migrations or Deletions element,
// design section 6.8's one hardcoded structural special case: both types
// render a none="true" example line and a populated-list example line,
// not the generic attrs/children rendering every other struct gets.
func isNoneUnion(n *node) bool {
	return n.GoType != nil && (n.GoType.Name() == "Migrations" || n.GoType.Name() == "Deletions")
}

// renderNoneUnion renders both forms of a none-union element: the
// self-closing none="true" line, commented from the None attribute's own
// doc tag, and the list line, commented as one-or-more (the semantic the
// none union enforces: present means non-empty) plus the item slice's own
// doc tag.
func renderNoneUnion(buf *strings.Builder, n *node, depth int) {
	indent := strings.Repeat("  ", depth)
	attrs, _, children := splitChildren(n.Children)

	var noneAttr, items *node
	for _, a := range attrs {
		if a.Name == "none" {
			noneAttr = a
		}
	}
	for _, c := range children {
		if c.Slice {
			items = c
		}
	}

	buf.WriteString(indent + "<" + n.Name + ` none="true"/>`)
	if noneAttr != nil {
		writeComment(buf, escape(noneAttr.Doc))
	}
	buf.WriteByte('\n')

	buf.WriteString(indent + "<" + n.Name + ">\n")
	if items != nil {
		oneOrMore := *items
		oneOrMore.HasMinItems, oneOrMore.MinItems, oneOrMore.HasMaxItems = true, 1, false
		renderChild(buf, &oneOrMore, depth+1, nil)
	}
	buf.WriteString(indent + "</" + n.Name + ">\n")
}

// ---- start tag + trailing comment ------------------------------------------

// startTagAndComment builds n's opening tag (with attrs' placeholder or
// overridden values) and its trailing comment: n's own note first (unless
// skipOwnNote), then each attribute's own note, joined in encounter order
// (design section 6.8: "attributes render inside their element's start
// tag, with their notes joined into that element's trailing comment").
func startTagAndComment(n *node, attrs []*node, override map[string]string, skipOwnNote bool) (tag, comment string) {
	var tagBuf strings.Builder
	tagBuf.WriteString("<" + n.Name)
	for _, a := range attrs {
		tagBuf.WriteString(" " + a.Name + `="` + escape(attrValue(a, override)) + `"`)
	}
	tag = tagBuf.String()

	var frags []string
	if !skipOwnNote {
		if own := noteParts(n); len(own) > 0 {
			frags = append(frags, strings.Join(own, ", "))
		}
	}
	for _, a := range attrs {
		if parts := noteParts(a); len(parts) > 0 {
			frags = append(frags, a.Name+": "+strings.Join(parts, ", "))
		}
	}
	return tag, strings.Join(frags, "; ")
}

func attrValue(a *node, override map[string]string) string {
	if v, ok := override[a.Name]; ok {
		return v
	}
	return "..."
}

func writeComment(buf *strings.Builder, comment string) {
	if comment == "" {
		return
	}
	buf.WriteString(" <!-- " + comment + " -->")
}

// ---- the note: type, optional, cardinality, allowed values, doc -----------

// noteParts builds n's comment pieces in design section 6.8's fixed order:
// field type, an optional marker (scalar fields only; a slice folds
// optionality into its cardinality phrase instead), the cardinality note,
// the allowed values, then the doc text, skipping any piece that does not
// apply. A doc identical to the immediately preceding piece (Question.
// Options' own doc tag literally reads "none, or two to four", the same
// text as its hardcoded cardinality note) is dropped rather than repeated.
func noteParts(n *node) []string {
	var parts []string
	if t := typeName(n); t != "" {
		parts = append(parts, t)
	}
	if !n.Slice && !n.Required {
		parts = append(parts, "optional")
	}
	if n.Slice {
		parts = append(parts, cardinalityNote(n))
	}
	if n.Enum {
		if values := enumValues(n); len(values) > 0 {
			parts = append(parts, "one of: "+strings.Join(values, " | "))
		}
	}
	if n.Doc != "" {
		d := escape(n.Doc)
		if len(parts) == 0 || parts[len(parts)-1] != d {
			parts = append(parts, d)
		}
	}
	return parts
}

// typeName renders n's Go field type as design section 6.8 names it:
// string, int, bool, or the enum or struct type's own name.
func typeName(n *node) string {
	if n.GoType == nil {
		return ""
	}
	if n.Enum {
		return n.GoType.Name()
	}
	switch n.GoType.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "int"
	default:
		return n.GoType.Name()
	}
}

// cardinalityNote maps a slice node's minItems/maxItems to the phrase
// design section 6.8 lists, generalizing beyond the four named examples
// for the bounds this schema actually uses (design section 6.8: "map the
// actual bounds found in the type's jsonschema tags to the right
// phrase"). Question.Options is the one hardcoded exception: its rule is
// "none, or two to four" regardless of its plain maxItems=4 tag, because
// that cardinality comes from the option-count check (plancheck.go),
// not from minItems.
func cardinalityNote(n *node) string {
	if n.Name == "option" && n.GoType != nil && n.GoType.Name() == "Option" {
		return "none, or two to four"
	}
	switch {
	case !n.HasMinItems && !n.HasMaxItems:
		return "optional, zero or more"
	case n.HasMinItems && !n.HasMaxItems:
		if n.MinItems == 1 {
			return "one or more"
		}
		return "at least " + numWord(n.MinItems)
	case !n.HasMinItems && n.HasMaxItems:
		return "at most " + numWord(n.MaxItems)
	default:
		return numWord(n.MinItems) + " to " + numWord(n.MaxItems)
	}
}

var numWords = [...]string{"", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}

// numWord spells out one through nine, matching design section 6.8's own
// "two to four" phrasing; a bound of 10 or more prints as a digit,
// matching its "at most 12".
func numWord(n int) string {
	if n >= 1 && n < len(numWords) {
		return numWords[n]
	}
	return strconv.Itoa(n)
}

// enumValues returns n's enum type's Values(), or nil if n.GoType does not
// carry one (should not happen when n.Enum is true; shape.go's Enum
// detection and this lookup read the same Values() method).
func enumValues(n *node) []string {
	if n.GoType == nil {
		return nil
	}
	v, ok := reflect.TypeAssert[interface{ Values() []string }](reflect.Zero(n.GoType))
	if !ok {
		return nil
	}
	return v.Values()
}

// ---- shared shape helpers ---------------------------------------------

// splitChildren partitions n's children into its attributes (rendered
// inside its own start tag), its chardata child if it has one (its own
// text content), and the rest (nested elements and wrappers, rendered as
// children), preserving each group's shape order.
func splitChildren(children []*node) (attrs []*node, chardata *node, rest []*node) {
	for _, c := range children {
		switch c.Kind {
		case kindAttr:
			attrs = append(attrs, c)
		case kindChardata:
			chardata = c
		default:
			rest = append(rest, c)
		}
	}
	return attrs, chardata, rest
}

// isStructType reports whether n's own Go value is a struct (so it may
// carry attrs, chardata, or nested elements of its own), as opposed to a
// scalar (string, int, bool, or an enum's underlying string), which
// always renders as a plain "<name>...</name>" leaf.
func isStructType(n *node) bool {
	return n.GoType != nil && n.GoType.Kind() == reflect.Struct
}

// escape replaces the two characters design section 6.8 requires escaped
// in any rendered value: '<' and '&'. Quotes are left alone; nothing this
// renderer emits needs XML attribute-quote escaping, since job and
// outcome (the only concrete values) are plain enum tokens and every
// other placeholder is the literal "...".
var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;")

func escape(s string) string {
	return xmlEscaper.Replace(s)
}
