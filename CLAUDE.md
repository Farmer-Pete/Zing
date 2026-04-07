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
