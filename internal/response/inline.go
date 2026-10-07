package response

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
)

// inlineFrame is one open element on flattenInline's stack: the schema
// node that names its children (nil once inline is true, since an inline
// tag's own children are never looked up), whether its content is being
// flattened, and the tag's own local name (read back on its end tag, to
// decide the replacement).
type inlineFrame struct {
	node   *node
	inline bool
	name   string
}

// flattenInline walks elem against the shape its header names and
// returns the rewritten bytes and the count of tags replaced or
// removed. It returns elem and 0 when the header names no registered
// pair, a token fails to decode, or nothing was inline. A start tag is
// inline when the top frame is already inline, or when it names no
// schema child of the top frame and isTextLessThan says the top frame
// treats it as text. A start tag that names no schema child and is not
// inline is skipped with Decoder.Skip and copied unchanged, children
// included.
func flattenInline(elem []byte) (out []byte, rewritten int) {
	root := candidateShape(elem)
	if root == nil {
		return elem, 0
	}

	dec := xml.NewDecoder(bytes.NewReader(elem))
	var buf bytes.Buffer
	buf.Grow(len(elem))
	var stack []inlineFrame
	var last int64

	for {
		before := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return elem, 0
		}
		after := dec.InputOffset()

		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) == 0 {
				buf.Write(elem[last:after])
				last = after
				stack = append(stack, inlineFrame{node: root})
				continue
			}

			top := stack[len(stack)-1]
			if top.inline {
				buf.Write(elem[last:before])
				rewritten += writeInlineStart(&buf, t.Name.Local, selfClosingTag(elem, after))
				last = after
				stack = append(stack, inlineFrame{inline: true, name: t.Name.Local})
				continue
			}

			if child := childNamed(top.node, t.Name.Local); child != nil {
				buf.Write(elem[last:after])
				last = after
				stack = append(stack, inlineFrame{node: child})
				continue
			}

			if isTextLessThan(top.node, t.Name.Local) {
				buf.Write(elem[last:before])
				rewritten += writeInlineStart(&buf, t.Name.Local, selfClosingTag(elem, after))
				last = after
				stack = append(stack, inlineFrame{inline: true, name: t.Name.Local})
				continue
			}

			// Not a schema child and not text: leave this element, and
			// everything inside it, exactly as written.
			buf.Write(elem[last:after])
			if err := dec.Skip(); err != nil {
				return elem, 0
			}
			skipEnd := dec.InputOffset()
			buf.Write(elem[after:skipEnd])
			last = skipEnd

		case xml.EndElement:
			if len(stack) == 0 {
				return elem, 0
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]

			if top.inline {
				if after > before {
					// A real end tag; a self-closing tag's implicit end
					// token has zero width and needs no span removed.
					buf.Write(elem[last:before])
					rewritten += writeInlineReplacement(&buf, top.name)
					last = after
				}
				continue
			}

			buf.Write(elem[last:after])
			last = after
			if len(stack) == 0 {
				buf.Write(elem[last:])
				if rewritten == 0 {
					return elem, 0
				}
				return buf.Bytes(), rewritten
			}
		}
	}

	return elem, 0
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

// writeInlineStart writes an inline start tag's replacement to out: a
// backtick for a paired local name code, nothing for a self-closing tag
// of any name or for any other paired name. It always returns 1, the one
// tag replaced or removed; a self-closing tag's end token has zero width
// and writes nothing on its own.
func writeInlineStart(out *bytes.Buffer, name string, selfClosing bool) int {
	if !selfClosing && name == "code" {
		out.WriteByte('`')
	}
	return 1
}

// selfClosingTag reports whether the start tag whose bytes end just
// before elem[after] was written self-closing, such as <code/>.
func selfClosingTag(elem []byte, after int64) bool {
	return after >= 2 && elem[after-2] == '/'
}
