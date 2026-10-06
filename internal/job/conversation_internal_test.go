package job

// conversation_internal_test.go tests conversation.go's own unexported,
// pure functions (design section 22.2, 22.6): checkConversation's handler-
// level rules and the two "conversation" prompt renderers. These need no
// store, no runtime, and no handler seam, so they live here rather than in
// planning_test.go (package job_test).

import (
	"encoding/json"
	"testing"

	"zing/internal/response"
	"zing/internal/store"
)

// ---- checkConversation -----------------------------------------------

// hasPathError reports whether errs contains one whose Path and Msg both
// equal want.
func hasPathError(errs []*response.PathError, path, msg string) bool {
	for _, e := range errs {
		if e.Path == path && e.Msg == msg {
			return true
		}
	}
	return false
}

func TestCheckConversation_UnknownKeyRejected(t *testing.T) {
	t.Parallel()
	ts := threadState{
		settled: map[string]bool{"Q1": false}, order: map[string]int64{"Q1": 1}, received: map[string]bool{},
	}
	errs := checkConversation(response.OutcomeQuestions, []response.Reply{{Question: "Q9", Text: "hi"}}, ts, false)
	if !hasPathError(errs, "replies/reply[0]/question", "no planning question Q9") {
		t.Errorf("checkConversation = %+v, want it to reject the unknown key Q9", errs)
	}
}

func TestCheckConversation_SettledKeyRejected(t *testing.T) {
	t.Parallel()
	ts := threadState{
		settled: map[string]bool{"Q1": true}, order: map[string]int64{"Q1": 1}, received: map[string]bool{},
	}
	errs := checkConversation(response.OutcomeQuestions, []response.Reply{{Question: "Q1", Text: "hi"}}, ts, false)
	if !hasPathError(errs, "replies/reply[0]/question", "Q1 is already settled and takes no more replies") {
		t.Errorf("checkConversation = %+v, want it to reject a reply to the settled Q1", errs)
	}
}

func TestCheckConversation_ReadyChildrenNothingToDoNeedEverythingSettled(t *testing.T) {
	t.Parallel()
	for _, outcome := range []response.Outcome{response.OutcomeReady, response.OutcomeChildren, response.OutcomeNothingToDo} {
		ts := threadState{
			settled:  map[string]bool{"Q1": false, "Q2": false},
			order:    map[string]int64{"Q1": 1, "Q2": 2},
			received: map[string]bool{},
		}
		errs := checkConversation(outcome, nil, ts, false)
		want := string(outcome) + " needs every question settled; still open: Q1, Q2"
		if !hasPathError(errs, "outcome", want) {
			t.Errorf("checkConversation(%s) = %+v, want %q", outcome, errs, want)
		}
	}
}

// TestCheckConversation_ReadyPassesOnceSettledThisTurn proves the "after
// this response's own settles" rule (design section 22.2): a reply that
// settles the one remaining open question in the same turn clears ready's
// own still-open check.
func TestCheckConversation_ReadyPassesOnceSettledThisTurn(t *testing.T) {
	t.Parallel()
	ts := threadState{
		settled: map[string]bool{"Q1": false}, order: map[string]int64{"Q1": 1}, received: map[string]bool{"Q1": true},
	}
	errs := checkConversation(response.OutcomeReady, []response.Reply{
		{Question: "Q1", Settled: true, Decision: "Use the recommended option.", Text: "Agreed."},
	}, ts, false)
	if len(errs) != 0 {
		t.Errorf("checkConversation = %+v, want none (Q1 settles in this same turn)", errs)
	}
}

func TestCheckConversation_RepliesNeedsOneOpen(t *testing.T) {
	t.Parallel()
	ts := threadState{
		settled: map[string]bool{"Q1": false}, order: map[string]int64{"Q1": 1}, received: map[string]bool{"Q1": true},
	}
	errs := checkConversation(response.OutcomeReplies, []response.Reply{
		{Question: "Q1", Settled: true, Decision: "All done.", Text: "Agreed."},
	}, ts, false)
	const want = "replies needs a question left open; every question is settled, so return ready, children, or nothing_to_do"
	if !hasPathError(errs, "outcome", want) {
		t.Errorf("checkConversation = %+v, want %q", errs, want)
	}
}

func TestCheckConversation_UnansweredOwnerMessageRejected(t *testing.T) {
	t.Parallel()
	ts := threadState{
		settled:  map[string]bool{"Q1": false, "Q2": false},
		order:    map[string]int64{"Q1": 1, "Q2": 2},
		received: map[string]bool{"Q1": true, "Q2": true},
	}
	errs := checkConversation(response.OutcomeQuestions, []response.Reply{{Question: "Q1", Text: "ok"}}, ts, false)
	const want = "no reply to the owner on Q2; answer every owner message you receive"
	if !hasPathError(errs, "replies", want) {
		t.Errorf("checkConversation = %+v, want %q", errs, want)
	}
}

func TestCheckConversation_ValidTurnHasNoErrors(t *testing.T) {
	t.Parallel()
	ts := threadState{
		settled:  map[string]bool{"Q1": false, "Q2": false},
		order:    map[string]int64{"Q1": 1, "Q2": 2},
		received: map[string]bool{"Q1": true},
	}
	errs := checkConversation(response.OutcomeQuestions, []response.Reply{{Question: "Q1", Text: "ok"}}, ts, false)
	if len(errs) != 0 {
		t.Errorf("checkConversation = %+v, want none", errs)
	}
}

// ---- D32: the confirming turn's own checkConversation rules ---------------

// TestCheckConversation_ConfirmedOutsideConfirmingTurnRefused proves
// confirmed's own guard (design section 22.12.3): the outcome answers only
// the gate's confirming turn.
func TestCheckConversation_ConfirmedOutsideConfirmingTurnRefused(t *testing.T) {
	t.Parallel()
	ts := threadState{settled: map[string]bool{"Q1": true}, order: map[string]int64{"Q1": 1}, received: map[string]bool{}}
	errs := checkConversation(response.OutcomeConfirmed, nil, ts, false)
	const want = "confirmed answers only the gate's confirming turn"
	if !hasPathError(errs, "outcome", want) {
		t.Errorf("checkConversation = %+v, want %q", errs, want)
	}
}

// TestCheckConversation_ConfirmedNeedsEverythingSettled proves confirmed
// reuses the same "needs every question settled" rule as ready, children,
// and nothing_to_do (design section 22.12.3).
func TestCheckConversation_ConfirmedNeedsEverythingSettled(t *testing.T) {
	t.Parallel()
	ts := threadState{
		settled: map[string]bool{"Q1": false}, order: map[string]int64{"Q1": 1}, received: map[string]bool{},
	}
	errs := checkConversation(response.OutcomeConfirmed, nil, ts, true)
	const want = "confirmed needs every question settled; still open: Q1"
	if !hasPathError(errs, "outcome", want) {
		t.Errorf("checkConversation = %+v, want %q", errs, want)
	}
}

// TestCheckConversation_ConfirmingTurnRefusesReplies proves the confirming
// turn's own narrowed outcome set (design section 22.12.3): replies,
// children, and nothing_to_do are refused -- "the owner asked to close the
// gate" -- even though an ordinary turn accepts all three.
func TestCheckConversation_ConfirmingTurnRefusesReplies(t *testing.T) {
	t.Parallel()
	for _, outcome := range []response.Outcome{response.OutcomeReplies, response.OutcomeChildren, response.OutcomeNothingToDo} {
		ts := threadState{
			settled: map[string]bool{"Q1": false}, order: map[string]int64{"Q1": 1}, received: map[string]bool{},
		}
		errs := checkConversation(outcome, nil, ts, true)
		const want = "the owner asked to close the gate: return confirmed, questions, or ready"
		if !hasPathError(errs, "outcome", want) {
			t.Errorf("checkConversation(%s, confirming) = %+v, want %q", outcome, errs, want)
		}
	}
}

// TestCheckConversation_ConfirmingTurnAllowsReadyAndQuestions proves the
// confirming turn still accepts ready and questions unchanged.
func TestCheckConversation_ConfirmingTurnAllowsReadyAndQuestions(t *testing.T) {
	t.Parallel()
	ts := threadState{
		settled: map[string]bool{"Q1": true}, order: map[string]int64{"Q1": 1}, received: map[string]bool{},
	}
	if errs := checkConversation(response.OutcomeReady, nil, ts, true); len(errs) != 0 {
		t.Errorf("checkConversation(ready, confirming) = %+v, want none", errs)
	}
	if errs := checkConversation(response.OutcomeQuestions, nil, ts, true); len(errs) != 0 {
		t.Errorf("checkConversation(questions, confirming) = %+v, want none", errs)
	}
}

// ---- the render functions, exact bytes (design section 22.5, 22.6) -------

// questionRow builds a store.MessageRow for a planning question: body is
// title alone when body is "", else "title\n\nbody" (questionMessagesFor's
// own stored shape).
func questionRow(id int64, key, title, body string, options []response.Option, recommended string) store.MessageRow {
	payload, _ := json.Marshal(response.QuestionPayload{ //nolint:errcheck // fixed, valid test input
		Key: key, Kind: response.QuestionKindQuestion, State: response.QuestionStateOpen,
		Recommended: recommended, Options: options,
	})
	text := title
	if body != "" {
		text = title + "\n\n" + body
	}
	return store.MessageRow{ID: id, Type: msgTypeQuestion, Author: authorZing, Body: text, Payload: payload}
}

func ownerAnswerRow(id, parentID, batch int64, option string) store.MessageRow {
	payload, _ := json.Marshal(response.AnswerPayload{Option: &option}) //nolint:errcheck // fixed, valid test input
	return store.MessageRow{ID: id, Type: msgTypeAnswer, Author: authorYou, ParentID: &parentID, BatchID: &batch, Payload: payload}
}

func ownerReplyRow(id, parentID, batch int64, body string) store.MessageRow {
	return store.MessageRow{ID: id, Type: msgTypeReply, Author: authorYou, ParentID: &parentID, BatchID: &batch, Body: body}
}

func agentReplyRow(id, parentID int64, body string) store.MessageRow {
	return store.MessageRow{ID: id, Type: msgTypeReply, Author: authorZing, ParentID: &parentID, Body: body}
}

// followupRow and resolvedRow build the two D32 turn kinds (design section
// 22.12.2): an owner reopen row, and the agent's own settling decision row
// (reused here, not just at a thread's first settling, so a reopened-then-
// resettled thread's turn order can be tested too).
func followupRow(id, parentID, batch int64) store.MessageRow {
	return store.MessageRow{ID: id, Type: msgTypeFollowup, Author: authorYou, ParentID: &parentID, BatchID: &batch, Body: "reopened"}
}

func resolvedRow(id, parentID int64, decision string) store.MessageRow {
	return store.MessageRow{ID: id, Type: msgTypeResolved, Author: authorZing, ParentID: &parentID, Body: decision}
}

// TestRenderResumeConversation proves design section 22.5, 22.6's exact
// resume-block bytes: a thread's key and title, its newest agent reply, an
// undelivered pick and a multi-line undelivered reply (continuation lines
// indented two spaces), and a trailing "Still open" line for a sibling
// thread with nothing undelivered.
func TestRenderResumeConversation(t *testing.T) {
	t.Parallel()
	q1 := questionRow(1, "Q1", "Where does the version come from?", "",
		[]response.Option{{Key: "a", Text: "git describe"}, {Key: "b", Text: "zing plus version"}}, "b")
	q2 := questionRow(2, "Q2", "Should it print JSON?", "", nil, "no")

	conv := store.PlanningConversation{
		Threads: []store.Thread{
			{Question: q1, Turns: []store.MessageRow{
				agentReplyRow(10, 1, "ReadBuildInfo gives the module version with no ldflags."),
			}},
			{Question: q2},
		},
	}
	undelivered := []store.MessageRow{
		ownerAnswerRow(20, 1, 4, "b"),
		ownerReplyRow(21, 1, 4, "Also print the commit hash.\nOne line is enough."),
	}

	got := renderResumeConversation(conv, undelivered)
	want := "Q1: Where does the version come from?\n" +
		"Your last reply: ReadBuildInfo gives the module version with no ldflags.\n" +
		"The owner, oldest first:\n" +
		"- picked option b: zing plus version\n" +
		"- wrote: Also print the commit hash.\n" +
		"  One line is enough.\n\n" +
		"Still open, nothing new from the owner: Q2."
	if got != want {
		t.Errorf("renderResumeConversation =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderFreshConversation proves design section 22.6's exact fresh-
// transcript bytes: a settled thread's two-line form, then an unsettled
// thread's key, title, indented body, options, recommendation, and every
// turn oldest first.
func TestRenderFreshConversation(t *testing.T) {
	t.Parallel()
	q1 := questionRow(1, "Q1", "Settled question", "", nil, "")
	q2 := questionRow(2, "Q3", "New frontier", "Multi-line\nbody here.",
		[]response.Option{{Key: "a", Text: "Yes"}, {Key: "b", Text: "No"}}, "a")

	conv := store.PlanningConversation{
		Threads: []store.Thread{
			{Question: q1, Settled: true, Decision: "Use git describe only."},
			{Question: q2, Turns: []store.MessageRow{
				agentReplyRow(10, 2, "What do you think?"),
				ownerAnswerRow(11, 2, 1, "b"),
				ownerReplyRow(12, 2, 1, "Because X."),
			}},
		},
	}

	got := renderFreshConversation(conv)
	want := "Q1 (settled): Settled question\nDecision: Use git describe only.\n\n" +
		"Q3: New frontier\n" +
		"Body:\n  Multi-line\n  body here.\n" +
		"Options: a: Yes; b: No\n" +
		"Recommended: a\n" +
		"Thread, oldest first:\n" +
		"- you: What do you think?\n" +
		"- the owner picked option b: No\n" +
		"- the owner wrote: Because X."
	if got != want {
		t.Errorf("renderFreshConversation =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderFreshConversation_OmitsOptionsAndThreadWhenEmpty proves the two
// "omitted when" rules (design section 22.6): no Options line for a
// question with none, and no Thread list for one with no turns yet.
func TestRenderFreshConversation_OmitsOptionsAndThreadWhenEmpty(t *testing.T) {
	t.Parallel()
	q := questionRow(1, "Q1", "A fresh question", "Body text.", nil, "wait and see")
	conv := store.PlanningConversation{Threads: []store.Thread{{Question: q}}}

	got := renderFreshConversation(conv)
	want := "Q1: A fresh question\nBody:\n  Body text.\nRecommended: wait and see"
	if got != want {
		t.Errorf("renderFreshConversation =\n%q\nwant\n%q", got, want)
	}
}

// ---- D32: a reopened thread's own rendering (design section 22.12.2) -----

// TestRenderResumeReopenedThread proves the two exact lines a reopen adds
// to a resume block, right after the key/title line: "The owner reopened
// <key>." and "Your decision was: <decision>". The followup row itself
// prints no bullet below -- these two lines already say it -- only the
// owner's own reply does.
func TestRenderResumeReopenedThread(t *testing.T) {
	t.Parallel()
	q1 := questionRow(1, "Q1", "Which output formats?", "", nil, "")

	conv := store.PlanningConversation{
		Delivered: 8,
		Threads: []store.Thread{
			{
				Question: q1,
				Decision: "Plain text only.",
				Turns: []store.MessageRow{
					followupRow(30, 1, 9),
					ownerReplyRow(31, 1, 9, "Print JSON too."),
				},
			},
		},
	}
	undelivered := []store.MessageRow{ownerReplyRow(31, 1, 9, "Print JSON too.")}

	got := renderResumeConversation(conv, undelivered)
	want := "Q1: Which output formats?\n" +
		"The owner reopened Q1.\n" +
		"Your decision was: Plain text only.\n" +
		"The owner, oldest first:\n" +
		"- wrote: Print JSON too."
	if got != want {
		t.Errorf("renderResumeConversation =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderFreshReopenedThread proves the fresh-transcript analogue
// (design section 22.12.2): a reopened thread renders as unsettled (not
// the settled two-line form), its earlier decision at its own place in
// turn order as "- you settled it: <decision>", and its followup row as
// "- the owner reopened the thread".
func TestRenderFreshReopenedThread(t *testing.T) {
	t.Parallel()
	q1 := questionRow(1, "Q1", "Which output formats?", "", nil, "")

	conv := store.PlanningConversation{
		Threads: []store.Thread{
			{
				Question: q1,
				Turns: []store.MessageRow{
					resolvedRow(29, 1, "Plain text only."),
					followupRow(30, 1, 9),
					ownerReplyRow(31, 1, 9, "Print JSON too."),
				},
			},
		},
	}

	got := renderFreshConversation(conv)
	want := "Q1: Which output formats?\n" +
		"Body:\n  \n" +
		"Recommended: \n" +
		"Thread, oldest first:\n" +
		"- you settled it: Plain text only.\n" +
		"- the owner reopened the thread\n" +
		"- the owner wrote: Print JSON too."
	if got != want {
		t.Errorf("renderFreshConversation =\n%q\nwant\n%q", got, want)
	}
}

// ---- questionMessagesFor -----------------------------------------------

// A planner question with no options (a free-text ask, such as "attach the
// transcript") must store "options": []. The question message schema wants an
// array, and a null fails the commit and stops the dispatcher.
func TestQuestionMessagesFor_NoOptionsStoresEmptyArray(t *testing.T) {
	msgs, err := questionMessagesFor(1, []response.Question{{Key: "q1", Title: "Attach the transcript", Recommended: "Attach it"}})
	if err != nil {
		t.Fatalf("questionMessagesFor: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(msgs[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got := string(payload["options"]); got != "[]" {
		t.Fatalf("options = %s, want []", got)
	}
}
