# Zing AI

CLI tool that installs skill/command markdown files into a user's Claude Code or OpenCode config directories.

## Tech stack

- Python >= 3.12, built with `uv_build`
- Runtime dependency: `click>=8.1`
- Dev tools: pytest, ruff, pyright, pyleft, pre-commit
- Entry point: `zing-ai` → `zing_ai.cli:main`

## Project structure

- `src/zing_ai/` — Main package. CLI entry point, installer, converter (adapts syntax for OpenCode), manifest (tracks installed file hashes for change detection), and backup/patch reapply logic.
- `src/zing_ai/commands/` — Bundled markdown skill files that get installed into the user's config directory. Contains the top-level `/zing` command, sub-commands (new, build, plan, audits, etc.), and shared review logic.
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
