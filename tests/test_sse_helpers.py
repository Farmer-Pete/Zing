"""Unit tests for sse_helpers.py — sse_toast and sse_btn_state."""

from __future__ import annotations

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

    def test_toast_uses_data_init_delay_for_self_removal(self) -> None:
        """Toast element uses data-init__delay.5000ms to self-remove after 5 s."""
        result = sse_toast("bye")
        self.assertIn("data-init__delay.5000ms=", result)
        self.assertIn("el.remove()", result)

    def test_invalid_kind_raises_value_error(self) -> None:
        """kind values outside {ok, err, info} raise ValueError."""
        for bad_kind in ("warn", "success", "", "OK", "<script>"):
            with self.subTest(bad_kind=bad_kind), self.assertRaises(ValueError):
                sse_toast("msg", kind=bad_kind)  # type: ignore[arg-type]

    def test_invalid_toast_id_raises_value_error(self) -> None:
        """toast_id with characters outside [A-Za-z0-9_-] raises ValueError."""
        for bad_id in ('x" onclick="alert(1)', "id with space", "id.dot", "", "id/slash"):
            with self.subTest(bad_id=bad_id), self.assertRaises(ValueError):
                sse_toast("msg", toast_id=bad_id)


class TestSseBtnState(unittest.TestCase):
    """Tests for sse_btn_state()."""

    def test_html_escaping_prevents_label_injection(self) -> None:
        """Label containing <script> is HTML-escaped in the SSE output."""
        result = sse_btn_state(
            "btn-1", '<script>alert("xss")</script>', class_="strip-primary-btn", kind="ok"
        )
        self.assertIn("&lt;script&gt;", result)
        self.assertNotIn("<script>", result)

    def test_class_preserves_originating_styles_with_kind_suffix(self) -> None:
        """class_ is preserved verbatim and a btn-ok / btn-err suffix is appended."""
        idle = sse_btn_state("btn-1", "label", class_="strip-primary-btn", kind="idle")
        ok = sse_btn_state("btn-1", "label", class_="strip-primary-btn", kind="ok")
        err = sse_btn_state("btn-1", "label", class_="strip-primary-btn", kind="err")
        self.assertIn('class="strip-primary-btn"', idle)
        self.assertIn('class="strip-primary-btn btn-ok"', ok)
        self.assertIn('class="strip-primary-btn btn-err"', err)

    def test_attrs_preserved_so_datastar_bindings_survive_outer_patch(self) -> None:
        """Caller-supplied attrs are interpolated verbatim onto the replacement button."""
        result = sse_btn_state(
            "btn-1",
            "label",
            class_="strip-primary-btn",
            attrs='data-on:click="@post(\'/foo\')" data-indicator="$busyButtons.x"',
            kind="idle",
        )
        self.assertIn("data-on:click=\"@post('/foo')\"", result)
        self.assertIn('data-indicator="$busyButtons.x"', result)

    def test_returns_valid_sse_patch_elements_event_with_correct_selector_and_mode(
        self,
    ) -> None:
        """Result is a valid SSE patch-elements event with OUTER mode and correct selector."""
        result = sse_btn_state("my-btn", "Save", class_="btn", kind="idle")
        self.assertIn("event: datastar-patch-elements", result)
        self.assertIn("data: selector #my-btn", result)
        # OUTER is the default mode — datastar_py omits the mode line for OUTER
        self.assertNotIn("data: mode", result)

    def test_disabled_flag_adds_disabled_attribute(self) -> None:
        """disabled=True adds the disabled attribute to the rendered button."""
        result = sse_btn_state("btn-2", "Wait", class_="btn", kind="idle", disabled=True)
        self.assertIn(" disabled", result)

    def test_invalid_button_id_raises_value_error(self) -> None:
        """button_id with characters outside [A-Za-z0-9_-] raises ValueError."""
        for bad_id in (
            'x" onclick="alert(1)',
            "btn with space",
            "btn.dot",
            "",
            "btn/slash",
        ):
            with self.subTest(bad_id=bad_id), self.assertRaises(ValueError):
                sse_btn_state(bad_id, "label", class_="btn", kind="ok")

    def test_valid_button_ids_accepted(self) -> None:
        """Letters, digits, hyphens and underscores are all accepted in button_id."""
        for good_id in ("btn-launch-BAK-1234", "btn_kill_session", "abc123"):
            with self.subTest(good_id=good_id):
                # Should not raise.
                result = sse_btn_state(good_id, "label", class_="btn", kind="ok")
                self.assertIn(f'id="{good_id}"', result)


if __name__ == "__main__":
    unittest.main()
