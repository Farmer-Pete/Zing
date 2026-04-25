"""Tests for zing_ai.server.zellij_proxy."""

from __future__ import annotations

import unittest
from unittest.mock import MagicMock

import httpx
from fastapi import FastAPI
from fastapi.testclient import TestClient

from zing_ai.server.zellij_proxy import (
    _INPUT_JS_PATCH2_REPLACEMENT,
    _INPUT_JS_PATCH2_TARGET,
    _INPUT_JS_PATCH_REPLACEMENT,
    _INPUT_JS_PATCH_TARGET,
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
        """GET /assets/input.js should have Cmd+C/A and Shift+Enter patches applied."""
        # Build fake input.js that contains both patch targets
        original_js = (
            "some js before\n"
            + _INPUT_JS_PATCH_TARGET
            + "\nsome js between\n"
            + _INPUT_JS_PATCH2_TARGET
            + "\nsome js after"
        )
        app = _make_app(response_content=original_js.encode())
        client = TestClient(app)
        resp = client.get("/assets/input.js")
        self.assertEqual(resp.status_code, 200)
        body = resp.text

        # Patch 1 replacement should be present
        self.assertIn("pass cmd-c onwards so that copy is interpreted by the browser", body)
        self.assertIn("pass cmd-a onwards so that select all works", body)
        # Patch 2 replacement should be present
        self.assertIn('sendFunction("\\x1b[13;2u")', body)
        # The full replacement strings should be present verbatim in the output.
        self.assertIn(_INPUT_JS_PATCH_REPLACEMENT, body)
        self.assertIn(_INPUT_JS_PATCH2_REPLACEMENT, body)

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
