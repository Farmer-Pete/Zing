"""Tests for the /config GET and POST routes."""

from __future__ import annotations

import shutil
import tempfile
import threading
from pathlib import Path

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


class TestConfigSave(ServerTestBase):
    def setUp(self):
        # Patch config_path to a temp file BEFORE calling super().setUp() so the
        # TestClient's app sees the patched path when handling requests.
        import zing_ai.config as cfg_mod

        self._tmpdir = tempfile.mkdtemp()
        self._tmp_config = Path(self._tmpdir) / "config.toml"
        self._orig_config_path = cfg_mod.config_path
        cfg_mod.config_path = lambda: self._tmp_config
        cfg_mod.save_config(cfg_mod.default_config())

        super().setUp()

    def tearDown(self):
        import zing_ai.config as cfg_mod

        super().tearDown()
        cfg_mod.config_path = self._orig_config_path
        shutil.rmtree(self._tmpdir, ignore_errors=True)

    def test_save_threshold_persists(self):
        r = self.client.post("/config/save/thresholds", json={"large_file_lines": 1500})
        self.assertEqual(r.status_code, 200)
        from zing_ai.config import load_config

        self.assertEqual(load_config().thresholds.large_file_lines, 1500)

    def test_save_invalid_value_returns_422(self):
        r = self.client.post("/config/save/thresholds", json={"large_file_lines": "not a number"})
        self.assertEqual(r.status_code, 422)

    def test_save_unknown_category_returns_400(self):
        r = self.client.post("/config/save/foobar", json={})
        self.assertEqual(r.status_code, 400)

    def test_save_uses_filelock(self):
        from filelock import FileLock, Timeout

        from zing_ai.config import config_path

        lock_path = str(config_path()) + ".lock"
        with FileLock(lock_path, timeout=10):
            errors = []

            def try_save():
                import zing_ai.config as cfg_mod

                orig = cfg_mod.FileLock

                def fast_lock(*args, **kwargs):
                    kwargs["timeout"] = 0.5
                    return orig(*args, **kwargs)

                cfg_mod.FileLock = fast_lock
                try:
                    cfg_mod.save_config(cfg_mod.default_config())
                except Timeout:
                    errors.append("timeout")
                except Exception as e:
                    errors.append(str(e))
                finally:
                    cfg_mod.FileLock = orig

            t = threading.Thread(target=try_save)
            t.start()
            t.join(timeout=5)
            self.assertEqual(errors, ["timeout"])

    def test_save_unknown_field_in_known_category(self):
        # ThresholdsConfig has no model_config with extra="forbid", so pydantic v2
        # silently ignores unknown fields — assert 200 and the field is not persisted.
        r = self.client.post("/config/save/thresholds", json={"nonexistent_field": 1})
        self.assertEqual(r.status_code, 200)
        from zing_ai.config import load_config

        cfg = load_config()
        self.assertFalse(hasattr(cfg.thresholds, "nonexistent_field"))
