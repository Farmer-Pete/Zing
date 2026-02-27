"""Pipeline-specific exceptions."""

from __future__ import annotations

import logging

logger = logging.getLogger(__name__)


class PipelineError(Exception):
    """An error that occurred during a specific pipeline stage.

    Parameters
    ----------
    stage:
        The pipeline stage where the error occurred (e.g. ``"plan"``).
    message:
        A human-readable description of what went wrong.
    """

    def __init__(self, stage: str, message: str) -> None:
        self.stage = stage
        self.message = message
        super().__init__(f"[{stage}] {message}")
        logger.warning("PipelineError in stage %r: %s", stage, message)
