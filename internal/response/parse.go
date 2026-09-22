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
	var firstLookupErr error
	for _, offset := range candidateOffsets(input) {
		if inRanges(offset, excluded) {
			continue
		}
		doc, err := tryDecode(input, offset)
		if doc != nil {
			return doc, nil
		}
		if err != nil && !errors.Is(err, errMalformedCandidate) && firstLookupErr == nil {
			firstLookupErr = err
		}
	}
	if firstLookupErr != nil {
		return nil, firstLookupErr
	}
	return nil, errors.New("no zing element in final message")
}

const zingMarker = "<zing"

// candidateOffsets returns every byte offset in input where the literal
// <zing is immediately followed by a name-boundary byte (space, tab,
// carriage return, newline, '>', or '/'), in order, so "<zinger>" never
// matches.
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
//
// An opener with no matching closer is not a delimited range at all: it is
// arbitrary log text that happens to start with "<!--", "<![CDATA[", or
// "<?" and never closes. Excluding "the rest of input" for it would hide
// any real <zing> document that follows, so an unmatched opener is simply
// skipped past (not recorded) and the scan continues right after it.
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
			i += len(openTag)
			continue
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
	// The whitespace set is XML's (space, tab, carriage return, line feed;
	// XML 1.0 section 2.3) plus the two tag terminators, so a CRLF-formatted
	// "<zing\r\n job=...>" tag is recognised, not skipped.
	case ' ', '\t', '\r', '\n', '>', '/':
		return true
	default:
		return false
	}
}

// errMalformedCandidate is tryDecode's internal sentinel for "this
// candidate is not well formed", letting Parse tell a malformed candidate
// apart from one that is well formed but names an unregistered pair
// (which reports Lookup's own error instead). It never escapes this
// package.
var errMalformedCandidate = errors.New("malformed zing candidate")

// tryDecode attempts to extract a Document starting at offset. It returns
// a non-nil Document only when the start element is exactly "zing" in no
// namespace, carries both a job and an outcome attribute naming a
// registered pair, and decodes cleanly.
//
// When the pair is not registered, tryDecode still skips the whole element
// to confirm it is otherwise well formed: on success it returns Lookup's
// own error (nil Document, non-nil error), so Parse can keep scanning for
// a later candidate whose pair does resolve before falling back to this
// one. A malformed candidate (any decode failure, including one with an
// unregistered pair) returns errMalformedCandidate instead, so it never
// contributes a lookup error of its own.
func tryDecode(input []byte, offset int) (*Document, error) {
	dec := xml.NewDecoder(bytes.NewReader(input[offset:]))

	tok, err := dec.Token()
	if err != nil {
		return nil, errMalformedCandidate
	}
	start, ok := tok.(xml.StartElement)
	if !ok || start.Name.Local != "zing" || start.Name.Space != "" {
		return nil, errMalformedCandidate
	}

	job, outcome, ok := headerAttrs(start.Attr)
	if !ok {
		return nil, errMalformedCandidate
	}

	r, err := Lookup(job, outcome)
	if err != nil {
		if skipErr := dec.Skip(); skipErr != nil {
			return nil, errMalformedCandidate
		}
		return nil, err
	}

	if err := dec.DecodeElement(r, &start); err != nil {
		return nil, errMalformedCandidate
	}

	//nolint:gosec // dec.InputOffset() is bounded by len(input[offset:]), which fits in an int already.
	end := offset + int(dec.InputOffset())
	return &Document{Response: r, Elem: input[offset:end]}, nil
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
