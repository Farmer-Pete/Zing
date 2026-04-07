"""Tests for the /config GET route."""

from __future__ import annotations

from tests.test_server_base import ServerTestBase


class TestConfigRoutes(ServerTestBase):
    def test_get_config_returns_200_and_html(self) -> None:
        resp = self.client.get("/config")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("text/html", resp.headers["content-type"])
        self.assertIn("Configuration", resp.text)

    def test_config_html_renders_all_fields(self) -> None:
        from zing_ai.server.routes import _FIELD_META

        resp = self.client.get("/config")
        for dot_key in _FIELD_META:
            self.assertIn(dot_key, resp.text)
