"""Tests for the OpenCode converter."""

from __future__ import annotations

import unittest

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
class TestConvertToolNames(unittest.TestCase):
    """Verify every tool name mapping and edge cases."""

    # -- Exhaustive mapping tests ----------------------------------------

    def test_bash(self) -> None:
        self.assertEqual(convert_tool_names("Use the Bash tool"), "Use the bash tool")

    def test_read(self) -> None:
        self.assertEqual(convert_tool_names("Use the Read tool"), "Use the read tool")

    def test_write(self) -> None:
        self.assertEqual(convert_tool_names("Use the Write tool"), "Use the write tool")

    def test_edit(self) -> None:
        self.assertEqual(convert_tool_names("Use the Edit tool"), "Use the edit tool")

    def test_grep(self) -> None:
        self.assertEqual(convert_tool_names("Use Grep to search"), "Use grep to search")

    def test_glob(self) -> None:
        self.assertEqual(convert_tool_names("Use Glob to list"), "Use glob to list")

    def test_webfetch(self) -> None:
        self.assertEqual(
            convert_tool_names("Use the WebFetch tool"),
            "Use the webfetch tool",
        )

    def test_websearch(self) -> None:
        self.assertEqual(
            convert_tool_names("Use the WebSearch tool"),
            "Use the websearch tool",
        )

    def test_ask_user_question(self) -> None:
        self.assertEqual(
            convert_tool_names("Use AskUserQuestion to ask the user"),
            "Use question to ask the user",
        )

    def test_skill(self) -> None:
        self.assertEqual(
            convert_tool_names("the Skill tool"),
            "the skill tool",
        )

    def test_task(self) -> None:
        self.assertEqual(
            convert_tool_names("Launch a Task subagent"),
            "Launch a task subagent",
        )

    def test_todowrite(self) -> None:
        self.assertEqual(
            convert_tool_names("Use TodoWrite to update"),
            "Use todowrite to update",
        )

    def test_todoread(self) -> None:
        self.assertEqual(
            convert_tool_names("Use TodoRead to check"),
            "Use todoread to check",
        )

    def test_taskcreate(self) -> None:
        self.assertEqual(
            convert_tool_names("Use TaskCreate to add"),
            "Use todowrite to add",
        )

    def test_taskupdate(self) -> None:
        self.assertEqual(
            convert_tool_names("Use TaskUpdate to mark"),
            "Use todowrite to mark",
        )

    def test_tasklist(self) -> None:
        self.assertEqual(
            convert_tool_names("Use TaskList to see"),
            "Use todoread to see",
        )

    def test_taskget(self) -> None:
        self.assertEqual(
            convert_tool_names("Use TaskGet to fetch"),
            "Use todoread to fetch",
        )

    # -- Word boundary edge cases ----------------------------------------

    def test_reading_not_replaced(self) -> None:
        """'Reading' should NOT become 'reading' due to matching 'Read'."""
        self.assertEqual(
            convert_tool_names("Reading the file now"),
            "Reading the file now",
        )

    def test_editing_not_replaced(self) -> None:
        """'Editing' should NOT become 'editing' due to matching 'Edit'."""
        self.assertEqual(
            convert_tool_names("Editing the file now"),
            "Editing the file now",
        )

    def test_writing_not_replaced(self) -> None:
        """'Writing' should NOT become 'writing' due to matching 'Write'."""
        self.assertEqual(
            convert_tool_names("Writing the file now"),
            "Writing the file now",
        )

    def test_bashing_not_replaced(self) -> None:
        """'Bashing' should NOT become 'bashing' due to matching 'Bash'."""
        self.assertEqual(
            convert_tool_names("Bashing the keyboard"),
            "Bashing the keyboard",
        )

    def test_grepping_not_replaced(self) -> None:
        """'Grepping' should NOT become 'grepping' due to matching 'Grep'."""
        self.assertEqual(
            convert_tool_names("Grepping through files"),
            "Grepping through files",
        )

    def test_globbing_not_replaced(self) -> None:
        """'Globbing' should NOT become 'globbing' due to matching 'Glob'."""
        self.assertEqual(
            convert_tool_names("Globbing for patterns"),
            "Globbing for patterns",
        )

    def test_tasking_not_replaced(self) -> None:
        """'Tasking' should NOT match 'Task'."""
        self.assertEqual(
            convert_tool_names("Tasking someone with this"),
            "Tasking someone with this",
        )

    def test_tool_at_end_of_sentence(self) -> None:
        """Tool names at the end of a sentence (followed by '.') are replaced."""
        self.assertEqual(
            convert_tool_names("Read the file with Read."),
            "read the file with read.",
        )

    def test_tool_in_backticks(self) -> None:
        """Tool names inside backticks are replaced (they're prompt text)."""
        self.assertEqual(
            convert_tool_names("Use `Read` to view"),
            "Use `read` to view",
        )

    def test_multiple_tools_in_one_line(self) -> None:
        """Multiple different tool names in one line are all replaced."""
        self.assertEqual(
            convert_tool_names("Use Read and Edit and Grep"),
            "Use read and edit and grep",
        )

    def test_taskcreate_not_partial_task(self) -> None:
        """TaskCreate is replaced as 'todowrite', not as 'task' + 'Create'."""
        result = convert_tool_names("using TaskCreate")
        self.assertEqual(result, "using todowrite")
        self.assertNotIn("taskCreate", result)


# -----------------------------------------------------------------------
# Skill chaining conversion
# -----------------------------------------------------------------------
class TestConvertSkillCalls(unittest.TestCase):
    """Verify Skill(...) call conversion."""

    def test_skill_with_args(self) -> None:
        text = "invoke `Skill(skill: 'zing:plan', args: '.zing/recipe-app.md')` to continue"
        expected = 'invoke `skill({ name: "zing-plan", args: ".zing/recipe-app.md" })` to continue'
        self.assertEqual(convert_skill_calls(text), expected)

    def test_skill_without_args(self) -> None:
        text = "invoke `Skill(skill: 'zing:build-audit')` to start"
        expected = 'invoke `skill({ name: "zing-build-audit" })` to start'
        self.assertEqual(convert_skill_calls(text), expected)

    def test_skill_name_colon_to_dash(self) -> None:
        """Colon in skill name is converted to dash."""
        text = "`Skill(skill: 'zing:plan-linear')`"
        expected = '`skill({ name: "zing-plan-linear" })`'
        self.assertEqual(convert_skill_calls(text), expected)

    def test_skill_without_backticks(self) -> None:
        """Skill call without surrounding backticks is still converted."""
        text = "Skill(skill: 'zing:build-audit')"
        expected = '`skill({ name: "zing-build-audit" })`'
        self.assertEqual(convert_skill_calls(text), expected)

    def test_skill_with_args_without_backticks(self) -> None:
        text = "Skill(skill: 'zing:build', args: '.zing/app.md')"
        expected = '`skill({ name: "zing-build", args: ".zing/app.md" })`'
        self.assertEqual(convert_skill_calls(text), expected)

    def test_multiple_skill_calls(self) -> None:
        """Multiple skill calls in the same text are all converted."""
        text = (
            "First `Skill(skill: 'zing:plan')` then "
            "`Skill(skill: 'zing:build', args: 'x.md')`"
        )
        expected = (
            'First `skill({ name: "zing-plan" })` then '
            '`skill({ name: "zing-build", args: "x.md" })`'
        )
        self.assertEqual(convert_skill_calls(text), expected)

    def test_skill_call_with_plain_name(self) -> None:
        """Skill name without 'zing:' prefix still works."""
        text = "`Skill(skill: 'something-else')`"
        expected = '`skill({ name: "something-else" })`'
        self.assertEqual(convert_skill_calls(text), expected)


# -----------------------------------------------------------------------
# Task invocation conversion
# -----------------------------------------------------------------------
class TestConvertTaskInvocations(unittest.TestCase):
    """Verify Task subagent parameter conversion."""

    def test_subagent_type_with_equals(self) -> None:
        text = 'Launch Task with subagent_type="general-purpose" and prompt'
        expected = 'Launch Task with agent="general" and prompt'
        self.assertEqual(convert_task_invocations(text), expected)

    def test_subagent_type_with_colon(self) -> None:
        text = 'subagent_type: "general-purpose"'
        expected = 'agent="general"'
        self.assertEqual(convert_task_invocations(text), expected)

    def test_no_match_leaves_text_unchanged(self) -> None:
        text = "No subagent stuff here."
        self.assertEqual(convert_task_invocations(text), text)


# -----------------------------------------------------------------------
# Path reference conversion
# -----------------------------------------------------------------------
class TestConvertPathReferences(unittest.TestCase):
    """Verify Claude Code -> OpenCode path rewriting."""

    def test_shared_path(self) -> None:
        text = "Read `~/.claude/commands/zing/_shared/review-core.md`"
        expected = "Read `~/.config/opencode/commands/_shared/review-core.md`"
        self.assertEqual(convert_path_references(text), expected)

    def test_subcommand_path(self) -> None:
        text = "See `~/.claude/commands/zing/build.md`"
        expected = "See `~/.config/opencode/commands/zing-build.md`"
        self.assertEqual(convert_path_references(text), expected)

    def test_top_level_zing_path(self) -> None:
        text = "The file `~/.claude/commands/zing.md`"
        expected = "The file `~/.config/opencode/commands/zing.md`"
        self.assertEqual(convert_path_references(text), expected)

    def test_shared_path_preserves_filename(self) -> None:
        text = "~/.claude/commands/zing/_shared/something-else.md"
        expected = "~/.config/opencode/commands/_shared/something-else.md"
        self.assertEqual(convert_path_references(text), expected)

    def test_subcommand_with_dashes(self) -> None:
        text = "~/.claude/commands/zing/plan-audit.md"
        expected = "~/.config/opencode/commands/zing-plan-audit.md"
        self.assertEqual(convert_path_references(text), expected)

    def test_multiple_paths_in_text(self) -> None:
        text = (
            "Compare `~/.claude/commands/zing/build.md` "
            "with `~/.claude/commands/zing/_shared/review-core.md`"
        )
        expected = (
            "Compare `~/.config/opencode/commands/zing-build.md` "
            "with `~/.config/opencode/commands/_shared/review-core.md`"
        )
        self.assertEqual(convert_path_references(text), expected)

    def test_no_match_leaves_text_unchanged(self) -> None:
        text = "Some random text with no paths"
        self.assertEqual(convert_path_references(text), text)


# -----------------------------------------------------------------------
# Full document conversion (convert_for_opencode)
# -----------------------------------------------------------------------
class TestConvertForOpencode(unittest.TestCase):
    """Verify the main entry point applies all conversions correctly."""

    def test_full_document_conversion(self) -> None:
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

        self.assertEqual(convert_for_opencode(input_text), expected)

    def test_skill_calls_before_tool_names(self) -> None:
        """Skill calls are converted before tool name replacement so
        the Skill name in the call pattern isn't mangled."""
        text = "`Skill(skill: 'zing:build')` and use the Skill tool"
        result = convert_for_opencode(text)
        # The Skill() call should be fully converted.
        self.assertIn('skill({ name: "zing-build" })', result)
        # The standalone "Skill" tool reference should become "skill".
        self.assertIn("the skill tool", result)

    def test_idempotent_on_already_converted(self) -> None:
        """Running conversion on already-converted text should not break it."""
        text = 'Use the read tool and `skill({ name: "zing-plan" })`.'
        result = convert_for_opencode(text)
        self.assertEqual(result, text)

    def test_empty_string(self) -> None:
        self.assertEqual(convert_for_opencode(""), "")

    def test_no_convertible_content(self) -> None:
        text = "Just some plain text with no tools or paths."
        self.assertEqual(convert_for_opencode(text), text)

    def test_word_boundaries_in_full_conversion(self) -> None:
        """Ensure word boundaries are respected in the full pipeline."""
        text = "Reading and Editing are gerunds, not tools."
        self.assertEqual(convert_for_opencode(text), text)


if __name__ == "__main__":
    unittest.main()
