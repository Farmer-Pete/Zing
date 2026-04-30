"""Tests for the zellij_config module."""

from __future__ import annotations

import unittest
from pathlib import Path
from unittest.mock import patch


class TestGetZellijDataDir(unittest.TestCase):
    """Tests for get_zellij_data_dir()."""

    def test_get_zellij_data_dir_creates_directory(self) -> None:
        """get_zellij_data_dir() creates the expected directory under the mocked home."""
        import tempfile

        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            expected_dir = tmp_path / ".local" / "share" / "zing-ai" / "zellij"

            # We need to patch the module-level _ZELLIJ_DATA_DIR which is computed at
            # import time. Patch both the constant and Path.home so the function uses
            # the tmp dir.
            import zing_ai.server.zellij_config as zc

            with patch.object(zc, "_ZELLIJ_DATA_DIR", expected_dir):
                result = zc.get_zellij_data_dir()

            assert result == expected_dir
            assert expected_dir.exists()
            assert expected_dir.is_dir()


class TestEnsureZellijConfig(unittest.TestCase):
    """Tests for ensure_zellij_config()."""

    def test_ensure_zellij_config_writes_files(self) -> None:
        """ensure_zellij_config() creates config.kdl and bare.kdl with expected content."""
        import tempfile

        import zing_ai.server.zellij_config as zc

        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            data_dir = tmp_path / ".local" / "share" / "zing-ai" / "zellij"

            with patch.object(zc, "_ZELLIJ_DATA_DIR", data_dir):
                config_path, returned_dir = zc.ensure_zellij_config()

            assert returned_dir == data_dir
            assert config_path == data_dir / "config.kdl"
            assert config_path.exists()
            assert (data_dir / "bare.kdl").exists()

            config_content = config_path.read_text()
            assert "keybinds clear-defaults=true" in config_content
            assert 'web_sharing "on"' in config_content

            bare_content = (data_dir / "bare.kdl").read_text()
            assert "layout" in bare_content
            assert "pane" in bare_content

    def test_ensure_zellij_config_rewrites_on_drift(self) -> None:
        """If config content drifts from the bundled defaults, ensure_zellij_config rewrites it.

        The config files are owned by zing-ai, not user-customisable in place.
        Rewriting on drift lets us ship new options (e.g. ``show_startup_tips false``)
        and have them take effect on the next launch without users having to
        delete their config manually.
        """
        import tempfile

        import zing_ai.server.zellij_config as zc

        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            data_dir = tmp_path / ".local" / "share" / "zing-ai" / "zellij"

            with patch.object(zc, "_ZELLIJ_DATA_DIR", data_dir):
                zc.ensure_zellij_config()
                config_path = data_dir / "config.kdl"
                bare_path = data_dir / "bare.kdl"
                # Simulate an outdated config (stale defaults from a prior version).
                config_path.write_text("# outdated config")
                bare_path.write_text("# outdated bare")

                # Second call should detect drift and rewrite.
                zc.ensure_zellij_config()

            assert config_path.read_text() == zc._CONFIG_KDL
            assert bare_path.read_text() == zc._BARE_LAYOUT_KDL

    def test_ensure_zellij_config_idempotent_when_unchanged(self) -> None:
        """Calling ensure_zellij_config twice in a row is a no-op when content matches."""
        import tempfile

        import zing_ai.server.zellij_config as zc

        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            data_dir = tmp_path / ".local" / "share" / "zing-ai" / "zellij"

            with patch.object(zc, "_ZELLIJ_DATA_DIR", data_dir):
                zc.ensure_zellij_config()
                config_path = data_dir / "config.kdl"
                bare_path = data_dir / "bare.kdl"
                first_config_mtime = config_path.stat().st_mtime_ns
                first_bare_mtime = bare_path.stat().st_mtime_ns

                zc.ensure_zellij_config()

                assert config_path.stat().st_mtime_ns == first_config_mtime
                assert bare_path.stat().st_mtime_ns == first_bare_mtime

    def test_ensure_zellij_config_disables_tips_and_release_notes(self) -> None:
        """The bundled config disables Zellij's startup tips and release notes."""
        import tempfile

        import zing_ai.server.zellij_config as zc

        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            data_dir = tmp_path / ".local" / "share" / "zing-ai" / "zellij"

            with patch.object(zc, "_ZELLIJ_DATA_DIR", data_dir):
                config_path, _ = zc.ensure_zellij_config()

            content = config_path.read_text()
            assert "show_startup_tips false" in content
            assert "show_release_notes false" in content


class TestWriteCommandLayout(unittest.TestCase):
    """Tests for write_command_layout()."""

    def test_write_command_layout_creates_temp_file(self) -> None:
        """write_command_layout() returns a path to an existing KDL file with valid content."""
        from zing_ai.server.zellij_config import write_command_layout

        command = "/bin/bash"
        args = ["-c", "echo hello"]
        path = write_command_layout(command, args)

        try:
            assert path.exists()
            assert path.suffix == ".kdl"
            content = path.read_text()
            assert "layout" in content
            assert command in content
            assert "-c" in content
            assert "echo hello" in content
        finally:
            path.unlink(missing_ok=True)

    def test_write_command_layout_unique_per_call(self) -> None:
        """Two consecutive calls to write_command_layout() return different file paths."""
        from zing_ai.server.zellij_config import write_command_layout

        path1 = write_command_layout("echo", ["hello"])
        path2 = write_command_layout("echo", ["world"])

        try:
            assert path1 != path2
        finally:
            path1.unlink(missing_ok=True)
            path2.unlink(missing_ok=True)

    def test_args_emitted_on_single_line(self) -> None:
        """Multi-arg commands emit one `args` line.

        Regression test: previously each arg was emitted on its own line, which
        Zellij parsed as unknown pane properties (e.g. ``--session-id``).
        """
        from zing_ai.server.zellij_config import write_command_layout

        args = [
            "/zing:pr-audit https://github.com/example/repo/pull/1",
            "--session-id",
            "uuid-123",
            "--name",
            "demo",
        ]
        path = write_command_layout("claude", args)

        try:
            content = path.read_text()
            args_lines = [
                line for line in content.splitlines() if line.lstrip().startswith("args ")
            ]
            assert len(args_lines) == 1, f"expected one args line, got {args_lines}"
            args_line = args_lines[0]
            for arg in args:
                assert f'"{arg}"' in args_line, f"missing arg {arg!r} in {args_line!r}"
        finally:
            path.unlink(missing_ok=True)

    def test_args_escape_quotes_and_backslashes(self) -> None:
        """Args containing `"` or `\\` are escaped so the KDL string stays well-formed."""
        from zing_ai.server.zellij_config import write_command_layout

        path = write_command_layout("echo", ['a"b', "c\\d"])

        try:
            content = path.read_text()
            assert r'"a\"b"' in content
            assert r'"c\\d"' in content
        finally:
            path.unlink(missing_ok=True)

    def test_command_escapes_quotes(self) -> None:
        """The command field is escaped the same way as args."""
        from zing_ai.server.zellij_config import write_command_layout

        path = write_command_layout('/bin/foo"bar', ["x"])

        try:
            content = path.read_text()
            assert r'command="/bin/foo\"bar"' in content
        finally:
            path.unlink(missing_ok=True)


if __name__ == "__main__":
    unittest.main()
