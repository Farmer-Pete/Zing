"""Desktop and terminal notifications for zing TUI."""

import logging
import sys

logger = logging.getLogger(__name__)


def notify(title: str, body: str) -> None:
    """Send a desktop notification and ring the terminal bell.

    Writes '\\a' to stderr for the terminal bell, then attempts
    to send a desktop notification via plyer. If plyer fails for
    any reason, the error is logged at DEBUG level and execution
    continues.
    """
    logger.debug("Sending notification: title=%s", title)
    # Terminal bell
    sys.stderr.write("\a")
    sys.stderr.flush()
    logger.debug("Terminal bell sent")

    # Desktop notification via plyer
    try:
        from plyer import notification

        notification.notify(title=title, message=body)
        logger.debug("plyer notification sent successfully")
    except Exception:
        logger.debug("plyer notification failed", exc_info=True)
