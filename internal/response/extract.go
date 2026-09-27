package response

import (
	"bytes"
	"encoding/xml"
)

// ExtractAll returns every syntactically well-formed <zing ...>...</zing>
// root found in text, in order, tolerating arbitrary log text around them
// (design section 4.1). It reuses Parse's own candidate-offset scan
// (parse.go) so the two functions never drift on what counts as a
// candidate or what text excludes one (a comment, a CDATA section, a
// processing instruction).
//
// Unlike Parse, a candidate counts here the moment its start element
// decodes as a bare "zing" element in no namespace and its matching end
// element is found: ExtractAll never looks the (job, outcome) pair up in
// the registry. That is what lets a hostile or unregistered header still
// come back as one root, for runtime.Run's final-message rule (design
// section 4.1) to react to with its own named reason, rather than losing
// the candidate to a generic "no zing element" result.
func ExtractAll(text string) []string {
	input := []byte(text)
	excluded := excludedRanges(input)

	var roots []string
	for _, offset := range candidateOffsets(input) {
		if inRanges(offset, excluded) {
			continue
		}
		if end, ok := wellFormedRootExtent(input, offset); ok {
			roots = append(roots, string(input[offset:end]))
		}
	}
	return roots
}

// wellFormedRootExtent reports the end offset of the well-formed root
// starting at offset, when its start element is exactly "zing" in no
// namespace and dec.Skip finds a matching end element.
func wellFormedRootExtent(input []byte, offset int) (int, bool) {
	dec := xml.NewDecoder(bytes.NewReader(input[offset:]))

	tok, err := dec.Token()
	if err != nil {
		return 0, false
	}
	start, ok := tok.(xml.StartElement)
	if !ok || start.Name.Local != zingElementName || start.Name.Space != "" {
		return 0, false
	}
	if err := dec.Skip(); err != nil {
		return 0, false
	}

	//nolint:gosec // dec.InputOffset() is bounded by len(input[offset:]), which fits in an int already.
	return offset + int(dec.InputOffset()), true
}
