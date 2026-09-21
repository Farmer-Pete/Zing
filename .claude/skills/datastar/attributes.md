# Datastar attributes, actions, expressions, and SSE wire format

This is the exhaustive reference. Every attribute, action, and event format below is from Datastar v1.0.4. Multi-part attribute names use a colon delimiter, for example `data-on:click` and `data-signals:foo`.

## Free core attributes

These are all part of the open-source core, no license needed. The Pro attributes are listed in their own section further down.

| Attribute | Purpose | Key syntax |
| --- | --- | --- |
| data-signals | Declare or patch reactive signals | `data-signals:name="value"` or `data-signals="{a: 1, b: {c: 2}}"` |
| data-computed | Read-only derived signal | `data-computed:name="$a + $b"` |
| data-bind | Two-way bind a form control to a signal | `data-bind:name` or `data-bind="name"` |
| data-ref | Signal holding an element reference | `data-ref:name` |
| data-text | Set element text from an expression | `data-text="$foo"` |
| data-show | Toggle visibility via CSS display | `data-show="$open"` |
| data-class | Toggle a class on truthiness | `data-class:font-bold="$strong"` |
| data-style | Set inline CSS properties reactively | `data-style:display="$hiding && 'none'"` |
| data-attr | Set any HTML attribute reactively | `data-attr:aria-label="$label"` |
| data-on | Attach an event listener | `data-on:click="$foo = ''"` |
| data-effect | Run a side effect on load and on change | `data-effect="$a = $b + $c"` |
| data-indicator | Boolean signal true while a fetch is in flight | `data-indicator:fetching` |
| data-init | Run an expression once when the element loads | `data-init="$count = 1"` |
| data-on-intersect | Run when the element enters or exits the viewport | `data-on-intersect="$seen = true"` |
| data-on-interval | Run on a timer | `data-on-interval="$count++"` |
| data-on-signal-patch | Run when any signal is patched | `data-on-signal-patch="$log.push(patch)"` |
| data-on-signal-patch-filter | Limit which signals trigger the patch handler | `data-on-signal-patch-filter="{include: /^counter$/}"` |
| data-json-signals | Render current signals as JSON text | `data-json-signals` |
| data-ignore | Exclude an element and its subtree from processing | `data-ignore` |
| data-ignore-morph | Skip an element's subtree during a morph | `data-ignore-morph` |
| data-preserve-attr | Keep named attributes across a morph | `data-preserve-attr="open"` |

### Signals and derived state

data-signals declares or patches signals. Single form `data-signals:signal-name="value"` sets one signal; object form `data-signals="{key: value, nested: {foo: 1}}"` sets several and supports nesting. Dot notation nests too, so `data-signals:form.baz="2"` creates `$form.baz`. Modifiers are `__case` to pick key casing and `__ifmissing` to set only when the signal does not already exist, which avoids clobbering state on reconnect or patch. Setting a signal to `null` or `undefined` removes it. Signals hold any JSON value, so arrays and nested objects work with normal literal syntax.

data-computed creates a read-only derived signal that recomputes whenever a referenced signal changes. Syntax `data-computed:name="expression"`. Modifier `__case`. A computed expression must be pure, no side effects and no assignment to other signals; use data-effect for those. You cannot assign to a computed signal.

data-bind two-way binds a form control's value to a signal, creating the signal if absent. Syntax `data-bind:signalName` or `data-bind="signalName"`. When the signal changes Datastar writes the value into the element; when the element fires its change or input event Datastar reads the value back. Predefined signal types are preserved, so a numeric input stays a number. A `<input type="file">` bind base64-encodes the files; the signal becomes an array of `{name, contents, mime}` objects. Modifiers are `__case`, `__prop` to bind a named DOM property, and `__event` to bind on a named event.

data-ref creates a non-reactive signal holding a reference to the element it is on, for DOM access rather than state. Syntax `data-ref:name`. Read it elsewhere as `$name`, for example `$box.offsetHeight`. Modifier `__case`.

data-json-signals renders a live JSON dump of signal state, usually into a `<pre>`. Bare form dumps all signals; `data-json-signals="{include: /regex/, exclude: /regex/}"` filters. Modifier `__terse` strips whitespace for compact output.

### Rendering and DOM

data-text sets text content from an expression and re-renders when a referenced signal changes. Syntax `data-text="expression"`.

data-show toggles visibility using CSS display, not DOM removal. Syntax `data-show="expression"`. Set `style="display: none"` inline on any element that should start hidden, or it flashes visible until Datastar first processes the attribute.

data-class adds or removes a class on truthiness. Key form `data-class:class-name="expression"`, object form `data-class="{className: expression}"`. Modifier `__case`.

data-style sets inline CSS properties reactively and keeps them in sync. Syntax `data-style:property="expression"` or object form. A falsy value restores the original inline style or removes the property rather than writing the falsy value.

data-attr sets any HTML attribute reactively. Key form `data-attr:attribute-name="expression"`, object form `data-attr="{'key': expression}"` for several at once.

To drive `class` or `style`, choose either the dedicated attribute (data-class, data-style) or data-attr, never both on the same property of the same element; combining them breaks rendering (issue #1019).

### Events, lifecycle, and loading state

data-on attaches an event listener and runs an expression when the event fires. Syntax `data-on:event-name="expression"`, for example `data-on:click` or `data-on:keydown`. Inside the expression `evt` is the DOM event, so `evt.detail.value` works. Separate multiple statements with semicolons. Modifiers are described in the modifier catalogue below; data-on carries the largest set.

data-init runs an expression once when the element initializes into the DOM. Syntax `data-init="expression"`. Modifiers `__delay` and `__viewtransition`. This replaces the removed `data-on-load` attribute.

data-on-intersect runs when the element enters the viewport, using IntersectionObserver. Syntax `data-on-intersect="expr"`. Modifiers are `__once`, `__exit` (fire when the element exits the viewport instead of entering), `__half` (fire at 50 percent visible), `__full` (fire at 100 percent visible), `__threshold.25` and similar for a custom fraction, `__delay`, `__debounce`, `__throttle`, `__viewtransition`.

data-on-interval runs on a timer. Syntax `data-on-interval="expr"`. Modifier `__duration.500ms` or `__duration.1s` sets the period; append `.leading` to also fire immediately on load. Also supports `__viewtransition`. Default period is 1 second.

data-on-signal-patch runs whenever one or more signals are patched, client or server side. A `patch` variable inside the expression holds the change details. Pair it with data-on-signal-patch-filter, whose value is `{include: /regex/, exclude: /regex/}`, to limit which signal names trigger it. Modifiers `__debounce` and `__delay`. Note that `__debounce` here may miss some rapid changes (issue #1065).

data-effect runs an expression once on load and again whenever a referenced signal changes, for side effects rather than rendering. Syntax `data-effect="expression"`.

data-indicator creates a boolean signal that is true while a triggered backend action is in flight and false otherwise. Syntax `data-indicator:signalName`. Pair it with a spinner under `data-show="$fetching"`. Modifier `__case`.

### Morph control

data-ignore prevents Datastar from processing the element and, by default, its descendants. Modifier `__self` limits the ignore to just that element.

data-ignore-morph tells the morph algorithm to skip this element's subtree when a patch would otherwise re-morph it, preserving DOM managed by third-party widgets.

data-preserve-attr keeps named attributes unchanged across a morph even when the incoming HTML differs. Syntax `data-preserve-attr="name1 name2"`, for example `<details open data-preserve-attr="open">`.

## Pro attributes

These require a Datastar Pro license and are not in the free core. Flag the license requirement before using any of them.

| Attribute | Purpose |
| --- | --- |
| data-animate | Animate CSS properties |
| data-custom-validity | Set a custom form validation message from an expression |
| data-match-media | Sync a signal to a CSS media-query match |
| data-on-raf | Run an expression every requestAnimationFrame tick |
| data-on-resize | Run on element size change, via ResizeObserver |
| data-persist | Persist signals in localStorage, or sessionStorage with `__session` |
| data-query-string | Two-way sync signals with URL query params |
| data-replace-url | Replace the browser URL via history.replaceState |
| data-scroll-into-view | Scroll the element into view |
| data-view-transition | Set view-transition-name from an expression |

Do not confuse the Pro attribute data-view-transition, which names a transition target, with the free `__viewtransition` event modifier, which wraps an event handler's DOM update in `document.startViewTransition()`.

## Expressions

A Datastar expression is a JavaScript-like string inside a `data-*` value. Each `$name` token is substituted with that signal's current value, then the result runs inside a `Function()` sandbox, so it is not raw `eval` and global access is restricted. Standard operators work, including ternaries, `||`, `&&`, property access, arithmetic, and method calls.

Two context variables are always available. `el` is the element the attribute is on, for example `data-text="el.offsetHeight"`. `evt` is the triggering event, available only in event-handler expressions like data-on, for example `evt.detail.value`. This differs from legacy inline `onclick` handlers, where the implicit variable is named `event`.

Separate multiple statements with semicolons; a line break alone is not a separator. Keep most logic inline, since signals and actions only resolve inside Datastar expressions. Move genuinely complex logic into an external script or a Rocket component rather than writing large expressions.

## Casing and evaluation order

Hyphenated attribute names auto-convert to camelCase by removing the hyphen and uppercasing the next letter, so `data-signals:my-signal` creates `$mySignal` and `data-computed:foo-bar` creates `$fooBar`. Attribute keys that do not define a signal, such as a data-class target class name, default to kebab-case instead. The `__case` modifier overrides the convention per attribute and accepts `camel` (default), `kebab`, `snake`, or `pascal`. It applies to data-bind, data-class, data-computed, data-indicator, data-on, data-ref, and data-signals.

Datastar walks the DOM depth-first. On a single element, attributes apply in source order, so a data-signals that a later data-on on the same element depends on must appear first.

## Modifier catalogue

Chain modifiers by appending `__name` or `__name.arg` segments in any order, separated by double underscores, for example `data-on:click__debounce.300ms__prevent__viewtransition="@post('/save')"`. Arguments use single dots after the modifier name.

| Modifier | Effect | Applies to |
| --- | --- | --- |
| __once | Fire once, then detach | data-on, data-on-intersect |
| __exit | Fire when the element exits the viewport instead of entering | data-on-intersect |
| __half | Fire at 50 percent visible | data-on-intersect |
| __full | Fire at 100 percent visible | data-on-intersect |
| __threshold.25 | Fire at a custom visibility fraction | data-on-intersect |
| __passive | Skip preventDefault, built-in DOM events only | data-on |
| __capture | Listen in the capture phase, built-in events only | data-on |
| __prevent | Call event.preventDefault() | data-on |
| __stop | Call event.stopPropagation() | data-on |
| __window | Attach the listener to window | data-on |
| __document | Attach the listener to document, for non-bubbling document-only events | data-on |
| __outside | Fire when the event occurs outside the element | data-on |
| __debounce.500ms | Debounce, with `.leading` and `.notrailing` options | data-on, data-on-intersect, data-on-signal-patch |
| __throttle.500ms | Throttle, with `.noleading` and `.trailing` options | data-on, data-on-intersect, data-on-signal-patch |
| __delay.500ms | Delay the handler | data-init, data-on, data-on-intersect, data-on-signal-patch |
| __viewtransition | Wrap the DOM effect in document.startViewTransition() | data-init, data-on, data-on-intersect, data-on-interval, data-on-signal-patch |
| __duration.500ms | Set the timer period; append `.leading` to also fire immediately on load | data-on-interval |
| __case.camel | Override signal or key casing | data-bind, data-class, data-computed, data-indicator, data-on, data-ref, data-signals |
| __ifmissing | Set a signal only when it does not already exist | data-signals |
| __self | Restrict ignore scope to the element only | data-ignore |
| __terse | Compact JSON output | data-json-signals |

There is no `__outer` modifier; that name does not exist and is usually a misremembering of `__outside`. Modifier documentation lives only on the attributes reference page; there is no separate /reference/modifiers page.

## Actions

Actions are called with an `@` prefix inside any expression.

### Backend actions

`@get(uri, options)`, `@post(uri, options)`, `@put(uri, options)`, `@patch(uri, options)`, and `@delete(uri, options)` call the backend. `@get` sends signals as a JSON-encoded `datastar` query parameter; the others send signals as a JSON request body. `@query(uri, options)` is a read-only call that sends signals in the body. All backend actions send a `Datastar-Request: true` header. A `text/html` response morphs into elements matched by id; a `text/event-stream` response streams SSE patches. Local, underscore-prefixed signals are excluded from requests by default.

The options object supports these keys.

| Option | Meaning | Default |
| --- | --- | --- |
| contentType | 'json' or 'form' | 'json' |
| filterSignals | {include: RegExp, exclude: RegExp} for which signals to send | include /.*/, exclude /(^_\|\._).*/ |
| selector | CSS selector for form targeting when contentType is 'form' | none |
| headers | Custom HTTP headers object | none |
| openWhenHidden | Keep the SSE connection open when the tab is hidden | false |
| payload | Override the fetch payload | none |
| retry | 'auto', 'error', 'always', or 'never' | 'auto' |
| retryInterval | Milliseconds between retries | 1000 |
| retryScaler | Multiplier per retry | 2 |
| retryMaxWait | Max milliseconds between retries | 30000 |
| retryMaxCount | Max retry attempts | 10 |
| requestCancellation | 'auto', 'cleanup', 'disabled', or an AbortController | 'auto' |

Example with options:

```html
<button data-on:click="@get('/endpoint', { filterSignals: {include: /^foo\./}, headers: {'X-Csrf-Token': 'token'}, openWhenHidden: true })">Load</button>
```

Backend actions fire `datastar-fetch` lifecycle events (started, finished, error, retrying, retries-failed) that you can handle in a data-on expression.

### Utility actions

`@peek(() => $signal)` reads a signal inside a callback without creating a reactive subscription, so the surrounding computed or effect does not re-run when that signal changes. `@setAll(value, filter)` sets all signals matching a regex filter to a value. `@toggleAll(filter)` flips boolean signals matching a filter. Note that setAll and toggleAll currently match on the signal name path prefix (issue #793).

### Pro actions

`@clipboard(text, isBase64)`, `@fit(v, oldMin, oldMax, newMin, newMax, shouldClamp, shouldRound)` for range remapping, and `@intl(type, value, options, locale)` for locale-aware formatting are Pro-only.

## SSE wire format

A Datastar backend responds with `Content-Type: text/event-stream`. Each event is one `event:` line, one or more `data: <key> <value>` lines, and a blank line that terminates the frame. Multi-line payloads repeat the `data:` prefix per line. One connection can carry many events over time. In Go, the SDK writes these frames; the format below is what to expect on the wire or when writing a stream by hand.

### datastar-patch-elements

Patches one or more elements into the DOM. With no selector it matches top-level elements in the incoming HTML by their `id` against existing DOM elements.

| Field | Meaning | Default |
| --- | --- | --- |
| selector | CSS selector for the target | none, falls back to id matching |
| mode | outer, inner, replace, prepend, append, before, after, or remove | outer |
| elements | The HTML string to patch, required unless mode is remove | none |
| namespace | svg or mathml, for those content types | HTML |
| useViewTransition | Wrap the patch in the View Transitions API | false |
| viewTransitionSelector | Scope the view transition to a selector | document |

The eight modes: outer morphs the target's outer HTML in place and preserves identity and state where possible; inner morphs only the inner HTML; replace swaps the outer HTML wholesale with no morphing; prepend inserts as first children; append inserts as last children; before inserts as a preceding sibling; after inserts as a following sibling; remove deletes the target, needing only a selector.

Wire example for an append:

```text
event: datastar-patch-elements
data: selector #feed
data: mode append
data: elements <div id="item-1">New item</div>

```

Note that the v1 mode names differ from pre-1.0 semantics. The old `morph` mode is now `outer` and is the default, the old `outer` full-replace is now `replace`, `upsertAttributes` was removed, and `remove` was added. Any code using `datastar-merge-fragments`, `datastar-merge-signals`, or a `mergeMode` field is pre-1.0 and must be rewritten.

### datastar-patch-signals

Merges a JSON object of signal key and value pairs into client state, in the same shape as a data-signals value, using RFC 7386 JSON Merge Patch semantics.

| Field | Meaning | Default |
| --- | --- | --- |
| signals | JSON object of signal values, required | none |
| onlyIfMissing | Apply a signal only if it does not already exist client-side | false |

Setting a signal value to `null` in the payload removes that signal client-side. Use onlyIfMissing to seed defaults without overwriting state the user has changed.

### SSE transport fields

`eventId` sets the standard SSE `id:` line for the frame. `retryDuration` sets the SSE `retry:` line, the browser reconnect delay; the SDK default is 1 second.

### Raw content-type responses

Instead of an SSE stream, a backend may respond with a plain content type for a single patch. A `text/html` response patches elements, an `application/json` response patches signals, and a `text/javascript` response executes the script. Response headers `datastar-selector`, `datastar-mode`, and `datastar-use-view-transition` control the element patch behaviour in that case.
