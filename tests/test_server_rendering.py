"""Tests for Zing server rendering: finding fragments, markdown filter, and enum validation."""

from __future__ import annotations

import unittest
from datetime import datetime
from unittest.mock import patch

from tests.test_server_base import ServerTestBase
from zing_ai.server.models import (
    Category,
    ChoiceOption,
    Confidence,
    CriterionRating,
    EvaluationFinding,
    LitmusTest,
    Location,
    Notification,
    Rating,
    Session,
    Severity,
    TextFinding,
    TriageFinding,
    WarningSign,
    ZingSession,
)
from zing_ai.server.routes import finding_fragment
from zing_ai.server.templates import render


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

    def test_triage_finding_without_metadata_hides_meta_bar(self) -> None:
        """Triage finding without category/severity/confidence hides the meta bar."""
        finding = TriageFinding(
            type="triage",
            title="Design question",
            body="Should we use pattern X?",
            options=[ChoiceOption(label="A", description="Use pattern X")],
            category=None,
            severity=None,
            confidence=None,
        )
        html = finding_fragment(finding, "test-session")
        # Meta bar should NOT be present
        self.assertNotIn("finding-meta", html)
        # Action buttons should be present
        self.assertIn("action-btn", html)
        self.assertIn("accept", html)
        self.assertIn("drop", html)
        self.assertIn("downgrade", html)
        self.assertIn("discuss", html)
        # Complexity selector should be present
        self.assertIn("complexity-selector", html)
        # Suggested approaches should be present
        self.assertIn("triage-options", html)
        self.assertIn("Suggested approaches:", html)
        self.assertIn("Use pattern X", html)

    def test_triage_finding_renders_action_buttons(self) -> None:
        """Triage finding renders action buttons with data-bind."""
        finding = TriageFinding(
            id="tri1",
            title="Unused import os",
            body="The `os` module is imported but **never used**.",
            category=Category.STYLE,
            severity=Severity.LOW,
            confidence=Confidence.HIGH,
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
        # Bracket-form access — js_str renders the finding-id as &quot;tri1&quot;
        # inside the attribute so the JS literal survives HTML attribute parsing.
        self.assertIn("data-class:selected=\"$responses[&quot;tri1&quot;] === 'accept'\"", html)

    def test_evaluation_finding_renders_tables(self) -> None:
        """Evaluation finding renders structured tables with badges."""
        finding = EvaluationFinding(
            id="eval1",
            title="Pass 1: Design Fundamentals",
            criteria=[
                CriterionRating(name="Clarity", rating=Rating.STRONG, justification="Very clear"),
                CriterionRating(name="YAGNI", rating=Rating.WEAK, justification="Over-engineered"),
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
                CriterionRating(
                    name="Code Quality",
                    rating=Rating.ADEQUATE,
                    justification="Decent",
                ),
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
                "test-session",
                self.step_id,
                {
                    "type": "triage",
                    "title": "Some finding",
                    "category": "style",
                    "severity": "invalid",
                    "confidence": "high",
                },
            )

    def test_enum_validation_rejects_invalid_confidence(self) -> None:
        """Triage with invalid confidence raises ValidationError."""
        from pydantic import ValidationError

        self._create_session()
        with self.assertRaises(ValidationError):
            self.manager.add_finding(
                "test-session",
                self.step_id,
                {
                    "type": "triage",
                    "title": "Some finding",
                    "category": "style",
                    "severity": "low",
                    "confidence": "invalid",
                },
            )

    def test_enum_validation_rejects_invalid_category(self) -> None:
        """Triage with invalid category raises ValidationError."""
        from pydantic import ValidationError

        self._create_session()
        with self.assertRaises(ValidationError):
            self.manager.add_finding(
                "test-session",
                self.step_id,
                {
                    "type": "triage",
                    "title": "Some finding",
                    "category": "invalid",
                    "severity": "low",
                    "confidence": "high",
                },
            )

    def test_structured_location(self) -> None:
        """Triage with structured location stores and renders correctly."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Missing null check",
                "category": "correctness",
                "severity": "high",
                "confidence": "high",
                "location": {"file": "src/main.py", "line": 42},
            },
        )
        assert isinstance(finding, TriageFinding)
        assert finding.location is not None
        self.assertEqual(finding.location.file, "src/main.py")
        self.assertEqual(finding.location.line, 42)
        html = finding_fragment(finding, "test-session")
        self.assertIn("src/main.py:42", html)

    def test_location_without_line(self) -> None:
        """Triage with location without line works correctly."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "File-level issue",
                "category": "architecture",
                "severity": "medium",
                "confidence": "medium",
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
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Consider refactoring",
                "category": "readability",
                "severity": "medium",
                "confidence": "medium",
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
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Simple finding",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("accept", html)
        self.assertNotIn("triage-options", html)

    def test_info_severity_badge(self) -> None:
        """Triage with severity info renders badge-info class."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Informational note",
                "category": "architecture",
                "severity": "info",
                "confidence": "low",
            },
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("badge-info", html)

    def test_triage_finding_renders_complexity_selector(self) -> None:
        """Triage finding renders complexity selector with three buttons."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Needs refactoring",
                "category": "readability",
                "severity": "medium",
                "confidence": "high",
            },
        )
        html = finding_fragment(finding, "test-session")
        self.assertIn("complexity-selector", html)
        self.assertIn("complexity-btn", html)
        self.assertIn('data-complexity="simple"', html)
        self.assertIn('data-complexity="standard"', html)
        self.assertIn('data-complexity="complex"', html)
        # Labels
        self.assertIn("Simple", html)
        self.assertIn("Standard", html)
        self.assertIn("Complex", html)

    def test_triage_finding_complexity_default_standard(self) -> None:
        """Triage finding defaults to 'standard' complexity in its data-signals."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Default complexity",
                "category": "style",
                "severity": "low",
                "confidence": "high",
            },
        )
        html = finding_fragment(finding, "test-session")
        # The data-signals attribute should contain the default complexity value
        self.assertIn("_complexity", html)
        self.assertIn("standard", html)

    def test_triage_finding_custom_complexity_in_signals(self) -> None:
        """Triage finding with explicit complexity renders that value in data-signals."""
        self._create_session()
        finding = self.manager.add_finding(
            "test-session",
            self.step_id,
            {
                "type": "triage",
                "title": "Complex fix",
                "category": "architecture",
                "severity": "high",
                "confidence": "high",
                "complexity": "complex",
            },
        )
        html = finding_fragment(finding, "test-session")
        # The data-signals should contain the specified complexity
        self.assertIn('"complex"', html)


class TestNotificationTimeline(unittest.TestCase):
    """Tests for the notification_timeline.html template fragment."""

    def _make_session(self, notifications: list[Notification] | None = None) -> Session:
        """Create a minimal Session object for template rendering."""
        return ZingSession(
            session_id="notif-test",
            title="Test Session",
            notifications=notifications or [],
        )

    def test_notification_timeline_with_notifications(self) -> None:
        """Timeline renders notification entries when notifications exist."""
        notifications = [
            Notification(
                title="Session created",
                body="Your session is ready",
                created_at=datetime(2026, 3, 14, 10, 30),
            ),
            Notification(
                title="Step ready",
                body="Review step is ready",
                created_at=datetime(2026, 3, 14, 11, 45),
            ),
        ]
        session = self._make_session(notifications=notifications)
        html = render("fragments/notification_timeline.html", s=session)
        self.assertIn('id="notifications-notif-test"', html)
        self.assertIn("notification-entry", html)
        self.assertIn("Session created", html)
        self.assertIn("Step ready", html)

    def test_notification_timeline_empty(self) -> None:
        """Empty notifications list renders the container div with no entries."""
        session = self._make_session(notifications=[])
        html = render("fragments/notification_timeline.html", s=session)
        self.assertIn('id="notifications-notif-test"', html)
        self.assertIn("notification-timeline", html)
        self.assertNotIn("notification-entry", html)

    def test_notification_timeline_correct_time_and_title(self) -> None:
        """Timeline entries display the correct formatted time and title."""
        notifications = [
            Notification(
                title="Build started",
                created_at=datetime(2026, 3, 14, 9, 5),
            ),
        ]
        session = self._make_session(notifications=notifications)
        html = render("fragments/notification_timeline.html", s=session)
        self.assertIn("notification-time", html)
        self.assertIn("09:05", html)
        self.assertIn("notification-title", html)
        self.assertIn("Build started", html)


class TestRepoChooserModal(unittest.TestCase):
    """Tests for the repo_chooser_modal.html fragment."""

    def test_repo_chooser_modal_renders_standalone(self) -> None:
        """repo_chooser_modal renders heading, repo buttons, and cancel standalone."""
        repos = [
            {"path": "owner/repo1", "label": "owner/repo1"},
            {"path": "owner/repo2", "label": "owner/repo2"},
        ]
        html = render("fragments/repo_chooser_modal.html", card_key="TKT-1", repos=repos)
        # Heading present
        self.assertIn("Choose a repo", html)
        # Both repo labels rendered
        self.assertIn("owner/repo1", html)
        self.assertIn("owner/repo2", html)
        # Datastar post calls present
        self.assertIn("@post('/command-center/launch-background'", html)
        # card_key interpolated for both buttons via js_str (renders as
        # double-quoted JS string literal with HTML-escaped quotes so the
        # surrounding double-quoted data-on:click attribute stays well-formed)
        self.assertEqual(html.count("card_key: &quot;TKT-1&quot;"), 2)
        # repo paths interpolated
        self.assertIn("repo: &quot;owner/repo1&quot;", html)
        self.assertIn("repo: &quot;owner/repo2&quot;", html)
        # Cancel button closes repoChooser signal
        self.assertIn("$modals.repoChooser = false", html)
