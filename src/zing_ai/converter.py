"""Convert Claude Code command files for OpenCode compatibility.

This module transforms tool names, skill invocations, task parameters, and
path references from Claude Code format to OpenCode format.
"""

from __future__ import annotations

import re

# ---------------------------------------------------------------------------
# Tool name mapping — Claude Code name -> OpenCode name.
# Order matters: longer / more-specific names must come first so that e.g.
# ``TaskCreate`` is replaced before ``Task``.
# ---------------------------------------------------------------------------
_TOOL_MAP: list[tuple[str, str]] = [
    ("AskUserQuestion", "question"),
    ("TaskCreate", "todowrite"),
    ("TaskUpdate", "todowrite"),
    ("TaskList", "todoread"),
    ("TaskGet", "todoread"),
    ("TodoWrite", "todowrite"),
    ("TodoRead", "todoread"),
    ("WebFetch", "webfetch"),
    ("WebSearch", "websearch"),
    # Single-word names last so multi-word names are matched first.
    ("Bash", "bash"),
    ("Read", "read"),
    ("Write", "write"),
    ("Edit", "edit"),
    ("Grep", "grep"),
    ("Glob", "glob"),
    ("Skill", "skill"),
    ("Task", "task"),
]

# Pre-compile word-boundary patterns for each tool name.
_TOOL_PATTERNS: list[tuple[re.Pattern[str], str]] = [
    (re.compile(rf"\b{re.escape(cc_name)}\b"), oc_name) for cc_name, oc_name in _TOOL_MAP
]


# ---------------------------------------------------------------------------
# Skill chaining: Skill(skill: 'zing:foo', args: 'bar')
#              ->  skill({ name: "zing-foo", args: "bar" })
# Also handles the form without args.
# ---------------------------------------------------------------------------
_SKILL_WITH_ARGS_RE = re.compile(
    r"""`?Skill\(\s*skill:\s*'([^']+)'\s*,\s*args:\s*'([^']+)'\s*\)`?"""
)
_SKILL_NO_ARGS_RE = re.compile(r"""`?Skill\(\s*skill:\s*'([^']+)'\s*\)`?""")


def _convert_skill_name(name: str) -> str:
    """Convert a Claude Code skill name to OpenCode format.

    ``zing:plan`` becomes ``zing-plan``.
    """
    return name.replace(":", "-")


def _skill_with_args_repl(m: re.Match[str]) -> str:
    name = _convert_skill_name(m.group(1))
    args = m.group(2)
    return f'`skill({{ name: "{name}", args: "{args}" }})`'


def _skill_no_args_repl(m: re.Match[str]) -> str:
    name = _convert_skill_name(m.group(1))
    return f'`skill({{ name: "{name}" }})`'


# ---------------------------------------------------------------------------
# Task invocation conversion
# ---------------------------------------------------------------------------
_SUBAGENT_TYPE_RE = re.compile(r'\bsubagent_type\s*[=:]\s*"general-purpose"')


# ---------------------------------------------------------------------------
# Path reference conversion
#
# 1. ~/.claude/commands/zing/_shared/  ->  ~/.config/opencode/commands/_shared/
# 2. ~/.claude/commands/zing/X.md      ->  ~/.config/opencode/commands/zing-X.md
# 3. ~/.claude/commands/zing.md        ->  ~/.config/opencode/commands/zing.md
# ---------------------------------------------------------------------------
_PATH_SHARED_RE = re.compile(r"~/.claude/commands/zing/_shared/")

_PATH_ZING_SUBCOMMAND_RE = re.compile(r"~/.claude/commands/zing/(?!_shared/)([A-Za-z0-9_-]+\.md)")

_PATH_ZING_TOP_RE = re.compile(r"~/.claude/commands/zing\.md")


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------


def convert_tool_names(text: str) -> str:
    """Replace Claude Code tool names with their OpenCode equivalents.

    Uses word-boundary matching to avoid partial replacements (e.g. ``Edit``
    inside ``Editing`` is left alone).  Longer names are replaced first.
    """
    for pattern, replacement in _TOOL_PATTERNS:
        text = pattern.sub(replacement, text)
    return text


def convert_skill_calls(text: str) -> str:
    """Convert ``Skill(skill: 'zing:...')`` calls to OpenCode format."""
    # Process the with-args form first (it's more specific).
    text = _SKILL_WITH_ARGS_RE.sub(_skill_with_args_repl, text)
    text = _SKILL_NO_ARGS_RE.sub(_skill_no_args_repl, text)
    return text


def convert_task_invocations(text: str) -> str:
    """Convert Task subagent parameters to OpenCode equivalents."""
    text = _SUBAGENT_TYPE_RE.sub('agent="general"', text)
    return text


def convert_path_references(text: str) -> str:
    """Rewrite Claude Code path references to OpenCode paths."""
    # Order matters: shared paths first (they include ``zing/_shared/``),
    # then sub-command paths, then the top-level zing.md.
    text = _PATH_SHARED_RE.sub("~/.config/opencode/commands/_shared/", text)
    text = _PATH_ZING_SUBCOMMAND_RE.sub(r"~/.config/opencode/commands/zing-\1", text)
    text = _PATH_ZING_TOP_RE.sub("~/.config/opencode/commands/zing.md", text)
    return text


def convert_for_opencode(text: str) -> str:
    """Apply all Claude Code -> OpenCode conversions.

    Conversions are applied in an order that avoids interference:
    1. Skill calls (before tool names, since ``Skill`` is also a tool name)
    2. Tool names
    3. Task invocations
    4. Path references
    """
    text = convert_skill_calls(text)
    text = convert_tool_names(text)
    text = convert_task_invocations(text)
    text = convert_path_references(text)
    return text
