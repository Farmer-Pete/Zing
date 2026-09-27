package runtime

import (
	"encoding/xml"
	"strings"

	"zing/internal/response"
)

// The five closed InvalidOutputError.Reason strings the final-message rule
// (design section 4.1) can produce. Nothing else is ever interpolated into
// Reason: the detailed validation error rides back separately, in the
// string parseFinalMessage returns beside its Response, which a caller
// stores only in RunResult.Log, never in Reason, a marker, a prompt, or a
// log line (D14, D15).
const (
	reasonNoZingElement      = "no zing element in final message"
	reasonMultipleZingDocs   = "more than one zing document in final message"
	reasonWrongJob           = "zing document names a wrong job"
	reasonUnsupportedOutcome = "zing document names an unsupported outcome"
	reasonFailedValidation   = "zing document failed validation"
)

// finalMessageHeader reads only a zing root's job and outcome attributes.
// parseFinalMessage checks these before it ever asks the registry to
// resolve a concrete type, so a hostile job attribute is rejected on its
// own terms rather than folded into "unsupported outcome".
type finalMessageHeader struct {
	XMLName xml.Name         `xml:"zing"`
	Job     response.Job     `xml:"job,attr"`
	Outcome response.Outcome `xml:"outcome,attr"`
}

// parseFinalMessage applies the final-message rule (design section 4.1) to
// text, a run's final assistant message, for the job that ran. It is
// shared by every Runtime's Run method (claude.go today; codex.go in a
// later package), so the rule can never drift between them.
//
// response.ExtractAll finds every well-formed <zing> root, tolerating log
// text around them. Exactly one root, naming job and a (job, outcome) pair
// the registry recognises for job, must then pass response.Validate.
// Every failure returns one of the five closed Reason constants above and
// nothing else; the second return carries the detailed validation error
// text for RunResult.Log alone.
//
//nolint:ireturn // parseFinalMessage's whole job is to hand back the dynamic response type response.Lookup resolves for the document's (job, outcome) pair.
func parseFinalMessage(text string, job response.Job) (response.Response, string, error) {
	roots := response.ExtractAll(text)
	switch {
	case len(roots) == 0:
		return nil, "", &InvalidOutputError{Reason: reasonNoZingElement}
	case len(roots) > 1:
		return nil, "", &InvalidOutputError{Reason: reasonMultipleZingDocs}
	}

	root := roots[0]

	var hdr finalMessageHeader
	if err := xml.Unmarshal([]byte(root), &hdr); err != nil {
		// ExtractAll already proved root well formed, so this would be a bug
		// in that proof, not a real document; report it the same as an
		// outcome the registry does not recognise rather than panic.
		return nil, "", &InvalidOutputError{Reason: reasonUnsupportedOutcome}
	}
	if hdr.Job != job {
		return nil, "", &InvalidOutputError{Reason: reasonWrongJob}
	}

	resp, err := response.Lookup(hdr.Job, hdr.Outcome)
	if err != nil {
		return nil, "", &InvalidOutputError{Reason: reasonUnsupportedOutcome}
	}
	if err := xml.Unmarshal([]byte(root), resp); err != nil {
		return nil, "", &InvalidOutputError{Reason: reasonUnsupportedOutcome}
	}

	doc := &response.Document{Response: resp, Elem: []byte(root)}
	if errs := response.Validate(doc, response.ValidateContext{Job: job}); len(errs) > 0 {
		return nil, formatValidationErrors(errs), &InvalidOutputError{Reason: reasonFailedValidation}
	}
	return resp, "", nil
}

// formatValidationErrors renders every response.Validate failure for
// RunResult.Log, never for Reason.
func formatValidationErrors(errs []*response.PathError) string {
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}
