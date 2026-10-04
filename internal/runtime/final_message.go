package runtime

import (
	"encoding/xml"
	"strings"
	"unicode/utf8"

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
		// Parse names the XML error when a <zing> header was found but its
		// body would not decode, so a retry learns what to fix.
		// The error can quote model text, so it goes in the fenced Detail.
		var detail string
		if _, perr := response.Parse([]byte(text)); perr != nil {
			detail = capDetail(perr.Error())
		}
		return nil, "", &InvalidOutputError{Reason: reasonNoZingElement, Detail: detail}
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
		// Detail carries the errors themselves, for the retry prompt (bug
		// fix: a live planning turn wrote the vague word "large" twice in a
		// row because the retry said only "zing document failed
		// validation"). Reason stays closed: Detail can quote the model's
		// own text, so the job layer fences it.
		detail := formatValidationErrors(errs)
		return nil, detail, &InvalidOutputError{Reason: reasonFailedValidation, Detail: capDetail(detail)}
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
	return strings.Join(parts, "\n")
}

// maxFinalMessageBytes caps RunResult.FinalMessage, the same 64 KiB cap
// RunResult.Stderr uses.
const maxFinalMessageBytes = maxStderrBytes

// finalMessageCutSuffix ends a FinalMessage that capFinalMessage shortened.
const finalMessageCutSuffix = "\n(final message cut at 64 KiB)"

// capFinalMessage cuts s on a rune boundary so the result, suffix
// included, is at most maxFinalMessageBytes.
func capFinalMessage(s string) string {
	if len(s) <= maxFinalMessageBytes {
		return s
	}
	cut := maxFinalMessageBytes - len(finalMessageCutSuffix)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + finalMessageCutSuffix
}

// maxDetailBytes caps an InvalidOutputError Detail, which is stored in a
// marker and shown in the retry prompt (Q1: the same 64 KiB cap stderr and
// the final message use).
const maxDetailBytes = 64 << 10

// detailCutSuffix marks a Detail that capDetail shortened.
const detailCutSuffix = " (more errors cut)"

// capDetail returns detail unchanged when it is at most maxDetailBytes
// (64 KiB). Otherwise it cuts at maxDetailBytes - len(detailCutSuffix),
// steps back to the nearest UTF-8 rune start so no rune is split, and
// appends detailCutSuffix (" (more errors cut)"). The result, suffix
// included, is then at most 65536 bytes and valid UTF-8 whenever detail
// was.
func capDetail(detail string) string {
	if len(detail) <= maxDetailBytes {
		return detail
	}
	cut := maxDetailBytes - len(detailCutSuffix)
	for cut > 0 && !utf8.RuneStart(detail[cut]) {
		cut--
	}
	return detail[:cut] + detailCutSuffix
}
