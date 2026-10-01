package templates

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"zing/internal/store"
)

// renderItemRow renders itemRow(1, 2, item, itemDecisionsPerimeter) to a
// string, failing the test on a render error.
func renderItemRow(t *testing.T, item ThreadItem) string {
	t.Helper()
	var sb strings.Builder
	if err := itemRow(1, 2, item, itemDecisionsPerimeter, "").Render(t.Context(), &sb); err != nil {
		t.Fatalf("itemRow.Render: %v", err)
	}
	return sb.String()
}

// testWaitingOnYou is the StateLabel string.go's questionStateLabel gives
// an open question (views.go), repeated three or more times below
// (goconst).
const testWaitingOnYou = "waiting on you"

// testApprovePlanTitle and testSplitTicketTitle are two question titles
// repeated across this file's fixtures (goconst), named once rather than
// retyped.
const (
	testApprovePlanTitle = "Approve the plan?"
	testSplitTicketTitle = "Split this ticket?"
)

// emptyBodyHTML is a minimal templ.Component for a ThreadQuestion.BodyHTML
// test stand-in: questionGroup renders it unconditionally, and its own
// content is not what these tests assert on.
var emptyBodyHTML = templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
	_, err := io.WriteString(w, "body")
	return err
})

// renderQuestionGroup renders questionGroup(1, row) to a string, failing the
// test on a render error.
func renderQuestionGroup(t *testing.T, row ThreadRow) string {
	t.Helper()
	var sb strings.Builder
	if err := questionGroup(1, row).Render(t.Context(), &sb); err != nil {
		t.Fatalf("questionGroup.Render: %v", err)
	}
	return sb.String()
}

// TestPerimeterRowRendersParts proves itemRow's Task 11b rendering (design
// section 6.5, 9.2): a section 6.5-shaped Item.Text splits into the path,
// the marker pill, the builder reason, and the change, each its own
// element; a plain item (no marker) renders no pill; and a text outside the
// format renders unchanged, in one <span class="item-text">, exactly as
// before Task 11b -- the stored text is never touched, only how it renders.
func TestPerimeterRowRendersParts(t *testing.T) {
	t.Parallel()
	t.Run("a trust root item renders the pill, the reason, and the change in separate elements", func(t *testing.T) {
		t.Parallel()
		got := renderItemRow(t, ThreadItem{
			Ref:  "machine.toml",
			Text: "[trust root] Builder: the build needed the sandbox allowlist updated Change: added the hello package test command",
		})
		for _, want := range []string{
			`<span class="item-ref">machine.toml</span>`,
			`<span class="pill item-marker">trust root</span>`,
			`<span class="item-reason">the build needed the sandbox allowlist updated</span>`,
			`<span class="item-change">added the hello package test command</span>`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("rendered item-row missing %q; got:\n%s", want, got)
			}
		}
		if strings.Contains(got, `class="item-text"`) {
			t.Errorf("rendered item-row carries a plain item-text span, want the split parts only; got:\n%s", got)
		}
	})

	t.Run("a plain item renders no marker pill", func(t *testing.T) {
		t.Parallel()
		got := renderItemRow(t, ThreadItem{
			Ref:  "internal/hello/handler.go",
			Text: "Builder: the handler needed a small helper Change: added a formatGreeting helper",
		})
		if strings.Contains(got, "item-marker") {
			t.Errorf("rendered item-row carries a marker pill for an unmarked item; got:\n%s", got)
		}
		for _, want := range []string{
			`<span class="item-reason">the handler needed a small helper</span>`,
			`<span class="item-change">added a formatGreeting helper</span>`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("rendered item-row missing %q; got:\n%s", want, got)
			}
		}
	})

	t.Run("a text outside the format renders unchanged", func(t *testing.T) {
		t.Parallel()
		const text = "new HTTP handler for GET /hello"
		got := renderItemRow(t, ThreadItem{Ref: "internal/hello/handler.go", Text: text})
		if !strings.Contains(got, `<span class="item-text">`+text+`</span>`) {
			t.Errorf("rendered item-row missing the unchanged text %q; got:\n%s", text, got)
		}
		if strings.Contains(got, "item-marker") || strings.Contains(got, "item-reason") || strings.Contains(got, "item-change") {
			t.Errorf("rendered item-row split a non-conforming text into parts; got:\n%s", got)
		}
	})
}

// TestQuestionGroupLabelsMessageCount proves the bug fix for a stray,
// unlabeled number after every question's title in its summary line (e.g.
// "Q1 Where does the version string come from? 1 [waiting on you]"):
// row.Question.MessageCount (the question's own message plus every reply,
// answer, followup, or resolved row naming it as a parent -- views.go's
// buildThreadQuestion) is meaningful, so questionGroup labels it plainly
// ("1 message" / "N messages") instead of rendering the bare digit.
func TestQuestionGroupLabelsMessageCount(t *testing.T) {
	t.Parallel()

	t.Run("one message reads as singular", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 1,
			Question: &ThreadQuestion{
				Key: "Q1", Title: "Where does the version string come from?",
				StateLabel: testWaitingOnYou, BodyHTML: emptyBodyHTML, MessageCount: 1,
			},
		})
		if !strings.Contains(got, `<span class="q-count">1 message</span>`) {
			t.Errorf("rendered question group missing a labeled, singular message count; got:\n%s", got)
		}
	})

	t.Run("more than one message reads as plural", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 2,
			Question: &ThreadQuestion{
				Key: "Q2", Title: testApprovePlanTitle,
				StateLabel: testWaitingOnYou, BodyHTML: emptyBodyHTML, MessageCount: 3,
			},
		})
		if !strings.Contains(got, `<span class="q-count">3 messages</span>`) {
			t.Errorf("rendered question group missing a labeled, plural message count; got:\n%s", got)
		}
	})

	t.Run("the bare unlabeled digit never renders on its own", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 3,
			Question: &ThreadQuestion{
				Key: "Q3", Title: testSplitTicketTitle,
				StateLabel: testWaitingOnYou, BodyHTML: emptyBodyHTML, MessageCount: 1,
			},
		})
		if strings.Contains(got, `<span class="q-count">1</span>`) {
			t.Errorf("rendered question group still carries the bare, unlabeled count; got:\n%s", got)
		}
	})
}

// TestFreeReplyRendersDraftConflictSpan proves the bug fix for a 409 beside
// the reply box: freeReply always renders an empty ".draft-conflict" span
// (CSS hides it empty, shell.templ) as a sibling of the reply-input,
// console.js's showDraftConflict own stable target regardless of whether
// any conflict has happened yet.
func TestFreeReplyRendersDraftConflictSpan(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	if err := freeReply(1, 2, "").Render(t.Context(), &sb); err != nil {
		t.Fatalf("freeReply.Render: %v", err)
	}
	got := sb.String()
	if !strings.Contains(got, `<p class="draft-conflict" aria-live="polite"></p>`) {
		t.Errorf("rendered freeReply missing the draft-conflict span; got:\n%s", got)
	}
}

// TestFreeReplyRendersDraftSavedSpan proves bug fix 11 (Enter saved the
// draft but the reply box emptied and stayed empty): freeReply always
// renders an empty ".draft-saved" span (CSS hides it empty, shell.templ) as
// a sibling of the reply-input, console.js's postDraftRequest own stable
// "Saved." target regardless of whether a save has happened yet, and the
// reply box still starts with any already-saved draft text as its value --
// the piece of 01e4713 this bug fix builds on, not clears on Enter.
func TestFreeReplyRendersDraftSavedSpan(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	if err := freeReply(1, 2, "already saved text").Render(t.Context(), &sb); err != nil {
		t.Fatalf("freeReply.Render: %v", err)
	}
	got := sb.String()
	if !strings.Contains(got, `<p class="draft-saved" aria-live="polite"></p>`) {
		t.Errorf("rendered freeReply missing the draft-saved span; got:\n%s", got)
	}
	if !strings.Contains(got, `value="already saved text"`) {
		t.Errorf("rendered freeReply lost the draft's own text as the input's value; got:\n%s", got)
	}
}

// TestQuestionGroupLocksAnAnsweredQuestion proves the bug fix for "render
// an answered question's controls as disabled or locked with its answer
// shown, so you can't type into a closed question at all": a non-interactive
// question with AnsweredText set renders answeredLocked's note instead of
// optionChips/itemRows/freeReply, and a non-interactive question with no
// AnsweredText (nothing decoded, or a reply-only question) renders neither.
func TestQuestionGroupLocksAnAnsweredQuestion(t *testing.T) {
	t.Parallel()

	t.Run("an answered question shows its locked note, not a reply box", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 1,
			Question: &ThreadQuestion{
				Key: "Q1", Title: testApprovePlanTitle, StateLabel: "answered",
				BodyHTML: emptyBodyHTML, MessageCount: 1,
				Interactive: false, AnsweredText: "a",
			},
		})
		if !strings.Contains(got, `<p class="q-answered">Answered: a</p>`) {
			t.Errorf("rendered question group missing the locked note; got:\n%s", got)
		}
		if strings.Contains(got, "reply-input") || strings.Contains(got, `class="chips"`) {
			t.Errorf("rendered question group still carries live controls on a closed question; got:\n%s", got)
		}
	})

	t.Run("a non-interactive question with no answer text shows no locked note", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 2,
			Question: &ThreadQuestion{
				Key: "Q2", Title: "Resolved already", StateLabel: "resolved",
				BodyHTML: emptyBodyHTML, MessageCount: 1,
				Interactive: false, AnsweredText: "",
			},
		})
		if strings.Contains(got, "q-answered") {
			t.Errorf("rendered question group has a locked note with no answer text; got:\n%s", got)
		}
	})

	t.Run("an open question still renders its live reply box, no locked note", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 3,
			Question: &ThreadQuestion{
				Key: "Q3", Title: testSplitTicketTitle, StateLabel: testWaitingOnYou,
				BodyHTML: emptyBodyHTML, MessageCount: 1,
				Interactive: true,
			},
		})
		if !strings.Contains(got, "reply-input") {
			t.Errorf("rendered question group missing the live reply box for an open question; got:\n%s", got)
		}
		if strings.Contains(got, "q-answered") {
			t.Errorf("rendered question group has a locked note on an open question; got:\n%s", got)
		}
	})
}

// TestQuestionGroupShowsReviseNoteWhileStillRevisable proves D30: an
// answered question the owner can still revise (its ticket still waits on
// this round) keeps its live controls and shows reviseNote's own note, not
// bug 7's locked answeredLocked note -- the two are mutually exclusive.
func TestQuestionGroupShowsReviseNoteWhileStillRevisable(t *testing.T) {
	t.Parallel()

	t.Run("a revisable question keeps its chips and shows the revise note", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 1,
			Question: &ThreadQuestion{
				Key: "Q1", Title: testApprovePlanTitle, StateLabel: "answered · can change",
				BodyHTML: emptyBodyHTML, MessageCount: 1,
				Options:     []ThreadOption{{Key: "a", Text: "Approve"}, {Key: "b", Text: "Reject"}},
				Interactive: true, Revisable: true, DraftOption: "a",
			},
		})
		if !strings.Contains(got, `<p class="q-revisable">Answered. You can change this until Zing resumes the agent.</p>`) {
			t.Errorf("rendered question group missing the revise note; got:\n%s", got)
		}
		if !strings.Contains(got, `class="chips"`) {
			t.Errorf("rendered question group missing its live chips while still revisable; got:\n%s", got)
		}
		if strings.Contains(got, "q-answered") {
			t.Errorf("rendered question group carries the locked note while still revisable; got:\n%s", got)
		}
	})

	t.Run("an ordinary open question shows no revise note", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 2,
			Question: &ThreadQuestion{
				Key: "Q2", Title: testSplitTicketTitle, StateLabel: testWaitingOnYou,
				BodyHTML: emptyBodyHTML, MessageCount: 1,
				Interactive: true, Revisable: false,
			},
		})
		if strings.Contains(got, "q-revisable") {
			t.Errorf("rendered question group has a revise note on a never-answered open question; got:\n%s", got)
		}
	})
}

// TestThreadShowsWaitProgress proves the bug fix for a silent partial-batch
// wait: Thread renders waitProgressBanner's own line only when wait.Total >
// 0, with the exact "Answered X of Y." count views.go's buildWaitProgress
// computed, and renders nothing extra when the ticket is not currently
// question-blocked.
func TestThreadShowsWaitProgress(t *testing.T) {
	t.Parallel()
	ticket := &store.Ticket{ID: 1, Title: "Add a hello endpoint"}

	t.Run("a blocked ticket shows the progress line", func(t *testing.T) {
		t.Parallel()
		var sb strings.Builder
		if err := Thread(ticket, nil, WaitProgress{Answered: 1, Total: 2}).Render(t.Context(), &sb); err != nil {
			t.Fatalf("Thread.Render: %v", err)
		}
		got := sb.String()
		want := `<p class="wait-progress">Answered 1 of 2. Zing resumes the agent when every question is answered.</p>`
		if !strings.Contains(got, want) {
			t.Errorf("rendered thread missing the wait progress line; want %q; got:\n%s", want, got)
		}
	})

	t.Run("an unblocked ticket shows no progress line", func(t *testing.T) {
		t.Parallel()
		var sb strings.Builder
		if err := Thread(ticket, nil, WaitProgress{}).Render(t.Context(), &sb); err != nil {
			t.Fatalf("Thread.Render: %v", err)
		}
		if strings.Contains(sb.String(), "wait-progress") {
			t.Errorf("rendered thread has a wait progress line for an unblocked ticket; got:\n%s", sb.String())
		}
	})
}

// TestQuestionGroupRendersSentRepliesUnderItsOptions proves bug fix 10: a
// question's own sent replies and answers (views.go's buildThreadRows, now
// folded into SentReplies instead of their own standalone ThreadRow) render
// inside this question's own <details> block, under its options, each
// prefixed "You: " -- not as a separate, detached message card. A question
// with no sent replies renders no ".q-sent-replies" region at all.
func TestQuestionGroupRendersSentRepliesUnderItsOptions(t *testing.T) {
	t.Parallel()

	t.Run("sent replies and answers render in order, each prefixed You:", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 1,
			Question: &ThreadQuestion{
				Key: "Q1", Title: testApprovePlanTitle, StateLabel: testWaitingOnYou,
				BodyHTML: emptyBodyHTML, MessageCount: 3, Interactive: true,
				SentReplies: []string{"Explain these three options in more detail", "Keep it simple"},
			},
		})
		optionsIdx := strings.Index(got, `class="reply"`)
		repliesIdx := strings.Index(got, `class="q-sent-replies"`)
		if optionsIdx < 0 || repliesIdx < 0 || repliesIdx < optionsIdx {
			t.Errorf("rendered question group does not show sent replies under its options; got:\n%s", got)
		}
		for _, want := range []string{
			`<p class="q-sent-reply">You: Explain these three options in more detail</p>`,
			`<p class="q-sent-reply">You: Keep it simple</p>`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("rendered question group missing %q; got:\n%s", want, got)
			}
		}
	})

	t.Run("no sent replies renders no sent-replies region", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 2,
			Question: &ThreadQuestion{
				Key: "Q2", Title: testSplitTicketTitle, StateLabel: testWaitingOnYou,
				BodyHTML: emptyBodyHTML, MessageCount: 1, Interactive: true,
			},
		})
		if strings.Contains(got, "q-sent-replies") {
			t.Errorf("rendered question group has a sent-replies region with nothing sent; got:\n%s", got)
		}
	})
}
