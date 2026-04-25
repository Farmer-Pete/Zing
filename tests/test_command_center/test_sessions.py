"""Tests for get_live_sessions() in command_center.py."""

from __future__ import annotations

import unittest
from unittest.mock import MagicMock, patch

from zing_ai.server.command_center import get_live_sessions


class TestGetLiveSessions(unittest.TestCase):
    """Tests for get_live_sessions()."""

    def test_parses_output(self) -> None:
        """Parses multi-line zellij output into a set of zing-prefixed session names."""
        mock_result = MagicMock()
        mock_result.returncode = 0
        mock_result.stdout = "zing--session1\nzing--session2\nother-session\n"
        with patch(
            "zing_ai.server.command_center.subprocess.run", return_value=mock_result
        ) as mock_run:
            result = get_live_sessions()
            mock_run.assert_called_once_with(
                ["zellij", "list-sessions", "-sn"],
                capture_output=True,
                text=True,
            )
        self.assertEqual(result, {"zing--session1", "zing--session2"})

    def test_zellij_not_running(self) -> None:
        """Returns empty set when zellij returns non-zero exit code."""
        mock_result = MagicMock()
        mock_result.returncode = 1
        mock_result.stdout = ""
        with patch("zing_ai.server.command_center.subprocess.run", return_value=mock_result):
            result = get_live_sessions()
        self.assertEqual(result, set())

    def test_zellij_not_installed(self) -> None:
        """Returns empty set when zellij is not installed (FileNotFoundError)."""
        with patch("zing_ai.server.command_center.subprocess.run", side_effect=FileNotFoundError):
            result = get_live_sessions()
        self.assertEqual(result, set())
