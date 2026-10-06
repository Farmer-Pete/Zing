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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	zing "zing"
	"zing/internal/console/templates"
	"zing/internal/machine"
	"zing/internal/response"
	"zing/internal/store"
)

// updateRow builds a sent (never draft) type="update" message row with the
// given body, the only two fields displayBody's update case reads.
func updateRow(body string) *store.MessageRow {
	return &store.MessageRow{Message: store.Message{Type: msgTypeUpdate, Body: body}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
}

// testBodyConversationPending is one "conversation pending" marker body,
// shared by TestConversationMarkersHidden, markerShapeCases, and
// TestMarkerRecognized (goconst: three literal copies of the same string
// is one too many).
const testBodyConversationPending = "conversation pending run 31 batch 4"

// testBodyUnknownMarker is one body that matches none of updateLine's own
// marker prefixes, shared by TestDisplayBody_UnknownUpdateBodyIsUnchanged,
// TestUnknownMarkerIsADivider, and TestMarkerRecognized (goconst).
const testBodyUnknownMarker = "some future bookkeeping marker nobody recognizes yet"

// testEscalationBody is one escalation row's own Body, shared by
// TestNoMessageKindRendersOutsideItsThread,
// TestEscalationWithQuestionChildRendersNoCard, and
// TestEscalationWithNoQuestionChildKeepsItsCard (goconst: three literal
// copies of the same string is one too many).
const testEscalationBody = "escalation summary"

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
		"The agent's last response did not pass Zing's checks.",
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

// TestDisplayBody_ResponseInvalidShowsReasonAndErrors proves a "response
// invalid run <id>" marker (invalidOutputCommit) renders the run id, the
// closed reason, and the validator's errors, one per line, in place of the
// old generic sentence.
func TestDisplayBody_ResponseInvalidShowsReasonAndErrors(t *testing.T) {
	t.Parallel()
	const want = "Run 9's response could not be used: zing document failed validation. If a final message was kept, it is linked under Runs in the side panel.\nplan/goals: required\nplan/review: required"
	body := "response invalid run 9\nzing document failed validation\nplan/goals: required\nplan/review: required"
	if got := displayBody(updateRow(body)); got != want {
		t.Errorf("displayBody(%q) = %q, want %q", body, got, want)
	}
}

// TestDisplayBody_ResponseInvalidWithoutErrors proves a "response invalid
// run <id>" marker with no error-list line renders just the head sentence.
func TestDisplayBody_ResponseInvalidWithoutErrors(t *testing.T) {
	t.Parallel()
	const want = "Run 4's response could not be used: no zing element in final message. If a final message was kept, it is linked under Runs in the side panel."
	body := "response invalid run 4\nno zing element in final message"
	if got := displayBody(updateRow(body)); got != want {
		t.Errorf("displayBody(%q) = %q, want %q", body, got, want)
	}
}

// TestResponseInvalidDetailFence proves responseInvalidDetail's own three
// cases (design H2): a lens renders in the head line, a nil lens drops that
// clause, and a marker with no error lines falls back to "Reason: ...."
// rather than an empty "Validator errors:" section. The fence-length case --
// an errors block that itself contains a run of backticks -- is proved
// through the full render path by
// TestEscalationQuestion_ResponseInvalidShowsValidatorErrors, since
// responseInvalidDetail's own markdown is meaningless until Render turns it
// into HTML.
func TestResponseInvalidDetailFence(t *testing.T) {
	t.Parallel()
	lens := "correctness"

	marker := "response invalid run 5\nthe final message failed validation\nplan/goals: required"
	got := responseInvalidDetail("review", &lens, 5, marker)
	for _, want := range []string{"Job: review, lens correctness. Run 5.", "plan/goals: required"} {
		if !strings.Contains(got, want) {
			t.Errorf("responseInvalidDetail(lens) = %q, want it to contain %q", got, want)
		}
	}

	got = responseInvalidDetail("planning", nil, 5, marker)
	if strings.Contains(got, "lens") {
		t.Errorf("responseInvalidDetail(nil lens) = %q, want no lens clause", got)
	}
	if !strings.Contains(got, "Job: planning. Run 5.") {
		t.Errorf("responseInvalidDetail(nil lens) = %q, want it to contain %q", got, "Job: planning. Run 5.")
	}

	noErrors := "response invalid run 5\nthe final message failed validation"
	got = responseInvalidDetail("planning", nil, 5, noErrors)
	const want = "Job: planning. Run 5.\n\nReason: the final message failed validation."
	if got != want {
		t.Errorf("responseInvalidDetail(no errors) = %q, want %q", got, want)
	}
	if strings.Contains(got, "Validator errors") {
		t.Errorf("responseInvalidDetail(no errors) = %q, want no \"Validator errors\" heading", got)
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
	const body = testBodyUnknownMarker
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
		{
			"check failed pending",
			"check failed pending run 12\ntest command: go test ./...\nexit code: 1\noutput:\n--- FAIL: TestPing",
			"Test or lint failed for run 12:\ntest command: go test ./...\nexit code: 1\noutput:\n--- FAIL: TestPing",
		},
		{"check failed delivered", "check failed delivered run 12", "Test and lint output sent back to run 12."},
		{"perimeter resolved", "perimeter resolved run 9", "Perimeter decided for run 9."},
		{"retry requested", updateMarkerRetryRequested, "Retry requested."},
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
		{
			"judge host check",
			"judge host 1 s1 exit 3 cmd " + strings.Repeat("ab", 32) + "\nFAIL",
			"Host check for s1 exited 3.",
		},
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
		{"merge retry", "merge retry " + sha, "Main moved during the merge; Zing checks the pull request again in 10 seconds."},
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

// TestUpdateLineFixMarkers proves job/fix.go's own two marker shapes --
// "fix requested <kind> after run <R>" (fixRequestMessage) and "fix landed
// <mid> sha <sha>" (building.go's land and its adopted-commit twin) -- each
// render as their own owner-facing sentence (task D31-4a: cmd/zing's e2e
// second guard caught the real pipeline writing the first of these with no
// updateMarker* case recognizing it).
func TestUpdateLineFixMarkers(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct{ name, body, want string }{
		{
			"fix requested failure",
			"fix requested failure after run 5\ns2: expected 200, got 500",
			"Fix requested after run 5 (failure):\ns2: expected 200, got 500",
		},
		{
			"fix requested findings",
			"fix requested findings after run 6\n2 findings remain",
			"Fix requested after run 6 (findings):\n2 findings remain",
		},
		{
			"fix requested ci_log",
			"fix requested ci_log after run 7\nthe build step failed",
			"Fix requested after run 7 (ci_log):\nthe build step failed",
		},
		{
			"fix requested threads",
			"fix requested threads after run 8\n2 threads need a reply",
			"Fix requested after run 8 (threads):\n2 threads need a reply",
		},
		{"fix landed", "fix landed 3 sha " + sha, "Fix landed at 0123456."},
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

// TestUpdateLineGateMarkers proves D32's own four gate marker shapes
// (design section 22.12.1, 22.12.3a, 22.12.4; the D31-6 console follow-up)
// each render with the real agent name updateLine is given, not the
// generic agentFallback displayBody and MarkerRecognized pass when they
// have no ticket-specific one to offer: "gate confirmed run <R> plan v<V>
// gate <QID> answer <AID>", the two "gate approval cancelled gate <QID>
// ..." shapes (by run, the agent cancelling in its own confirming turn, or
// by batch, the owner reopening a thread instead), and the two-line "seal
// refused gate <QID>\n<reason>".
func TestUpdateLineGateMarkers(t *testing.T) {
	t.Parallel()
	const agent = "Fable"
	cases := []struct{ name, body, want string }{
		{
			"gate confirmed",
			"gate confirmed run 40 plan v2 gate 12 answer 99",
			"Fable confirmed nothing is open.",
		},
		{
			"gate approval cancelled by the agent (run)",
			"gate approval cancelled gate 12 run 41",
			"Fable found open questions; the approval is cancelled.",
		},
		{
			"gate approval cancelled by the owner (batch)",
			"gate approval cancelled gate 12 batch 7",
			"You reopened a thread; the approval is cancelled.",
		},
		{
			"seal refused",
			"seal refused gate 12\napproval of gate question 12 was cancelled",
			"Zing did not seal: approval of gate question 12 was cancelled.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := updateRow(tc.body)
			got, show := updateLine(row, agent)
			if !show {
				t.Fatalf("updateLine(%q) hidden, want shown", tc.body)
			}
			if got != tc.want {
				t.Errorf("updateLine(%q) = %q, want %q", tc.body, got, tc.want)
			}
			if !MarkerRecognized(*row) {
				t.Errorf("MarkerRecognized(%q) = false, want true", tc.body)
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

	got, err := buildThreadRows(&store.Ticket{}, rows, nil, nil, nil, store.PlanningConversation{}, "The agent")
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
	// Outside a planning conversation, an answer's own pick is not a turn
	// of its own -- AnsweredHTML shows it instead (bug fix: "duplicate
	// Answered: plus You:", design section 22.7) -- so only the reply
	// shows up here.
	texts := renderTurnBodies(t, question.Question.Turns)
	if len(texts) != 1 || !strings.Contains(texts[0], "Explain these three options in more detail") {
		t.Errorf("question.Turns rendered = %v, want one turn for the reply only", texts)
	}

	threadLevel := got[1]
	if threadLevel.Question != nil || threadLevel.Body != "a thread-level note" {
		t.Errorf("got[1] = %+v, want the unaffected thread-level reply", threadLevel)
	}
}

// renderTurnBodies renders each turn's BodyHTML to the post-markdown HTML
// text Render produces, the same way thread.templ's own template tests
// read a question's rendered body: this file's tests assert on that text
// rather than on the pre-render markdown string, since buildTurns never
// keeps the latter.
func renderTurnBodies(t *testing.T, turns []templates.Turn) []string {
	t.Helper()
	out := make([]string, len(turns))
	for i, turn := range turns {
		var sb strings.Builder
		if err := turn.BodyHTML.Render(t.Context(), &sb); err != nil {
			t.Fatalf("render turn %d: %v", i, err)
		}
		out[i] = sb.String()
	}
	return out
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
		got := buildWaitProgress(ticket, rows, store.PlanningConversation{})
		if got.Answered != 1 || got.Total != 2 {
			t.Errorf("buildWaitProgress = %+v, want {Answered:1 Total:2}", got)
		}
	})

	t.Run("a ticket not waiting on anything reports no progress", func(t *testing.T) {
		t.Parallel()
		got := buildWaitProgress(&store.Ticket{}, []store.MessageRow{questionRowForWait(t, msgStateOpen, response.QuestionKindQuestion)}, store.PlanningConversation{})
		if got.Total != 0 {
			t.Errorf("buildWaitProgress = %+v, want Total=0 (not waiting)", got)
		}
	})

	t.Run("a non-question-backed wait reason reports no progress", func(t *testing.T) {
		t.Parallel()
		waiting := "error"
		ticket := &store.Ticket{WaitingOn: &waiting}
		got := buildWaitProgress(ticket, []store.MessageRow{questionRowForWait(t, msgStateOpen, response.QuestionKindQuestion)}, store.PlanningConversation{})
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
	revisableRows, err := buildThreadRows(&store.Ticket{WaitingOn: &waiting}, rows, nil, nil, nil, store.PlanningConversation{}, "The agent")
	if err != nil {
		t.Fatalf("buildThreadRows (revisable): %v", err)
	}
	if got := revisableRows[0].Question.StateLabel; got != "answered · can change" {
		t.Errorf("revisable answered question StateLabel = %q, want %q", got, "answered · can change")
	}

	lockedRows, err := buildThreadRows(&store.Ticket{}, rows, nil, nil, nil, store.PlanningConversation{}, "The agent")
	if err != nil {
		t.Fatalf("buildThreadRows (locked): %v", err)
	}
	if got := lockedRows[0].Question.StateLabel; got != "answered" {
		t.Errorf("locked answered question StateLabel = %q, want %q", got, "answered")
	}
}

// messagesCheckValues parses 0001_init.sql's messages table and returns its
// type and author CHECK lists, in schema order (task D31-4a,
// design/threading-design.md's guard): a parse off the real migration, not a
// hand-kept copy, so adding a type or an author to the schema breaks
// TestNoMessageKindRendersOutsideItsThread until buildThreadRows gives it a
// home, rather than silently rendering it as a loose card forever.
func messagesCheckValues(t *testing.T) (types, authors []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "store", "migrations", "0001_init.sql"))
	if err != nil {
		t.Fatalf("read 0001_init.sql: %v", err)
	}
	src := string(data)
	start := strings.Index(src, "CREATE TABLE messages")
	if start < 0 {
		t.Fatalf("0001_init.sql: no CREATE TABLE messages")
	}
	end := strings.Index(src[start:], ");")
	if end < 0 {
		t.Fatalf("0001_init.sql: messages table has no closing );")
	}
	block := src[start : start+end]
	return parseCheckList(t, block, "type IN"), parseCheckList(t, block, "author IN")
}

// parseCheckList extracts the first quoted, comma-separated CHECK (... IN
// (...)) list in block that follows marker.
func parseCheckList(t *testing.T, block, marker string) []string {
	t.Helper()
	i := strings.Index(block, marker)
	if i < 0 {
		t.Fatalf("messages table: no %q", marker)
	}
	rest := block[i+len(marker):]
	open := strings.Index(rest, "(")
	closeAt := strings.Index(rest, ")")
	if open < 0 || closeAt < 0 || closeAt < open {
		t.Fatalf("messages table: malformed %q list", marker)
	}
	var out []string
	for v := range strings.SplitSeq(rest[open+1:closeAt], ",") {
		v = strings.Trim(strings.TrimSpace(v), "'")
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// seedQuestionRow builds a minimal, valid "question" row (id 1, key "Q1")
// for TestNoMessageKindRendersOutsideItsThread to parent synthetic rows to.
func seedQuestionRow(t *testing.T) store.MessageRow {
	t.Helper()
	payload, err := json.Marshal(response.QuestionPayload{Key: "Q1", Kind: response.QuestionKindQuestion})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	return store.MessageRow{ID: 1, Message: store.Message{Type: msgTypeQuestion, Author: authorZing, Payload: payload}} //nolint:modernize // keyed on purpose: MessageRow's ID and CreatedAt fields precede the embedded Message, so the key cannot be dropped
}

// markerShapeCases is every known "update" marker body shape this view must
// place somewhere other than a loose top-level card (task D31-4a): one
// example per shape, collected from the marker constants and Sprintf
// formats across internal/job and internal/store (and, for the kinds
// console already recognizes by name, the same literal examples
// TestUpdateLineBuildMarkers, TestUpdateLineReviewMarkers, and
// TestUpdateLineJudgeShippingRespondMarkers above already use). The last
// two groups are D31's own conversation pending/delivered pair (hidden
// outright, TestConversationMarkersHidden's own narrower proof) and D32's
// gate markers (plan section 22.12.1, not written by any job or store code
// yet on this branch, enumerated from the plan text alone). "fix
// requested"/"fix landed" (internal/job/fix.go, building.go) had no
// updateMarker* case until cmd/zing's e2e second guard
// (verifySelftestMarkersAllRecognized) caught the real pipeline writing one
// nobody had registered here; fixUpdateLine now renders both.
var markerShapeCases = []struct{ name, body string }{
	{"planreview pending", "planreview v3 pending"},
	{"planreview delivered", "planreview v3 delivered"},
	{"validation errors pending", "validation errors pending run 7\nscenarios/scenario[0]/then: then must not be empty"},
	{"validation errors delivered", "validation errors delivered run 7"},
	{"response invalid", "response invalid run 9\nmissing required field \"plan\""},
	{"seal mismatch", "seal mismatch cohort 4"},
	{"claims ok", "claims ok run 12"},
	{"claim errors pending", "claim errors pending run 12\nclaims/test_exit: observed 1, want 0"},
	{"claim errors delivered", "claim errors delivered run 12"},
	{"check failed pending", "check failed pending run 12\nlint command: make lint\nexit code: 2\noutput: none"},
	{"check failed delivered", "check failed delivered run 12"},
	{"perimeter resolved", "perimeter resolved run 9"},
	{"retry requested", updateMarkerRetryRequested},
	{"perimeter question dropped", "perimeter question dropped run 4"},
	{"review round done", "review round 1 done sha abc123 lenses correctness,security\nkept 2 dropped 1 merged 1"},
	{"review round asked", "review round 2 asked\nruns 5,6\ndone correctness"},
	{"review round failed", "review round 1 failed\nlens security: timed out"},
	{"review round void", "review round 1 void\nhead moved from a to b"},
	{"review discussed", "review discussed r1f2\nrun 12 batch r1f2 kept 1"},
	{"review note", "review note r1f2\nb.go:3 is generated, see the header"},
	{"judge round started", "judge round 1 started sha 0123456789abcdef0123456789abcdef01234567 after run 5"},
	{"judge round verdicts", "judge round 1 verdicts run 7"},
	{"judge round passed", "judge round 1 passed"},
	{"judge round failed", "judge round 1 failed\ns2,s3"},
	{"judge round retry", "judge round 2 retry after run 9"},
	{"judge coverage failed", "judge coverage failed run 12\nmissing verdict for scenario s2"},
	{"judge coverage delivered", "judge coverage delivered run 12"},
	{"judge check", "judge check 1 s2 exit 0"},
	{"judge host check", "judge host 1 s1 exit 0 cmd " + strings.Repeat("ab", 32)},
	{"pr opened", "pr opened 42"},
	{"ci waiting", "ci waiting ci,lint"},
	{"reviewers re-requested", "reviewers re-requested 0123456789abcdef0123456789abcdef01234567\nalice,bob"},
	{"pr ready", "pr ready 0123456789abcdef0123456789abcdef01234567"},
	{"pr draft", "pr draft 0123456789abcdef0123456789abcdef01234567"},
	{"threads blocking", "threads blocking t3f9a0c1b2d4e5f60,t1a2b3c4d5e6f7081"},
	{"merge asked", "merge asked 0123456789abcdef0123456789abcdef01234567"},
	{"merge held", "merge held 0123456789abcdef0123456789abcdef01234567"},
	{"merge withdrawn", "merge withdrawn 0123456789abcdef0123456789abcdef01234567"},
	{"merge retry", "merge retry 0123456789abcdef0123456789abcdef01234567"},
	{"merge refused", "merge refused 0123456789abcdef0123456789abcdef01234567\nthe head moved"},
	{"pr merged", "pr merged 0123456789abcdef0123456789abcdef01234567"},
	{
		"respond batch started",
		"respond batch 1 started sha 0123456789abcdef0123456789abcdef01234567 after run 5\n" +
			"t3f9a0c1b2d4e5f60,t1a2b3c4d5e6f7081\nseen t3f9a0c1b2d4e5f60=d1,t1a2b3c4d5e6f7081=d2",
	},
	{"respond coverage failed", "respond coverage failed run 9\nthread t3f9a0c1b2d4e5f60 is not in this batch"},
	{"respond coverage delivered", "respond coverage delivered run 9"},
	{"respond batch stale", "respond batch 2 stale\nthe pull request head moved"},
	{"respond batch skipped", "respond batch 3 skipped"},
	{
		"respond batch retry",
		"respond batch 4 retry sha 0123456789abcdef0123456789abcdef01234567 after run 11\n" +
			"t3f9a0c1b2d4e5f60\nseen t3f9a0c1b2d4e5f60=d1",
	},
	{"respond applied", "respond applied 7\nreplied 3 fixing 1 skipped 0"},
	{"fix replies posted", "fix replies posted 8"},
	// fix requested/landed (internal/job/fix.go's fixRequestMessage,
	// building.go's two land call sites, and judging.go's, reviewing.go's,
	// and shipping.go's own fixRequested*Prefix constants): fixUpdateLine's
	// own case.
	{"fix requested failure", "fix requested failure after run 5\nthe scenario still fails"},
	{"fix requested findings", "fix requested findings after run 5\n2 findings remain"},
	{"fix requested ci_log", "fix requested ci_log after run 5\nthe build step failed"},
	{"fix requested threads", "fix requested threads after run 5\n2 threads need a reply"},
	{"fix landed", "fix landed 3 sha 0123456789abcdef0123456789abcdef01234567"},
	// D31's own conversation markers (store/reserve.go, conversation_reads.go).
	{"conversation pending", testBodyConversationPending},
	{"conversation delivered", "conversation delivered run 31 batch 4"},
	// D32's own markers (plan section 22.12.1), not written by any job or
	// store code yet on this branch.
	{"gate confirmed", "gate confirmed run 40 plan v2 gate 12 answer 99"},
	{"gate approval cancelled by run", "gate approval cancelled gate 12 run 41"},
	{"gate approval cancelled by batch", "gate approval cancelled gate 12 batch 7"},
	{"seal refused", "seal refused gate 12\napproval of gate question 12 was cancelled"},
}

// TestNoMessageKindRendersOutsideItsThread is the permanent guard for
// placement by structure (task D31-4a, design/threading-design.md (d)): it
// enumerates every messages.type value crossed with every author (both
// parsed from the real schema, messagesCheckValues), parented to a question
// or not, plus every known marker shape (markerShapeCases), and asserts
// that a parented row never produces a top-level row of its own, that only
// a question or an escalation opens its own thread, and that no row
// produces a plain message card except an unparented reply authored "you".
func TestNoMessageKindRendersOutsideItsThread(t *testing.T) {
	t.Parallel()
	types, authors := messagesCheckValues(t)
	if len(types) == 0 || len(authors) == 0 {
		t.Fatalf("messagesCheckValues returned no types/authors: %v / %v", types, authors)
	}

	t.Run("every type and author, parented or not", func(t *testing.T) {
		t.Parallel()
		for _, typ := range types {
			for _, author := range authors {
				for _, parented := range []bool{true, false} {
					name := typ + "_" + author
					if parented {
						name += "_parented"
					} else {
						name += "_unparented"
					}
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						question := seedQuestionRow(t)
						row := store.MessageRow{ID: 2, Message: store.Message{Type: typ, Author: author}} //nolint:modernize // keyed on purpose
						switch typ {
						case msgTypeEscalation:
							// escalationLine prefers a non-empty Body over
							// decoding Payload, so a bare escalation row
							// still renders without a payload.
							row.Body = testEscalationBody
						case msgTypeQuestion:
							// A second, distinct question row, so
							// buildThreadQuestion decodes a valid payload and
							// opens its own interactive group rather than
							// falling back to a plain card for a payload it
							// cannot parse.
							payload, err := json.Marshal(response.QuestionPayload{Key: "Q2", Kind: response.QuestionKindQuestion})
							if err != nil {
								t.Fatalf("marshal question payload: %v", err)
							}
							row.Payload = payload
						}
						if parented {
							qid := question.ID
							row.ParentID = &qid
						}

						got, err := buildThreadRows(&store.Ticket{}, []store.MessageRow{question, row}, nil, nil, nil, store.PlanningConversation{}, "The agent")
						if err != nil {
							t.Fatalf("buildThreadRows: %v", err)
						}

						switch {
						case typ == msgTypeQuestion || typ == msgTypeEscalation:
							if len(got) != 2 {
								t.Fatalf("%s/%s always opens its own thread: got %d rows, want 2: %+v", typ, author, len(got), got)
							}
							if typ == msgTypeQuestion && got[1].Question == nil {
								t.Errorf("question row did not open its own interactive group: %+v", got[1])
							}
						case parented:
							if len(got) != 1 {
								t.Fatalf("a parented %s/%s row produced %d top-level rows, want 1 (folded into its question): %+v",
									typ, author, len(got), got)
							}
						case typ == msgTypeReply && author == authorYou:
							if len(got) != 2 {
								t.Fatalf("unparented reply/you: got %d rows, want 2 (the question plus its own card): %+v", len(got), got)
							}
							if got[1].Question != nil || got[1].Divider {
								t.Errorf("unparented reply/you did not render as a plain message card: %+v", got[1])
							}
						default:
							if len(got) != 2 {
								t.Fatalf("unparented %s/%s: got %d rows, want 2 (the question plus a divider): %+v",
									typ, author, len(got), got)
							}
							if got[1].Question != nil {
								t.Errorf("unparented %s/%s rendered as a question group: %+v", typ, author, got[1])
							}
							if !got[1].Divider {
								t.Errorf("unparented %s/%s rendered as a full card, not a one-line divider: %+v", typ, author, got[1])
							}
						}
					})
				}
			}
		}
	})

	t.Run("every known marker shape", func(t *testing.T) {
		t.Parallel()
		for _, tc := range markerShapeCases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				t.Run("parented, folds into its question", func(t *testing.T) {
					t.Parallel()
					question := seedQuestionRow(t)
					qid := question.ID
					row := store.MessageRow{ID: 2, Message: store.Message{ //nolint:modernize // keyed on purpose
						Type: msgTypeUpdate, Author: authorSystem, Body: tc.body, ParentID: &qid,
					}}
					got, err := buildThreadRows(&store.Ticket{}, []store.MessageRow{question, row}, nil, nil, nil, store.PlanningConversation{}, "The agent")
					if err != nil {
						t.Fatalf("buildThreadRows: %v", err)
					}
					if len(got) != 1 {
						t.Fatalf("parented marker %q produced %d top-level rows, want 1 (folded into its question): %+v",
							tc.body, len(got), got)
					}
				})

				t.Run("unparented, never a card", func(t *testing.T) {
					t.Parallel()
					row := store.MessageRow{Message: store.Message{Type: msgTypeUpdate, Author: authorSystem, Body: tc.body}} //nolint:modernize // keyed on purpose
					got, err := buildThreadRows(&store.Ticket{}, []store.MessageRow{row}, nil, nil, nil, store.PlanningConversation{}, "The agent")
					if err != nil {
						t.Fatalf("buildThreadRows: %v", err)
					}
					switch len(got) {
					case 0:
						// Hidden: acceptable (D31's own conversation markers).
					case 1:
						if got[0].Question != nil || !got[0].Divider {
							t.Errorf("marker %q rendered as a full card, not a divider or hidden: %+v", tc.body, got[0])
						}
					default:
						t.Fatalf("marker %q produced %d rows, want 0 or 1: %+v", tc.body, len(got), got)
					}
				})
			})
		}
	})
}

// TestEscalationWithQuestionChildRendersNoCard proves the bug fix for
// ticket 1's duplicate card (escalation 48, parent_id NULL; question 49,
// parent_id 48, whose body repeats the escalation's own summary text):
// buildThreadRows' own rule ("a question or escalation row always opens
// its own thread, whatever its own parent_id") used to give the escalation
// a full card of its own even when its question child already carries the
// same text, so the owner saw it twice, stacked right above the question.
// An escalation with a question child now renders no row of its own; the
// question alone carries the thread, same as any other escalation-linked
// question (design/threading-design.md (d)).
func TestEscalationWithQuestionChildRendersNoCard(t *testing.T) {
	t.Parallel()
	escalation := store.MessageRow{ID: 48, Message: store.Message{Type: msgTypeEscalation, Author: authorZing, Body: testEscalationBody}} //nolint:modernize // keyed on purpose
	payload, err := json.Marshal(response.QuestionPayload{Key: "Q7", Kind: response.QuestionKindQuestion})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	parentID := escalation.ID
	question := store.MessageRow{ID: 49, Message: store.Message{Type: msgTypeQuestion, Author: authorZing, Payload: payload, ParentID: &parentID, Body: testEscalationBody + "\n\nHow should Zing proceed?"}} //nolint:modernize // keyed on purpose

	got, err := buildThreadRows(&store.Ticket{}, []store.MessageRow{escalation, question}, nil, nil, nil, store.PlanningConversation{}, "The agent")
	if err != nil {
		t.Fatalf("buildThreadRows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("buildThreadRows returned %d rows, want 1 (the question alone, no separate escalation card): %+v", len(got), got)
	}
	if got[0].ID != question.ID || got[0].Question == nil {
		t.Errorf("buildThreadRows' one row = %+v, want the question's own interactive group", got[0])
	}
}

// TestEscalationWithNoQuestionChildKeepsItsCard proves the fix above is
// scoped to an escalation with a question child: an escalation the owner
// has not yet been asked a question about (no child row at all) keeps
// today's behavior, its own full card.
func TestEscalationWithNoQuestionChildKeepsItsCard(t *testing.T) {
	t.Parallel()
	escalation := store.MessageRow{ID: 1, Message: store.Message{Type: msgTypeEscalation, Author: authorZing, Body: testEscalationBody}} //nolint:modernize // keyed on purpose

	got, err := buildThreadRows(&store.Ticket{}, []store.MessageRow{escalation}, nil, nil, nil, store.PlanningConversation{}, "The agent")
	if err != nil {
		t.Fatalf("buildThreadRows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("buildThreadRows returned %d rows, want 1 (the escalation's own card): %+v", len(got), got)
	}
	if got[0].ID != escalation.ID || got[0].Question != nil {
		t.Errorf("buildThreadRows' one row = %+v, want the escalation's own plain card", got[0])
	}
}

// TestUnknownMarkerIsADivider proves the default fallback for an "update"
// marker buildThreadRows does not recognize: a one-line divider carrying
// the marker's own raw body, never a card -- the defensive fallback
// updateLine's own default case always had, now reached through
// buildThreadRows (task D31-4a) instead of threadMessageRow.
func TestUnknownMarkerIsADivider(t *testing.T) {
	t.Parallel()
	const body = testBodyUnknownMarker
	row := store.MessageRow{ID: 1, Message: store.Message{Type: msgTypeUpdate, Author: authorSystem, Body: body}} //nolint:modernize // keyed on purpose

	got, err := buildThreadRows(&store.Ticket{}, []store.MessageRow{row}, nil, nil, nil, store.PlanningConversation{}, "The agent")
	if err != nil {
		t.Fatalf("buildThreadRows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("buildThreadRows returned %d rows, want 1: %+v", len(got), got)
	}
	if !got[0].Divider {
		t.Errorf("unknown marker did not render as a divider: %+v", got[0])
	}
	if got[0].Question != nil {
		t.Errorf("unknown marker rendered a question group: %+v", got[0])
	}
	if got[0].Body != body {
		t.Errorf("divider body = %q, want the marker's raw body %q", got[0].Body, body)
	}
}

// TestConversationMarkersHidden proves D31's own "conversation pending" and
// "conversation delivered" markers (store/reserve.go,
// store/conversation_reads.go) never render in the Thread view at all --
// not as a card, not even as a divider -- since neither carries a word the
// owner would read as content (design/threading-design.md (d)).
func TestConversationMarkersHidden(t *testing.T) {
	t.Parallel()
	rows := []store.MessageRow{
		{ID: 1, Message: store.Message{Type: msgTypeUpdate, Author: authorSystem, Body: testBodyConversationPending}},             //nolint:modernize // keyed on purpose
		{ID: 2, Message: store.Message{Type: msgTypeUpdate, Author: authorSystem, Body: "conversation delivered run 31 batch 4"}}, //nolint:modernize // keyed on purpose
	}
	got, err := buildThreadRows(&store.Ticket{}, rows, nil, nil, nil, store.PlanningConversation{}, "The agent")
	if err != nil {
		t.Fatalf("buildThreadRows: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("conversation markers rendered %d rows, want 0 (hidden): %+v", len(got), got)
	}
}

// TestMarkerRecognized proves the exported predicate cmd/zing's e2e second
// guard calls (design/threading-design.md: "asserts that updateLine
// recognized every update row the run wrote"): a known marker and a
// deliberately hidden one both report true, an unregistered marker reports
// false, and a non-"update" row always reports true (nothing to
// recognize).
func TestMarkerRecognized(t *testing.T) {
	t.Parallel()
	checkRerun, unknownKind := store.EventKindCheckRerun, "not_a_kind"
	for _, tc := range []struct {
		name string
		row  store.MessageRow
		want bool
	}{
		{
			"a known marker is recognized",
			store.MessageRow{Message: store.Message{Type: msgTypeUpdate, Author: authorSystem, Body: updateMarkerRetryRequested}}, //nolint:modernize // keyed on purpose
			true,
		},
		{
			"a deliberately hidden marker is recognized",
			store.MessageRow{Message: store.Message{Type: msgTypeUpdate, Author: authorSystem, Body: testBodyConversationPending}}, //nolint:modernize // keyed on purpose
			true,
		},
		{
			"an unregistered marker is not recognized",
			store.MessageRow{Message: store.Message{Type: msgTypeUpdate, Author: authorSystem, Body: testBodyUnknownMarker}}, //nolint:modernize // keyed on purpose
			false,
		},
		{
			"a non-update row has nothing to recognize",
			store.MessageRow{Message: store.Message{Type: msgTypeState, Author: authorSystem, Body: "queued -> planning"}}, //nolint:modernize // keyed on purpose
			true,
		},
		{
			"a typed event with a registered rule is recognized",
			store.MessageRow{Message: store.Message{ //nolint:modernize // keyed on purpose
				Type: msgTypeUpdate, Author: authorSystem, EventKind: &checkRerun,
				Payload: []byte(`{"check":"test","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","run_id":1,"check_run_id":1,"reason":"flaky"}`),
			}},
			true,
		},
		{
			"a typed event with no registered rule is not recognized",
			store.MessageRow{Message: store.Message{ //nolint:modernize // keyed on purpose
				Type: msgTypeUpdate, Author: authorSystem, EventKind: &unknownKind,
			}},
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := MarkerRecognized(tc.row); got != tc.want {
				t.Errorf("MarkerRecognized(%+v) = %v, want %v", tc.row, got, tc.want)
			}
		})
	}
}

// TestResolvedSystemRowShowsAsANoteNotAnEmptyCard proves bug 14 (the
// owner's "empty resolved system cards" report): a resolved/system row
// (ResolveQuestions or a withdraw, section 22.3 -- its own Body is always
// empty, the decision text living only on an agent-authored resolved row)
// folds into its question as an ordinary turn reading "Resolved.", never a
// loose, empty top-level card. D31-4a's structural placement already fixed
// the loose card; this is the regression test the task asked for, proving
// it still holds and that the row reads as something, not nothing.
func TestResolvedSystemRowShowsAsANoteNotAnEmptyCard(t *testing.T) {
	t.Parallel()
	question := seedQuestionRow(t)
	qid := question.ID
	resolved := store.MessageRow{ID: 2, Message: store.Message{ //nolint:modernize // keyed on purpose
		Type: msgTypeResolved, Author: authorSystem, ParentID: &qid,
	}}

	got, err := buildThreadRows(&store.Ticket{}, []store.MessageRow{question, resolved}, nil, nil, nil, store.PlanningConversation{}, "The agent")
	if err != nil {
		t.Fatalf("buildThreadRows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("buildThreadRows returned %d rows, want 1 (folded into its question); got %+v", len(got), got)
	}
	texts := renderTurnBodies(t, got[0].Question.Turns)
	if len(texts) != 1 || !strings.Contains(texts[0], "Resolved.") {
		t.Errorf("question.Turns rendered = %v, want one turn reading \"Resolved.\"", texts)
	}
}

// TestAgentNameFromModel proves console.agentName (design section 22.7):
// the planning job's own model, first letter upper-cased, or "The agent"
// when there is no machine or no planning model to read one from.
func TestAgentNameFromModel(t *testing.T) {
	t.Parallel()

	// The real machine.toml names the planner after its planning alias:
	// opus, so the console says "Opus is working." and "Settled by Opus:".
	realMachine, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("machine.Load: %v", err)
	}
	if got := agentName(realMachine); got != "Opus" {
		t.Errorf("agentName(real machine.toml) = %q, want Opus", got)
	}

	for _, tc := range []struct {
		name string
		m    *machine.Machine
		want string
	}{
		{"a nil machine falls back", nil, "The agent"},
		{"a machine with no planning job falls back", &machine.Machine{Jobs: map[string]machine.Job{}}, "The agent"},
		{"a lowercase model is capitalized", &machine.Machine{Jobs: map[string]machine.Job{"planning": {Model: "fable"}}}, "Fable"},
		{"an already-capitalized model is unchanged", &machine.Machine{Jobs: map[string]machine.Job{"planning": {Model: "Claude"}}}, "Claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := agentName(tc.m); got != tc.want {
				t.Errorf("agentName(...) = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildWaitProgressSkipsPlanningQuestions proves design section 22.7:
// a planning question shares its payload's Kind with an ordinary
// classify-round question, so buildWaitProgress must exclude it by id
// (conv's own thread set) rather than by Kind alone, or a ticket whose
// waiting_on happens to be "questions" for a planning reason would
// double-count it against a round it was never part of.
func TestBuildWaitProgressSkipsPlanningQuestions(t *testing.T) {
	t.Parallel()
	waiting := waitReasonQuestions
	ticket := &store.Ticket{WaitingOn: &waiting}

	planningQ := questionRowForWait(t, msgStateOpen, response.QuestionKindQuestion)
	planningQ.ID = 1
	ordinaryQ := questionRowForWait(t, msgStateOpen, response.QuestionKindQuestion)
	ordinaryQ.ID = 2

	conv := store.PlanningConversation{Threads: []store.Thread{{Question: planningQ}}}
	got := buildWaitProgress(ticket, []store.MessageRow{planningQ, ordinaryQ}, conv)
	if got.Total != 1 || got.Answered != 0 {
		t.Errorf("buildWaitProgress = %+v, want {Answered:0 Total:1} (the planning question excluded)", got)
	}
}

// TestBuildThreadRowsInterleavesConversation proves design section 22.7: a
// planning question's own Turns render in PlanningConversation's own turn
// order, each labeled by who wrote it (You, or the agent's own display
// name), and an owner turn whose batch is past the delivery watermark
// carries the Queued tag.
func TestBuildThreadRowsInterleavesConversation(t *testing.T) {
	t.Parallel()
	qid := int64(10)
	questionPayload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindQuestion,
		Options: []response.Option{{Key: "a", Text: "ReadBuildInfo only"}},
	})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	question := store.MessageRow{ID: qid, Message: store.Message{Type: msgTypeQuestion, Payload: questionPayload}} //nolint:modernize // keyed on purpose

	batch4, batch6 := int64(4), int64(6)
	ownerText := store.MessageRow{ID: 11, Message: store.Message{ //nolint:modernize // keyed on purpose
		Type: msgTypeReply, Author: authorYou, ParentID: &qid, Body: "Also print the commit hash.", BatchID: &batch4,
	}}
	agentReply := store.MessageRow{ID: 12, Message: store.Message{ //nolint:modernize // keyed on purpose
		Type: msgTypeReply, Author: authorZing, ParentID: &qid, Body: "Agreed.",
	}}
	queuedText := store.MessageRow{ID: 13, Message: store.Message{ //nolint:modernize // keyed on purpose
		Type: msgTypeReply, Author: authorYou, ParentID: &qid, Body: "One more thing.", BatchID: &batch6,
	}}
	rows := []store.MessageRow{question, ownerText, agentReply, queuedText}

	conv := store.PlanningConversation{
		Delivered: 4,
		Threads: []store.Thread{{
			Question: question,
			Turns:    []store.MessageRow{ownerText, agentReply, queuedText},
		}},
	}

	got, err := buildThreadRows(&store.Ticket{}, rows, nil, nil, nil, conv, "Fable")
	if err != nil {
		t.Fatalf("buildThreadRows: %v", err)
	}
	if len(got) != 1 || got[0].Question == nil {
		t.Fatalf("buildThreadRows returned %+v, want one question row", got)
	}
	turns := got[0].Question.Turns
	if len(turns) != 3 {
		t.Fatalf("question.Turns has %d entries, want 3: %+v", len(turns), turns)
	}
	if turns[0].Author != "You" || turns[0].Queued {
		t.Errorf("turns[0] = %+v, want the delivered owner turn, not queued", turns[0])
	}
	if turns[1].Author != "Fable" {
		t.Errorf("turns[1].Author = %q, want the agent's own display name", turns[1].Author)
	}
	if turns[2].Author != "You" || !turns[2].Queued {
		t.Errorf("turns[2] = %+v, want the undelivered owner turn, queued", turns[2])
	}
}

// TestBuildThreadRowsPlacesSystemResolvedRowByID proves the bug fix (live
// console, ticket 1, Q1): store.agentRowsByQuestion reads only
// zing-authored reply and resolved rows, so a system-authored resolved row
// (ResolveQuestions or withdraw, internal/store/commit.go) never reaches
// PlanningConversation's own Turns -- conv.Threads[0].Turns below stands in
// for that gap, carrying ids 5, 7, 15, and 16 but not 9. mergeThreadOrder
// must still place row 9 inside the thread, by id, between the owner
// answer (7) and the next owner reply (15), rather than after every turn
// (which used to put "Resolved." last, after "You reopened Q1.").
func TestBuildThreadRowsPlacesSystemResolvedRowByID(t *testing.T) {
	t.Parallel()
	qid := int64(1)
	questionPayload, err := json.Marshal(response.QuestionPayload{Key: "Q1", Kind: response.QuestionKindQuestion})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	question := store.MessageRow{ID: qid, Message: store.Message{Type: msgTypeQuestion, Payload: questionPayload}} //nolint:modernize // keyed on purpose

	reply5 := store.MessageRow{ID: 5, Message: store.Message{ //nolint:modernize // keyed on purpose
		Type: msgTypeReply, Author: authorYou, ParentID: &qid, Body: "owner reply",
	}}
	answer7 := store.MessageRow{ID: 7, Message: store.Message{ //nolint:modernize // keyed on purpose
		Type: msgTypeReply, Author: authorZing, ParentID: &qid, Body: "owner answer",
	}}
	resolved9 := store.MessageRow{ID: 9, Message: store.Message{ //nolint:modernize // keyed on purpose
		Type: msgTypeResolved, Author: authorSystem, ParentID: &qid,
	}}
	reply15 := store.MessageRow{ID: 15, Message: store.Message{ //nolint:modernize // keyed on purpose
		Type: msgTypeReply, Author: authorYou, ParentID: &qid, Body: "owner reopen reply",
	}}
	followup16 := store.MessageRow{ID: 16, Message: store.Message{ //nolint:modernize // keyed on purpose
		Type: msgTypeFollowup, Author: authorYou, ParentID: &qid, Body: "reopened",
	}}

	// rows carries every child in id order, the same order the store's own
	// id-ordered query returns them in; conv's Turns is store's
	// PlanningConversation output today -- missing row 9, the bug this test
	// pins.
	rows := []store.MessageRow{question, reply5, answer7, resolved9, reply15, followup16}
	conv := store.PlanningConversation{
		Threads: []store.Thread{{
			Question: question,
			Turns:    []store.MessageRow{reply5, answer7, reply15, followup16},
		}},
	}

	got, err := buildThreadRows(&store.Ticket{}, rows, nil, nil, nil, conv, "The agent")
	if err != nil {
		t.Fatalf("buildThreadRows: %v", err)
	}
	if len(got) != 1 || got[0].Question == nil {
		t.Fatalf("buildThreadRows returned %+v, want one question row", got)
	}
	texts := renderTurnBodies(t, got[0].Question.Turns)
	want := []string{"owner reply", "owner answer", "Resolved.", "owner reopen reply", "You reopened Q1."}
	if len(texts) != len(want) {
		t.Fatalf("question.Turns rendered = %v, want %v", texts, want)
	}
	for i, w := range want {
		if !strings.Contains(texts[i], w) {
			t.Errorf("question.Turns[%d] = %q, want it to contain %q (chronological order by id)", i, texts[i], w)
		}
	}
}

// gatePayload marshals a gate-kind question payload for the reopen-box
// tests below (hasOpenGateQuestion, gateApprovalInProgress): the only
// field those two read is Kind.
func gatePayload(t *testing.T) json.RawMessage {
	t.Helper()
	p, err := json.Marshal(response.QuestionPayload{Key: "Q2", Kind: response.QuestionKindGate})
	if err != nil {
		t.Fatalf("marshal gate payload: %v", err)
	}
	return p
}

// TestReopenPlaceholderOnSettledReopenableThread proves D32's own reply-box
// rule (design section 22.12.2, 22.12.4, the D31-6 console follow-up): a
// settled planning question's reply box stays while the ticket is still
// reopenable (state "planning"), named "Write to reopen Q1" plus "and
// withdraw the gate" only while a gate question is actually open, and
// disappears once the ticket has sealed into "building".
func TestReopenPlaceholderOnSettledReopenableThread(t *testing.T) {
	t.Parallel()
	qid := int64(10)
	questionPayload, err := json.Marshal(response.QuestionPayload{Key: "Q1", Kind: response.QuestionKindQuestion})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	question := store.MessageRow{ID: qid, Message: store.Message{Type: msgTypeQuestion, Payload: questionPayload}} //nolint:modernize // keyed on purpose
	conv := store.PlanningConversation{Threads: []store.Thread{{Question: question, Settled: true, Decision: "Plain text only."}}}

	t.Run("reopenable, no gate open: plain reopen placeholder", func(t *testing.T) {
		t.Parallel()
		ticket := &store.Ticket{State: ticketStatePlanning}
		got, err := buildThreadRows(ticket, []store.MessageRow{question}, nil, nil, nil, conv, "Fable")
		if err != nil {
			t.Fatalf("buildThreadRows: %v", err)
		}
		if len(got) != 1 || got[0].Question == nil {
			t.Fatalf("buildThreadRows returned %+v, want one question row", got)
		}
		q := got[0].Question
		if q.Interactive {
			t.Errorf("q.Interactive = true, want false (settled)")
		}
		const want = "Write to reopen Q1"
		if q.ReopenPlaceholder != want {
			t.Errorf("q.ReopenPlaceholder = %q, want %q", q.ReopenPlaceholder, want)
		}
	})

	t.Run("reopenable, a gate question is open: names withdrawing it too", func(t *testing.T) {
		t.Parallel()
		openState := msgStateOpen
		gateQuestion := store.MessageRow{ID: 20, Message: store.Message{Type: msgTypeQuestion, Payload: gatePayload(t), State: &openState}} //nolint:modernize // keyed on purpose
		ticket := &store.Ticket{State: ticketStatePlanning}
		got, err := buildThreadRows(ticket, []store.MessageRow{question, gateQuestion}, nil, nil, nil, conv, "Fable")
		if err != nil {
			t.Fatalf("buildThreadRows: %v", err)
		}
		var q1 *templates.ThreadQuestion
		for i := range got {
			if got[i].ID == qid {
				q1 = got[i].Question
			}
		}
		if q1 == nil {
			t.Fatalf("buildThreadRows returned %+v, want Q1's own row among them", got)
		}
		const want = "Write to reopen Q1 and withdraw the gate"
		if q1.ReopenPlaceholder != want {
			t.Errorf("q1.ReopenPlaceholder = %q, want %q", q1.ReopenPlaceholder, want)
		}
	})

	t.Run("locked once the ticket left planning", func(t *testing.T) {
		t.Parallel()
		ticket := &store.Ticket{State: "building"}
		got, err := buildThreadRows(ticket, []store.MessageRow{question}, nil, nil, nil, conv, "Fable")
		if err != nil {
			t.Fatalf("buildThreadRows: %v", err)
		}
		if len(got) != 1 || got[0].Question == nil {
			t.Fatalf("buildThreadRows returned %+v, want one question row", got)
		}
		if q := got[0].Question; q.ReopenPlaceholder != "" {
			t.Errorf("q.ReopenPlaceholder = %q, want empty once the ticket left planning", q.ReopenPlaceholder)
		}
	})
}

// TestEarlierDecisionShownWhileReopened proves D32's own closing-line rule
// (design section 22.12.4): a reopened thread (Settled false again) that
// still carries a past decision shows "Earlier decision: <decision>", not
// "Settled by <agent>", until the agent settles it again.
func TestEarlierDecisionShownWhileReopened(t *testing.T) {
	t.Parallel()
	qid := int64(10)
	questionPayload, err := json.Marshal(response.QuestionPayload{Key: "Q1", Kind: response.QuestionKindQuestion})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	question := store.MessageRow{ID: qid, Message: store.Message{Type: msgTypeQuestion, Payload: questionPayload}} //nolint:modernize // keyed on purpose
	conv := store.PlanningConversation{Threads: []store.Thread{{Question: question, Settled: false, Decision: "Plain text only."}}}

	got, err := buildThreadRows(&store.Ticket{State: ticketStatePlanning}, []store.MessageRow{question}, nil, nil, nil, conv, "Fable")
	if err != nil {
		t.Fatalf("buildThreadRows: %v", err)
	}
	if len(got) != 1 || got[0].Question == nil {
		t.Fatalf("buildThreadRows returned %+v, want one question row", got)
	}
	q := got[0].Question
	if q.SettledLabel != "Earlier decision" {
		t.Errorf("q.SettledLabel = %q, want %q", q.SettledLabel, "Earlier decision")
	}
	if q.SettledHTML == nil {
		t.Errorf("q.SettledHTML is nil, want the earlier decision rendered")
	}
	if !q.Interactive {
		t.Errorf("q.Interactive = false, want true (reopened, unsettled)")
	}
}

// TestGateApprovalInProgress proves the pure predicate behind threadBanner's
// own approval banners (design section 22.12.1, 22.12.4): true only for a
// gate-kind question in state "answered" whose newest sent answer picks
// option "a" -- not an open or resolved gate question, not an answered
// non-gate question, and not an answered gate question whose pick was a
// reject (any key but "a").
func TestGateApprovalInProgress(t *testing.T) {
	t.Parallel()
	answered := msgStateAnswered
	open := msgStateOpen

	answerRow := func(parent int64, option string) store.MessageRow {
		payload, err := json.Marshal(response.AnswerPayload{Option: &option})
		if err != nil {
			t.Fatalf("marshal answer payload: %v", err)
		}
		return store.MessageRow{Message: store.Message{Type: msgTypeAnswer, ParentID: &parent, Payload: payload}} //nolint:modernize // keyed on purpose
	}

	t.Run("answered gate question approved (option a): true", func(t *testing.T) {
		t.Parallel()
		gate := store.MessageRow{ID: 1, Message: store.Message{Type: msgTypeQuestion, Payload: gatePayload(t), State: &answered}} //nolint:modernize // keyed on purpose
		if !gateApprovalInProgress([]store.MessageRow{gate, answerRow(1, "a")}) {
			t.Errorf("gateApprovalInProgress = false, want true")
		}
	})

	t.Run("answered gate question rejected (option b): false", func(t *testing.T) {
		t.Parallel()
		gate := store.MessageRow{ID: 1, Message: store.Message{Type: msgTypeQuestion, Payload: gatePayload(t), State: &answered}} //nolint:modernize // keyed on purpose
		if gateApprovalInProgress([]store.MessageRow{gate, answerRow(1, "b")}) {
			t.Errorf("gateApprovalInProgress = true, want false")
		}
	})

	t.Run("gate question still open: false", func(t *testing.T) {
		t.Parallel()
		gate := store.MessageRow{ID: 1, Message: store.Message{Type: msgTypeQuestion, Payload: gatePayload(t), State: &open}} //nolint:modernize // keyed on purpose
		if gateApprovalInProgress([]store.MessageRow{gate}) {
			t.Errorf("gateApprovalInProgress = true, want false")
		}
	})

	t.Run("answered, but not a gate question: false", func(t *testing.T) {
		t.Parallel()
		payload, err := json.Marshal(response.QuestionPayload{Key: "Q1", Kind: response.QuestionKindQuestion})
		if err != nil {
			t.Fatalf("marshal question payload: %v", err)
		}
		q := store.MessageRow{ID: 1, Message: store.Message{Type: msgTypeQuestion, Payload: payload, State: &answered}} //nolint:modernize // keyed on purpose
		if gateApprovalInProgress([]store.MessageRow{q, answerRow(1, "a")}) {
			t.Errorf("gateApprovalInProgress = true, want false")
		}
	})
}

// TestConversationPills proves console.planningPill's own four-way split
// (design section 22.7 item 1): settled always wins; otherwise "with
// <agent>" when the in-flight run has already taken delivery of an owner
// message in this thread, "queued" when one is sent but no run has taken
// delivery of it yet, and "your turn" when nothing is outstanding.
func TestConversationPills(t *testing.T) {
	t.Parallel()
	batch3, batch5 := int64(3), int64(5)
	ownerDelivered := store.MessageRow{Message: store.Message{Author: authorYou, BatchID: &batch3}}   //nolint:modernize // keyed on purpose
	ownerUndelivered := store.MessageRow{Message: store.Message{Author: authorYou, BatchID: &batch5}} //nolint:modernize // keyed on purpose

	for _, tc := range []struct {
		name string
		th   store.Thread
		conv store.PlanningConversation
		want string
	}{
		{
			"settled wins even with an outstanding owner turn",
			store.Thread{Settled: true, Turns: []store.MessageRow{ownerUndelivered}},
			store.PlanningConversation{Delivered: 2},
			"settled",
		},
		{
			"with <agent>: the in-flight run already took delivery",
			store.Thread{Turns: []store.MessageRow{ownerDelivered}},
			store.PlanningConversation{Delivered: 2, InFlight: &store.InFlightRun{RunID: 9, ThroughBatch: 4}},
			"with Fable",
		},
		{
			"queued: sent, but no run has taken delivery of it yet",
			store.Thread{Turns: []store.MessageRow{ownerUndelivered}},
			store.PlanningConversation{Delivered: 2, InFlight: &store.InFlightRun{RunID: 9, ThroughBatch: 4}},
			"queued",
		},
		{
			"queued with no run in flight at all",
			store.Thread{Turns: []store.MessageRow{ownerUndelivered}},
			store.PlanningConversation{Delivered: 2},
			"queued",
		},
		{
			"your turn: nothing outstanding from the owner",
			store.Thread{},
			store.PlanningConversation{Delivered: 2},
			"your turn",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := planningPill(tc.th, tc.conv, "Fable"); got != tc.want {
				t.Errorf("planningPill(...) = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAgentStatusBanner proves console.buildAgentStatus's own table
// (design section 22.7): a run in flight reports it is working plus what
// is queued for its next turn; no run but undelivered messages reports
// what the agent gets on its next turn; no run, nothing undelivered, but
// some thread open names the open threads; every thread settled renders no
// banner at all.
func TestAgentStatusBanner(t *testing.T) {
	t.Parallel()

	questionPayload := func(t *testing.T, key string) json.RawMessage {
		t.Helper()
		p, err := json.Marshal(response.QuestionPayload{Key: key, Kind: response.QuestionKindQuestion})
		if err != nil {
			t.Fatalf("marshal question payload: %v", err)
		}
		return p
	}

	t.Run("a run in flight: working, plus what is queued for its next turn", func(t *testing.T) {
		t.Parallel()
		b3, b5 := int64(3), int64(5)
		conv := store.PlanningConversation{
			Delivered: 2,
			InFlight:  &store.InFlightRun{RunID: 9, ThroughBatch: 4},
			Threads: []store.Thread{{
				Question: store.MessageRow{ID: 1, Message: store.Message{Payload: questionPayload(t, "Q1")}}, //nolint:modernize // keyed on purpose
				Turns: []store.MessageRow{
					{Message: store.Message{Type: msgTypeReply, Author: authorYou, BatchID: &b3}}, //nolint:modernize // keyed on purpose
					{Message: store.Message{Type: msgTypeReply, Author: authorYou, BatchID: &b5}}, //nolint:modernize // keyed on purpose
				},
			}},
		}
		const want = "Fable is working. 1 message queued for its next turn."
		if got := buildAgentStatus(conv, "Fable"); got != want {
			t.Errorf("buildAgentStatus(...) = %q, want %q", got, want)
		}
	})

	t.Run("no run in flight, undelivered owner messages", func(t *testing.T) {
		t.Parallel()
		b3, b4 := int64(3), int64(4)
		conv := store.PlanningConversation{
			Delivered: 2,
			Threads: []store.Thread{{
				Question: store.MessageRow{ID: 1, Message: store.Message{Payload: questionPayload(t, "Q1")}}, //nolint:modernize // keyed on purpose
				Turns: []store.MessageRow{
					{Message: store.Message{Type: msgTypeReply, Author: authorYou, BatchID: &b3}}, //nolint:modernize // keyed on purpose
					{Message: store.Message{Type: msgTypeReply, Author: authorYou, BatchID: &b4}}, //nolint:modernize // keyed on purpose
				},
			}},
		}
		const want = "Fable gets your 2 messages on its next turn."
		if got := buildAgentStatus(conv, "Fable"); got != want {
			t.Errorf("buildAgentStatus(...) = %q, want %q", got, want)
		}
	})

	t.Run("no run, nothing undelivered, some thread still open", func(t *testing.T) {
		t.Parallel()
		conv := store.PlanningConversation{
			Delivered: 2,
			Threads: []store.Thread{
				{Question: store.MessageRow{ID: 1, Message: store.Message{Payload: questionPayload(t, "Q1")}}},                //nolint:modernize // keyed on purpose
				{Question: store.MessageRow{ID: 2, Message: store.Message{Payload: questionPayload(t, "Q2")}}, Settled: true}, //nolint:modernize // keyed on purpose
			},
		}
		const want = "Waiting on you: Q1."
		if got := buildAgentStatus(conv, "Fable"); got != want {
			t.Errorf("buildAgentStatus(...) = %q, want %q", got, want)
		}
	})

	t.Run("every thread settled renders no banner", func(t *testing.T) {
		t.Parallel()
		conv := store.PlanningConversation{
			Threads: []store.Thread{{Question: store.MessageRow{ID: 1}, Settled: true}},
		}
		if got := buildAgentStatus(conv, "Fable"); got != "" {
			t.Errorf("buildAgentStatus(...) = %q, want empty (every thread settled)", got)
		}
	})
}

// TestOptionChipTextRendersBackticksAsCode proves the bug fix end to end
// through buildThreadQuestion (design section 22.7's owner-reported
// locked-view complaint "raw backticks in option chips"): an option whose
// own Text carries a backtick renders TextHTML with a <code> span, not the
// literal backtick optionChips used to print.
func TestOptionChipTextRendersBackticksAsCode(t *testing.T) {
	t.Parallel()
	questionPayload, err := json.Marshal(response.QuestionPayload{
		Key: "Q1", Kind: response.QuestionKindQuestion,
		Options: []response.Option{{Key: "a", Text: "Run `zing version`"}},
	})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	openState := msgStateOpen
	rows := []store.MessageRow{{Message: store.Message{Type: msgTypeQuestion, State: &openState, Payload: questionPayload}}} //nolint:modernize // keyed on purpose

	got, err := buildThreadRows(&store.Ticket{}, rows, nil, nil, nil, store.PlanningConversation{}, "The agent")
	if err != nil {
		t.Fatalf("buildThreadRows: %v", err)
	}
	if len(got) != 1 || got[0].Question == nil || len(got[0].Question.Options) != 1 {
		t.Fatalf("buildThreadRows returned %+v, want one question with one option", got)
	}
	var sb strings.Builder
	if err := got[0].Question.Options[0].TextHTML.Render(t.Context(), &sb); err != nil {
		t.Fatalf("render option TextHTML: %v", err)
	}
	if !strings.Contains(sb.String(), "<code>zing version</code>") {
		t.Errorf("option TextHTML = %q, want it to contain <code>zing version</code>", sb.String())
	}
}

func TestIsWithdrawnGate(t *testing.T) {
	t.Parallel()
	resolved, open := msgStateResolved, "open"
	tests := []struct {
		name     string
		kind     response.QuestionKind
		state    *string
		answered bool
		want     bool
	}{
		{"resolved gate with no answer is withdrawn", response.QuestionKindGate, &resolved, false, true},
		{"resolved gate with an answer is not", response.QuestionKindGate, &resolved, true, false},
		{"open gate is not", response.QuestionKindGate, &open, false, false},
		{"resolved question is not", response.QuestionKindQuestion, &resolved, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isWithdrawnGate(tc.kind, tc.state, tc.answered); got != tc.want {
				t.Errorf("isWithdrawnGate = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsSupersededGate proves the D32 case isWithdrawnGate's own "resolved
// gate with no answer" test does not tell apart from a plain withdrawal: a
// resolved gate whose children include a "gate approval cancelled" marker
// (job/planning.go's confirming turn, answered with a revised plan instead
// of "confirmed") closed because the agent cancelled the owner's approval,
// not because nobody answered it at all. It reads "superseded", not
// "withdrawn".
func TestIsSupersededGate(t *testing.T) {
	t.Parallel()
	resolved, open := msgStateResolved, "open"
	cancelled := []store.MessageRow{*updateRow(updateMarkerGateApprovalCancelledPrefix + "7 run 3")}
	other := []store.MessageRow{*updateRow("gate confirmed run 3 plan v2 gate 7 answer 9")}
	tests := []struct {
		name     string
		kind     response.QuestionKind
		state    *string
		children []store.MessageRow
		want     bool
	}{
		{"resolved gate with a cancellation marker is superseded", response.QuestionKindGate, &resolved, cancelled, true},
		{"resolved gate with an unrelated marker is not", response.QuestionKindGate, &resolved, other, false},
		{"resolved gate with no children is not", response.QuestionKindGate, &resolved, nil, false},
		{"open gate with a cancellation marker is not", response.QuestionKindGate, &open, cancelled, false},
		{"resolved question with a cancellation marker is not", response.QuestionKindQuestion, &resolved, cancelled, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isSupersededGate(tc.kind, tc.state, tc.children); got != tc.want {
				t.Errorf("isSupersededGate = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGateVerdict proves the bug fix for ticket 1's question 44 (gate,
// resolved, answer row 45 picking option "b"): a gate that closed neither
// withdrawn nor superseded used to fall through to questionStateLabel's
// plain "resolved" pill, which does not say which way it went. It now
// reads "approved" or "rejected" off the owner's chosen option, matched by
// the payload's own option text (job's own gateOptionApprove/
// gateOptionReject key letters, internal/job/planning.go, are unexported,
// so this reads "Approve"/"Reject" instead of hard-coding "a"/"b").
func TestGateVerdict(t *testing.T) {
	t.Parallel()
	resolved, open := msgStateResolved, msgStateOpen
	options := []response.Option{{Key: "a", Text: "Approve"}, {Key: "b", Text: "Reject"}}
	approve, reject := "a", "b"
	tests := []struct {
		name     string
		kind     response.QuestionKind
		state    *string
		answered bool
		ap       response.AnswerPayload
		options  []response.Option
		want     string
		wantOK   bool
	}{
		{"resolved gate approved", response.QuestionKindGate, &resolved, true, response.AnswerPayload{Option: &approve}, options, "approved", true},
		{"resolved gate rejected", response.QuestionKindGate, &resolved, true, response.AnswerPayload{Option: &reject}, options, "rejected", true},
		{"resolved gate with no sent answer", response.QuestionKindGate, &resolved, false, response.AnswerPayload{}, options, "", false},
		{"open gate is not a verdict yet", response.QuestionKindGate, &open, true, response.AnswerPayload{Option: &approve}, options, "", false},
		{"resolved question is not a gate", response.QuestionKindQuestion, &resolved, true, response.AnswerPayload{Option: &approve}, options, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := gateVerdict(tc.kind, tc.state, tc.answered, tc.ap, tc.options)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("gateVerdict = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestGateShowsPlan(t *testing.T) {
	t.Parallel()
	open, answered, resolved := msgStateOpen, msgStateAnswered, msgStateResolved
	tests := []struct {
		name  string
		kind  response.QuestionKind
		state *string
		want  bool
	}{
		{"open gate shows the plan", response.QuestionKindGate, &open, true},
		{"answered gate awaiting its confirming turn shows the plan", response.QuestionKindGate, &answered, true},
		{"resolved gate does not", response.QuestionKindGate, &resolved, false},
		{"open question does not", response.QuestionKindQuestion, &open, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := gateShowsPlan(tc.kind, tc.state); got != tc.want {
				t.Errorf("gateShowsPlan = %v, want %v", got, tc.want)
			}
		})
	}
}

// projectSectionsRefs returns tickets' tracker refs, in order, for
// TestProjectSections' assertions.
func projectSectionsRefs(tickets []store.Ticket) []string {
	out := make([]string, len(tickets))
	for i := range tickets {
		out[i] = tickets[i].TrackerRef
	}
	return out
}

// TestProjectSections proves projectSections splits a project's tickets
// into live and closed (design section 6.5, Task 3): live tickets sort by
// their state's index in order, highest first, ties broken by the input's
// own issue-number order (TicketsByProject); a state missing from order
// sorts last; closed tickets keep that input order unchanged. With nil
// order and nil terminal (no machine loaded), everything is live, in input
// order, and nothing is closed.
func TestProjectSections(t *testing.T) {
	t.Parallel()

	realMachine, err := machine.Load(zing.Assets, "machine.toml")
	if err != nil {
		t.Fatalf("machine.Load: %v", err)
	}

	tickets := []store.Ticket{
		{TrackerRef: "65", State: string(response.TicketStateBuilding)},
		{TrackerRef: "95", State: string(response.TicketStateQueued)},
		{TrackerRef: "96", State: "done"},
		{TrackerRef: "102", State: string(response.TicketStateBuilding)},
		{TrackerRef: "110", State: "reviewing"},
		{TrackerRef: "120", State: "escalated"},
		{TrackerRef: "130", State: "mystery"},
	}

	t.Run("with a real machine's order and terminal", func(t *testing.T) {
		t.Parallel()
		live, closed := projectSections(tickets, realMachine.States.Order, realMachine.States.Terminal)
		if got, want := projectSectionsRefs(live), []string{"110", "65", "102", "95", "130"}; !slices.Equal(got, want) {
			t.Errorf("live refs = %v, want %v", got, want)
		}
		if got, want := projectSectionsRefs(closed), []string{"96", "120"}; !slices.Equal(got, want) {
			t.Errorf("closed refs = %v, want %v", got, want)
		}
	})

	t.Run("with nil order and nil terminal, everything is live in input order", func(t *testing.T) {
		t.Parallel()
		live, closed := projectSections(tickets, nil, nil)
		if got, want := projectSectionsRefs(live), projectSectionsRefs(tickets); !slices.Equal(got, want) {
			t.Errorf("live refs = %v, want input order %v", got, want)
		}
		if len(closed) != 0 {
			t.Errorf("closed = %v, want empty", closed)
		}
	})
}

// navParkFixtureState is InsertTicket's own required starting state
// (goconst: a bare "queued" literal here would be this file's third,
// alongside the "queued" turn-status label TestXxx's own table tests
// already use twice, for an unrelated concept).
const navParkFixtureState = "queued"

// navParkRun claims a fresh ticket on testNavProject, reserves one open run
// on it, and parks it through the real store.ParkRuns, returning the
// ticket's own id: TestNavComponent_ClaudeHold's own fixture for a ticket
// whose sidebar row carries a real ParkedUntil, the same write path a
// genuine Claude session limit uses, rather than a buildNavThreads-only
// proof (r2f2).
func navParkRun(t *testing.T, s *store.Store, ref string, until time.Time) int64 {
	t.Helper()
	projectID, err := s.EnsureProject(t.Context(), store.Project{
		Name: "nav-claude-hold", RepoURL: "https://example.invalid/nav-claude-hold.git",
		LocalPath: t.TempDir(), Tracker: "github",
	})
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ticketID, err := s.InsertTicket(t.Context(), store.Ticket{
		ProjectID: projectID, TrackerRef: ref, Title: "nav claude hold fixture", State: navParkFixtureState,
	})
	if err != nil {
		t.Fatalf("InsertTicket(%s): %v", ref, err)
	}
	owner := "nav-park-" + ref
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	claimed, err := s.Claim(t.Context(), ticketID, owner, expires)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim: got false, want true")
	}
	if _, err := s.Reserve(t.Context(), ticketID, owner, expires,
		store.SessionUpsert{Job: "nav-park-fixture", Runtime: "claude"}, store.RunSeed{Model: "test-model"}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := s.ParkRuns(t.Context(), ticketID, owner, expires, until, ""); err != nil {
		t.Fatalf("ParkRuns: %v", err)
	}
	return ticketID
}

// TestNavComponent_ClaudeHold proves navComponent's own ClaudeHold read and
// its own now comparison (review fix r1f1: the previous test only covered
// buildNavThreads, never navComponent's own call to c.store.ClaudeHold or
// its own "still in the future" check): rendering #nav shows "claude:
// capped until" while the stored hold is in the future, and shows neither
// that line nor a parked badge once the hold (seeded here directly, the
// same store.ParkRuns a real park would use) has passed. It also proves the
// sidebar's own per-ticket parked badge (r2f2, not just buildNavThreads in
// isolation): a ticket real store.ParkRuns parked with a future reset shows
// the badge, and one parked with a reset already past does not.
func TestNavComponent_ClaudeHold(t *testing.T) {
	t.Parallel()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := &console{store: s}

	render := func() string {
		t.Helper()
		comp, err := c.navComponent(t.Context(), 0)
		if err != nil {
			t.Fatalf("navComponent: %v", err)
		}
		var buf strings.Builder
		if err := comp.Render(t.Context(), &buf); err != nil {
			t.Fatalf("Render: %v", err)
		}
		return buf.String()
	}

	if got := render(); strings.Contains(got, "claude: capped") {
		t.Errorf("navComponent with no hold set shows a claude-hold line; got:\n%s", got)
	}

	future := time.Now().Add(20 * time.Minute)
	if err := s.SetSettings(t.Context(), "claude_hold_until", future.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	gotFuture := render()
	if !strings.Contains(gotFuture, "claude: capped until "+clockLabel(future)) {
		t.Errorf("navComponent with a future hold missing the claude-hold line %q; got:\n%s", "claude: capped until "+clockLabel(future), gotFuture)
	}

	past := time.Now().Add(-20 * time.Minute)
	if err := s.SetSettings(t.Context(), "claude_hold_until", past.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if gotPast := render(); strings.Contains(gotPast, "claude: capped") {
		t.Errorf("navComponent with a past hold still shows a claude-hold line; got:\n%s", gotPast)
	}

	parkedFutureUntil := time.Now().Add(20 * time.Minute)
	navParkRun(t, s, "nav-parked-future", parkedFutureUntil)
	gotParkedFuture := render()
	if !strings.Contains(gotParkedFuture, `class="badge badge-parked"`) {
		t.Errorf("navComponent with a ticket parked until the future shows no parked badge; got:\n%s", gotParkedFuture)
	}
	if !strings.Contains(gotParkedFuture, "parked until "+clockLabel(parkedFutureUntil)) {
		t.Errorf("navComponent with a ticket parked until the future missing %q; got:\n%s",
			"parked until "+clockLabel(parkedFutureUntil), gotParkedFuture)
	}

	// A second ticket parked with a reset already past must not add a
	// second parked badge: the first ticket's own future park is still
	// live, so the count must stay 1, not drop to 0 or rise to 2.
	parkedPastUntil := time.Now().Add(-20 * time.Minute)
	navParkRun(t, s, "nav-parked-past", parkedPastUntil)
	gotParkedPast := render()
	if n := strings.Count(gotParkedPast, `class="badge badge-parked"`); n != 1 {
		t.Errorf("navComponent parked-badge count = %d, want 1 (only the still-future ticket); got:\n%s", n, gotParkedPast)
	}
}

// TestBuildNavThreads_ParkedUntil proves buildNavThreads' own now comparison
// (#45): a LiveTicket.ParkedUntil still after now gives NavThread.ParkedUntil
// equal to the literal "3:04pm"-style clock (not clockLabel itself, the
// function under test: review fix r1f1); one at or before now gives the
// empty string. future and past are built directly in time.Local, so the
// literal "4:01pm" is correct regardless of the test machine's own zone.
func TestBuildNavThreads_ParkedUntil(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 5, 16, 0, 0, 0, time.Local)

	future := time.Date(2026, 10, 5, 16, 1, 0, 0, time.Local)
	past := now.Add(-time.Minute)
	items := []store.LiveTicket{
		{Ticket: store.Ticket{ID: 1}, ParkedUntil: &future},
		{Ticket: store.Ticket{ID: 2}, ParkedUntil: &past},
		{Ticket: store.Ticket{ID: 3}},
	}

	got := buildNavThreads(items, now)
	if len(got) != 3 {
		t.Fatalf("buildNavThreads returned %d threads, want 3", len(got))
	}
	if got[0].ParkedUntil != "4:01pm" {
		t.Errorf("buildNavThreads[0].ParkedUntil = %q, want %q (future)", got[0].ParkedUntil, "4:01pm")
	}
	if got[1].ParkedUntil != "" {
		t.Errorf("buildNavThreads[1].ParkedUntil = %q, want \"\" (past)", got[1].ParkedUntil)
	}
	if got[2].ParkedUntil != "" {
		t.Errorf("buildNavThreads[2].ParkedUntil = %q, want \"\" (nil)", got[2].ParkedUntil)
	}
}
