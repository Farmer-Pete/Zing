"""Per-step validators run by mcp_tools.step_stop. Static dict; no registry."""

from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path

from zing_ai.server.models import WorkflowStep, ZingSession
from zing_ai.viz import validate as viz_validate


class ValidationException(Exception):
    """Raised by a step validator to short-circuit step_stop with a recoverable error.

    The exception message is the full formatted error list, ready for Claude
    to display and act on. Validators may either return list[str] of issues
    (caller joins them) or raise this — both end up as {"error": ...} in
    step_stop. Use the exception form when an early return makes the control
    flow cleaner than building a list.
    """


StepValidator = Callable[[ZingSession, WorkflowStep], list[str]]
"""Returns a list of formatted error strings; empty list = valid."""


def _plan_viz_validator(sess: ZingSession, step: WorkflowStep) -> list[str]:
    """Validate that .zing/<slug>.viz.json exists, parses, and conforms to the schema.

    Returns an actionable error list when ``sess.zing_file`` is unset (the
    session was never passed through ``session_update``); a bare ``assert``
    here would propagate as a 500 because ``step_stop`` only catches
    ``(json.JSONDecodeError, ValidationException)``.
    """
    if sess.zing_file is None:
        return [
            "session has no zing_file — call session_update with the plan markdown "
            "path before step_stop"
        ]
    md_path = Path(sess.zing_file)
    viz_path = md_path.with_name(md_path.stem + ".viz.json")
    if not viz_path.exists():
        return [f"viz JSON not written; expected at {viz_path}"]
    graph = json.loads(viz_path.read_text(encoding="utf-8"))
    issues = viz_validate.validate(graph)
    return [issue.format(str(viz_path)) for issue in issues]


STEP_VALIDATORS: dict[str, list[StepValidator]] = {
    "plan": [_plan_viz_validator],
    "plan-audit": [_plan_viz_validator],
    "build-audit": [_plan_viz_validator],
}
