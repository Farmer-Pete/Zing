package templates

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"zing/internal/response"
	"zing/internal/store"
)

// renderItemRow renders itemRow(1, 2, item, itemDecisionsPerimeter, "",
// true) to a string, failing the test on a render error.
func renderItemRow(t *testing.T, item ThreadItem) string {
	t.Helper()
	var sb strings.Builder
	if err := itemRow(1, 2, item, itemDecisionsPerimeter, "", true).Render(t.Context(), &sb); err != nil {
		t.Fatalf("itemRow.Render: %v", err)
	}
	return sb.String()
}

// textComponent is a minimal templ.Component test stand-in that renders s
// verbatim, used for ThreadQuestion.AnsweredHTML, SettledHTML, and
// Turn.BodyHTML fixtures: this file's tests assert on these fields'
// rendered text, not on exercising Render's own markdown path (render_test.go
// already covers that).
func textComponent(s string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	})
}

// threadOption builds a ThreadOption test fixture with TextHTML set to a
// textComponent of text (console.RenderInline's own job in real code):
// optionChips always renders TextHTML, so a fixture that leaves it nil
// panics on render.
func threadOption(key, text string) ThreadOption {
	return ThreadOption{Key: key, Text: text, TextHTML: textComponent(text)}
}

// testWaitingOnYou is the StateLabel string.go's questionStateLabel gives
// an open question (views.go), repeated three or more times below
// (goconst).
const testWaitingOnYou = "waiting on you"

// testYouAuthor is Turn.Author for an owner turn (bug fix, design section
// 22.7), repeated across this file's fixtures (goconst).
const testYouAuthor = "You"

// testSettledStateLabel is a settled question's StateLabel, repeated across
// this file's fixtures (goconst).
const testSettledStateLabel = "settled"

// testApprovePlanTitle, testSplitTicketTitle, and testVersionQuestionTitle
// are question titles repeated across this file's fixtures (goconst),
// named once rather than retyped.
const (
	testApprovePlanTitle     = "Approve the plan?"
	testSplitTicketTitle     = "Split this ticket?"
	testVersionQuestionTitle = "Where does the version string come from?"
)

// testHelloTicketTitle is a ticket title repeated across this package's
// fixtures (goconst).
const testHelloTicketTitle = "Add a hello endpoint"

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

// detailsOpenTag returns the opening <details ...> tag (up to and including
// its closing >) from a rendered questionGroup, split into whitespace
// tokens, so a test can check for an exact attribute token (e.g. "open" or
// `id="question-1"`) without a substring match wrongly hitting
// data-preserve-attr="open".
func detailsOpenTag(t *testing.T, got string) []string {
	t.Helper()
	end := strings.Index(got, ">")
	if end < 0 {
		t.Fatalf("rendered question group has no closing > on its opening tag; got:\n%s", got)
	}
	return strings.Fields(got[:end])
}

// TestQuestionGroupKeepsOpenStateAcrossPatches proves the bug fix: a live
// patch re-renders #main from the server's HTML on every bus wake
// (stream.go), and Datastar's morph strips any attribute the new HTML
// lacks unless that HTML's data-preserve-attr names it (datastar.js:9).
// questionGroup's <details> never carried data-preserve-attr="open", so an
// owner-expanded question (or a gate's chips) collapsed on the next frame.
// This proves every question's <details> carries data-preserve-attr="open"
// and a stable id (so the morph pairs it by id, not position), and that an
// Interactive question (row.Question.Interactive, true for every question
// still waiting on the owner, gates included) renders open from the start.
func TestQuestionGroupKeepsOpenStateAcrossPatches(t *testing.T) {
	t.Parallel()

	t.Run("an interactive question renders open, preserved, and identified", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 1,
			Question: &ThreadQuestion{
				Key: "Q1", Title: testVersionQuestionTitle, StateLabel: testWaitingOnYou,
				BodyHTML: emptyBodyHTML, Options: []ThreadOption{threadOption("a", "ReadBuildInfo only")},
				Interactive: true,
			},
		})
		tag := detailsOpenTag(t, got)
		for _, want := range []string{"open", `data-preserve-attr="open"`, `id="question-1"`} {
			if !slices.Contains(tag, want) {
				t.Errorf("rendered <details> tag missing token %q; got tag:\n%s", want, strings.Join(tag, " "))
			}
		}
	})

	t.Run("a non-interactive question renders no open token, but stays preserved and identified", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 1,
			Question: &ThreadQuestion{
				Key: "Q1", Title: testVersionQuestionTitle, StateLabel: testSettledStateLabel,
				BodyHTML: emptyBodyHTML, Options: []ThreadOption{threadOption("a", "ReadBuildInfo only")},
				Interactive: false,
			},
		})
		tag := detailsOpenTag(t, got)
		if slices.Contains(tag, "open") {
			t.Errorf("rendered <details> tag carries a bare open token on a non-interactive question; got tag:\n%s", strings.Join(tag, " "))
		}
		for _, want := range []string{`data-preserve-attr="open"`, `id="question-1"`} {
			if !slices.Contains(tag, want) {
				t.Errorf("rendered <details> tag missing token %q; got tag:\n%s", want, strings.Join(tag, " "))
			}
		}
	})

	t.Run("each question's id names its own row, so the morph pairs them apart", func(t *testing.T) {
		t.Parallel()
		for _, id := range []int64{1, 2} {
			got := renderQuestionGroup(t, ThreadRow{
				ID: id,
				Question: &ThreadQuestion{
					Key: "Q1", Title: testVersionQuestionTitle, StateLabel: testWaitingOnYou,
					BodyHTML: emptyBodyHTML, Options: []ThreadOption{threadOption("a", "ReadBuildInfo only")},
					Interactive: true,
				},
			})
			tag := detailsOpenTag(t, got)
			want := fmt.Sprintf(`id="question-%d"`, id)
			if !slices.Contains(tag, want) {
				t.Errorf("rendered <details> tag missing token %q; got tag:\n%s", want, strings.Join(tag, " "))
			}
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
				Key: "Q1", Title: testVersionQuestionTitle,
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
	if err := freeReply(1, 2, "", defaultReplyPlaceholder).Render(t.Context(), &sb); err != nil {
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
	if err := freeReply(1, 2, "already saved text", defaultReplyPlaceholder).Render(t.Context(), &sb); err != nil {
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
// question with AnsweredHTML set renders answeredLocked's note instead of
// freeReply, and a non-interactive question with no AnsweredHTML (nothing
// decoded, or a reply-only question) renders no locked note at all.
func TestQuestionGroupLocksAnAnsweredQuestion(t *testing.T) {
	t.Parallel()

	t.Run("an answered question shows its locked note, not a reply box", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 1,
			Question: &ThreadQuestion{
				Key: "Q1", Title: testApprovePlanTitle, StateLabel: "answered",
				BodyHTML: emptyBodyHTML, MessageCount: 1,
				Interactive: false, AnsweredHTML: textComponent("a"),
			},
		})
		if !strings.Contains(got, `<div class="q-answered">`) || !strings.Contains(got, "Answered:") || !strings.Contains(got, ">a<") {
			t.Errorf("rendered question group missing the locked note; got:\n%s", got)
		}
		if strings.Contains(got, "reply-input") {
			t.Errorf("rendered question group still carries a live reply box on a closed question; got:\n%s", got)
		}
	})

	t.Run("a non-interactive question with no answer html shows no locked note", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 2,
			Question: &ThreadQuestion{
				Key: "Q2", Title: "Resolved already", StateLabel: "resolved",
				BodyHTML: emptyBodyHTML, MessageCount: 1,
				Interactive: false,
			},
		})
		if strings.Contains(got, "q-answered") {
			t.Errorf("rendered question group has a locked note with no answer html; got:\n%s", got)
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

// TestLockedQuestionShowsOptions proves the bug fix for "options vanish
// once locked" (design section 22.7 item 4, the owner's locked-view
// complaint): a settled or answered question -- Interactive false -- still
// renders its numbered option chips and item-decision rows, disabled and
// marked picked for whatever was actually sent, instead of nothing at all.
func TestLockedQuestionShowsOptions(t *testing.T) {
	t.Parallel()

	t.Run("a locked option-kind question still shows its chips, picked and disabled", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 1,
			Question: &ThreadQuestion{
				Key: "Q1", Title: testApprovePlanTitle, StateLabel: testSettledStateLabel,
				BodyHTML: emptyBodyHTML, MessageCount: 1,
				Options:      []ThreadOption{threadOption("a", "Approve"), threadOption("b", "Reject")},
				Interactive:  false,
				PickedOption: "b",
			},
		})
		if !strings.Contains(got, `class="chips"`) {
			t.Errorf("rendered question group lost its chips once locked; got:\n%s", got)
		}
		if !strings.Contains(got, `class="chip picked locked"`) || !strings.Contains(got, "2. Reject") {
			t.Errorf("rendered question group does not mark the sent option picked; got:\n%s", got)
		}
		if strings.Contains(got, "data-on:click") {
			t.Errorf("rendered question group's locked chips still carry a click handler; got:\n%s", got)
		}
	})

	t.Run("a locked item-kind question still shows its rows, picked and disabled", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 2,
			Question: &ThreadQuestion{
				Key: "Q2", Title: "Review this change", Kind: "review", StateLabel: "resolved",
				BodyHTML: emptyBodyHTML, MessageCount: 1,
				Items:       []ThreadItem{{Ref: "a.go", Text: "a change"}},
				Interactive: false,
				PickedItems: map[string]response.Decision{"a.go": response.DecisionAccept},
			},
		})
		if !strings.Contains(got, `class="decision picked locked"`) {
			t.Errorf("rendered question group does not mark the sent decision picked; got:\n%s", got)
		}
		if strings.Contains(got, "data-on:click") {
			t.Errorf("rendered question group's locked item row still carries a click handler; got:\n%s", got)
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
				Options:     []ThreadOption{threadOption("a", "Approve"), threadOption("b", "Reject")},
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
	ticket := &store.Ticket{ID: 1, Title: testHelloTicketTitle}

	t.Run("a blocked ticket shows the progress line", func(t *testing.T) {
		t.Parallel()
		var sb strings.Builder
		if err := Thread(ticket, nil, WaitProgress{Answered: 1, Total: 2}, "", TicketActions{}).Render(t.Context(), &sb); err != nil {
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
		if err := Thread(ticket, nil, WaitProgress{}, "", TicketActions{}).Render(t.Context(), &sb); err != nil {
			t.Fatalf("Thread.Render: %v", err)
		}
		if strings.Contains(sb.String(), "wait-progress") {
			t.Errorf("rendered thread has a wait progress line for an unblocked ticket; got:\n%s", sb.String())
		}
	})
}

// TestQuestionGroupRendersSentRepliesUnderItsOptions proves bug fix 10: a
// question's own sent replies and answers (views.go's buildThreadRows, now
// folded into Turns instead of their own standalone ThreadRow) render
// inside this question's own <details> block, under its options, each
// labeled "You:" -- not as a separate, detached message card. A question
// with no turns renders no ".q-turns" region at all.
func TestQuestionGroupRendersSentRepliesUnderItsOptions(t *testing.T) {
	t.Parallel()

	t.Run("sent replies and answers render in order, each labeled You:", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 1,
			Question: &ThreadQuestion{
				Key: "Q1", Title: testApprovePlanTitle, StateLabel: testWaitingOnYou,
				BodyHTML: emptyBodyHTML, MessageCount: 3, Interactive: true,
				Turns: []Turn{
					{Author: testYouAuthor, BodyHTML: textComponent("Explain these three options in more detail")},
					{Author: testYouAuthor, BodyHTML: textComponent("Keep it simple")},
				},
			},
		})
		optionsIdx := strings.Index(got, `class="reply"`)
		repliesIdx := strings.Index(got, `class="q-turns"`)
		if optionsIdx < 0 || repliesIdx < 0 || repliesIdx < optionsIdx {
			t.Errorf("rendered question group does not show its turns under its options; got:\n%s", got)
		}
		for _, want := range []string{
			"You:</span>Explain these three options in more detail",
			"You:</span>Keep it simple",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("rendered question group missing %q; got:\n%s", want, got)
			}
		}
	})

	t.Run("no turns renders no turns region", func(t *testing.T) {
		t.Parallel()
		got := renderQuestionGroup(t, ThreadRow{
			ID: 2,
			Question: &ThreadQuestion{
				Key: "Q2", Title: testSplitTicketTitle, StateLabel: testWaitingOnYou,
				BodyHTML: emptyBodyHTML, MessageCount: 1, Interactive: true,
			},
		})
		if strings.Contains(got, "q-turns") {
			t.Errorf("rendered question group has a turns region with nothing sent; got:\n%s", got)
		}
	})
}

// TestSettledThreadShowsDecisionAndLocks proves design section 22.7 item 6:
// a settled planning question renders its agent's decision as "Settled by
// <agent>:", with its options locked (no click handler) and no reply box.
func TestSettledThreadShowsDecisionAndLocks(t *testing.T) {
	t.Parallel()
	got := renderQuestionGroup(t, ThreadRow{
		ID: 1,
		Question: &ThreadQuestion{
			Key: "Q1", Title: testVersionQuestionTitle, StateLabel: testSettledStateLabel,
			BodyHTML: emptyBodyHTML, MessageCount: 2,
			Options:      []ThreadOption{threadOption("a", "ReadBuildInfo only")},
			Interactive:  false,
			PickedOption: "a",
			SettledLabel: "Settled by Fable",
			SettledHTML:  textComponent("Agreed, no ldflags."),
		},
	})
	if !strings.Contains(got, `class="q-settled"`) || !strings.Contains(got, "Settled by Fable:") || !strings.Contains(got, "Agreed, no ldflags.") {
		t.Errorf("rendered question group missing its settled decision; got:\n%s", got)
	}
	if strings.Contains(got, "reply-input") {
		t.Errorf("rendered question group still carries a reply box once settled; got:\n%s", got)
	}
	if strings.Contains(got, "data-on:click") {
		t.Errorf("rendered question group's chips still carry a click handler once settled; got:\n%s", got)
	}
	if !strings.Contains(got, `class="chip picked locked"`) {
		t.Errorf("rendered question group lost its picked chip once settled; got:\n%s", got)
	}
}

// TestOpenThreadKeepsReplyBox proves the other half of item 6: an open
// (unsettled) planning question keeps its clickable chips and its reply
// box, and shows no settled line.
func TestOpenThreadKeepsReplyBox(t *testing.T) {
	t.Parallel()
	got := renderQuestionGroup(t, ThreadRow{
		ID: 1,
		Question: &ThreadQuestion{
			Key: "Q1", Title: testVersionQuestionTitle, StateLabel: "your turn",
			BodyHTML: emptyBodyHTML, MessageCount: 1,
			Options:     []ThreadOption{threadOption("a", "ReadBuildInfo only")},
			Interactive: true,
		},
	})
	if !strings.Contains(got, "reply-input") {
		t.Errorf("rendered question group missing its reply box while open; got:\n%s", got)
	}
	if !strings.Contains(got, "data-on:click") {
		t.Errorf("rendered question group's chips are not clickable while open; got:\n%s", got)
	}
	if strings.Contains(got, "q-settled") {
		t.Errorf("rendered question group shows a settled line while still open; got:\n%s", got)
	}
}

// TestLockedQuestionHasNoDuplicateAnswerLine proves the owner's
// locked-view complaint: a locked, non-planning question's own sent answer
// shows exactly once (Answered:), never also as a turn -- the duplicate
// bug ("answeredLocked prints the sent answer, and sentReplies prints it
// again"), fixed by views.go's buildThreadQuestion never adding an answer
// row to Turns outside a planning conversation.
func TestLockedQuestionHasNoDuplicateAnswerLine(t *testing.T) {
	t.Parallel()
	got := renderQuestionGroup(t, ThreadRow{
		ID: 1,
		Question: &ThreadQuestion{
			Key: "Q1", Title: testApprovePlanTitle, StateLabel: "answered",
			BodyHTML: emptyBodyHTML, MessageCount: 1,
			Interactive: false, AnsweredHTML: textComponent("hello, world"),
		},
	})
	if n := strings.Count(got, "hello, world"); n != 1 {
		t.Errorf("rendered question group shows the sent answer %d times, want 1; got:\n%s", n, got)
	}
}

// TestRecommendedKeyMapsToChipNumber proves design section 22.7 item 3, the
// owner's locked-view complaint "Recommended: a:" not matching the option
// numbers: a recommendation whose text opens with a known option's key
// maps to that option's own chip number and text, and a recommendation
// that does not open with a key, or names no option, renders unchanged.
func TestRecommendedKeyMapsToChipNumber(t *testing.T) {
	t.Parallel()
	options := []ThreadOption{{Key: "a", Text: "ReadBuildInfo only"}, {Key: "b", Text: "zing plus version"}}

	for _, tc := range []struct {
		name, text, want string
	}{
		{"a bare key maps to its chip number and text", "a", "1. ReadBuildInfo only."},
		{"a colon-separated key keeps the rest as markdown", "b: zing plus version", "2. zing plus version. zing plus version"},
		{"a key the options do not name renders unchanged", "c: unknown", "c: unknown"},
		{"text with no leading key renders unchanged", "ReadBuildInfo seems simplest", "ReadBuildInfo seems simplest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RecommendedDisplayText(tc.text, options); got != tc.want {
				t.Errorf("RecommendedDisplayText(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestTurnsRenderMarkdown proves the owner's locked-view complaint "raw
// backticks in option chips and the Feed": a turn's own body is rendered
// through this package's templ.Component contract (views.go's buildTurns
// calls console.Render, the markdown path), so questionGroup only ever
// writes out whatever that component renders -- never the raw markdown
// string -- for every turn, regardless of kind.
func TestTurnsRenderMarkdown(t *testing.T) {
	t.Parallel()
	got := renderQuestionGroup(t, ThreadRow{
		ID: 1,
		Question: &ThreadQuestion{
			Key: "Q1", Title: testApprovePlanTitle, StateLabel: testWaitingOnYou,
			BodyHTML: emptyBodyHTML, MessageCount: 2, Interactive: true,
			Turns: []Turn{{Author: testYouAuthor, BodyHTML: textComponent("<code>zing version</code>")}},
		},
	})
	if !strings.Contains(got, "<code>zing version</code>") {
		t.Errorf("rendered question group lost the turn's own rendered markdown; got:\n%s", got)
	}
}

// TestNoReviseNoteOnPlanningQuestion proves a planning question never shows
// D30's reviseNote ("Answered. You can change this..."): Revisable is
// always false for a planning question (views.go's buildThreadQuestion),
// since D31 never sets state=answered (section 22.3) -- a planning
// question is only ever open or settled, with its own pill and settled
// line in place of D30's answered/revisable rendering.
func TestNoReviseNoteOnPlanningQuestion(t *testing.T) {
	t.Parallel()
	got := renderQuestionGroup(t, ThreadRow{
		ID: 1,
		Question: &ThreadQuestion{
			Key: "Q1", Title: testApprovePlanTitle, StateLabel: "your turn",
			BodyHTML: emptyBodyHTML, MessageCount: 1,
			Interactive: true, Revisable: false,
		},
	})
	if strings.Contains(got, "q-revisable") {
		t.Errorf("rendered question group shows the revise note on a planning question; got:\n%s", got)
	}
}

// TestThreadRendersSandboxRunBox proves Thread renders sandboxRunBox after
// the rows, for the open ticket's id, and renders nothing of it when there
// is no open ticket (split from #73: the console action's own form).
func TestThreadRendersSandboxRunBox(t *testing.T) {
	t.Parallel()

	t.Run("an open ticket renders the sandbox run box for its own id", func(t *testing.T) {
		t.Parallel()
		ticket := &store.Ticket{ID: 7, Title: testHelloTicketTitle}
		var sb strings.Builder
		if err := Thread(ticket, nil, WaitProgress{}, "", TicketActions{}).Render(t.Context(), &sb); err != nil {
			t.Fatalf("Thread.Render: %v", err)
		}
		got := sb.String()
		for _, want := range []string{
			`class="sandbox-run-box"`,
			`data-sandbox-run-ticket="7"`,
			`class="sandbox-run-cmd"`,
			`<button type="submit">Run</button>`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("rendered thread missing %q; got:\n%s", want, got)
			}
		}
	})

	t.Run("no open ticket renders no sandbox run box", func(t *testing.T) {
		t.Parallel()
		var sb strings.Builder
		if err := Thread(nil, nil, WaitProgress{}, "", TicketActions{}).Render(t.Context(), &sb); err != nil {
			t.Fatalf("Thread.Render: %v", err)
		}
		if got := sb.String(); strings.Contains(got, "sandbox-run") {
			t.Errorf("rendered thread has a sandbox run box with no open ticket; got:\n%s", got)
		}
	})
}
