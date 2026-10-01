// views_internal_test.go is a whitebox test for displayBody's
// msgTypeUpdate case (views.go), F012: "the owner sees raw internal marker
// text in the ticket thread". It lives in package console, not
// console_test, the same rail_internal_test.go precedent, because
// displayBody and the marker bodies it recognizes are cleanest proved
// directly against a synthetic store.MessageRow rather than through a real
// ticket and the job package's own commit-building machinery.
package console

import (
	"encoding/json"
	"strings"
	"testing"

	"zing/internal/console/templates"
	"zing/internal/response"
	"zing/internal/store"
)

// updateRow builds a sent (never draft) type="update" message row with the
// given body, the only two fields displayBody's update case reads.
func updateRow(body string) *store.MessageRow {
	return &store.MessageRow{Message: store.Message{Type: msgTypeUpdate, Body: body}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
}

// TestDisplayBody_PlanreviewPendingMarkerIsHumanReadable proves a
// "planreview vN pending" marker (job.planreviewPendingMarker) no longer
// renders as-is, and instead reads as the owner-facing sentence explaining
// that planning is about to resume on its own.
func TestDisplayBody_PlanreviewPendingMarkerIsHumanReadable(t *testing.T) {
	t.Parallel()
	got := displayBody(updateRow("planreview v3 pending"))
	if got == "planreview v3 pending" {
		t.Fatalf("displayBody returned the raw marker unchanged: %q", got)
	}
	if !strings.Contains(got, "Planning resumes") {
		t.Errorf("displayBody(%q) = %q, want it to contain %q", "planreview v3 pending", got, "Planning resumes")
	}
}

// TestDisplayBody_PlanreviewDeliveredMarkerIsHumanReadable proves a
// "planreview vN delivered" marker renders as a plain sentence too.
func TestDisplayBody_PlanreviewDeliveredMarkerIsHumanReadable(t *testing.T) {
	t.Parallel()
	const want = "Planning resumed with the review findings."
	if got := displayBody(updateRow("planreview v3 delivered")); got != want {
		t.Errorf("displayBody(%q) = %q, want %q", "planreview v3 delivered", got, want)
	}
}

// TestDisplayBody_ValidationErrorsPendingRendersFieldLines proves a
// "validation errors pending run <id>" marker's own response.PathError
// lines (formatReadyErrors' "path: msg" shape) render as "Field <path>:
// <message>" lines under the explanatory sentence, rather than the raw
// path syntax the owner has no reason to parse.
func TestDisplayBody_ValidationErrorsPendingRendersFieldLines(t *testing.T) {
	t.Parallel()
	body := "validation errors pending run 7\n" +
		"scenarios/scenario[0]/then: then must not be empty\n" +
		"plan/overview/problem: problem is required"
	got := displayBody(updateRow(body))

	for _, want := range []string{
		"The plan did not pass its final checks.",
		"Field scenarios/scenario[0]/then: then must not be empty",
		"Field plan/overview/problem: problem is required",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("displayBody(%q) = %q, want it to contain %q", body, got, want)
		}
	}
}

// TestDisplayBody_ValidationErrorsDeliveredIsHumanReadable proves a
// "validation errors delivered run <id>" marker renders as a plain
// sentence.
func TestDisplayBody_ValidationErrorsDeliveredIsHumanReadable(t *testing.T) {
	t.Parallel()
	const want = "The agent received the check results."
	if got := displayBody(updateRow("validation errors delivered run 7")); got != want {
		t.Errorf("displayBody(%q) = %q, want %q", "validation errors delivered run 7", got, want)
	}
}

// TestDisplayBody_ResponseInvalidIsHumanReadable proves a "response invalid
// run <id>" marker (invalidOutputCommit) renders as a plain sentence, its
// invErr.Reason line dropped rather than shown raw.
func TestDisplayBody_ResponseInvalidIsHumanReadable(t *testing.T) {
	t.Parallel()
	const want = "The agent's last response could not be used. Zing retries once."
	body := "response invalid run 9\nmissing required field \"plan\""
	if got := displayBody(updateRow(body)); got != want {
		t.Errorf("displayBody(%q) = %q, want %q", body, got, want)
	}
}

// TestDisplayBody_SealMismatchIsHumanReadable proves a "seal mismatch
// cohort <runID>" marker (store.CountSealMismatches) renders as a plain
// sentence.
func TestDisplayBody_SealMismatchIsHumanReadable(t *testing.T) {
	t.Parallel()
	const want = "The scenario set changed before approval. Zing re-reads it on the next tick."
	if got := displayBody(updateRow("seal mismatch cohort 4")); got != want {
		t.Errorf("displayBody(%q) = %q, want %q", "seal mismatch cohort 4", got, want)
	}
}

// TestDisplayBody_UnknownUpdateBodyIsUnchanged proves an "update" body that
// matches none of the known markers falls through to the raw Body
// (updateLine's own default case), the same defensive fallback
// stateLine, escalationLine, and answerLine already use for a payload
// they cannot decode.
func TestDisplayBody_UnknownUpdateBodyIsUnchanged(t *testing.T) {
	t.Parallel()
	const body = "some future bookkeeping marker nobody recognizes yet"
	if got := displayBody(updateRow(body)); got != body {
		t.Errorf("displayBody(%q) = %q, want it unchanged", body, got)
	}
}

// TestUpdateLineBuildMarkers proves the six build markers design section
// 9.2 names each render as their own owner-facing sentence: "claims ok run
// <rid>", "claim errors pending run <rid>" (with its error lines kept
// below the header sentence), "claim errors delivered run <rid>",
// "perimeter resolved run <rid>", "retry requested" (no run id), and
// "perimeter question dropped run <rid>".
func TestUpdateLineBuildMarkers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body, want string
	}{
		{"claims ok", "claims ok run 12", "Claims checked for run 12."},
		{
			"claim errors pending",
			"claim errors pending run 12\nclaims/test_exit: observed 1, want 0",
			"Claim check failed for run 12:\nclaims/test_exit: observed 1, want 0",
		},
		{"claim errors delivered", "claim errors delivered run 12", "Claim errors sent back to run 12."},
		{"perimeter resolved", "perimeter resolved run 9", "Perimeter decided for run 9."},
		{"retry requested", "retry requested", "Retry requested."},
		{"perimeter question dropped", "perimeter question dropped run 4", "Perimeter question dropped for run 4."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := displayBody(updateRow(tc.body)); got != tc.want {
				t.Errorf("displayBody(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestUpdateLineReviewMarkers proves the six review markers reviewing.go
// writes (design section 5.1, 6.2, 6.2a, 6.5, 6.6) each render as their own
// owner-facing sentence: the four "review round <n> ..." round markers
// (done, with its own kept/dropped/merged line kept below the header;
// asked; failed; void), "review discussed <id>", and "review note <id>"
// (with an owner's note, and with none).
func TestUpdateLineReviewMarkers(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, body, want string }{
		{
			"round done",
			"review round 1 done sha abc123 lenses correctness,security\nkept 2 dropped 1 merged 1",
			"Review round 1 finished.\nkept 2 dropped 1 merged 1",
		},
		{"round asked", "review round 2 asked\nruns 5,6\ndone correctness", "Review round 2 is waiting on a lens question."},
		{"round failed", "review round 1 failed\nlens security: timed out", "Review round 1 failed. Zing retries the round."},
		{"round void", "review round 1 void\nhead moved from a to b", "Review round 1 restarted: the branch moved during the round."},
		{"discussed", "review discussed r1f2\nrun 12 batch r1f2 kept 1", "Finding r1f2 discussed with the lens."},
		{
			"note with text",
			"review note r1f2\nb.go:3 is generated, see the header",
			"Owner's note on r1f2:\nb.go:3 is generated, see the header",
		},
		{
			"note with none",
			"review note r1f2\n(the owner gave no note)",
			"Owner's note on r1f2:\n(the owner gave no note)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := displayBody(updateRow(tc.body)); got != tc.want {
				t.Errorf("displayBody(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestUpdateLineJudgeShippingRespondMarkers proves every judge, shipping,
// and respond marker of design section 5.1's table (sections 7, 8, 9) each
// render as their own owner-facing sentence, the same way
// TestUpdateLineReviewMarkers proves reviewing.go's markers do. judging.go
// already writes the eight judge marker shapes this test covers (verified
// against its judgeRoundStartedLine, judgeRoundRetryLine,
// judgeRoundFailedLine, judgeRoundVerdictsLine, judgeCheckLine,
// judgeCoverageFailedFmt, and judgeCoverageDeliveredFmt); shipping.go and
// respond.go do not yet write the shipping and respond marker shapes this
// test covers, so those cases are built from the plan's literal marker
// text (section 5.1's table, 8.2-8.9, 9.2-9.4) rather than read back from a
// producer.
func TestUpdateLineJudgeShippingRespondMarkers(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct{ name, body, want string }{
		// judge round markers (design section 5.1, 7.2, 7.6).
		{"judge round started", "judge round 1 started sha " + sha + " after run 5", "Judge round 1 started on 0123456."},
		{"judge round verdicts", "judge round 1 verdicts run 7", "Judge round 1 returned its verdicts."},
		{"judge round passed", "judge round 1 passed", "Judge round 1 passed."},
		{"judge round failed", "judge round 1 failed\ns2,s3", "Judge round 1 failed: s2,s3."},
		{"judge round retry", "judge round 2 retry after run 9", "Judge round 2 restarted."},
		// judge coverage and check markers (design section 5.1, 7.2 step 5, 7.5).
		{
			"judge coverage failed",
			"judge coverage failed run 12\nmissing verdict for scenario s2",
			"Judge verdicts incomplete for run 12:\nmissing verdict for scenario s2",
		},
		{"judge coverage delivered", "judge coverage delivered run 12", "Coverage errors sent back to run 12."},
		{"judge check pass", "judge check 1 s2 exit 0", "Check for s2 exited 0."},
		{"judge check timeout", "judge check 1 s3 exit -1", "Check for s3 exited -1."},
		// shipping markers (design section 5.1, 8.2-8.9).
		{"pr opened", "pr opened 42", "Draft pull request #42 opened."},
		{"ci waiting", "ci waiting ci,lint", "CI is waiting for ci,lint."},
		{
			"reviewers re-requested",
			"reviewers re-requested " + sha + "\nalice,bob",
			"Review re-requested from alice,bob.",
		},
		{"reviewers re-requested none", "reviewers re-requested " + sha + "\n", "Review re-requested from ."},
		{"pr ready", "pr ready " + sha, "Pull request marked ready at 0123456."},
		{"pr draft", "pr draft " + sha, "Pull request moved back to draft at 0123456."},
		{
			"threads blocking",
			"threads blocking t3f9a0c1b2d4e5f60,t1a2b3c4d5e6f7081",
			"Review threads Zing cannot read are blocking the merge: t3f9a0c1b2d4e5f60,t1a2b3c4d5e6f7081.",
		},
		{"merge asked", "merge asked " + sha, "Asked whether to merge 0123456."},
		{"merge held", "merge held " + sha, "Merge held at 0123456."},
		{"merge withdrawn", "merge withdrawn " + sha, "The merge question was withdrawn; the loop reopened."},
		{"merge refused", "merge refused " + sha + "\nthe head moved", "Merge refused: the head moved"},
		{"pr merged", "pr merged " + sha, "Pull request merged at 0123456."},
		// respond markers (design section 5.1, 9.2-9.4, 5.6).
		{
			"respond batch started",
			"respond batch 1 started sha " + sha + " after run 5\n" +
				"t3f9a0c1b2d4e5f60,t1a2b3c4d5e6f7081\n" +
				"seen t3f9a0c1b2d4e5f60=d1,t1a2b3c4d5e6f7081=d2",
			"Answering 2 review threads.",
		},
		{
			"respond coverage failed",
			"respond coverage failed run 9\nthread t3f9a0c1b2d4e5f60 is not in this batch",
			"Thread actions incomplete for run 9:\nthread t3f9a0c1b2d4e5f60 is not in this batch",
		},
		{"respond coverage delivered", "respond coverage delivered run 9", "Thread errors sent back to run 9."},
		{
			"respond batch stale",
			"respond batch 2 stale\nthe pull request head moved",
			"Review threads changed; Zing will read them again.",
		},
		{"respond batch skipped", "respond batch 3 skipped", "Review threads were resolved before Zing answered."},
		{
			"respond batch retry",
			"respond batch 4 retry sha " + sha + " after run 11\nt3f9a0c1b2d4e5f60\nseen t3f9a0c1b2d4e5f60=d1",
			"Answering the review threads again.",
		},
		{
			"respond applied",
			"respond applied 7\nreplied 3 fixing 1 skipped 0",
			"Replied to 3 threads; 1 go to a fix run.",
		},
		{
			"respond applied with fix request",
			"respond applied 8\nreplied 2 fixing 1 skipped 0\nfix request after run 14",
			"Replied to 2 threads; 1 go to a fix run.",
		},
		{"fix replies posted", "fix replies posted 8", "Replied to the fixed threads."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := displayBody(updateRow(tc.body)); got != tc.want {
				t.Errorf("displayBody(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestSentAnswerText proves sentAnswerText's own formatting (bug fix,
// questionGroup's locked note): an option answer shows the option's text,
// falling back to its bare key when it no longer matches any of the
// question's own options; an item answer shows "ref: decision" pairs in
// ref order; neither present renders empty.
func TestSentAnswerText(t *testing.T) {
	t.Parallel()
	options := []templates.ThreadOption{{Key: "a", Text: "Keep it simple"}, {Key: "b", Text: "Add a flag"}}

	t.Run("an option answer shows the option's text", func(t *testing.T) {
		t.Parallel()
		opt := "a"
		got := sentAnswerText(response.AnswerPayload{Option: &opt}, options)
		if got != "Keep it simple" {
			t.Errorf("sentAnswerText = %q, want %q", got, "Keep it simple")
		}
	})

	t.Run("an option that no longer matches falls back to its bare key", func(t *testing.T) {
		t.Parallel()
		opt := "z"
		got := sentAnswerText(response.AnswerPayload{Option: &opt}, options)
		if got != "z" {
			t.Errorf("sentAnswerText = %q, want %q", got, "z")
		}
	})

	t.Run("an item answer shows ref: decision pairs in ref order", func(t *testing.T) {
		t.Parallel()
		got := sentAnswerText(response.AnswerPayload{Items: map[string]response.Decision{
			"greet.go": response.DecisionAccept, "machine.toml": response.DecisionReject,
		}}, nil)
		want := "greet.go: accept, machine.toml: reject"
		if got != want {
			t.Errorf("sentAnswerText = %q, want %q", got, want)
		}
	})

	t.Run("neither option nor items renders empty", func(t *testing.T) {
		t.Parallel()
		if got := sentAnswerText(response.AnswerPayload{}, options); got != "" {
			t.Errorf("sentAnswerText = %q, want empty", got)
		}
	})
}

// TestCollectSentAnswers proves collectSentAnswers reads only "answer" rows
// (bug fix): a reply row, and an answer row with no ParentID, are both
// skipped, and a decodable answer row is keyed by its own ParentID.
func TestCollectSentAnswers(t *testing.T) {
	t.Parallel()
	questionID := int64(7)
	rows := []store.MessageRow{
		//nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{Message: store.Message{Type: msgTypeAnswer, ParentID: &questionID, Payload: []byte(`{"option":"a"}`)}},
		//nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{Message: store.Message{Type: msgTypeReply, ParentID: &questionID, Body: "a reply"}},
		//nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{Message: store.Message{Type: msgTypeAnswer, Payload: []byte(`{"option":"b"}`)}},
	}
	got := collectSentAnswers(rows)
	if len(got) != 1 {
		t.Fatalf("collectSentAnswers returned %d entries, want 1", len(got))
	}
	ap, ok := got[questionID]
	if !ok {
		t.Fatalf("collectSentAnswers missing question %d", questionID)
	}
	if ap.Option == nil || *ap.Option != "a" {
		t.Errorf("collectSentAnswers[%d].Option = %v, want \"a\"", questionID, ap.Option)
	}
}

// TestBuildThreadRowsNestsSentRepliesAndAnswersUnderTheirQuestion proves the
// bug fix for F10: a sent reply or answer naming a question as its parent
// used to also get its own standalone ThreadRow, rendering as a
// thread-level "reply you"/"answer you" card at the bottom of the thread,
// detached from the question it actually answered. buildThreadRows now
// folds both into that question's own SentReplies instead of emitting a
// second, separate row for them; a thread-level reply (ParentID nil) is
// unaffected and keeps its own top-level row.
func TestBuildThreadRowsNestsSentRepliesAndAnswersUnderTheirQuestion(t *testing.T) {
	t.Parallel()
	questionID := int64(1)
	questionPayload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindQuestion,
		Options: []response.Option{{Key: "a", Text: "Pick the terse option"}},
	})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	answerPayload := []byte(`{"option":"a"}`)

	rows := []store.MessageRow{
		//nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: questionID, Message: store.Message{Type: msgTypeQuestion, Payload: questionPayload}},
		//nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: 2, Message: store.Message{
			Type: msgTypeReply, ParentID: &questionID, Body: "Explain these three options in more detail",
		}},
		//nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: 3, Message: store.Message{Type: msgTypeAnswer, ParentID: &questionID, Payload: answerPayload}},
		//nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
		{ID: 4, Message: store.Message{Type: msgTypeReply, Body: "a thread-level note"}},
	}

	got, err := buildThreadRows(&store.Ticket{}, rows, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildThreadRows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("buildThreadRows returned %d rows, want 2 (the question and the thread-level reply); got %+v", len(got), got)
	}

	question := got[0]
	if question.Question == nil {
		t.Fatalf("got[0] is not the question row: %+v", question)
	}
	want := []string{"Explain these three options in more detail", "Pick the terse option"}
	if strings.Join(question.Question.SentReplies, "|") != strings.Join(want, "|") {
		t.Errorf("question.SentReplies = %v, want %v", question.Question.SentReplies, want)
	}

	threadLevel := got[1]
	if threadLevel.Question != nil || threadLevel.Body != "a thread-level note" {
		t.Errorf("got[1] = %+v, want the unaffected thread-level reply", threadLevel)
	}
}

// questionRowForWait builds a "question" message row for buildWaitProgress
// tests: state and kind are the two fields it reads, plus a minimal valid
// QuestionPayload so json.Unmarshal succeeds.
func questionRowForWait(t *testing.T, state string, kind response.QuestionKind) store.MessageRow {
	t.Helper()
	payload, err := json.Marshal(response.QuestionPayload{Key: "Q", Kind: kind, Options: []response.Option{}})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	return store.MessageRow{Message: store.Message{Type: msgTypeQuestion, State: &state, Payload: payload}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
}

// TestBuildWaitProgress proves the bug fix for a silent partial-batch wait:
// buildWaitProgress counts only the current round's questions -- those
// whose own Kind matches ticket.WaitingOn's round and whose state is still
// open or already answered -- and reports nothing (Total == 0) for a
// ticket that is not currently question-blocked, or whose waiting_on names
// a reason no question Kind backs.
func TestBuildWaitProgress(t *testing.T) {
	t.Parallel()

	t.Run("counts answered and total within the current round, ignoring other kinds and closed rounds", func(t *testing.T) {
		t.Parallel()
		waiting := waitReasonQuestions
		ticket := &store.Ticket{WaitingOn: &waiting}
		rows := []store.MessageRow{
			questionRowForWait(t, msgStateAnswered, response.QuestionKindQuestion),
			questionRowForWait(t, msgStateOpen, response.QuestionKindQuestion),
			questionRowForWait(t, "resolved", response.QuestionKindQuestion), // an earlier, already-cleared round
			questionRowForWait(t, msgStateOpen, response.QuestionKindGate),   // a different kind's round
		}
		got := buildWaitProgress(ticket, rows)
		if got.Answered != 1 || got.Total != 2 {
			t.Errorf("buildWaitProgress = %+v, want {Answered:1 Total:2}", got)
		}
	})

	t.Run("a ticket not waiting on anything reports no progress", func(t *testing.T) {
		t.Parallel()
		got := buildWaitProgress(&store.Ticket{}, []store.MessageRow{questionRowForWait(t, msgStateOpen, response.QuestionKindQuestion)})
		if got.Total != 0 {
			t.Errorf("buildWaitProgress = %+v, want Total=0 (not waiting)", got)
		}
	})

	t.Run("a non-question-backed wait reason reports no progress", func(t *testing.T) {
		t.Parallel()
		waiting := "error"
		ticket := &store.Ticket{WaitingOn: &waiting}
		got := buildWaitProgress(ticket, []store.MessageRow{questionRowForWait(t, msgStateOpen, response.QuestionKindQuestion)})
		if got.Total != 0 {
			t.Errorf("buildWaitProgress = %+v, want Total=0 (\"error\" is not question-backed)", got)
		}
	})
}

// TestQuestionStateLabel proves the bug fix for F9: an answered question
// was badged "resuming" regardless of whether anything was actually about
// to resume. "resuming" implied the agent was already on its way back,
// which was false whenever the ticket still waited on other questions in
// the same round (D30's revisable state) -- nothing resumes until every
// question in the round is answered. questionStateLabel now takes the same
// revisable flag buildThreadQuestion already computes, and answered splits
// into "answered · can change" (still revisable) and "answered" (locked,
// the round is done with this question).
func TestQuestionStateLabel(t *testing.T) {
	t.Parallel()
	answered := msgStateAnswered
	open := msgStateOpen
	resolved := "resolved"
	other := "weird"
	for _, tc := range []struct {
		name      string
		state     *string
		revisable bool
		want      string
	}{
		{"nil state renders empty", nil, false, ""},
		{"open renders waiting on you", &open, false, "waiting on you"},
		{"answered and revisable renders answered, can change", &answered, true, "answered · can change"},
		{"answered and locked renders plain answered", &answered, false, "answered"},
		{"resolved renders resolved", &resolved, false, "resolved"},
		{"an unrecognized state renders as-is", &other, false, "weird"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := questionStateLabel(tc.state, tc.revisable)
			if got != tc.want {
				t.Errorf("questionStateLabel(%v, %v) = %q, want %q", tc.state, tc.revisable, got, tc.want)
			}
		})
	}
}

// TestBuildThreadRowsBadgesRevisableAnsweredDifferentlyFromLocked is
// TestQuestionStateLabel's render-level proof: buildThreadRows, the real
// seam the Thread view renders through, gives a still-revisable answered
// question (ticket.WaitingOn == "questions") a different badge than the
// same question once the ticket is no longer waiting on it.
func TestBuildThreadRowsBadgesRevisableAnsweredDifferentlyFromLocked(t *testing.T) {
	t.Parallel()
	rows := []store.MessageRow{questionRowForWait(t, msgStateAnswered, response.QuestionKindQuestion)}

	waiting := waitReasonQuestions
	revisableRows, err := buildThreadRows(&store.Ticket{WaitingOn: &waiting}, rows, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildThreadRows (revisable): %v", err)
	}
	if got := revisableRows[0].Question.StateLabel; got != "answered · can change" {
		t.Errorf("revisable answered question StateLabel = %q, want %q", got, "answered · can change")
	}

	lockedRows, err := buildThreadRows(&store.Ticket{}, rows, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildThreadRows (locked): %v", err)
	}
	if got := lockedRows[0].Question.StateLabel; got != "answered" {
		t.Errorf("locked answered question StateLabel = %q, want %q", got, "answered")
	}
}
