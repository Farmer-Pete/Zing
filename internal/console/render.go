// render.go renders model- and user-authored markdown (message bodies, plan
// prose) to safe HTML (design section 6.10). One goldmark parser and one
// goldmark renderer are built at package init, both package-level vars, not
// per-call: parsing and rendering markdown needs no per-request state, so
// building them once is both the obvious reading and the cheap one.
//
// Red-team constraints (design section 0, "Dependency set"), load-bearing:
//
//   - html.WithUnsafe is never passed. Raw HTML in the source renders as the
//     literal comment goldmark's own default emits ("<!-- raw HTML omitted
//     -->"), never as a live tag.
//   - The mermaid renderer registered below is client-side only: it turns a
//     ```mermaid fence into a `<pre class="mermaid">` block and nothing else.
//     NewMermaidServerRenderer and NewPlantUMLRenderer are never called
//     anywhere in this file.
//   - plantuml fences are excluded from diagram rendering (WithExcludeLanguages),
//     so they fall through to goldmark's ordinary escaped fenced-code-block
//     rendering. The diagram package's own default HTMLRenderer var is never
//     used, because its default renderer set includes the PlantUML
//     server-side shell-out; this file builds its own renderer via
//     NewHTMLRenderer with an explicit, minimal option set instead.
package console

import (
	"bytes"
	"fmt"

	"github.com/a-h/templ"
	diagram "github.com/yuin/goldmark-diagram"
	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/util"
)

// markdownParser and markdownRenderer are the one goldmark.Markdown
// equivalent the design calls for. goldmark v2 removed the v1
// goldmark.New/goldmark.Markdown convenience entirely (its own
// migrate-goldmark-v1-to-v2 skill, bundled in the module, names this as the
// first breaking change and gives this exact two-piece replacement: a
// parser.Parser and a renderer.Renderer built and composed separately, then
// driven by hand with Parse then Render). Building both here, as
// package-level vars rather than inside a function, is "at startup" without
// a gochecknoinits-flagged init() (golang skill, .golangci.yml): there is no
// error path to check, so a var initializer is the plain reading.
// diagramExtension is the goldmark-diagram extension built with the exact
// option set the red-team constraints require (design section 0, 6.10):
// plantuml excluded, and mermaid registered through renderMermaidBlock
// rather than the package's own NewMermaidClientRenderer.
//
// That substitution is deliberate, not a shortcut: goldmark-diagram v1.1.0's
// NewMermaidClientRenderer unconditionally marks the render context with the
// mermaid module URL it was given (even the default one, when no option is
// passed), and NewHTMLRenderer's document-level decorator reads that mark to
// append a <script type="module"> (or, with WithMermaidUMDURL, a <script
// src=...> plus an inline mermaid.initialize call) once at the end of the
// document. There is no option to register the client renderer without
// that side effect. The design requires exactly one mermaid load, already
// wired as the classic, non-module <script src="/static/mermaid.js"> in
// shell.templ's head (section 6.10, the v10 change log), so this second,
// library-injected load must never fire. renderMermaidBlock reproduces
// NewMermaidClientRenderer's own HTML emission byte-for-byte (the escaped
// fence body inside <pre class="mermaid">) but never calls the
// renderer.Context.Set that arms the decorator, using the package's own
// documented extension point for this instead (WithRenderer: "the extension
// point that allows ... replacing the rendering strategy of an existing
// one").
var diagramExtension = diagram.NewHTMLRenderer(
	diagram.WithExcludeLanguages(diagram.LanguagePlantUML),
	diagram.WithRenderer(diagram.LanguageMermaid, diagram.RendererFunc(renderMermaidBlock)),
)

var (
	markdownParser   = parser.New(parser.WithExtensions(parser.CommonMark))
	markdownRenderer = html.New(html.WithExtensions(diagramExtension))
)

// renderMermaidBlock renders one ```mermaid fence as a client-side
// <pre class="mermaid"> block, matching goldmark-diagram's own
// NewMermaidClientRenderer output, but see diagramExtension for why it is
// a separate, hand-written renderer rather than that one.
func renderMermaidBlock(w util.BufWriter, source []byte, n *ast.CodeBlock, rc renderer.Context) error {
	if _, err := w.WriteString(`<pre class="mermaid">`); err != nil {
		return fmt.Errorf("console: render mermaid block: %w", err)
	}
	if _, err := n.Value.WriteTo(html.ContextTextWriter(rc), source); err != nil {
		return fmt.Errorf("console: render mermaid block: %w", err)
	}
	if _, err := w.WriteString("</pre>\n"); err != nil {
		return fmt.Errorf("console: render mermaid block: %w", err)
	}
	return nil
}

// Render renders md (a message body, or plan prose) to HTML through the
// shared parser and renderer above, and wraps the result with templ.Raw:
// the one audited boundary (design section 6.10) where already-escaped
// goldmark output is trusted verbatim, so no caller needs its own
// html/template import or its own escaping judgment call.
func Render(md string) (templ.Component, error) {
	source := util.StringToReadOnlyBytes(md)
	doc := markdownParser.Parse(source)

	var buf bytes.Buffer
	if err := markdownRenderer.Render(&buf, source, doc); err != nil {
		return nil, fmt.Errorf("console: render markdown: %w", err)
	}
	return templ.Raw(buf.String()), nil
}
