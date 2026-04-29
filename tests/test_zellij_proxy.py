"""Tests for zing_ai.server.zellij_proxy."""

from __future__ import annotations

import unittest
from unittest.mock import MagicMock

import httpx
from fastapi import FastAPI
from fastapi.testclient import TestClient

from zing_ai.server import zellij_proxy
from zing_ai.server.zellij_proxy import (
    _CMD_PASSTHROUGH_REPLACEMENT,
    _CMD_PASSTHROUGH_TARGET,
    _SHIFT_ENTER_REPLACEMENT,
    _SHIFT_ENTER_TARGET,
    JS_PATCHES,
    create_zellij_router,
)


def _make_app(*, zellij_available: bool = True, response_content: bytes = b"ok") -> FastAPI:
    """Build a minimal FastAPI app with the zellij proxy router mounted."""
    app = FastAPI()

    # Set up a mock httpx.AsyncClient on app.state
    mock_client = MagicMock(spec=httpx.AsyncClient)

    # Build a fake httpx.Response
    mock_resp = MagicMock(spec=httpx.Response)
    mock_resp.content = response_content
    mock_resp.status_code = 200
    mock_resp.headers = httpx.Headers({"content-type": "text/plain"})

    # Make all relevant async methods return the mock response

    async def _fake_request(*args, **kwargs):
        return mock_resp

    async def _fake_get(*args, **kwargs):
        return mock_resp

    mock_client.request = _fake_request
    mock_client.get = _fake_get

    app.state.zellij_available = zellij_available
    app.state.zellij_http_client = mock_client
    app.state.zellij_session_cookie = None

    router = create_zellij_router()
    app.include_router(router)
    return app


class TestProxyHttp(unittest.TestCase):
    """Tests for the /zellij/{path} route."""

    def test_proxy_http_forwards_request(self):
        """GET /zellij/some/page should return the upstream content."""
        app = _make_app(response_content=b"hello from zellij")
        client = TestClient(app)
        resp = client.get("/zellij/some/page")
        self.assertEqual(resp.status_code, 200)
        self.assertEqual(resp.content, b"hello from zellij")

    def test_proxy_http_returns_503_when_zellij_unavailable(self):
        """GET /zellij/... should return 503 when zellij_available is False."""
        app = _make_app(zellij_available=False)
        client = TestClient(app)
        resp = client.get("/zellij/some/page")
        self.assertEqual(resp.status_code, 503)


class TestProxyAssets(unittest.TestCase):
    """Tests for the /assets/{path} route."""

    def test_proxy_assets_patches_input_js(self):
        """GET /assets/input.js applies Cmd+C/A, modifier-only drop, and Shift+Enter patches."""
        original_js = (
            "some js before\n"
            + _CMD_PASSTHROUGH_TARGET
            + "\nsome js between\n"
            + _SHIFT_ENTER_TARGET
            + "\nsome js after"
        )
        app = _make_app(response_content=original_js.encode())
        client = TestClient(app)
        resp = client.get("/assets/input.js")
        self.assertEqual(resp.status_code, 200)
        body = resp.text

        self.assertIn("pass cmd-c onwards so that copy is interpreted by the browser", body)
        self.assertIn("pass cmd-a onwards so that select all works", body)
        # Modifier-only key drop (regression: pressing Cmd alone used to type "m").
        self.assertIn('ev.key == "Meta"', body)
        self.assertIn('sendFunction("\\x1b[13;2u")', body)
        self.assertIn(_CMD_PASSTHROUGH_REPLACEMENT, body)
        self.assertIn(_SHIFT_ENTER_REPLACEMENT, body)

    def test_proxy_assets_appends_shift_click_to_terminal_js(self):
        """GET /assets/terminal.js appends the shift-click handler closure."""
        original_js = b"// pretend this is Zellij's terminal.js\nexport function initTerminal(){}\n"
        app = _make_app(response_content=original_js)
        client = TestClient(app)
        resp = client.get("/assets/terminal.js")
        self.assertEqual(resp.status_code, 200)
        body = resp.text
        # Original content preserved verbatim at the start.
        self.assertTrue(body.startswith(original_js.decode()))
        # The append payload (a recognisable, distinctive substring) is present.
        self.assertIn("__zingShiftClickInstalled", body)
        self.assertIn("URL_RE", body)

    def test_proxy_assets_input_js_anchor_missing_logs_warning_and_serves_unpatched(self):
        """When upstream Zellij changes and an anchor disappears, the patch
        no-ops, logs a one-time warning, and serves the original asset."""
        # input.js without either anchor — simulates a Zellij upgrade.
        original_js = b"// brand new input.js with completely refactored handlers\n"
        # Reset the warned-once cache so this test is deterministic.
        zellij_proxy._patch_anchor_warned.clear()
        app = _make_app(response_content=original_js)
        client = TestClient(app)

        with self.assertLogs("zing_ai.server.zellij_proxy", level="WARNING") as captured:
            resp = client.get("/assets/input.js")
            # Hit it again — log should NOT fire a second time per patch.
            client.get("/assets/input.js")

        self.assertEqual(resp.status_code, 200)
        self.assertEqual(resp.content, original_js)
        # Two patches anchor in input.js → exactly two warnings on the first
        # request, none on the second.
        anchor_warnings = [m for m in captured.output if "anchor not found" in m]
        self.assertEqual(len(anchor_warnings), 2)
        self.assertTrue(any("input-js/cmd-passthrough" in m for m in anchor_warnings))
        self.assertTrue(any("input-js/shift-enter" in m for m in anchor_warnings))

    def test_js_patches_registry_well_formed(self):
        """Every JsPatch has a unique name and a non-empty why."""
        names = [p.name for p in JS_PATCHES]
        self.assertEqual(len(names), len(set(names)), "patch names must be unique")
        for patch in JS_PATCHES:
            self.assertTrue(patch.why.strip(), f"{patch.name} missing why")
            self.assertTrue(patch.target_file, f"{patch.name} missing target_file")

    def test_proxy_assets_passes_through_links_js(self):
        """GET /assets/links.js is not in the registry — Zellij's stock handler runs."""
        original_js = (
            b"const newWindow = window.open(uri, '_blank');\n"
            b"if (newWindow) newWindow.opener = null;\n"
        )
        app = _make_app(response_content=original_js)
        client = TestClient(app)
        resp = client.get("/assets/links.js")
        self.assertEqual(resp.status_code, 200)
        self.assertEqual(resp.content, original_js)

    def test_proxy_assets_passes_through_other_files(self):
        """GET /assets/styles.css should return content unchanged."""
        original_content = b".body { color: red; }"
        app = _make_app(response_content=original_content)
        client = TestClient(app)
        resp = client.get("/assets/styles.css")
        self.assertEqual(resp.status_code, 200)
        self.assertEqual(resp.content, original_content)

    def test_proxy_assets_returns_503_when_zellij_unavailable(self):
        """GET /assets/... should return 503 when zellij_available is False."""
        app = _make_app(zellij_available=False)
        client = TestClient(app)
        resp = client.get("/assets/input.js")
        self.assertEqual(resp.status_code, 503)


class TestProxyCommand(unittest.TestCase):
    """Tests for the /command/{path} route."""

    def test_proxy_command_forwards_get(self):
        """GET /command/status should forward to upstream and return content."""
        app = _make_app(response_content=b'{"status": "ok"}')
        client = TestClient(app)
        resp = client.get("/command/status")
        self.assertEqual(resp.status_code, 200)
        self.assertEqual(resp.content, b'{"status": "ok"}')

    def test_proxy_command_returns_503_when_unavailable(self):
        """GET /command/... should return 503 when zellij is unavailable."""
        app = _make_app(zellij_available=False)
        client = TestClient(app)
        resp = client.get("/command/status")
        self.assertEqual(resp.status_code, 503)


class TestProxySession(unittest.TestCase):
    """Tests for the /session route."""

    def test_proxy_session_forwards_get(self):
        """GET /session should forward to upstream."""
        app = _make_app(response_content=b'{"sessions": []}')
        client = TestClient(app)
        resp = client.get("/session")
        self.assertEqual(resp.status_code, 200)

    def test_proxy_session_returns_503_when_unavailable(self):
        """GET /session should return 503 when zellij is unavailable."""
        app = _make_app(zellij_available=False)
        client = TestClient(app)
        resp = client.get("/session")
        self.assertEqual(resp.status_code, 503)


class TestProxyInfo(unittest.TestCase):
    """Tests for the /info/{path} route."""

    def test_proxy_info_forwards_request(self):
        """GET /info/version should return upstream content."""
        app = _make_app(response_content=b'{"version": "1.0"}')
        client = TestClient(app)
        resp = client.get("/info/version")
        self.assertEqual(resp.status_code, 200)
        self.assertEqual(resp.content, b'{"version": "1.0"}')

    def test_proxy_info_returns_503_when_unavailable(self):
        """GET /info/... should return 503 when zellij is unavailable."""
        app = _make_app(zellij_available=False)
        client = TestClient(app)
        resp = client.get("/info/version")
        self.assertEqual(resp.status_code, 503)


if __name__ == "__main__":
    unittest.main()
