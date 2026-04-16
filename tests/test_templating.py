"""Tests for zing_ai.templating (Jinja rendering pipeline)."""

from __future__ import annotations

import unittest

import jinja2

from zing_ai.config import default_config
from zing_ai.templating import render_template

# ---------------------------------------------------------------------------
# test_simple_substitution
# ---------------------------------------------------------------------------


class TestSimpleSubstitution(unittest.TestCase):
    def test_simple_substitution(self) -> None:
        """Renders a config value into the template string."""
        cfg = default_config()
        result = render_template("x={{ thresholds.large_file_lines }}", cfg)
        self.assertEqual(result, "x=1000")


# ---------------------------------------------------------------------------
# test_undefined_raises
# ---------------------------------------------------------------------------


class TestUndefinedRaises(unittest.TestCase):
    def test_undefined_raises(self) -> None:
        """References to missing keys raise jinja2.UndefinedError."""
        cfg = default_config()
        with self.assertRaises(jinja2.UndefinedError):
            render_template("x={{ does.not.exist }}", cfg)


# ---------------------------------------------------------------------------
# test_idempotent_for_plain_markdown
# ---------------------------------------------------------------------------


class TestIdempotentForPlainMarkdown(unittest.TestCase):
    def test_idempotent_for_plain_markdown(self) -> None:
        """Plain markdown with no template tags is returned unchanged."""
        text = "# heading\nno templates here"
        cfg = default_config()
        result = render_template(text, cfg)
        self.assertEqual(result, text)


# ---------------------------------------------------------------------------
# test_conditional_renders_correct_branch
# ---------------------------------------------------------------------------


class TestConditionalRendersCorrectBranch(unittest.TestCase):
    def test_conditional_branch_mode(self) -> None:
        """Conditional renders 'A' when workflow_mode is 'branch' (default)."""
        cfg = default_config()
        template = "{% if git.workflow_mode == 'branch' %}A{% else %}B{% endif %}"
        result = render_template(template, cfg)
        self.assertEqual(result, "A")

    def test_conditional_worktree_mode(self) -> None:
        """Conditional renders 'B' when workflow_mode is 'worktree'."""
        cfg = default_config()
        cfg.git.workflow_mode = "worktree"
        template = "{% if git.workflow_mode == 'branch' %}A{% else %}B{% endif %}"
        result = render_template(template, cfg)
        self.assertEqual(result, "B")


if __name__ == "__main__":
    unittest.main()
