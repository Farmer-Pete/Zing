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
- Source: https://cdn.jsdelivr.net/npm/mermaid@11.4.1/dist/mermaid.min.js
- Project: https://github.com/mermaid-js/mermaid
- SHA-256: `a43bc1afd446f9c4cc66ac5dd45d02e8d65e26fc5344ec0ef787f88d6ddb6f9e`
- License: MIT (Mermaid Contributors). Full text: https://github.com/mermaid-js/mermaid/blob/develop/LICENSE
- Vendored unmodified, byte-for-byte, straight from the CDN response: fetched read-only, hashed, and scanned for `eval`, `document.write`, `new Function`, dynamic `import()`, and unexpected external hosts before vendoring, none found (every URL string in the bundle is a license or upstream-project link). `internal/console/static_test.go`'s `TestMermaidAssetDigestMatchesRecorded` asserts the embedded bytes match the recorded digest above (design section 0, dependency set).
- **Replaces the Task 1 ESM entry (design v10 change log, "build-time correction").** Task 1 vendored `mermaid.esm.min.mjs`, which is not self-contained: it carries static and dynamic `import`/`import()` statements to about thirty sibling chunk files under a relative `./chunks/` path, which a single embedded asset cannot serve offline. This entry is the self-contained UMD build instead (esbuild's IIFE output ending `globalThis.mermaid = globalThis.__esbuild_esm_mermaid.default`), one file with no chunk imports. It sets the `mermaid` global (`window.mermaid` in a browser) when loaded via a classic, non-module `<script>` tag, which is how `internal/console/templates/shell.templ` loads it, in the document head. `internal/console/static/console.js`'s `runMermaidGuarded` step (previously guarded off) now calls `window.mermaid.initialize({securityLevel:'strict', startOnLoad:false})` once and `window.mermaid.run()` over the collected diagram nodes on every patch (design section 6.10). `internal/console/render.go`'s goldmark-diagram wiring never registers a script-emitting renderer (it replaces `NewMermaidClientRenderer` with a renderer that writes the same `<pre class="mermaid">` block but never marks the render context that would make goldmark-diagram append its own `<script>` tag), so this classic script tag is the one and only mermaid load, matching the design's "exactly one mermaid load."

## console.js, keyboard.mjs, keys.json

Authored in this repo, not vendored. `keys.json` is generated from
`internal/console/keys.go` (design section 6.4, 7.3; `go generate
./internal/console`, checked by `keys_test.go`). `keyboard.mjs` is the pure
keyboard logic (the chord machine, key-to-action resolution, input-context
detection, the send-chord platform check, the id-based focus step,
`reconcileFocus`, `collectPatchWork`), covered by `console.test.js` under
`node --test`. `console.js` is the DOM wiring: it reads `keys.json`,
installs the keyboard handler and the `#main`/`#rail` `MutationObserver`,
and dispatches `zing-nav` on `#stream-ctl`. Its mermaid step
(`runMermaidGuarded`) now initializes `window.mermaid` (set by the classic
script tag above) once and runs it over unprocessed `.mermaid` nodes on
each patch (design section 6.3, 6.10).
