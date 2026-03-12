"""Tests for Zing server rendering: finding fragments, markdown filter, and enum validation."""

from __future__ import annotations

import unittest
from unittest.mock import patch

from zing_ai.server.models import (
    ChoiceFinding,
    ChoiceOption,
    CriterionRating,
    EvaluationFinding,
    LitmusTest,
    Location,
    TextFinding,
    TriageFinding,
    WarningSign,
)
from zing_ai.server.routes import finding_fragment

from tests.test_server_base import ServerTestBase


class TestFindingFragment(unittest.TestCase):
    """Tests for the finding_fragment() template renderer."""

    def test_text_finding_renders_textarea(self) -> None:
        """Text finding renders with a textarea and data-bind."""
        finding = TextFinding(id="txt1", title="What do you think?", body="Some **markdown**")
        html = finding_fragment(finding, "test-session")
        self.assertIn("finding-txt1", html)
        self.assertIn("<textarea", html)
        self.assertIn('data-bind="responses.txt1"', html)
        self.assertIn("What do you think?", html)

    def test_text_finding_with_context(self) -> None:
        """Text finding renders context when provided."""
        finding = TextFinding(id="txt2", title="Your thoughts?", context="Some context")
        html = finding_fragment(finding, "test-session")
        self.assertIn("Some context", html)

    def test_text_finding_with_markdown_body(self) -> None:
        """Text finding with markdown body renders HTML in finding-body div."""
        finding = TextFinding(
            id="txt3",
            title="Architecture review",
            body="Using **bold** and `inline code` in the body.",
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("finding-body", html)
        self.assertIn("<strong>bold</strong>", html)
        self.assertIn("<code>inline code</code>", html)

    def test_choice_finding_renders_radio_buttons(self) -> None:
        """Choice finding renders radio buttons with data-bind for each option."""
        finding = ChoiceFinding(
            id="ch1",
            title="Pick one",
            body="Some context about the choice",
            options=[
                ChoiceOption(label="A", description="Option A"),
                ChoiceOption(label="B", description="Option B"),
            ],
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("finding-ch1", html)
        self.assertIn('type="radio"', html)
        self.assertIn('data-bind="responses.ch1"', html)
        self.assertIn("Option A", html)
        self.assertIn("Option B", html)
        # Should have skip and other options
        self.assertIn('value="skip"', html)
        self.assertIn('value="__other__"', html)

    def test_choice_finding_has_other_textarea(self) -> None:
        """Choice finding renders an 'Other' option with conditional textarea."""
        finding = ChoiceFinding(
            id="ch2",
            title="Pick one",
            options=[ChoiceOption(label="A", description="Option A")],
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("__other__", html)
        self.assertIn("data-show", html)
        self.assertIn("ch2_other", html)

    def test_choice_finding_preserves_context(self) -> None:
        """ChoiceFinding stores context field when provided."""
        finding = ChoiceFinding(
            id="ch_ctx",
            title="Improvement suggestion",
            body="Consider this change",
            context="Referenced in plan section 3.2",
            options=[
                ChoiceOption(label="A", description="Option A"),
                ChoiceOption(label="B", description="Option B"),
            ],
        )
        self.assertEqual(finding.context, "Referenced in plan section 3.2")

    def test_choice_finding_renders_context(self) -> None:
        """Choice finding renders context when provided."""
        finding = ChoiceFinding(
            id="ch_ctx2",
            title="Pick an approach",
            context="From plan section: Error Handling",
            options=[
                ChoiceOption(label="A", description="Option A"),
            ],
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("From plan section: Error Handling", html)

    def test_triage_finding_renders_action_buttons(self) -> None:
        """Triage finding renders action buttons with data-bind."""
        finding = TriageFinding(
            id="tri1",
            title="Unused import os",
            body="The `os` module is imported but **never used**.",
            category="style",
            severity="low",
            confidence="high",
            location=Location(file="src/main.py", line=5),
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("finding-tri1", html)
        self.assertIn("Unused import os", html)
        self.assertIn("src/main.py:5", html)
        self.assertIn("low", html)
        self.assertIn("high", html)
        # Body renders as HTML (markdown processed)
        self.assertIn("<strong>never used</strong>", html)
        # Action buttons
        self.assertIn("accept", html)
        self.assertIn("drop", html)
        self.assertIn("downgrade", html)
        self.assertIn("discuss", html)
        self.assertIn("data-class:selected=\"$responses.tri1 === 'accept'\"", html)


    def test_evaluation_finding_renders_tables(self) -> None:
        """Evaluation finding renders structured tables with badges."""
        finding = EvaluationFinding(
            id="eval1",
            title="Pass 1: Design Fundamentals",
            criteria=[
                CriterionRating(name="Clarity", rating="strong", justification="Very clear"),
                CriterionRating(name="YAGNI", rating="weak", justification="Over-engineered"),
            ],
            litmus_tests=[
                LitmusTest(name="Simplest thing?", result="Could be simpler"),
            ],
            warnings=[
                WarningSign(name="Future flexibility", found=True, details="Plugin system"),
                WarningSign(name="Only one approach", found=False),
            ],
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("finding-eval1", html)
        self.assertIn("Pass 1: Design Fundamentals", html)
        self.assertIn("eval-table", html)
        # Criteria
        self.assertIn("Clarity", html)
        self.assertIn("badge-strong", html)
        self.assertIn("Very clear", html)
        self.assertIn("YAGNI", html)
        self.assertIn("badge-weak", html)
        # Litmus tests
        self.assertIn("Simplest thing?", html)
        self.assertIn("Could be simpler", html)
        # Warnings
        self.assertIn("Future flexibility", html)
        self.assertIn("badge-warn-yes", html)
        self.assertIn("Plugin system", html)
        # Informational meta
        self.assertIn("Informational", html)
        # No input controls
        self.assertNotIn("<textarea", html)
        self.assertNotIn('type="radio"', html)

    def test_evaluation_finding_without_optional_sections(self) -> None:
        """Evaluation finding with only criteria renders no litmus/warning tables."""
        finding = EvaluationFinding(
            id="eval2",
            title="Pass 4: Code Quality",
            criteria=[
                CriterionRating(name="Code Quality", rating="adequate", justification="Decent"),
            ],
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("Code Quality", html)
        self.assertIn("badge-adequate", html)
        # Should not render litmus or warning tables
        self.assertNotIn("Litmus Test", html)
        self.assertNotIn("Warning Sign", html)


class TestMarkdownFilter(unittest.TestCase):
    """Tests for the _render_markdown Jinja2 filter."""

    def test_renders_basic_markdown(self) -> None:
        """Basic markdown with bold and italic renders to HTML tags."""
        from zing_ai.server.templates import _render_markdown

        result = _render_markdown("**bold** and *italic*")
        self.assertIn("<strong>", result)
        self.assertIn("<em>", result)

    def test_renders_code_blocks(self) -> None:
        """Fenced code blocks render with Pygments syntax highlighting."""
        from zing_ai.server.templates import _render_markdown

        md = "```python\ndef hello():\n    return 42\n```"
        result = _render_markdown(md)
        self.assertIn('<div class="highlight">', result)
        self.assertIn("<span", result)

    def test_empty_input(self) -> None:
        """Empty string input returns empty string without error."""
        from zing_ai.server.templates import _render_markdown

        result = _render_markdown("")
        self.assertEqual(result, "")

    def test_returns_markup(self) -> None:
        """Return type is markupsafe.Markup, not plain str."""
        import markupsafe

        from zing_ai.server.templates import _render_markdown

        result = _render_markdown("hello")
        self.assertIsInstance(result, markupsafe.Markup)

    def test_fallback_on_render_error(self) -> None:
        """If mistune raises, filter falls back to escaped text in <pre> tags."""
        from zing_ai.server.templates import _render_markdown

        with patch("zing_ai.server.templates._markdown", side_effect=Exception("boom")):
            result = _render_markdown("<script>alert('xss')</script>")
        self.assertIn("<pre>", result)
        self.assertIn("&lt;script&gt;", result)
        self.assertNotIn("<script>", result)


class TestTriageEnumValidation(ServerTestBase):
    """Tests for StrEnum validation on triage findings (via manager)."""

    def test_enum_validation_rejects_invalid_severity(self) -> None:
        """Triage with invalid severity raises ValidationError."""
        from pydantic import ValidationError

        self._create_session()
        with self.assertRaises(ValidationError):
            self.manager.add_finding(
                "test-session", self.step_id,
                {
                    "type": "triage", "title": "Some finding",
                    "category": "style", "severity": "invalid", "confidence": "high",
                },
            )

    def test_enum_validation_rejects_invalid_confidence(self) -> None:
        """Triage with invalid confidence raises ValidationError."""
        from pydantic import ValidationError

        self._create_session()
        with self.assertRaises(ValidationError):
            self.manager.add_finding(
                "test-session", self.step_id,
                {
                    "type": "triage", "title": "Some finding",
                    "category": "style", "severity": "low", "confidence": "invalid",
                },
            )

    def test_enum_validation_rejects_invalid_category(self) -> None:
        """Triage with invalid category raises ValidationError."""
        from pydantic import ValidationError

        self._create_session()
        with self.assertRaises(ValidationError):
            self.manager.add_finding(
                "test-session", self.step_id,
                {
                    "type": "triage", "title": "Some finding",
                    "category": "invalid", "severity": "low", "confidence": "high",
                },
            )

    def test_structured_location(self) -> None:
        """Triage with structured location stores and renders correctly."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session", self.step_id,
            {
                "type": "triage", "title": "Missing null check",
                "category": "correctness", "severity": "high", "confidence": "high",
                "location": {"file": "src/main.py", "line": 42},
            },
        )
        assert finding.location is not None
        self.assertEqual(finding.location.file, "src/main.py")
        self.assertEqual(finding.location.line, 42)
        html = finding_fragment(finding, "test-session")
        self.assertIn("src/main.py:42", html)

    def test_location_without_line(self) -> None:
        """Triage with location without line works correctly."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session", self.step_id,
            {
                "type": "triage", "title": "File-level issue",
                "category": "architecture", "severity": "medium", "confidence": "medium",
                "location": {"file": "src/main.py"},
            },
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("src/main.py", html)
        self.assertNotIn(":null", html)
        self.assertNotIn(":None", html)

    def test_triage_with_options(self) -> None:
        """Triage with options renders both action buttons and options text."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session", self.step_id,
            {
                "type": "triage", "title": "Consider refactoring",
                "category": "readability", "severity": "medium", "confidence": "medium",
                "options": [
                    {"label": "Extract method", "description": "Pull the loop into a helper"},
                    {"label": "Inline comments", "description": "Add comments to clarify intent"},
                ],
            },
        )
        html = finding_fragment(finding, "test-session")
        # Action buttons still present
        self.assertIn("accept", html)
        self.assertIn("drop", html)
        # Options rendered
        self.assertIn("triage-options", html)
        self.assertIn("Suggested approaches:", html)
        self.assertIn("Extract method", html)
        self.assertIn("Pull the loop into a helper", html)
        self.assertIn("Inline comments", html)

    def test_triage_without_options(self) -> None:
        """Triage without options renders only action buttons, no options div."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session", self.step_id,
            {
                "type": "triage", "title": "Simple finding",
                "category": "style", "severity": "low", "confidence": "high",
            },
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("accept", html)
        self.assertNotIn("triage-options", html)

    def test_info_severity_badge(self) -> None:
        """Triage with severity info renders badge-info class."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session", self.step_id,
            {
                "type": "triage", "title": "Informational note",
                "category": "architecture", "severity": "info", "confidence": "low",
            },
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("badge-info", html)
