"""Tests for zing TUI notifications."""

from unittest.mock import MagicMock, call, patch

from zing_ai.orchestrator.tui.notifications import notify


class TestTerminalBell:
    """notify() should write the bell character to stderr."""

    def test_bell_written_to_stderr(self):
        with (
            patch("zing_ai.orchestrator.tui.notifications.sys.stderr") as mock_stderr,
            patch("plyer.notification.notify"),
        ):
            notify("title", "body")
            # The first write call should be the bell character
            assert call("\a") in mock_stderr.write.call_args_list


class TestPlyerNotification:
    """notify() should call plyer.notification.notify with correct args."""

    def test_plyer_called_with_correct_args(self):
        with (
            patch("zing_ai.orchestrator.tui.notifications.sys.stderr"),
            patch("plyer.notification.notify") as mock_plyer_notify,
        ):
            notify("Test Title", "Test Body")
            mock_plyer_notify.assert_called_once_with(
                title="Test Title", message="Test Body"
            )


class TestGracefulDegradation:
    """notify() should not raise when plyer fails."""

    def test_continues_when_plyer_raises(self):
        with (
            patch("zing_ai.orchestrator.tui.notifications.sys.stderr"),
            patch(
                "plyer.notification.notify",
                side_effect=RuntimeError("no notification backend"),
            ),
        ):
            # Should not raise
            notify("title", "body")

    def test_continues_when_plyer_import_fails(self):
        with (
            patch("zing_ai.orchestrator.tui.notifications.sys.stderr"),
            patch.dict("sys.modules", {"plyer": None}),
        ):
            # Should not raise even if plyer cannot be imported
            notify("title", "body")
