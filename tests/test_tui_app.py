"""Tests for ZingApp - TUI foundation."""

import pytest
from textual.screen import Screen
from textual.widgets import Label

from zing_ai.orchestrator.tui.app import ZingApp


class _DismissScreen(Screen[str]):
    """A minimal screen that dismisses itself immediately with a known value."""

    def compose(self):
        yield Label("test")

    def on_mount(self) -> None:
        self.dismiss("hello-from-screen")


class TestZingAppTheme:
    """ZingApp should launch with the zing theme active."""

    @pytest.mark.asyncio
    async def test_zing_theme_registered(self):
        app = ZingApp()
        async with app.run_test():
            assert "zing" in app.available_themes
            assert app.theme == "zing"


class TestRunWithScreen:
    """run_with_screen should return the dismiss result synchronously."""

    @pytest.mark.asyncio
    async def test_returns_dismiss_result(self):
        """Push a screen that immediately dismisses with a string value."""
        app = ZingApp(screen_to_push=_DismissScreen())
        async with app.run_test():
            # The screen should have been pushed and dismissed during mount.
            # After the dismiss callback fires, _screen_result is set.
            assert app._screen_result == "hello-from-screen"

    @pytest.mark.asyncio
    async def test_returns_none_without_screen(self):
        """When no screen is pushed, result stays None."""
        app = ZingApp()
        async with app.run_test():
            assert app._screen_result is None
