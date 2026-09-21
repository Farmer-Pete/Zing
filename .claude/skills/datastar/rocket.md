# Datastar Rocket web components

Rocket is Datastar's web component API, a JavaScript layer over the browser's native custom-element model. Datastar still handles reactivity, local signal scoping, action dispatch, and DOM application inside the component; Rocket auto-scopes the `data-*` attributes to each component instance and gives you a typed prop API.

Rocket is in beta and its API is subject to change. As of Datastar v1.0.4 it is free and ships as its own bundle, `datastar-rocket.js`, at the same tagged CDN path as the core. That bundle contains the Datastar core as well, so a page loads either `datastar.js` or `datastar-rocket.js`, never both. Do not confuse this with the Rust web framework also called Rocket; the Datastar Rust SDK has a separate `rocket.rs` integration module that is unrelated to this component API.

## When to use Rocket

Use Rocket for a reusable, encapsulated custom element with a public API, typed props decoded from HTML attributes, per-instance local state, and a choice of light or shadow DOM, for example a `<demo-counter>` widget dropped into many pages. Use plain `data-*` attributes for ordinary page-level interactivity that does not need component encapsulation or reuse as a named tag. Default to props when data is part of the component's public API, and to local Rocket signals for internal reactive state and imperative integration points.

## Defining a component

Import `rocket` from the Rocket bundle and register a component. The tag name is the first argument, must contain a hyphen, must be unique, and becomes the HTML tag you place in the page.

```javascript
import { rocket } from '/static/datastar-rocket.js'

rocket('demo-counter', {
  mode: 'light',
  props: ({ number, string }) => ({
    count: number.step(1).min(0),
    label: string.trim.default('Counter'),
  }),
  setup: ({ $$, props }) => {
    $$.count = props.count
  },
  render: ({ html, props: { count, label } }) => {
    return html`<button data-on:click="$$count += 1">${label}: ${count}</button>`
  },
})
```

Use it in markup:

```html
<demo-counter count="5" label="Inventory"></demo-counter>
```

## Rendering mode

The `mode` field controls shadow DOM. Use `light` for no shadow root, so the component inherits page styles; this is the recommended default when the component should look like normal page content. Use `open` for shadow DOM with encapsulated styles that still allows external debugging via `element.shadowRoot`. Use `closed` for fully encapsulated shadow DOM with no external access.

## Props

Props use a codec-based builder, `props: ({ number, string, bool, array, object }) => ({ ... })`. Each declaration, for example `number.step(1).min(0)` or `string.trim.default('Counter')`, creates three things at once: an observed HTML attribute in kebab-case, a JS property accessor on the element instance, and a decoded, normalized value passed into setup and render. Assigning the JS property, for example `el.priority = 7`, reflects back to the kebab-case attribute automatically. If a codec's decode throws, Rocket calls `console.warn` and falls back to that codec's default rather than throwing to the caller.

Compose codecs for structured props. An array of objects looks like this:

```javascript
props: ({ array, object, string }) => ({
  items: array(object({
    href: string.trim.default('#'),
    label: string.trim.default('Untitled'),
  })),
})
```

Map it in the template with `${items.map((item) => html`<li><a href="${item.href}">${item.label}</a></li>`)}`.

## Signals and state wiring

Inside setup and render, `$$` is the instance-scoped signal accessor. Create local signals with `$$.count = 0` or a computed form `$$.label = () => 'Item ' + $$.count`. In a template's Datastar expressions, reference them as `$$count`, for example `data-on:click="$$count += 1"` or `data-text="$$count"`. Rocket rewrites `$$name` to an instance-scoped path, so multiple copies of the same tag on a page never collide.

Reach top-level page signals from inside a component with the plain `$` accessor in setup, for example `if ($.analyticsEnabled !== false) { ... }`. That reads and writes the same global signals that plain `data-*` attributes elsewhere on the page use.

The `__root` modifier escapes component scoping to target the true document root, but it only affects the `data-bind`, `data-computed`, `data-indicator`, and `data-ref` attribute families, not every attribute inside the component.

## Lifecycle

`setup({$$, props, $, host, cleanup, ...})` runs once per connected instance, before the first render, roughly connectedCallback. Use it for local signals, prop observers, timers, and registering cleanup. Register teardown with `cleanup(fn)` inside setup; there is no separate disconnected hook.

`render({html, props})` runs on every prop change by default and returns declarative DOM output. It can return a Rocket tagged-template fragment built with the `html` helper (there is an `svg` helper too), a string or number, an iterable of those, or `null` or `undefined` to render nothing. `renderOnPropChange`, a boolean or function, defaults to true and controls whether a given prop change re-triggers render.

The full setup context is `{ $$, props, $, host, effect, cleanup, observeProps, overrideProp, defineHostProp, action, actions, render }`.

`onFirstRender` runs once, after the initial render and after Datastar's apply pass and ref population. Put ref-dependent code here, for example DOM measurement or third-party widget init. Refs from `data-ref` are not available in setup, only from onFirstRender onward, because they populate during the apply pass that runs between the first render and onFirstRender.

## Reacting imperatively

To react to prop changes without a full re-render, use `observeProps((props, changes) => { if ('src' in changes) { refs.video.src = props.src } }, 'src')`. To react to local signal changes, use `effect(() => { if ($$.count > 10) console.log('too high') })`.

## Events

Rocket has no built-in event emitter. Dispatch custom events the standard DOM way:

```javascript
host.dispatchEvent(new CustomEvent('close', { bubbles: true, composed: true }))
```

Document emitted events for tooling with a `manifest.events` array, for example `manifest: { events: [{ name: 'close', kind: 'custom-event', bubbles: true, composed: true, description: 'Fired when dismissed.' }] }`. The action name `dispatchRocket` is reserved for Rocket's internal dispatcher; do not define a custom action with that name.

## Templates, lists, and conditionals

`data-for`, `data-if`, `data-else-if`, and `data-else` are Rocket template attributes. They are not in the core attribute set, so they only work inside a Rocket component's render output. List rendering: `<template data-for="letter, row in $$letters"><li data-text="row + 1 + ': ' + letter"></li></template>`. Rocket does not preserve row identity across reorders in this version, and it does not support an index-only loop form; always supply an item alias alongside any index alias.

Conditional rendering uses `data-if`, `data-else-if`, and `data-else` on `<template>` elements:

```html
<template data-if="$$step === 0"><p>Idle</p></template>
<template data-else-if="$$step === 1"><p>Loading</p></template>
<template data-else><p>Ready</p></template>
```

## Worked example, stepper

```javascript
rocket('demo-stepper', {
  mode: 'light',
  props: ({ number, string }) => ({
    start: number.min(0),
    step: number.min(1).default(1),
    label: string.trim.default('Count'),
  }),
  setup: ({ $$, props }) => { $$.count = props.start },
  render: ({ html, props: { label, step } }) => html`
    <section>
      <h3>${label}</h3>
      <button data-on:click="$$count -= ${step}">-</button>
      <output data-text="$$count"></output>
      <button data-on:click="$$count += ${step}">+</button>
    </section>
  `,
})
```

## Backend interaction

Rocket is purely client-side. The Go backend only needs to serve the `datastar-rocket.js` bundle and otherwise interacts with Rocket components the same way as any Datastar-enhanced DOM, through PatchElements and PatchSignals. See go-sdk.md.
