package templates

import (
	"strings"
	"testing"
)

// renderItemRow renders itemRow(1, 2, item, itemDecisionsPerimeter) to a
// string, failing the test on a render error.
func renderItemRow(t *testing.T, item ThreadItem) string {
	t.Helper()
	var sb strings.Builder
	if err := itemRow(1, 2, item, itemDecisionsPerimeter).Render(t.Context(), &sb); err != nil {
		t.Fatalf("itemRow.Render: %v", err)
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
