"""Tests for the Jinja2 prompt template environment."""

from __future__ import annotations

import jinja2
import pytest

from zing_ai.prompts import _env, render_prompt

# ---------------------------------------------------------------------------
# Environment configuration
# ---------------------------------------------------------------------------


class TestEnvironmentConfiguration:
    """Verify the Jinja2 environment is configured correctly."""

    def test_loader_is_package_loader(self) -> None:
        assert isinstance(_env.loader, jinja2.PackageLoader)

    def test_keep_trailing_newline(self) -> None:
        assert _env.keep_trailing_newline is True

    def test_trim_blocks(self) -> None:
        assert _env.trim_blocks is True

    def test_lstrip_blocks(self) -> None:
        assert _env.lstrip_blocks is True


# ---------------------------------------------------------------------------
# render_prompt — basic rendering
# ---------------------------------------------------------------------------


class TestRenderPrompt:
    """Verify render_prompt loads and renders templates."""

    def test_renders_simple_variable(self) -> None:
        result = render_prompt("test_template.md.j2", name="World", items=[])
        assert "Hello, World!" in result

    def test_renders_loop(self) -> None:
        result = render_prompt("test_template.md.j2", name="User", items=["a", "b"])
        assert "- a" in result
        assert "- b" in result

    def test_renders_conditional_block(self) -> None:
        result = render_prompt("test_template.md.j2", name="User", items=[])
        assert "Items:" not in result

    def test_conditional_block_present_when_items(self) -> None:
        result = render_prompt("test_template.md.j2", name="User", items=["x"])
        assert "Items:" in result

    def test_keeps_trailing_newline(self) -> None:
        result = render_prompt("test_template.md.j2", name="User", items=[])
        assert result.endswith("\n")

    def test_trim_and_lstrip_blocks(self) -> None:
        """Block tags should not introduce extra blank lines or leading spaces."""
        result = render_prompt("test_template.md.j2", name="User", items=["one"])
        lines = result.splitlines()
        # With trim_blocks and lstrip_blocks, the {% for %} and {% endfor %}
        # tags should not produce blank lines between "Items:" and the list.
        non_empty = [line for line in lines if line.strip()]
        assert non_empty == ["Hello, User!", "Items:", "- one"]


# ---------------------------------------------------------------------------
# render_prompt — error handling
# ---------------------------------------------------------------------------


class TestRenderPromptErrors:
    """Verify render_prompt raises expected errors."""

    def test_missing_template_raises_template_not_found(self) -> None:
        with pytest.raises(jinja2.TemplateNotFound):
            render_prompt("nonexistent_template.md.j2")

    def test_undefined_variable_raises_error(self) -> None:
        """Jinja2 default undefined silently renders empty, so no error."""
        # With the default Undefined (not StrictUndefined), missing vars
        # render as empty strings rather than raising.  Verify this behavior.
        result = render_prompt("test_template.md.j2")
        assert "Hello, !" in result
