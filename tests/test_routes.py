"""Tests for the /config GET and POST routes."""

from __future__ import annotations

import shutil
import tempfile
import threading
from pathlib import Path
from unittest.mock import patch

from tests.test_server_base import ServerTestBase


class TestConfigRoutes(ServerTestBase):
    def test_get_config_returns_200_and_html(self) -> None:
        resp = self.client.get("/config")
        self.assertEqual(resp.status_code, 200)
        self.assertIn("text/html", resp.headers["content-type"])
        self.assertIn("Configuration", resp.text)

    def test_config_html_renders_all_fields(self) -> None:
        from zing_ai.server.config_meta import FIELD_META

        resp = self.client.get("/config")
        for dot_key in FIELD_META:
            self.assertIn(dot_key, resp.text)


class TestConfigSave(ServerTestBase):
    def setUp(self):
        # Patch config_path via mock.patch so it's reversed even if setUp raises.
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
        # Unknown payload keys are now rejected with 422 so typo'd field names
        # surface as errors instead of silently dropping the change.
        r = self.client.post("/config/save/thresholds", json={"nonexistent_field": 1})
        self.assertEqual(r.status_code, 422)
        self.assertIn("unknown fields", r.text)


class TestInstallRoutes(ServerTestBase):
    def setUp(self):
        import tempfile

        self._tmp_install = tempfile.mkdtemp()
        self._tmp_config_dir = tempfile.mkdtemp()

        import zing_ai.server.routes_install as install_mod

        self._orig_target = install_mod._install_target_for

        def fake_target(runtime: str) -> Path:
            return Path(self._tmp_install) / runtime

        install_mod._install_target_for = fake_target

        import zing_ai.config as cfg_mod

        self._orig_cfg_path = cfg_mod.config_path
        cfg_mod.config_path = lambda: Path(self._tmp_config_dir) / "config.toml"

        super().setUp()

    def tearDown(self):
        super().tearDown()

        import zing_ai.server.routes_install as install_mod

        install_mod._install_target_for = self._orig_target

        import zing_ai.config as cfg_mod

        cfg_mod.config_path = self._orig_cfg_path

        shutil.rmtree(self._tmp_install, ignore_errors=True)
        shutil.rmtree(self._tmp_config_dir, ignore_errors=True)

    def test_get_install_returns_status_for_both_runtimes(self):
        r = self.client.get("/install")
        self.assertEqual(r.status_code, 200)
        self.assertIn("claude", r.text)
        self.assertIn("opencode", r.text)

    def test_install_html_shows_updates_pending_when_stale(self):
        # No manifest exists in tmp dirs → both runtimes are stale
        r = self.client.get("/install")
        self.assertIn("Updates pending", r.text)

    def _tmp_install_path(self, runtime: str) -> Path:
        """Return the temp install target path for a given runtime."""
        return Path(self._tmp_install) / runtime

    def test_run_install_flips_badge(self):
        # Patch install_claude to be a no-op success that creates a manifest
        import zing_ai.server.routes_install as install_mod
        from zing_ai import __version__
        from zing_ai import installer as installer_mod
        from zing_ai.config import config_hash, default_config
        from zing_ai.manifest import write_manifest

        target = self._tmp_install_path("claude")
        target.mkdir(parents=True, exist_ok=True)

        def fake_install(target_dir: Path | None = None, config=None):
            assert target_dir is not None
            # Write a matching manifest so is_install_stale returns False
            write_manifest(
                target_dir,
                "claude-code",
                [],
                config_hash=config_hash(config or default_config()),
                source_mtime_max=12345.0,
                package_version=__version__,
            )

        orig_claude = install_mod.install_claude
        orig_mtime = installer_mod._source_mtime_max
        install_mod.install_claude = fake_install
        installer_mod._source_mtime_max = lambda *_a, **_kw: 12345.0
        try:
            r = self.client.post("/install/run", json={"runtime": "claude"})
            self.assertEqual(r.status_code, 200)
            self.assertIn("Up to date", r.text)
        finally:
            install_mod.install_claude = orig_claude
            installer_mod._source_mtime_max = orig_mtime

    def test_run_install_surfaces_install_error(self):
        import zing_ai.server.routes_install as install_mod
        from zing_ai.installer import InstallError

        def boom(target_dir=None, config=None):
            raise InstallError("boom")

        orig_claude = install_mod.install_claude
        install_mod.install_claude = boom
        try:
            r = self.client.post("/install/run", json={"runtime": "claude"})
            self.assertEqual(r.status_code, 200)
            self.assertIn("boom", r.text)
        finally:
            install_mod.install_claude = orig_claude
