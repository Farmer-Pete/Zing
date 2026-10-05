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
//
// When the strict pass finds no root, ExtractAll tries once more with each
// registered candidate's bare < escaped (extractRepaired), so an agent's
// free text holding a literal < does not cost a whole retry run. The cap
// still applies, and hitting it in the strict pass returns nothing, as
// before.
//
// logAttrs are extra slog key-value pairs appended to the repair record a
// repaired pass logs, such as "run_token", token. Callers that have none
// pass none.
func ExtractAll(text string, logAttrs ...any) []string {
	input := escapeBareAmpersands([]byte(text))
	roots, capped := extractStrict(input)
	if len(roots) > 0 || capped {
		return roots
	}
	return extractRepaired(input, logAttrs)
}

// extractStrict is ExtractAll's first pass over input exactly as written.
// capped is true when it stopped at maxRootCandidates, and then roots is
// nil.
func extractStrict(input []byte) (roots []string, capped bool) {
	excluded := excludedRanges(input)
	scanned := 0
	for _, offset := range candidateOffsets(input) {
		if inRanges(offset, excluded) {
			continue
		}
		// Bound the work. An unclosed "<zing" start makes wellFormedRootExtent
		// scan its decoder to EOF, so an output packed with them would be
		// O(n^2) on the 4 MiB allowed output. Past a small cap of real
		// candidates, stop and report nothing: parseFinalMessage then yields
		// its ordinary no-zing-element InvalidOutputError -- a bounded, named
		// failure rather than a stalled worker. A genuine document sits at the
		// first well-formed candidate, far below this cap.
		if scanned == maxRootCandidates {
			return nil, true
		}
		scanned++
		if end, ok := wellFormedRootExtent(input, offset); ok {
			roots = append(roots, string(input[offset:end]))
		}
	}
	return roots, false
}

// extractRepaired is ExtractAll's second pass, run only when the strict
// pass found no root: a candidate counts when its repaired bytes form a
// well-formed root on their own.
func extractRepaired(input []byte, logAttrs []any) []string {
	excluded := excludedRanges(input)
	var roots []string
	total := 0
	scanned := 0
	for _, offset := range candidateOffsets(input) {
		if inRanges(offset, excluded) {
			continue
		}
		if scanned == maxRootCandidates {
			return nil
		}
		scanned++
		repaired, escaped, ok := repairCandidate(input[offset:])
		if !ok {
			continue
		}
		if end, whole := wellFormedRootExtent(repaired, 0); whole {
			roots = append(roots, string(repaired[:end]))
			total += escaped
		}
	}
	if len(roots) > 0 {
		logRepair(total, len(roots), logAttrs...)
	}
	return roots
}

// maxRootCandidates caps how many "<zing" candidates ExtractAll will attempt
// to parse in one output (design section 4.1). It is generous: a valid final
// message carries one document, and even chatty log text rarely names "<zing"
// more than a handful of times.
const maxRootCandidates = 64

// wellFormedRootExtent reports the end offset of the well-formed root
// starting at offset, when its start element is exactly "zing" in no
// namespace and dec.Skip finds a matching end element.
func wellFormedRootExtent(input []byte, offset int) (int, bool) {
	dec := xml.NewDecoder(bytes.NewReader(input[offset:]))

	tok, err := dec.Token()
	if err != nil {
		return 0, false
	}
	if _, ok := isZingStart(tok); !ok {
		return 0, false
	}
	if err := dec.Skip(); err != nil {
		return 0, false
	}

	//nolint:gosec // dec.InputOffset() is bounded by len(input[offset:]), which fits in an int already.
	return offset + int(dec.InputOffset()), true
}
