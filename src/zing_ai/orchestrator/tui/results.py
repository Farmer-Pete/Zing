from dataclasses import dataclass
from typing import Literal


@dataclass
class ProgressResult:
    """Returned by ProgressScreen when all subprocesses complete."""

    outputs: dict[str, str]  # subprocess_id -> full captured output
    statuses: dict[str, str]  # subprocess_id -> final status ("success" | "failed")


@dataclass
class ReviewResult:
    """Returned by PlanReviewScreen on user approval or re-plan."""

    action: Literal["approve", "replan"]
    # empty if action=="approve"; list of {"choice_id": str, "new_selection": int | None} if replan
    changes: list[dict]


@dataclass
class BuildResult:
    """Returned by BuildScreen when all steps complete."""

    completed_steps: list[
        tuple[int, int]
    ]  # (stage_idx, step_idx) pairs that succeeded
    failed_step: tuple[int, int] | None  # first (stage_idx, step_idx) that failed, or None


@dataclass
class AuditResult:
    """Returned by AuditScreen with user decisions on findings."""

    # [{"finding_index": int, "action": "fix" | "skip" | "discuss"}]
    decisions: list[dict]
