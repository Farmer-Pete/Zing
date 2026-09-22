package response

import (
	"bytes"
	"encoding/xml"
	"errors"
)

// Document is an extracted zing element: its dispatched, decoded Response
// and the exact bytes of the element as it appeared in the input, opening
// tag included.
type Document struct {
	Response Response
	Elem     []byte
}

// Parse extracts the first well-formed <zing> element from input and
// dispatches it to its registered type.
//
// Extraction never feeds the whole message to an xml.Decoder, because the
// log text around the document is arbitrary and may contain a raw '<' or
// '&' that is not well-formed XML on its own. Instead this is a candidate
// scan plus a scoped decode (design section 6.3): every byte offset where
// the literal "<zing" is immediately followed by a name-boundary byte is a
// candidate, tried in order. A candidate fails when its start element is
// not exactly "zing" in no namespace, when it lacks a job or outcome
// attribute, when its (job, outcome) pair is not registered, or when it
// does not decode; any failure moves on to the next candidate.
//
// A missing or malformed element both fall through to the same result:
// Parse returns the exact string "no zing element in final message".
func Parse(input []byte) (*Document, error) {
	excluded := excludedRanges(input)
	for _, offset := range candidateOffsets(input) {
		if inRanges(offset, excluded) {
			continue
		}
		if doc, ok := tryDecode(input, offset); ok {
			return doc, nil
		}
	}
	return nil, errors.New("no zing element in final message")
}

const zingMarker = "<zing"

// candidateOffsets returns every byte offset in input where the literal
// <zing is immediately followed by a name-boundary byte (space, tab,
// newline, '>', or '/'), in order, so "<zinger>" never matches.
func candidateOffsets(input []byte) []int {
	var offsets []int
	for i := 0; ; {
		idx := bytes.Index(input[i:], []byte(zingMarker))
		if idx < 0 {
			return offsets
		}
		at := i + idx
		next := at + len(zingMarker)
		if next < len(input) && isNameBoundary(input[next]) {
			offsets = append(offsets, at)
		}
		i = at + 1
	}
}

// excludedRanges finds every comment, CDATA section, and processing
// instruction in input by their literal delimiters, without requiring the
// surrounding bytes to be well-formed XML (log text may contain a raw '<'
// or '&'). A candidate offset inside one of these is log commentary about
// zing syntax, not a document, and must not match (design section 6.3: the
// scan "skips comments and CDATA and PIs").
func excludedRanges(input []byte) [][2]int {
	var ranges [][2]int
	for i := 0; i < len(input); {
		openTag, closeTag, ok := openDelim(input[i:])
		if !ok {
			i++
			continue
		}
		end := bytes.Index(input[i+len(openTag):], []byte(closeTag))
		if end < 0 {
			ranges = append(ranges, [2]int{i, len(input)})
			return ranges
		}
		stop := i + len(openTag) + end + len(closeTag)
		ranges = append(ranges, [2]int{i, stop})
		i = stop
	}
	return ranges
}

func openDelim(rest []byte) (openTag, closeTag string, ok bool) {
	switch {
	case bytes.HasPrefix(rest, []byte("<!--")):
		return "<!--", "-->", true
	case bytes.HasPrefix(rest, []byte("<![CDATA[")):
		return "<![CDATA[", "]]>", true
	case bytes.HasPrefix(rest, []byte("<?")):
		return "<?", "?>", true
	default:
		return "", "", false
	}
}

func inRanges(offset int, ranges [][2]int) bool {
	for _, r := range ranges {
		if offset >= r[0] && offset < r[1] {
			return true
		}
	}
	return false
}

func isNameBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '>', '/':
		return true
	default:
		return false
	}
}

// tryDecode attempts to extract a Document starting at offset. It reports
// success only when the start element is exactly "zing" in no namespace,
// carries both a job and an outcome attribute naming a registered pair,
// and decodes cleanly.
func tryDecode(input []byte, offset int) (*Document, bool) {
	dec := xml.NewDecoder(bytes.NewReader(input[offset:]))

	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	start, ok := tok.(xml.StartElement)
	if !ok || start.Name.Local != "zing" || start.Name.Space != "" {
		return nil, false
	}

	job, outcome, ok := headerAttrs(start.Attr)
	if !ok {
		return nil, false
	}

	r, err := Lookup(job, outcome)
	if err != nil {
		return nil, false
	}

	if err := dec.DecodeElement(r, &start); err != nil {
		return nil, false
	}

	//nolint:gosec // dec.InputOffset() is bounded by len(input[offset:]), which fits in an int already.
	end := offset + int(dec.InputOffset())
	return &Document{Response: r, Elem: input[offset:end]}, true
}

func headerAttrs(attrs []xml.Attr) (job Job, outcome Outcome, ok bool) {
	var haveJob, haveOutcome bool
	for _, a := range attrs {
		switch a.Name.Local {
		case attrJob:
			job, haveJob = Job(a.Value), true
		case attrOutcome:
			outcome, haveOutcome = Outcome(a.Value), true
		}
	}
	return job, outcome, haveJob && haveOutcome
}
