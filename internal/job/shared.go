// shared.go holds the prompt-building helpers more than one job uses:
// rendering an answered round into prompt inputs, reading embedded assets,
// rendering response schemas, and the small log-field renderers.
package job

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/fs"
	"strings"

	"zing"
	"zing/internal/prompt"
	"zing/internal/response"
	"zing/internal/store"
)

// planXMLFor renders plan back to its XML form for the plan-review prompt
// (design section 6.5): internal/response has no dedicated renderer for
// response.Plan (it only ever decodes one, in Parse), so this uses
// encoding/xml's own marshaller against Plan's wire tags directly, with a
// "plan" start element standing in for the XMLName a decoded document
// carries.
func planXMLFor(plan response.Plan) (string, error) {
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)
	if err := enc.EncodeElement(plan, xml.StartElement{Name: xml.Name{Local: "plan"}}); err != nil {
		return "", fmt.Errorf("encode plan xml: %w", err)
	}
	return buf.String(), nil
}

// int64OrZero renders a nullable id for a log line as 0 when absent, never a
// bare pointer (design section 9's "structured, never a raw output" rule):
// escalation resolution's own session_id and run_id fields are both
// sometimes nil (design section 6.7's RunID/SessionID table).
func int64OrZero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// ---- rendering a round's answers into prompt inputs -----------------------

// questionIDs returns round's question ids, in the order AnsweredRounds
// grouped them (ascending, by id).
func questionIDs(round store.Round) []int64 {
	ids := make([]int64, len(round.Questions))
	for i := range round.Questions {
		ids[i] = round.Questions[i].ID
	}
	return ids
}

// groupByParent buckets msgs by ParentID, the shape both an answered
// round's Answers and its Replies arrive in (design section 4.5).
func groupByParent(msgs []store.MessageRow) map[int64][]store.MessageRow {
	out := make(map[int64][]store.MessageRow, len(msgs))
	for i := range msgs {
		if msgs[i].ParentID != nil {
			out[*msgs[i].ParentID] = append(out[*msgs[i].ParentID], msgs[i])
		}
	}
	return out
}

// optionTextFor returns the option text for key among options, or "" if
// key names none of them (a reply-only answer, or a stale option key).
func optionTextFor(options []response.Option, key string) string {
	for _, o := range options {
		if o.Key == key {
			return o.Text
		}
	}
	return ""
}

// renderAnswerText renders one question's key, stored body (title then
// body), every sent answer's chosen option and its text, and every sent
// reply, the shape section 6.3's per-question resume input describes.
// answers arrives in id order (store.messagesByParent); D30: the console
// now lets an already-answered question take a revised draft while the
// ticket still waits, so a question can carry more than one sent answer
// here. The first stays "chose ..."; every later one -- the current pick,
// since SendBatch only ever appends -- is marked "(revised)" so the model
// reads it as superseding the one(s) before it, not as a second, unrelated
// choice.
func renderAnswerText(q store.MessageRow, qp response.QuestionPayload, answers, replies []store.MessageRow) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: %s\n", qp.Key, q.Body)
	for i := range answers {
		var ap response.AnswerPayload
		if err := json.Unmarshal(answers[i].Payload, &ap); err == nil && ap.Option != nil {
			if i == 0 {
				fmt.Fprintf(&sb, "chose %s: %s\n", *ap.Option, optionTextFor(qp.Options, *ap.Option))
			} else {
				fmt.Fprintf(&sb, "chose %s: %s (revised)\n", *ap.Option, optionTextFor(qp.Options, *ap.Option))
			}
		}
	}
	for i := range replies {
		fmt.Fprintf(&sb, "reply: %s\n", replies[i].Body)
	}
	return sb.String()
}

// answerInputsForRound builds one prompt.Answer per round.Questions (design
// section 6.3, 6.4): a true resume's own input shape, one labeled block per
// question rather than the single combined block renderRoundAnswers builds
// for a fresh restart.
func answerInputsForRound(round store.Round) ([]prompt.NamedInput, error) {
	answersByQ, repliesByQ := groupByParent(round.Answers), groupByParent(round.Replies)
	out := make([]prompt.NamedInput, 0, len(round.Questions))
	for i := range round.Questions {
		q := &round.Questions[i]
		var qp response.QuestionPayload
		if err := json.Unmarshal(q.Payload, &qp); err != nil {
			return nil, fmt.Errorf("job: unmarshal round question %d payload: %w", q.ID, err)
		}
		out = append(out, prompt.Answer(renderAnswerText(*q, qp, answersByQ[q.ID], repliesByQ[q.ID])))
	}
	return out, nil
}

// renderRoundAnswers renders round's questions and answers into one
// combined block (design section 5.1 steps 1(c) and 1(d): "prompt.Answers
// (rendered round)"), used when a round's answers restart classify or
// planning fresh rather than resuming a session.
func renderRoundAnswers(round store.Round) (string, error) {
	answersByQ, repliesByQ := groupByParent(round.Answers), groupByParent(round.Replies)
	parts := make([]string, 0, len(round.Questions))
	for i := range round.Questions {
		q := &round.Questions[i]
		var qp response.QuestionPayload
		if err := json.Unmarshal(q.Payload, &qp); err != nil {
			return "", fmt.Errorf("job: unmarshal round question %d payload: %w", q.ID, err)
		}
		parts = append(parts, strings.TrimRight(renderAnswerText(*q, qp, answersByQ[q.ID], repliesByQ[q.ID]), "\n"))
	}
	return strings.Join(parts, "\n\n"), nil
}

// readAsset reads path out of the embedded zing.Assets tree (machine.toml's
// own prompt and style paths), the same embed.FS the machine loader
// validates those paths against.
func readAsset(path string) (string, error) {
	data, err := fs.ReadFile(zing.Assets, path)
	if err != nil {
		return "", fmt.Errorf("job: read asset %s: %w", path, err)
	}
	return string(data), nil
}

// renderSchemas renders one response.RenderTemplate per outcome, in order,
// then the two universal outcomes question and error (design section 4.2's
// schema order rule).
func renderSchemas(job response.Job, outcomes ...response.Outcome) ([]string, error) {
	all := append(append([]response.Outcome{}, outcomes...), response.OutcomeQuestion, response.OutcomeError)
	out := make([]string, 0, len(all))
	for _, o := range all {
		s, err := response.RenderTemplate(job, o)
		if err != nil {
			return nil, fmt.Errorf("job: render template %s/%s: %w", job, o, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// sessionStateName renders a store.SessionState for the entry-decision log
// line (design section 5.1): never logged as a bare int.
func sessionStateName(s store.SessionState) string {
	switch s {
	case store.SessionNone:
		return noneLiteral
	case store.SessionIdless:
		return "idless"
	case store.SessionOpen:
		return "open"
	case store.SessionExhausted:
		return "exhausted"
	default:
		return "unknown"
	}
}
