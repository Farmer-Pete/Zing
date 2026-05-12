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

    Caller (mcp_tools.step_stop) trusts that sess.zing_file is absolute and exists —
    session_update enforces that invariant at the boundary, so this validator
    doesn't re-check.
    """
    assert sess.zing_file is not None
    md_path = Path(sess.zing_file)
    viz_path = md_path.with_name(md_path.stem + ".viz.json")
    if not viz_path.exists():
        return [f"viz JSON not written; expected at {viz_path}"]
    graph = json.loads(viz_path.read_text())
    issues = viz_validate.validate(graph)
    return [issue.format(str(viz_path)) for issue in issues]


STEP_VALIDATORS: dict[str, list[StepValidator]] = {
    "plan": [_plan_viz_validator],
    "plan-audit": [_plan_viz_validator],
    "build-audit": [_plan_viz_validator],
}
