---
name: datastar
description: Use when writing HTML with data-* attributes, Datastar signals or expressions, server-sent event handlers that patch the DOM, Rocket web components, or any frontend behaviour.
---

# Datastar

Datastar is the hypermedia frontend for this repo. It has two halves. Client reactivity comes from `data-*` attributes on plain HTML, similar to Alpine. Server-driven updates come from the backend patching the DOM and signals over Server-Sent Events, similar to htmx. There is no virtual DOM and no build step for the core library.

The versions this repo targets are Datastar core v1.0.4 (the browser bundle, released 2026-09-21) and the Go SDK `github.com/starfederation/datastar-go` (latest tag v1.2.2, which tracks core v1.0.2). Check https://github.com/starfederation/datastar-go/tags for a newer SDK tag before assuming SDK and core are at the same version.

## Mental model

Signals are reactive client state. Reference a signal in an expression with a `$` prefix, for example `$count`. Create signals with `data-signals`, or implicitly the first time an attribute like `data-bind` needs one. A signal whose name starts with an underscore, for example `$_draft`, stays local and is not sent to the backend.

Expressions are JavaScript-like strings that live inside `data-*` attribute values. Datastar substitutes each `$name` for the signal value, then runs the result in a sandbox. See attributes.md for the exact rules, including the `el` and `evt` context variables and the semicolon statement separator.

The backend drives the frontend by patching. A handler streams `datastar-patch-elements` events that morph HTML into the DOM by element id, and `datastar-patch-signals` events that merge new signal values into client state. One open SSE connection can carry many patches over time. The Go SDK writes these frames for you.

## Go SDK, minimal handler

The SDK is router agnostic and takes a plain `http.ResponseWriter` and `*http.Request`.

```go
import (
	"fmt"
	"net/http"

	"github.com/starfederation/datastar-go/datastar"
)

func handler(w http.ResponseWriter, r *http.Request) {
	type Store struct {
		Count int `json:"count"`
	}
	store := &Store{}
	if err := datastar.ReadSignals(r, store); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	sse := datastar.NewSSE(w, r)
	store.Count++
	sse.PatchElements(fmt.Sprintf(`<div id="count">%d</div>`, store.Count))
	sse.MarshalAndPatchSignals(store)
}
```

For long-lived streams, clear the write deadline and watch the request context. See go-sdk.md for the full function list, options, and the SSE write-timeout gotcha.

## Serving the script

Self-host the bundle in production rather than depending on a CDN at runtime. Vendor the file into the repo and embed it with `//go:embed`, then serve it from a static route, so the served JS version is pinned in the repo next to the SDK version. Load it as a module in the page head.

```html
<script type="module" src="/static/datastar.js"></script>
```

The upstream file for v1.0.4 is https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.4/bundles/datastar.js (about 11.9 KiB). Always pin the exact `@vX.Y.Z` tag when fetching it; attribute and SSE syntax changed across pre-1.0 versions. Rocket ships as its own bundle, `datastar-rocket.js`, at the same tagged path, and that bundle already contains the core. A page that uses Rocket components loads `datastar-rocket.js` instead of `datastar.js`, not in addition to it.

## Rules of thumb

Pick the attribute that names the job. Use `data-text` to set text, `data-show` to toggle visibility, `data-bind` on form inputs, `data-on` for events, `data-computed` for read-only derived values, `data-effect` for side effects on signal change, and `data-indicator` for loading state. attributes.md has the full set.

Reach for Rocket when you need a reusable, encapsulated custom element with a typed prop API and isolated per-instance state, for example a `<demo-counter>` dropped into many pages. Use plain `data-*` attributes for page-specific interactivity where state is naturally shared across the page. Client-side loops and conditionals (`data-for`, `data-if`) exist only inside Rocket components; with plain attributes, the server renders lists and branches and patches them in. See rocket.md.

Avoid these traps, each of which has a correct target.

- Use `data-init` for run-on-load behaviour. The old `data-on-load` name was removed. LLM training data often still emits `data-on-load`; do not.
- Use the colon delimiter for multi-part attributes, for example `data-on:click` and `data-signals:foo`. The hyphen form `data-on-click` is pre-1.0 and no longer works.
- Emit `datastar-patch-elements` and `datastar-patch-signals` from handlers. The old `datastar-merge-fragments` and `datastar-merge-signals` events and the `mergeMode` field are pre-1.0. The Go SDK already uses the current names.
- Separate multiple statements in one expression with semicolons, for example `$open = true; @post('/save')`. A line break alone is not a statement separator.
- Set `style="display: none"` inline on any element that starts hidden under `data-show`, so it does not flash before Datastar processes the attribute.
- To drive one property, use either the dedicated attribute (`data-class`, `data-style`) or the generic `data-attr`, not both on the same property of the same element. Combining them breaks rendering (issue #1019).
- Ten attributes are Datastar Pro, a paid license, and are not in the free core: `data-animate`, `data-custom-validity`, `data-match-media`, `data-on-raf`, `data-on-resize`, `data-persist`, `data-query-string`, `data-replace-url`, `data-scroll-into-view`, `data-view-transition`. Before scaffolding any of these, flag the license requirement to the user rather than assuming they are available.

## Sibling reference files

- attributes.md. Open when writing or reviewing any `data-*` attribute, an `@`-prefixed action, an expression, or the raw SSE wire format. It lists every core attribute with syntax, modifiers, and gotchas, the Pro attributes, the full modifier catalogue, the actions, and the SSE event formats.
- rocket.md. Open when building a reusable web component or when you see a hyphenated custom element tag in the markup.
- go-sdk.md. Open when writing a Go handler that reads signals or streams patches, or when choosing a templating library.
