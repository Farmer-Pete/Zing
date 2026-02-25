"""ZingApp - Main Textual application for the zing orchestrator TUI."""

from __future__ import annotations

import logging
from pathlib import Path
from typing import Any

from textual.app import App
from textual.screen import Screen
from textual.theme import Theme

from zing_ai.orchestrator.tui.notifications import notify as _notify

logger = logging.getLogger(__name__)

# The zing theme, registered on every ZingApp instance.
ZING_THEME = Theme(
    name="zing",
    primary="#4F8CFF",
    secondary="#9DA7B3",
    accent="#9B59B6",
    warning="#F5B041",
    error="#E74C3C",
    success="#2ECC71",
    background="#0E1116",
    surface="#151A22",
    panel="#1C222C",
    dark=True,
)

_CSS_PATH = Path(__file__).parent / "theme.tcss"


class ZingApp(App[Any]):
    """Textual application for the zing orchestrator.

    Use the class method ``run_with_screen`` to launch a screen and
    retrieve its dismiss result synchronously::

        result = ZingApp.run_with_screen(my_screen)
    """

    CSS_PATH = _CSS_PATH

    def __init__(self, screen_to_push: Screen[Any] | None = None) -> None:
        super().__init__()
        self._pending_screen: Screen[Any] | None = screen_to_push
        self._screen_result: Any = None
        logger.debug(
            "ZingApp created, pending screen: %s",
            type(screen_to_push).__name__ if screen_to_push else None,
        )

    def on_mount(self) -> None:
        """Register zing theme and push the pending screen if one was set."""
        logger.debug("Registering zing theme")
        self.register_theme(ZING_THEME)
        self.theme = "zing"

        if self._pending_screen is not None:
            logger.debug("Pushing pending screen: %s", type(self._pending_screen).__name__)
            self.push_screen(self._pending_screen, callback=self._on_screen_dismiss)

    def _on_screen_dismiss(self, result: Any) -> None:
        """Callback invoked when the pushed screen calls ``self.dismiss(result)``."""
        logger.debug("Screen dismissed with result type: %s", type(result).__name__)
        self._screen_result = result
        self.exit()

    @classmethod
    def run_with_screen(cls, screen: Screen[Any]) -> Any:
        """Create a ZingApp, push *screen* on mount, and return its dismiss result.

        This is fully synchronous from the caller's perspective.  Textual
        manages its own event loop inside ``app.run()``.

        Args:
            screen: The Screen instance to display.

        Returns:
            Whatever value the screen passed to ``self.dismiss(result)``.
        """
        logger.debug("run_with_screen: launching %s", type(screen).__name__)
        app = cls(screen_to_push=screen)
        app.run()
        logger.debug("run_with_screen: finished, result type=%s", type(app._screen_result).__name__)
        return app._screen_result

    def notify_user(self, title: str, body: str) -> None:
        """Ring terminal bell and send a desktop notification.

        Wraps :func:`zing_ai.orchestrator.tui.notifications.notify` so
        screens can call ``self.app.notify_user(...)`` conveniently.
        """
        _notify(title, body)
