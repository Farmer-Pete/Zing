package templates

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/a-h/templ"
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
				Key: "Q2", Title: "Approve the plan?",
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
				Key: "Q3", Title: "Split this ticket?",
				StateLabel: testWaitingOnYou, BodyHTML: emptyBodyHTML, MessageCount: 1,
			},
		})
		if strings.Contains(got, `<span class="q-count">1</span>`) {
			t.Errorf("rendered question group still carries the bare, unlabeled count; got:\n%s", got)
		}
	})
}
