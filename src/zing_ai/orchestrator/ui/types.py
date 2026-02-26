from dataclasses import dataclass
from typing import Literal, TypedDict


class MenuOption(TypedDict):
    label: str
    description: str


class ReviewChange(TypedDict):
    choice_set_id: str
    selected_index: int


class AuditDecision(TypedDict):
    finding_index: int
    category: str
    severity: str
    title: str
    action: Literal["fix", "skip", "discuss"]


class InvestigationEntry(TypedDict):
    id: str
    label: str


@dataclass
class BuildProgress:
    completed_steps: list[tuple[int, int]]
    failed_step: tuple[int, int] | None


@dataclass
class InvestigationResult:
    outputs: dict[str, str]
    statuses: dict[str, str]
