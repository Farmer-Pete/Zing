"""Tests for the /config/save/command_center route."""

from __future__ import annotations

import shutil
import tempfile
from pathlib import Path
from unittest.mock import patch

from tests.test_server_base import ServerTestBase


class TestSaveCommandCenterConfig(ServerTestBase):
    def setUp(self):
        self._tmpdir = tempfile.mkdtemp()
        self._tmp_config = Path(self._tmpdir) / "config.toml"
        self._patcher = patch("zing_ai.config.config_path", return_value=self._tmp_config)
        self._patcher.start()
        try:
            import zing_ai.config as cfg_mod

            cfg_mod.save_config(cfg_mod.default_config())
            super().setUp()
        except Exception:
            self._patcher.stop()
            shutil.rmtree(self._tmpdir, ignore_errors=True)
            raise

    def tearDown(self):
        try:
            super().tearDown()
        finally:
            self._patcher.stop()
            shutil.rmtree(self._tmpdir, ignore_errors=True)

    def test_save_command_center_config_valid(self):
        """Posting valid command_center fields returns 200 and persists the values."""
        r = self.client.post(
            "/config/save/command_center",
            json={
                "linear_poll_seconds": 120,
                "github_poll_seconds": 90,
            },
        )
        self.assertEqual(r.status_code, 200)
        self.assertEqual(r.json(), {"status": "ok"})

        from zing_ai.config import load_config

        cc = load_config().command_center
        self.assertEqual(cc.linear_poll_seconds, 120)
        self.assertEqual(cc.github_poll_seconds, 90)

    def test_save_command_center_unknown_field_returns_422(self):
        """Unknown payload fields are rejected with 422."""
        r = self.client.post(
            "/config/save/command_center",
            json={"nonexistent_field": "bad"},
        )
        self.assertEqual(r.status_code, 422)
        self.assertIn("unknown fields", r.text)

    def test_save_command_center_invalid_value_returns_422(self):
        """Non-numeric poll seconds are rejected with 422."""
        r = self.client.post(
            "/config/save/command_center",
            json={"linear_poll_seconds": "not-a-number"},
        )
        self.assertEqual(r.status_code, 422)

    def test_save_api_keys(self):
        """Posting API keys persists them correctly."""
        r = self.client.post(
            "/config/save/command_center",
            json={
                "linear_api_key": "lin_test_123",
                "github_token": "ghp_test_456",
            },
        )
        self.assertEqual(r.status_code, 200)
        self.assertEqual(r.json(), {"status": "ok"})

        from zing_ai.config import load_config

        cc = load_config().command_center
        self.assertEqual(cc.linear_api_key, "lin_test_123")
        self.assertEqual(cc.github_token, "ghp_test_456")
