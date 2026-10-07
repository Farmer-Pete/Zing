package response

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
)

// inlineFrame is one open element on flattenInlineTags's stack: the schema
// node that names its children (nil once inline is true, since an inline
// tag's own children are never looked up), whether its content is being
// flattened, and the tag's own local name (read back on its end tag, to
// decide the replacement).
type inlineFrame struct {
	node   *node
	inline bool
	name   string
}

// flattenInline rewrites every inline start and end tag that sits inside a
// free-text field of elem's schema: a backtick for local name code, and
// nothing for any other name. A self-closing inline tag is removed.
// rewritten counts the tags replaced or removed. It is 0, and out is elem,
// when the header names no registered pair, a token fails to decode, or
// nothing was inline.
func flattenInline(elem []byte) (out []byte, rewritten int) {
	root := candidateShape(elem)
	if root == nil {
		return elem, 0
	}
	flattened, n, ok := flattenInlineTags(elem, root)
	if !ok || n == 0 {
		return elem, 0
	}
	return flattened, n
}

// flattenInlineTags walks elem against root's shape and returns the
// rewritten bytes, the count of tags replaced or removed, and whether
// elem decoded cleanly. A start tag is inline when the top frame is
// already inline, or when it names no schema child of the top frame and
// isTextLessThan says the top frame treats it as text. A start tag that
// names no schema child and is not inline is skipped with Decoder.Skip
// and copied unchanged, children included.
func flattenInlineTags(elem []byte, root *node) (flattened []byte, rewritten int, ok bool) {
	dec := xml.NewDecoder(bytes.NewReader(elem))
	var out bytes.Buffer
	out.Grow(len(elem))
	var stack []inlineFrame
	var last int64

	for {
		before := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, 0, false
		}
		after := dec.InputOffset()

		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) == 0 {
				out.Write(elem[last:after])
				last = after
				stack = append(stack, inlineFrame{node: root})
				continue
			}

			top := stack[len(stack)-1]
			if top.inline {
				out.Write(elem[last:before])
				rewritten += writeInlineReplacement(&out, t.Name.Local)
				last = after
				stack = append(stack, inlineFrame{inline: true, name: t.Name.Local})
				continue
			}

			if child := childNamed(top.node, t.Name.Local); child != nil {
				out.Write(elem[last:after])
				last = after
				stack = append(stack, inlineFrame{node: child})
				continue
			}

			if isTextLessThan(top.node, t.Name.Local) {
				out.Write(elem[last:before])
				rewritten += writeInlineReplacement(&out, t.Name.Local)
				last = after
				stack = append(stack, inlineFrame{inline: true, name: t.Name.Local})
				continue
			}

			// Not a schema child and not text: leave this element, and
			// everything inside it, exactly as written.
			out.Write(elem[last:after])
			if err := dec.Skip(); err != nil {
				return nil, 0, false
			}
			skipEnd := dec.InputOffset()
			out.Write(elem[after:skipEnd])
			last = skipEnd

		case xml.EndElement:
			if len(stack) == 0 {
				return nil, 0, false
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]

			if top.inline {
				if after > before {
					// A real end tag; a self-closing tag's implicit end
					// token has zero width and needs no span removed.
					out.Write(elem[last:before])
					rewritten += writeInlineReplacement(&out, top.name)
					last = after
				}
				continue
			}

			out.Write(elem[last:after])
			last = after
			if len(stack) == 0 {
				out.Write(elem[last:])
				return out.Bytes(), rewritten, true
			}
		}
	}

	return nil, 0, false
}

// logFlatten records a document whose free-text fields held inline tags
// at INFO, the level logRepair uses, so settings.log_level warn or error
// drops it. attrs are extra slog key-value pairs appended to the record,
// such as "run_token", token.
func logFlatten(tags int, attrs ...any) {
	slog.Info("flattened inline tags in zing document", append([]any{"tags", tags}, attrs...)...)
}

// writeInlineReplacement writes one inline tag's replacement to out: a
// backtick for local name code, nothing otherwise. It always returns 1,
// the one tag replaced or removed.
func writeInlineReplacement(out *bytes.Buffer, name string) int {
	if name == "code" {
		out.WriteByte('`')
	}
	return 1
}
