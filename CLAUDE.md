# Zing AI

CLI tool that installs skill/command markdown files into a user's Claude Code or OpenCode config directories.

## Tech stack

- Python >= 3.12, built with `uv_build`
- Runtime dependency: `click>=8.1`
- Dev tools: pytest, ruff, pyright, pyleft, pre-commit
- Entry point: `zing-ai` → `zing_ai.cli:main`

## Project structure

- `src/zing_ai/` — Main package. CLI entry point, installer, converter (adapts syntax for OpenCode), manifest (tracks installed file hashes for change detection), and backup/patch reapply logic.
- `src/zing_ai/config.py` and `src/zing_ai/templating.py` — User config (`~/.config/zing-ai/config.toml`) and the Jinja rendering pipeline that resolves command markdowns at install time. The LLM only ever sees fully-rendered markdown — config values and `{% if %}` blocks are resolved before files are written to the runtime directory.
- `src/zing_ai/commands/` — Bundled markdown skill files that get installed into the user's config directory. Contains the top-level `/zing` command, sub-commands (new, build, plan, audits, etc.), and shared review logic. **These `.md` files are Jinja2 templates** — they may contain `{{ var }}` substitutions and `{% if %}` / `{% for %}` blocks that reference user config (e.g. `{{ git.workflow_mode }}`, `{{ thresholds.large_file_lines }}`). They are rendered at install time; the LLM only ever sees the rendered output. When editing these files, treat them as Jinja2: escape literal `{{` / `{%` if you ever need them, and ensure conditional branches still produce valid markdown.
- `tests/` — Unit tests (one per module) plus an integration test for the end-to-end install flow.

## Code style

- `from __future__ import annotations` in every module
- Type hints on all function signatures
- Docstrings in triple-double-quote style
- Private functions prefixed with `_`
- unittest-based tests
- Ruff: line length 100, target py312

## How it works

1. CLI parses args and resolves which runtimes to install for (Claude Code, OpenCode, or both)
2. Installer copies markdown files from `commands/` to the target config directory, using the converter to adapt syntax when targeting OpenCode
3. Manifest writes installed file hashes so future installs can detect user modifications
4. Backup saves user-modified files as patches before overwriting, and can reapply them after reinstall

## Datastar usage

The web server (`src/zing_ai/server/`) renders Jinja templates and pushes updates to the browser via [Datastar v1.0.0](https://data-star.dev) (loaded from CDN at `templates/base.html`). The browser side is intentionally thin: nearly all interactivity is declarative (HTML attributes), and the server is the source of truth for visual state. Two small JS files remain (`base.js` for notification opt-in, `cc-modals.js` for iframe lifecycle and `ClipboardItem` writes) — both browser-API-only.

**Architecture rule.** Browser interactions either mutate a `data-signals` value (view-local UI state) or call a server endpoint that responds with SSE patches (`SSE.patch_elements` / `SSE.patch_signals`). Do not write `fetch()` + `.then(...)` + DOM mutation in JS. If you find yourself reaching for one, you are working against the framework.

**Decision tree for new interactions.**

- Server data or persistence? → server endpoint, decorate with `@datastar_response`, `yield` an async generator that emits `SSE.patch_elements` / `SSE.patch_signals`. Use the helpers `sse_toast(message, kind)` and `sse_btn_state(button_id, label, *, kind, reset_html=...)` from `src/zing_ai/server/sse_helpers.py`.
- Pure UI state (modal open, kebab open, tab selection, accordion expand)? → `data-signals` + `data-show` / `data-class` / `data-text`. View-local state lives on the page or fragment that owns it; never round-trip the server.
- Browser API only (clipboard, notifications, iframe lifecycle)? → small JS file, dispatched via a named `window.dispatchX(...)` function called from a `data-on:click` so templates stay declarative.

**Attribute syntax canon.** Event listeners use the colon form: `data-on:click`, `data-on:input`, `data-on:keydown__window__key.escape`. Lifecycle and signal-watch attributes use dashed names because they are *attribute names* in v1, not event listeners: `data-init` (fires on element initialization — page load and patch-into-DOM), `data-on-signal-patch`, `data-on-signal-patch-filter`. Never use `data-on-click` (legacy alias; banned by `tests/test_lint.py`). Never use `data-on-load` on a non-`<body>` element — the DOM `load` event does not fire on `<div>`/`<button>`/etc.; reach for `data-init` instead.

**Server response idioms.** When an endpoint is reached via `data-on:*`, decorate it with `@datastar_response` and follow the inner-`_stream()` pattern (see `routes_command_center.py` for canonical examples):

```python
@router.post("/foo")
@datastar_response
async def foo(payload: dict[str, Any], request: Request):  # noqa: ANN201
    async def _stream():  # noqa: ANN202
        yield _sse_toast("done", "ok")
    return _stream()
```

Single-line signature, `# noqa: ANN201/ANN202`, no `-> AsyncGenerator[...]` annotations (pyright strict rejects them; `routes_install.py` is the established pattern). Return `JSONResponse` only when the caller is a non-Datastar client (CLI, external webhook — e.g., `/command-center/session-question` is a shell-hook called by `notify-zing.sh`). Two small Datastar quirks worth remembering: setting a signal to `null` deletes it from the proxy *without* triggering reactive notifications (use `""` as the unset value for string signals), and `data-bind` takes a signal path (`kebabQuery`), not a `$`-prefixed expression.

**Signal naming.** `camelCase`, scoped to the page or fragment that owns them. Modal-open booleans group into a `modals: {}` sub-object on the page envelope (e.g. `$modals.drawer`, `$modals.standup`). Drawer-internal state (`triage`, `openSteps`, `prevSessionId`, `nextSessionId`, `sessionId`, `stepId`) lives on the drawer fragment's own `data-signals` so it clears with the DOM when the drawer closes.

**Where to look up Datastar.** Claude's training data on Datastar is out of date — the v1 API differs materially from earlier versions. When writing or reviewing Datastar code, always look up current docs via Context7: `mcp__context7__resolve-library-id "datastar"`, then `mcp__context7__query-docs` with `/websites/data-star_dev`.

**Canonical examples in this repo.** `templates/fragments/finding.html` (signals + bind + click + class binding + `@post`), `templates/fragments/config_field.html` (debounced `@post`), `routes_command_center.py:command_center_events` (mixing `patch_elements` + `patch_signals` in one stream), `sse_helpers.py` (helper-builder pattern), `cc-modals.js` (browser-API + named dispatch functions).

## Testing philosophy

The Playwright UI tests in `tests/test_ui/` are the most important safety net for the web server. When modifying server templates, routes, or Datastar bindings, always update or add Playwright tests that verify the **end-to-end behavior**, not just HTML structure.

Key principles:

- **Test through the browser, not just the HTTP layer.** Rendering tests (`test_server_rendering.py`) and route tests (`test_server_routes.py`) verify HTML output and HTTP responses in isolation. Playwright tests must verify that Datastar actually initializes, bindings fire, and POSTs succeed. A template can produce correct HTML that still breaks at runtime if attributes are malformed for the JS framework.
- **Test with pre-existing state, not just fresh sessions.** Many bugs only surface when loading a page that already has saved data (e.g., saved responses, completed steps). Always include a test that persists state, reloads the page, and verifies the UI still works.
- **Verify server-side effects, not just DOM changes.** After clicking a button, assert that the server state actually changed (e.g., `manager.get_step_by_id()` returns the expected response), not just that a CSS class toggled.
- **Check for console errors after Datastar actions.** `wait_for_load_state("networkidle")` does not mean Datastar initialized successfully. After interactions, check that no JS errors were thrown.

Run Playwright tests with: `uv run pytest tests/test_ui/ -m ui`

## Issue tracking

Issues and tickets are tracked in GitHub Issues on this repository. Use `gh issue create` to file new issues.

## Design system

See `src/zing_ai/server/DESIGN_SYSTEM.md` for UI design standards (colors, typography, components, interactions).
