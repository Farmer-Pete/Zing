# Vendored static assets

Every file here is served only through the explicit allowlist in
`internal/console/server.go` (design section 6.14, section 5): a request
for anything else under `/static/` gets a 404 from the mux itself, since
no catch-all file server is registered. `console.test.js`, `package.json`,
and this file are never served; they exist for `node --test` and for
repo-side bookkeeping only.

## datastar.js

- Version: v1.0.4
- Source: https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.4/bundles/datastar.js
- Project: https://github.com/starfederation/datastar
- SHA-256: `27fb94cd95af4bc2d5039223df237debb8e5828e881e8334903e391851c50d23`
- License: MIT (Star Federation); full text in `datastar.js.LICENSE`.
- Vendored by Package 3. The trailing `sourceMappingURL` comment was removed because the source map is not vendored; the bundle is otherwise unmodified.

## mermaid.js

- Version: 11.4.1
- Source: https://cdn.jsdelivr.net/npm/mermaid@11.4.1/dist/mermaid.esm.min.mjs
- Project: https://github.com/mermaid-js/mermaid
- SHA-256: `cff34e82c8bded4711ae36bc9cf1df0f1d05fa0594cb6540eb8dc3d2aced9426`
- License: MIT (Mermaid Contributors). Full text: https://github.com/mermaid-js/mermaid/blob/develop/LICENSE
- Vendored unmodified, byte-for-byte, straight from the CDN response. `internal/console/asset_test.go` asserts the embedded bytes match the recorded digest above (design section 0, dependency set).
- **Known gap, flagged for whoever wires `console.js`'s real mermaid import (Task 4/5):** this entry-point file is not self-contained. It carries 10 static `import` statements and 23 dynamic `import()` calls to sibling chunk files under a relative `./chunks/mermaid.esm.min/` path (mermaid's diagram-type renderers are code-split). Only this one file is vendored here, matching the plan's single-file `/static/mermaid.js` asset; the chunk files are not vendored and the static allowlist does not serve them. A browser that actually imports and runs this module will 404 on those chunk requests. Task 1 only serves and digest-verifies this file; it does not wire a working mermaid import (that starts at Task 4's `console.js` and Task 5's `render.go`). Whoever does that work must vendor the chunk tree (or reach for a different mermaid distribution) before mermaid can render a diagram end to end.

## console.js, keyboard.mjs, keys.json

Authored in this repo, not vendored. `keys.json` is generated from
`internal/console/keys.go` (design section 6.4, 7.3; `go generate
./internal/console`, checked by `keys_test.go`). `keyboard.mjs` is the pure
keyboard logic (the chord machine, key-to-action resolution, input-context
detection, the send-chord platform check, the id-based focus step,
`reconcileFocus`, `collectPatchWork`), covered by `console.test.js` under
`node --test`. `console.js` is the DOM wiring: it reads `keys.json`,
installs the keyboard handler and the `#main`/`#rail` `MutationObserver`,
and dispatches `zing-nav` on `#stream-ctl`. Its mermaid step is guarded off
(see the "Known gap" note above): it collects and marks diagram nodes but
does not import or run mermaid yet, since a static import of the
unvendored `mermaid.js` would fail module resolution in a real browser and
take the whole module down with it. Task 5 wires the real import once the
chunk-vendoring gap is fixed.
