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


# ---------------------------------------------------------------------------
# build_step.md.j2
# ---------------------------------------------------------------------------


class TestBuildStepTemplate:
    """Verify build_step.md.j2 renders with its required variables."""

    TEMPLATE = "build_step.md.j2"

    @pytest.fixture()
    def sample_context(self) -> dict:
        return {
            "zing_overview": "A recipe management app with search and filtering.",
            "step_label": "Step 2.1: Create the data models",
            "step_instructions": "Create SQLAlchemy models for Recipe and Ingredient.",
            "distilled_files": {
                "src/models/recipe.py": "class Recipe: ...",
                "src/models/ingredient.py": "class Ingredient: ...",
            },
            "mcp_mandate": (
                "Use Serena for code exploration, aid for analysis, "
                "CodeGraphContext for architecture."
            ),
        }

    def test_renders_without_error(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert isinstance(result, str)
        assert len(result) > 0

    def test_contains_zing_overview(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "recipe management app" in result

    def test_contains_step_label(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "Step 2.1: Create the data models" in result

    def test_contains_step_instructions(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "SQLAlchemy models for Recipe and Ingredient" in result

    def test_contains_distilled_files(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "src/models/recipe.py" in result
        assert "class Recipe: ..." in result
        assert "src/models/ingredient.py" in result
        assert "class Ingredient: ..." in result

    def test_contains_mcp_mandate(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "Serena for code exploration" in result

    def test_instructs_to_execute_step(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "Follow the step instructions exactly" in result

    def test_instructs_to_run_tests(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "Run tests" in result

    def test_instructs_to_verify_acceptance_criteria(
        self, sample_context: dict
    ) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "acceptance criteria" in result.lower()

    def test_instructs_no_git_commands(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "Do NOT run any git commands" in result

    def test_empty_distilled_files(self, sample_context: dict) -> None:
        sample_context["distilled_files"] = {}
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "Distilled File Context" in result


# ---------------------------------------------------------------------------
# build_audit_group.md.j2
# ---------------------------------------------------------------------------


class TestBuildAuditGroupTemplate:
    """Verify build_audit_group.md.j2 renders with its required variables."""

    TEMPLATE = "build_audit_group.md.j2"

    @pytest.fixture()
    def sample_context(self) -> dict:
        return {
            "file_list": [
                "src/auth.py",
                "src/models/user.py",
                "tests/test_auth.py",
                "src/components/Header.tsx",
            ],
            "distilled_files": {
                "src/auth.py": "def authenticate(token): ...",
                "src/models/user.py": "class User: ...",
                "tests/test_auth.py": "def test_authenticate(): ...",
                "src/components/Header.tsx": "export function Header() { ... }",
            },
        }

    def test_renders_without_error(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert isinstance(result, str)
        assert len(result) > 0

    def test_contains_file_list(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "src/auth.py" in result
        assert "src/models/user.py" in result
        assert "tests/test_auth.py" in result
        assert "src/components/Header.tsx" in result

    def test_contains_distilled_files(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "def authenticate(token): ..." in result
        assert "class User: ..." in result

    def test_instructs_to_group_files(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "Classify each changed file" in result

    def test_contains_four_groups(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "Correctness" in result
        assert "Security & Reliability" in result
        assert "Quality & Style" in result
        assert "Coverage & Performance" in result

    def test_contains_output_format(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "### Group 1: Correctness" in result
        assert "### Group 2: Security & Reliability" in result
        assert "### Group 3: Quality & Style" in result
        assert "### Group 4: Coverage & Performance" in result

    def test_empty_file_list(self, sample_context: dict) -> None:
        sample_context["file_list"] = []
        sample_context["distilled_files"] = {}
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "Changed Files" in result

    def test_default_rule_mentioned(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "Default rule" in result


# ---------------------------------------------------------------------------
# build_audit_review.md.j2
# ---------------------------------------------------------------------------


class TestBuildAuditReviewTemplate:
    """Verify build_audit_review.md.j2 renders with its required variables."""

    TEMPLATE = "build_audit_review.md.j2"

    @pytest.fixture()
    def sample_context(self) -> dict:
        return {
            "zing_content": "A recipe management app with search and filtering.",
            "group_files": [
                "src/models/recipe.py",
                "src/services/search.py",
            ],
            "distilled_code": {
                "src/models/recipe.py": "class Recipe:\n    name: str\n    ingredients: list",
                "src/services/search.py": "def search_recipes(query: str) -> list: ...",
            },
        }

    def test_renders_without_error(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert isinstance(result, str)
        assert len(result) > 0

    def test_contains_zing_content(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "recipe management app" in result

    def test_contains_group_files(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "src/models/recipe.py" in result
        assert "src/services/search.py" in result

    def test_contains_distilled_code(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "class Recipe:" in result
        assert "def search_recipes" in result

    def test_instructs_to_review_and_return_findings(
        self, sample_context: dict
    ) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "FINDING" in result
        assert "NO_FINDINGS" in result

    def test_contains_severity_scale(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "critical" in result
        assert "high" in result
        assert "medium" in result
        assert "low" in result

    def test_contains_confidence_scale(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        # Check confidence levels are described
        result_lower = result.lower()
        assert "confidence" in result_lower

    def test_contains_review_categories(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "Logic Errors and Bugs" in result
        assert "Error Handling" in result
        assert "Security" in result
        assert "Performance" in result

    def test_contains_tone_guidelines(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "senior developer" in result

    def test_contains_pipe_delimited_format(self, sample_context: dict) -> None:
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "FINDING|category|severity|confidence|file_path:line_number|description" in result

    def test_empty_group_files(self, sample_context: dict) -> None:
        sample_context["group_files"] = []
        sample_context["distilled_code"] = {}
        result = render_prompt(self.TEMPLATE, **sample_context)
        assert "Files Assigned to You" in result
