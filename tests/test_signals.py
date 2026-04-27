"""Unit tests for the Datastar signal-key sanitiser and the js_str template filter."""

from __future__ import annotations

import unittest

from zing_ai.server.models import ZingSession
from zing_ai.server.models_external import KanbanCard, LinearIssue
from zing_ai.server.signals import to_signal_key
from zing_ai.server.templates import _js_str


class TestToSignalKey(unittest.TestCase):
    """Tests for to_signal_key() — the canonical Datastar signal-name sanitiser."""

    def test_hyphens_replaced_with_underscores(self) -> None:
        self.assertEqual(to_signal_key("BAK-1234"), "BAK_1234")

    def test_dots_replaced_with_underscores(self) -> None:
        # If a Linear team prefix ever contains a dot, the template must not
        # treat it as a nested signal access.
        self.assertEqual(to_signal_key("FOO.BAR"), "FOO_BAR")

    def test_slashes_replaced(self) -> None:
        self.assertEqual(to_signal_key("owner/repo"), "owner_repo")

    def test_alphanumeric_preserved(self) -> None:
        self.assertEqual(to_signal_key("abc123"), "abc123")

    def test_underscore_preserved(self) -> None:
        self.assertEqual(to_signal_key("a_b_c"), "a_b_c")

    def test_unicode_collapses_to_underscores(self) -> None:
        # Non-ASCII characters fall outside [A-Za-z0-9_] and should collapse.
        self.assertEqual(to_signal_key("café"), "caf_")

    def test_empty_string_round_trips(self) -> None:
        self.assertEqual(to_signal_key(""), "")


class TestSignalKeyProperty(unittest.TestCase):
    """Tests for the .signal_key property on KanbanCard / LinearIssue / SessionBase."""

    def test_kanban_card_uses_to_signal_key(self) -> None:
        card = KanbanCard(key="BAK-1234")
        self.assertEqual(card.signal_key, "BAK_1234")

    def test_linear_issue_uses_to_signal_key(self) -> None:
        issue = LinearIssue(
            id="uuid-1",
            identifier="BAK-1234",
            title="t",
            state="In Progress",
            assignee=None,
            team=None,
            url="http://example/issue/1",
            updated_at="2026-01-01T00:00:00Z",  # type: ignore[arg-type]
        )
        self.assertEqual(issue.signal_key, "BAK_1234")

    def test_session_base_uses_to_signal_key(self) -> None:
        session = ZingSession(session_id="sess-abc-123", title="t")
        self.assertEqual(session.signal_key, "sess_abc_123")


class TestJsStrFilter(unittest.TestCase):
    """Tests for the js_str Jinja filter — the safe encoder for JS literals in HTML attrs.

    The output is a markupsafe.Markup whose `"` are HTML-escaped to `&quot;` so
    the result can sit safely inside a double-quoted HTML attribute. The
    browser then decodes `&quot;` back to `"` inside attribute parsing only,
    leaving the JS literal intact.
    """

    def test_plain_string_renders_with_html_escaped_quotes(self) -> None:
        self.assertEqual(str(_js_str("hello")), "&quot;hello&quot;")

    def test_single_quote_does_not_break_surrounding_attribute(self) -> None:
        # `'` is the historic foot-gun: `| e` would render `&#39;` which the
        # browser decodes back to `'` inside attribute parsing, terminating
        # any single-quoted JS literal early. js_str sidesteps that by
        # producing a double-quoted literal — and html.escape() encodes both
        # `"` (delimiters) and the inner `'` so the value sits safely inside
        # any-quote-style HTML attribute.
        out = str(_js_str("Refactor 'foo'"))
        self.assertEqual(out, "&quot;Refactor &#x27;foo&#x27;&quot;")

    def test_double_quote_is_escaped_to_backslash_quote(self) -> None:
        # json.dumps escapes the inner `"` to `\"`, then html.escape leaves
        # backslash alone. The surrounding `"` are HTML-escaped to `&quot;`.
        out = str(_js_str('say "hi"'))
        self.assertEqual(out, "&quot;say \\&quot;hi\\&quot;&quot;")

    def test_backslash_is_escaped(self) -> None:
        out = str(_js_str("a\\b"))
        self.assertEqual(out, "&quot;a\\\\b&quot;")

    def test_html_special_chars_are_html_escaped(self) -> None:
        # html.escape() also handles `<` / `>` / `&` so a value containing
        # `<script>` cannot break out of the surrounding HTML attribute.
        out = str(_js_str("<script>"))
        self.assertEqual(out, "&quot;&lt;script&gt;&quot;")

    def test_none_renders_as_empty_string_literal(self) -> None:
        self.assertEqual(str(_js_str(None)), "&quot;&quot;")

    def test_non_string_coerces_to_str(self) -> None:
        self.assertEqual(str(_js_str(42)), "&quot;42&quot;")


if __name__ == "__main__":
    unittest.main()
