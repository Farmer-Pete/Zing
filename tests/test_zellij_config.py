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

    def test_ensure_zellij_config_idempotent(self) -> None:
        """Calling ensure_zellij_config() twice does not overwrite existing files."""
        import tempfile

        import zing_ai.server.zellij_config as zc

        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            data_dir = tmp_path / ".local" / "share" / "zing-ai" / "zellij"

            with patch.object(zc, "_ZELLIJ_DATA_DIR", data_dir):
                zc.ensure_zellij_config()
                # Overwrite with custom content to verify it is not clobbered
                config_path = data_dir / "config.kdl"
                bare_path = data_dir / "bare.kdl"
                config_path.write_text("# custom config")
                bare_path.write_text("# custom bare")

                # Second call should not overwrite existing files
                zc.ensure_zellij_config()

            assert config_path.read_text() == "# custom config"
            assert bare_path.read_text() == "# custom bare"


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


if __name__ == "__main__":
    unittest.main()
