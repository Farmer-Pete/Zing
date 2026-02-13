"""Tests for the OpenCode converter."""

from __future__ import annotations

from zing_ai.converter import (
    convert_for_opencode,
    convert_path_references,
    convert_skill_calls,
    convert_task_invocations,
    convert_tool_names,
)

# -----------------------------------------------------------------------
# Tool name conversion
# -----------------------------------------------------------------------


def test_tool_bash():
    assert convert_tool_names("Use the Bash tool") == "Use the bash tool"


def test_tool_read():
    assert convert_tool_names("Use the Read tool") == "Use the read tool"


def test_tool_write():
    assert convert_tool_names("Use the Write tool") == "Use the write tool"


def test_tool_edit():
    assert convert_tool_names("Use the Edit tool") == "Use the edit tool"


def test_tool_grep():
    assert convert_tool_names("Use Grep to search") == "Use grep to search"


def test_tool_glob():
    assert convert_tool_names("Use Glob to list") == "Use glob to list"


def test_tool_webfetch():
    assert convert_tool_names("Use the WebFetch tool") == "Use the webfetch tool"


def test_tool_websearch():
    assert convert_tool_names("Use the WebSearch tool") == "Use the websearch tool"


def test_tool_ask_user_question():
    assert (
        convert_tool_names("Use AskUserQuestion to ask the user") == "Use question to ask the user"
    )


def test_tool_skill():
    assert convert_tool_names("the Skill tool") == "the skill tool"


def test_tool_task():
    assert convert_tool_names("Launch a Task subagent") == "Launch a task subagent"


def test_tool_todowrite():
    assert convert_tool_names("Use TodoWrite to update") == "Use todowrite to update"


def test_tool_todoread():
    assert convert_tool_names("Use TodoRead to check") == "Use todoread to check"


def test_tool_taskcreate():
    assert convert_tool_names("Use TaskCreate to add") == "Use todowrite to add"


def test_tool_taskupdate():
    assert convert_tool_names("Use TaskUpdate to mark") == "Use todowrite to mark"


def test_tool_tasklist():
    assert convert_tool_names("Use TaskList to see") == "Use todoread to see"


def test_tool_taskget():
    assert convert_tool_names("Use TaskGet to fetch") == "Use todoread to fetch"


# -- Word boundary edge cases ----------------------------------------


def test_reading_not_replaced():
    """'Reading' should NOT become 'reading' due to matching 'Read'."""
    assert convert_tool_names("Reading the file now") == "Reading the file now"


def test_editing_not_replaced():
    """'Editing' should NOT become 'editing' due to matching 'Edit'."""
    assert convert_tool_names("Editing the file now") == "Editing the file now"


def test_writing_not_replaced():
    """'Writing' should NOT become 'writing' due to matching 'Write'."""
    assert convert_tool_names("Writing the file now") == "Writing the file now"


def test_bashing_not_replaced():
    """'Bashing' should NOT become 'bashing' due to matching 'Bash'."""
    assert convert_tool_names("Bashing the keyboard") == "Bashing the keyboard"


def test_grepping_not_replaced():
    """'Grepping' should NOT become 'grepping' due to matching 'Grep'."""
    assert convert_tool_names("Grepping through files") == "Grepping through files"


def test_globbing_not_replaced():
    """'Globbing' should NOT become 'globbing' due to matching 'Glob'."""
    assert convert_tool_names("Globbing for patterns") == "Globbing for patterns"


def test_tasking_not_replaced():
    """'Tasking' should NOT match 'Task'."""
    assert convert_tool_names("Tasking someone with this") == "Tasking someone with this"


def test_tool_at_end_of_sentence():
    """Tool names at the end of a sentence (followed by '.') are replaced."""
    assert convert_tool_names("Read the file with Read.") == "read the file with read."


def test_tool_in_backticks():
    """Tool names inside backticks are replaced (they're prompt text)."""
    assert convert_tool_names("Use `Read` to view") == "Use `read` to view"


def test_multiple_tools_in_one_line():
    """Multiple different tool names in one line are all replaced."""
    assert convert_tool_names("Use Read and Edit and Grep") == "Use read and edit and grep"


def test_taskcreate_not_partial_task():
    """TaskCreate is replaced as 'todowrite', not as 'task' + 'Create'."""
    result = convert_tool_names("using TaskCreate")
    assert result == "using todowrite"
    assert "taskCreate" not in result


# -----------------------------------------------------------------------
# Skill chaining conversion
# -----------------------------------------------------------------------


def test_skill_with_args():
    text = "invoke `Skill(skill: 'zing:plan', args: '.zing/recipe-app.md')` to continue"
    expected = 'invoke `skill({ name: "zing-plan", args: ".zing/recipe-app.md" })` to continue'
    assert convert_skill_calls(text) == expected


def test_skill_without_args():
    text = "invoke `Skill(skill: 'zing:build-audit')` to start"
    expected = 'invoke `skill({ name: "zing-build-audit" })` to start'
    assert convert_skill_calls(text) == expected


def test_skill_name_colon_to_dash():
    """Colon in skill name is converted to dash."""
    text = "`Skill(skill: 'zing:plan-linear')`"
    expected = '`skill({ name: "zing-plan-linear" })`'
    assert convert_skill_calls(text) == expected


def test_skill_without_backticks():
    """Skill call without surrounding backticks is still converted."""
    text = "Skill(skill: 'zing:build-audit')"
    expected = '`skill({ name: "zing-build-audit" })`'
    assert convert_skill_calls(text) == expected


def test_skill_with_args_without_backticks():
    text = "Skill(skill: 'zing:build', args: '.zing/app.md')"
    expected = '`skill({ name: "zing-build", args: ".zing/app.md" })`'
    assert convert_skill_calls(text) == expected


def test_multiple_skill_calls():
    """Multiple skill calls in the same text are all converted."""
    text = "First `Skill(skill: 'zing:plan')` then `Skill(skill: 'zing:build', args: 'x.md')`"
    expected = (
        'First `skill({ name: "zing-plan" })` then `skill({ name: "zing-build", args: "x.md" })`'
    )
    assert convert_skill_calls(text) == expected


def test_skill_call_with_plain_name():
    """Skill name without 'zing:' prefix still works."""
    text = "`Skill(skill: 'something-else')`"
    expected = '`skill({ name: "something-else" })`'
    assert convert_skill_calls(text) == expected


# -----------------------------------------------------------------------
# Task invocation conversion
# -----------------------------------------------------------------------


def test_subagent_type_with_equals():
    text = 'Launch Task with subagent_type="general-purpose" and prompt'
    expected = 'Launch Task with agent="general" and prompt'
    assert convert_task_invocations(text) == expected


def test_subagent_type_with_colon():
    text = 'subagent_type: "general-purpose"'
    expected = 'agent="general"'
    assert convert_task_invocations(text) == expected


def test_task_no_match_leaves_text_unchanged():
    text = "No subagent stuff here."
    assert convert_task_invocations(text) == text


# -----------------------------------------------------------------------
# Path reference conversion
# -----------------------------------------------------------------------


def test_shared_path():
    text = "Read `~/.claude/commands/zing/_shared/review-core.md`"
    expected = "Read `~/.config/opencode/commands/_shared/review-core.md`"
    assert convert_path_references(text) == expected


def test_subcommand_path():
    text = "See `~/.claude/commands/zing/build.md`"
    expected = "See `~/.config/opencode/commands/zing-build.md`"
    assert convert_path_references(text) == expected


def test_top_level_zing_path():
    text = "The file `~/.claude/commands/zing.md`"
    expected = "The file `~/.config/opencode/commands/zing.md`"
    assert convert_path_references(text) == expected


def test_shared_path_preserves_filename():
    text = "~/.claude/commands/zing/_shared/something-else.md"
    expected = "~/.config/opencode/commands/_shared/something-else.md"
    assert convert_path_references(text) == expected


def test_subcommand_with_dashes():
    text = "~/.claude/commands/zing/plan-audit.md"
    expected = "~/.config/opencode/commands/zing-plan-audit.md"
    assert convert_path_references(text) == expected


def test_multiple_paths_in_text():
    text = (
        "Compare `~/.claude/commands/zing/build.md` "
        "with `~/.claude/commands/zing/_shared/review-core.md`"
    )
    expected = (
        "Compare `~/.config/opencode/commands/zing-build.md` "
        "with `~/.config/opencode/commands/_shared/review-core.md`"
    )
    assert convert_path_references(text) == expected


def test_path_no_match_leaves_text_unchanged():
    text = "Some random text with no paths"
    assert convert_path_references(text) == text


# -----------------------------------------------------------------------
# Full document conversion (convert_for_opencode)
# -----------------------------------------------------------------------


def test_full_document_conversion():
    """Realistic command file snippet with all conversion types."""
    input_text = """\
Use Glob to list all markdown files in the `.zing` directory.
If one or more files found, use AskUserQuestion to let the user pick.

Read the zing file using the Read tool. Create a task using TaskCreate.
Mark as completed using TaskUpdate. Check with TaskList.

Launch a Task subagent with subagent_type="general-purpose" and prompt.

Then invoke `Skill(skill: 'zing:build-audit')` to start the review.
Also try `Skill(skill: 'zing:plan', args: '.zing/recipe-app.md')`.

Read the shared review reference file at `~/.claude/commands/zing/_shared/review-core.md`.
See `~/.claude/commands/zing/build.md` for the build command.
Top-level: `~/.claude/commands/zing.md`.
"""

    expected = """\
Use glob to list all markdown files in the `.zing` directory.
If one or more files found, use question to let the user pick.

read the zing file using the read tool. Create a task using todowrite.
Mark as completed using todowrite. Check with todoread.

Launch a task subagent with agent="general" and prompt.

Then invoke `skill({ name: "zing-build-audit" })` to start the review.
Also try `skill({ name: "zing-plan", args: ".zing/recipe-app.md" })`.

read the shared review reference file at `~/.config/opencode/commands/_shared/review-core.md`.
See `~/.config/opencode/commands/zing-build.md` for the build command.
Top-level: `~/.config/opencode/commands/zing.md`.
"""

    assert convert_for_opencode(input_text) == expected


def test_skill_calls_before_tool_names():
    """Skill calls are converted before tool name replacement so
    the Skill name in the call pattern isn't mangled."""
    text = "`Skill(skill: 'zing:build')` and use the Skill tool"
    result = convert_for_opencode(text)
    assert 'skill({ name: "zing-build" })' in result
    assert "the skill tool" in result


def test_idempotent_on_already_converted():
    """Running conversion on already-converted text should not break it."""
    text = 'Use the read tool and `skill({ name: "zing-plan" })`.'
    assert convert_for_opencode(text) == text


def test_convert_empty_string():
    assert convert_for_opencode("") == ""


def test_convert_no_convertible_content():
    text = "Just some plain text with no tools or paths."
    assert convert_for_opencode(text) == text


def test_word_boundaries_in_full_conversion():
    """Ensure word boundaries are respected in the full pipeline."""
    text = "Reading and Editing are gerunds, not tools."
    assert convert_for_opencode(text) == text
