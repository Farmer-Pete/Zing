package response

import (
	"bytes"
	"encoding/xml"
	"log/slog"
	"reflect"
)

// repairFrame is one open element on repairBareLessThan's stack: the tag
// name exactly as written, and the shape node that names its children
// (nil when the enclosing frame's node is nil, or names no such child).
type repairFrame struct {
	name string
	node *node
}

// repairCandidate repairs the candidate that starts elem. ok is false when
// its header names no registered pair, the scan fails, or nothing needed
// escaping.
func repairCandidate(elem []byte) (repaired []byte, escaped int, ok bool) {
	root := candidateShape(elem)
	if root == nil {
		return nil, 0, false
	}
	repaired, escaped, ok = repairBareLessThan(elem, root)
	if !ok || escaped == 0 {
		return nil, 0, false
	}
	return repaired, escaped, true
}

// candidateShape returns the shape of the type registered for the zing
// header that starts elem, or nil when the first token is not a zing start
// element in no namespace with a registered job and outcome.
func candidateShape(elem []byte) *node {
	dec := xml.NewDecoder(bytes.NewReader(elem))
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	start, ok := tok.(xml.StartElement)
	if !ok || start.Name.Local != zingElementName || start.Name.Space != "" {
		return nil
	}
	job, outcome, ok := headerAttrs(start.Attr)
	if !ok {
		return nil
	}
	r, err := Lookup(job, outcome)
	if err != nil {
		return nil
	}
	return shapeOf(reflect.TypeOf(r).Elem())
}

// repairBareLessThan rewrites each < in elem that cannot start markup as
// &lt; (bug fix: four live runs quoted Go code or a placeholder such as
// <nil> inside <report>, and each spent a retry run). elem starts at a
// <zing tag; root is the shape of the type its header names.
//
// Inside a free-text element every opening < is text. In an element that
// mixes text with child elements, a < before a name that is not one of its
// children is text. Anywhere else only a < that cannot start a name is
// text. A closing tag is never escaped, so misnesting still fails. A
// comment, CDATA section, or processing instruction is always markup: one
// with no closer makes the whole scan fail rather than be escaped.
//
// It returns the root through its closing tag, the count of < escaped, and
// false when the root never closes, a tag is cut off, or a comment, CDATA
// section, or processing instruction never closes.
func repairBareLessThan(elem []byte, root *node) (repaired []byte, escaped int, ok bool) {
	var out bytes.Buffer
	out.Grow(len(elem) + 32)
	var stack []repairFrame

	for i := 0; i < len(elem); {
		rest := elem[i:]
		if rest[0] != '<' {
			out.WriteByte(rest[0])
			i++
			continue
		}

		if openTag, closeTag, ok := openDelim(rest); ok {
			end := bytes.Index(rest[len(openTag):], []byte(closeTag))
			if end < 0 {
				// An unterminated opener stays markup; the strict error stands.
				return nil, 0, false
			}
			stop := len(openTag) + end + len(closeTag)
			out.Write(rest[:stop])
			i += stop
			continue
		}

		if len(rest) > 1 && rest[1] == '/' {
			n := closeTagLen(rest, stack)
			if n == 0 {
				// A mismatched or cut-off closing tag, left for the decoder to reject.
				out.WriteByte('<')
				i++
				continue
			}
			out.Write(rest[:n])
			i += n
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return out.Bytes(), escaped, true
			}
			continue
		}

		name, nameLen := scanName(rest[1:])
		if len(stack) == 0 {
			if nameLen == 0 {
				return nil, 0, false
			}
		} else if isTextLessThan(stack[len(stack)-1].node, name) {
			out.WriteString("&lt;")
			escaped++
			i++
			continue
		}

		tagLen, selfClosing, attrEscaped, ok := copyTag(&out, rest, 1+nameLen)
		if !ok {
			return nil, 0, false
		}
		escaped += attrEscaped
		i += tagLen
		if selfClosing {
			if len(stack) == 0 {
				return out.Bytes(), escaped, true
			}
			continue
		}

		var child *node
		if len(stack) == 0 {
			child = root
		} else if top := stack[len(stack)-1].node; top != nil {
			child = childNamed(top, name)
		}
		stack = append(stack, repairFrame{name: name, node: child})
	}

	return nil, 0, false
}

// isTextLessThan reports whether a < opening name inside top is text. name
// is empty when the byte after < cannot start a name.
func isTextLessThan(top *node, name string) bool {
	switch {
	case name == "":
		return true
	case top == nil:
		return false
	case isFreeText(top):
		return true
	default:
		return hasChardata(top) && childNamed(top, name) == nil
	}
}

// isFreeText reports whether n is an element with no element children.
func isFreeText(n *node) bool {
	if n.Kind != kindElement {
		return false
	}
	for _, c := range n.Children {
		if c.Kind == kindElement || c.Kind == kindWrapper {
			return false
		}
	}
	return true
}

// hasChardata reports whether n holds text of its own beside its children.
func hasChardata(n *node) bool {
	for _, c := range n.Children {
		if c.Kind == kindChardata {
			return true
		}
	}
	return false
}

// childNamed returns n's element or wrapper child named name, or nil.
func childNamed(n *node, name string) *node {
	for _, c := range n.Children {
		if (c.Kind == kindElement || c.Kind == kindWrapper) && c.Name == name {
			return c
		}
	}
	return nil
}

// closeTagLen returns the length of the closing tag at the start of rest
// when it closes the top frame, else 0. XML whitespace (isXMLSpace) may
// sit between the name and the >.
func closeTagLen(rest []byte, stack []repairFrame) int {
	if len(stack) == 0 {
		return 0
	}
	name, n := scanName(rest[2:])
	if n == 0 || name != stack[len(stack)-1].name {
		return 0
	}
	j := 2 + n
	for j < len(rest) && isXMLSpace(rest[j]) {
		j++
	}
	if j < len(rest) && rest[j] == '>' {
		return j + 1
	}
	return 0
}

// isXMLSpace reports an XML whitespace byte: space, tab, carriage return,
// or line feed (XML 1.0 section 2.3).
func isXMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// scanName reads the XML name at the start of b and its length, or "" and
// 0 when b does not start with a name-start byte.
func scanName(b []byte) (name string, length int) {
	if len(b) == 0 || !isNameStart(b[0]) {
		return "", 0
	}
	n := 1
	for n < len(b) && isNameByte(b[n]) {
		n++
	}
	return string(b[:n]), n
}

// isNameStart reports a byte that may start an XML name: an ASCII letter,
// '_', ':', or any byte of a multi-byte UTF-8 rune.
func isNameStart(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || c == '_' || c == ':' || c >= 0x80
}

// isNameByte reports a byte that may continue an XML name: anything
// isNameStart allows, plus '-', '.', or an ASCII digit.
func isNameByte(c byte) bool {
	return isNameStart(c) || c == '-' || c == '.' || ('0' <= c && c <= '9')
}

// copyTag copies the start tag at the start of rest into out. from is the
// offset just past its name. A quoted attribute value is copied with each <
// in it escaped as &lt; (bug fix: run 75 failed with "unescaped < inside
// quoted string"). ok is false for a raw < outside a quoted attribute value
// or a tag cut off by the end of input.
func copyTag(out *bytes.Buffer, rest []byte, from int) (tagLen int, selfClosing bool, escaped int, ok bool) {
	out.Write(rest[:from])
	for j := from; j < len(rest); j++ {
		switch c := rest[j]; c {
		case '"', '\'':
			end := bytes.IndexByte(rest[j+1:], c)
			if end < 0 {
				return 0, false, 0, false
			}
			value := rest[j+1 : j+1+end]
			escaped += bytes.Count(value, []byte("<"))
			out.WriteByte(c)
			out.Write(bytes.ReplaceAll(value, []byte("<"), []byte("&lt;")))
			out.WriteByte(c)
			j += 1 + end
		case '>':
			out.WriteByte(c)
			return j + 1, j > from && rest[j-1] == '/', escaped, true
		case '<':
			return 0, false, 0, false
		default:
			out.WriteByte(c)
		}
	}
	return 0, false, 0, false
}

// logRepair records a repaired document at INFO, so settings.log_level
// warn or error drops it. Issue #94 adds the run token. roots is the
// number of well-formed roots the repair produced: parseRepaired always
// passes 1, but extractRepaired's candidate scan can repair more than one
// candidate in a single message. runtime.parseFinalMessage then rejects
// that as reasonMultipleZingDocs, so the count here keeps the record from
// reading as a clean success when it wasn't one.
func logRepair(escaped, roots int) {
	slog.Info("repaired bare < in zing document", "escaped", escaped, "roots", roots)
}
