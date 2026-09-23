package console_test

import (
	"context"
	"strings"
	"testing"

	"zing/internal/console"
)

// renderToString drives console.Render's returned templ.Component the same
// way a handler does (design section 6.2: "Handlers render ... with
// component.Render(ctx, w)"), so every test below exercises the real
// boundary, not an internal shortcut.
func renderToString(t *testing.T, md string) string {
	t.Helper()
	comp, err := console.Render(md)
	if err != nil {
		t.Fatalf("console.Render(%q): %v", md, err)
	}
	if comp == nil {
		t.Fatalf("console.Render(%q): returned a nil templ.Component", md)
	}
	var buf strings.Builder
	if err := comp.Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render(): %v", err)
	}
	return buf.String()
}

func TestRenderMarkdown(t *testing.T) {
	got := renderToString(t, "hello **world**")
	if !strings.Contains(got, "<strong>world</strong>") {
		t.Errorf("Render(%q) = %q, want it to contain <strong>world</strong>", "hello **world**", got)
	}
	if !strings.Contains(got, "<p>") {
		t.Errorf("Render(%q) = %q, want a <p> wrapper", "hello **world**", got)
	}
}

func TestRenderReturnsTemplComponent(t *testing.T) {
	// console.Render's signature already guarantees a templ.Component at
	// compile time; what this test proves is that the value is a real,
	// usable one, not a nil interface wrapping nothing.
	comp, err := console.Render("plain text")
	if err != nil {
		t.Fatalf("console.Render: %v", err)
	}
	if comp == nil {
		t.Fatal("console.Render returned a nil templ.Component")
	}
	var buf strings.Builder
	if err := comp.Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render(): %v", err)
	}
}

// TestRenderMermaidFenceBecomesClientSideBlock proves a ```mermaid fence
// renders as a client-side <pre class="mermaid"> block (design section
// 6.10) and, load-bearingly, that goldmark-diagram's own document-level
// script injection never fires (see render.go's diagramExtension doc
// comment for why the stock NewMermaidClientRenderer cannot be used
// directly): the design requires the classic <script src="/static/
// mermaid.js"> in shell.templ's head to be the only mermaid load.
func TestRenderMermaidFenceBecomesClientSideBlock(t *testing.T) {
	md := "```mermaid\ngraph TD\nA-->B\n```"
	got := renderToString(t, md)

	if !strings.Contains(got, `<pre class="mermaid">`) {
		t.Errorf("Render(mermaid fence) = %q, want a <pre class=\"mermaid\"> block", got)
	}
	if strings.Contains(got, "<script") {
		t.Errorf("Render(mermaid fence) = %q, want no <script> tag (goldmark-diagram's own document-level injection must never fire)", got)
	}
	if !strings.Contains(got, "graph TD") {
		t.Errorf("Render(mermaid fence) = %q, want the fence source preserved inside the block", got)
	}
}

// TestRenderEscapesRawHTML proves raw HTML in the source never reaches the
// output as a live tag (design section 0: "Raw HTML in model output is
// escaped"; section 9: "goldmark escapes raw HTML (never html.WithUnsafe)").
// Without html.WithUnsafe, goldmark replaces raw HTML with a fixed comment
// rather than passing it through, so this also pins that specific, safe
// behavior against a future accidental html.WithUnsafe(( )) regression.
func TestRenderEscapesRawHTML(t *testing.T) {
	got := renderToString(t, "before\n\n<script>alert(1)</script>\n\nafter")

	if strings.Contains(got, "<script>alert(1)</script>") {
		t.Errorf("Render(raw <script>) = %q, want the raw tag never emitted live", got)
	}
	if !strings.Contains(got, "raw HTML omitted") {
		t.Errorf("Render(raw <script>) = %q, want goldmark's raw-HTML-omitted placeholder", got)
	}
}

// TestRenderNeutralizesDangerousLink proves a javascript: link never reaches
// the output as a live href (design section 9: "a dangerous link is
// neutralized").
func TestRenderNeutralizesDangerousLink(t *testing.T) {
	got := renderToString(t, "[click me](javascript:alert(1))")

	if strings.Contains(got, "javascript:") {
		t.Errorf("Render(javascript: link) = %q, want the dangerous scheme never emitted", got)
	}
	if !strings.Contains(got, `href=""`) {
		t.Errorf(`Render(javascript: link) = %q, want an emptied href=""`, got)
	}
}

// TestRenderPlantUMLNeverShellsOut proves a ```plantuml fence never reaches
// goldmark-diagram's server-side PlantUML renderer (design section 0: "Never
// use the default HTMLRenderer; it seeds a PlantUML server-side
// shell-out"). WithExcludeLanguages("plantuml") (render.go,
// diagramExtension) makes it fall through to goldmark's ordinary escaped
// fenced-code-block rendering instead, so this also proves the plantuml
// binary is never invoked: there is no SVG in the output, and no
// plantuml-error placeholder (the shape NewPlantUMLRenderer emits on a
// failed exec), only the escaped source as plain code.
func TestRenderPlantUMLNeverShellsOut(t *testing.T) {
	md := "```plantuml\n@startuml\nA -> B\n@enduml\n```"
	got := renderToString(t, md)

	if strings.Contains(got, "<svg") {
		t.Errorf("Render(plantuml fence) = %q, want no server-rendered SVG", got)
	}
	if strings.Contains(got, "plantuml-error") {
		t.Errorf("Render(plantuml fence) = %q, want no plantuml-error placeholder (that would mean an exec was attempted)", got)
	}
	if strings.Contains(got, `<pre class="mermaid">`) {
		t.Errorf("Render(plantuml fence) = %q, want it not treated as a mermaid block", got)
	}
	if !strings.Contains(got, "@startuml") {
		t.Errorf("Render(plantuml fence) = %q, want the escaped source preserved as plain code", got)
	}
}
