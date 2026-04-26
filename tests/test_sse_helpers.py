"""Unit tests for sse_helpers.py — sse_toast and sse_btn_state."""

from __future__ import annotations

import json
import unittest

from zing_ai.server.sse_helpers import sse_btn_state, sse_toast


class TestSseToast(unittest.TestCase):
    """Tests for sse_toast()."""

    def test_html_escaping_prevents_script_injection(self) -> None:
        """Message containing <script> is HTML-escaped in the SSE output."""
        result = sse_toast('<script>alert("xss")</script>', kind="err")
        self.assertIn("&lt;script&gt;", result)
        self.assertNotIn("<script>", result)

    def test_toast_id_honored_when_supplied(self) -> None:
        """When toast_id is provided, the id attribute matches it exactly."""
        result = sse_toast("hello", toast_id="my-custom-id")
        self.assertIn('id="my-custom-id"', result)

    def test_toast_id_auto_generated_with_prefix_when_not_supplied(self) -> None:
        """When toast_id is omitted, an id starting with 'toast-' is generated."""
        result = sse_toast("hello")
        self.assertRegex(result, r'id="toast-[0-9a-f]{8}"')

    def test_kind_produces_correct_css_class(self) -> None:
        """Each kind value produces the corresponding cc-toast-{kind} CSS class."""
        for kind in ("ok", "err", "info"):
            with self.subTest(kind=kind):
                result = sse_toast("msg", kind=kind)  # type: ignore[arg-type]
                self.assertIn(f"cc-toast-{kind}", result)

    def test_returns_valid_sse_patch_elements_event_with_correct_selector_and_mode(
        self,
    ) -> None:
        """Result is a valid SSE patch-elements event with APPEND mode and correct selector."""
        result = sse_toast("test message")
        self.assertIn("event: datastar-patch-elements", result)
        self.assertIn("data: selector #cc-toast-container", result)
        self.assertIn("data: mode append", result)


class TestSseBtnState(unittest.TestCase):
    """Tests for sse_btn_state()."""

    def test_html_escaping_prevents_label_injection(self) -> None:
        """Label containing <script> is HTML-escaped in the SSE output."""
        result = sse_btn_state("btn-1", '<script>alert("xss")</script>', kind="ok")
        self.assertIn("&lt;script&gt;", result)
        self.assertNotIn("<script>", result)

    def test_kind_produces_correct_css_class(self) -> None:
        """Each kind value produces the correct btn CSS classes."""
        expected = {
            "idle": "btn",
            "ok": "btn btn-ok",
            "err": "btn btn-err",
        }
        for kind, cls in expected.items():
            with self.subTest(kind=kind):
                result = sse_btn_state("btn-1", "label", kind=kind)  # type: ignore[arg-type]
                self.assertIn(f'class="{cls}"', result)

    def test_reset_html_produces_button_with_delay_attribute(self) -> None:
        """reset_html embeds JSON-encoded markup into a data-on-load__delay.{ms}ms attribute."""
        original_markup = '<button id="btn-1" class="btn">Click me</button>'
        result = sse_btn_state(
            "btn-1", "Done", kind="ok", reset_html=original_markup, reset_after_ms=2000
        )
        expected_attr_prefix = "data-on-load__delay.2000ms="
        self.assertIn(expected_attr_prefix, result)
        # The reset markup must be JSON-encoded inside the attribute
        json_encoded = json.dumps(original_markup)
        self.assertIn(json_encoded, result)

    def test_reset_html_respects_custom_delay(self) -> None:
        """reset_after_ms controls the delay value in the attribute name."""
        result = sse_btn_state(
            "btn-x", "label", kind="ok", reset_html="<button/>", reset_after_ms=5000
        )
        self.assertIn("data-on-load__delay.5000ms=", result)
        self.assertNotIn("data-on-load__delay.2000ms=", result)

    def test_returns_valid_sse_patch_elements_event_with_correct_selector_and_mode(
        self,
    ) -> None:
        """Result is a valid SSE patch-elements event with OUTER mode and correct selector."""
        result = sse_btn_state("my-btn", "Save", kind="idle")
        self.assertIn("event: datastar-patch-elements", result)
        self.assertIn("data: selector #my-btn", result)
        # OUTER is the default mode — datastar_py omits the mode line for OUTER
        self.assertNotIn("data: mode", result)

    def test_disabled_flag_adds_disabled_attribute(self) -> None:
        """disabled=True adds the disabled attribute to the rendered button."""
        result = sse_btn_state("btn-2", "Wait", kind="idle", disabled=True)
        self.assertIn(" disabled", result)

    def test_no_reset_attr_when_reset_html_is_none(self) -> None:
        """When reset_html is not provided, no data-on-load__delay attribute is present."""
        result = sse_btn_state("btn-3", "Go", kind="ok")
        self.assertNotIn("data-on-load__delay", result)


if __name__ == "__main__":
    unittest.main()
