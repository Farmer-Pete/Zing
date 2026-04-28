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

- Server data or persistence? → server endpoint, decorate with `@datastar_response`, `yield` an async generator that emits `SSE.patch_elements` / `SSE.patch_signals`. Use the helpers `sse_toast(message, kind)` and `sse_btn_state(button_id, label, *, kind)` from `src/zing_ai/server/sse_helpers.py`. Reset the button's busy state by yielding `SSE.patch_signals({"busyButtons": {f"<verb>_<sig>": False}})` rather than snapshotting the original HTML.
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

## Debugging Kanban classification

The `zing-ai debug-card` CLI fetches a live PR and/or Linear ticket, runs the data through the same `aggregate()` pipeline the Command Center uses, builds the canonical `CardView`, and prints a structured trace. Output sections in order:

1. **`=== INPUT ===`** — current username, current time, done-window cutoff, the resolved `card.key` (matches the production format, e.g. `pr-{repo}-{number}` for orphan-PR cards), and `excluded_by_aggregate` (true iff the production board would drop this card via the `_user_involved_in_done_card` filter).
2. **`=== CARD VIEW (canonical render model) ===`** — the full `CardView` Pydantic model dumped via field introspection. This is the *exact same model* `kanban_card.html` reads from. Every field a renderer would draw appears here: the underlying `card` (ticket, PRs, sessions, audit_steps, review_group, done_group, in_progress_reason), the column and its CSS class, per-PR views (pill label/class, primary button label/skill, CI bucket counts and failing-checks list, `is_author`, `needs_response`), per-session views (status label, dot class, pending question text), `total_findings`, `has_active_action`, `footer_note`, `card_dom_id`, `extra_card_classes`, `excluded_from_done_view`. The printer walks `model_fields` recursively, so adding a field anywhere under `CardView` surfaces automatically — no debug-tool change needed.
3. **`=== PR_NEEDS_RESPONSE TRACE ===`** (one per PR) — line-by-line walk through `_pr_needs_response()`: every guard, branch, sub-branch with its inputs and the boolean it returned.
4. **`=== CARD SIGNALS ===`** — every field of the `CardSignals` dataclass that drives `_classify_card`.
5. **`=== DECISION TABLE TRACE ===`** — first-match-wins evaluation in `_classify_card()` order, marked `[FIRE]` / `[ ok ]` / `[skip]`.

Usage:

```bash
zing-ai debug-card --pr <url|owner/repo#N|N> [--ticket <ID>] [--repo <owner/name>] [--user <login>]
zing-ai debug-card --ticket BAK-1259
zing-ai debug-card --pr 1885 --repo turngate/backend-v1 --ticket BAK-1259
```

Source: `src/zing_ai/debug_card.py`. The architectural rule: **`CardView` (`src/zing_ai/server/card_view.py`) is the single source of truth** for everything a card renderer draws. The Jinja fragment (`templates/fragments/kanban_card.html`) reads from it, the debug tool prints it via Pydantic introspection, and `tests/test_command_center/test_card_view.py::TestDebugToolCoverage` pins the contract that every `card_view.py` field surfaces in the debug output. To add something to a card: add a field to the appropriate `CardView` sub-view, populate it in `build_card_view()`, and consume it from the template — the debug tool will pick it up automatically.

The `_pr_needs_response` and decision-table traces are *intentionally* hand-mirrored copies of the production functions for explanatory value (line-by-line trace is inherently re-implementation). Tests in `test_kanban_aggregate.py::TestPrNeedsResponse` pin the predicate's behaviour; if the trace output disagrees with the predicate's return, the trace is the bug.

**Verify, don't assume.** When investigating or fixing classification bugs (or any behaviour driven by external API data), do not reason from training-data intuitions about how GitHub/Linear shape their responses. Run `zing-ai debug-card` against a real example first to see the live data and confirm that the predicates fire the way the theory predicts. After applying a fix, run it again on the same example to verify the column actually changed. Synthetic test fixtures are easy to construct in ways that don't match real API behaviour — for instance, GitHub's `latestReviews` excludes reviewers currently in `reviewRequests`, so any fixture that puts the same user in both `reviewer_states` and `requested_reviewers` is unrealistic and will pass tests without proving the production case works. Treat unit tests as a regression net, not as proof that the fix matches reality; the debug-card output against a live PR is the proof.

## Issue tracking

Issues and tickets are tracked in GitHub Issues on this repository. Use `gh issue create` to file new issues.

## Design system

See `src/zing_ai/server/DESIGN_SYSTEM.md` for UI design standards (colors, typography, components, interactions).
