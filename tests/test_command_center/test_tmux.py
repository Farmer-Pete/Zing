"""Tests for get_live_sessions() in command_center.py."""

from __future__ import annotations

import unittest
from unittest.mock import MagicMock, patch

from zing_ai.server.command_center import get_live_sessions


class TestGetLiveTmuxSessions(unittest.TestCase):
    """Tests for get_live_sessions()."""

    def test_get_live_sessions_parses_output(self) -> None:
        """Parses multi-line tmux output into a set of session names."""
        mock_result = MagicMock()
        mock_result.returncode = 0
        mock_result.stdout = "session1\nsession2\nsession3\n"
        with patch("subprocess.run", return_value=mock_result) as mock_run:
            result = get_live_sessions()
            mock_run.assert_called_once_with(
                ["tmux", "list-sessions", "-F", "#{session_name}"],
                capture_output=True,
                text=True,
            )
        self.assertEqual(result, {"session1", "session2", "session3"})

    def test_get_live_sessions_tmux_not_running(self) -> None:
        """Returns empty set when tmux returns non-zero exit code."""
        mock_result = MagicMock()
        mock_result.returncode = 1
        mock_result.stdout = ""
        with patch("subprocess.run", return_value=mock_result):
            result = get_live_sessions()
        self.assertEqual(result, set())

    def test_get_live_sessions_tmux_not_installed(self) -> None:
        """Returns empty set when tmux is not installed (FileNotFoundError)."""
        with patch("subprocess.run", side_effect=FileNotFoundError):
            result = get_live_sessions()
        self.assertEqual(result, set())
