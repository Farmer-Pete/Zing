"""Pydantic data models for the Zing batch review server."""

from __future__ import annotations

import logging
from datetime import datetime
from enum import Enum, StrEnum
from typing import Annotated, Any, Literal
from uuid import uuid4

from pydantic import BaseModel, Field, model_validator


class Location(BaseModel):
    """A file location with optional line number."""

    file: str
    line: int | None = None


class Severity(StrEnum):
    """Severity levels for triage findings."""

    CRITICAL = "critical"
    HIGH = "high"
    MEDIUM = "medium"
    LOW = "low"
    INFO = "info"


class Confidence(StrEnum):
    """Confidence levels for triage findings."""

    HIGH = "high"
    MEDIUM = "medium"
    LOW = "low"


class Category(StrEnum):
    """Categories for triage findings."""

    ARCHITECTURE = "architecture"
    CORRECTNESS = "correctness"
    SECURITY = "security"
    READABILITY = "readability"
    PERFORMANCE = "performance"
    TESTING = "testing"
    STYLE = "style"


class Complexity(StrEnum):
    """Fix complexity classification for triage findings."""

    SIMPLE = "simple"
    STANDARD = "standard"
    COMPLEX = "complex"


class Rating(StrEnum):
    """Rating levels for plan evaluation criteria."""

    STRONG = "strong"
    ADEQUATE = "adequate"
    WEAK = "weak"
    MISSING = "missing"


class ChoiceOption(BaseModel):
    """A single option in a multiple-choice finding."""

    label: str
    description: str


class CriterionRating(BaseModel):
    """A single criterion's evaluation result."""

    name: str
    rating: Rating
    justification: str


class LitmusTest(BaseModel):
    """A single litmus test result."""

    name: str
    result: str


class WarningSign(BaseModel):
    """A single warning sign check result."""

    name: str
    found: bool
    details: str = ""


class TextFinding(BaseModel):
    """A free-form question for the user (planning questions, open-ended answers)."""

    id: str = Field(default_factory=lambda: uuid4().hex[:8])
    type: Literal["text"] = "text"
    title: str
    body: str = ""
    context: str | None = None


class TriageFinding(BaseModel):
    """A finding that supports triage actions and/or option selection.

    Used for code review findings (with category/severity/confidence metadata
    and accept/drop/downgrade/discuss actions) and for plan audit improvements
    or design decisions (with options but no metadata).
    """

    id: str = Field(default_factory=lambda: uuid4().hex[:8])
    type: Literal["triage"] = "triage"
    title: str
    body: str = ""
    category: Category | None = None
    severity: Severity | None = None
    confidence: Confidence | None = None
    complexity: Complexity = Complexity.STANDARD
    location: Location | None = None
    options: list[ChoiceOption] | None = None

    @model_validator(mode="after")
    def _validate_metadata_consistency(self) -> TriageFinding:
        metadata = (self.category, self.severity, self.confidence)
        if any(f is not None for f in metadata) and not all(f is not None for f in metadata):
            msg = "category, severity, and confidence must be either all set or all None"
            raise ValueError(msg)
        return self


class EvaluationFinding(BaseModel):
    """Informational evaluation pass: structured criteria, litmus tests, and warnings."""

    id: str = Field(default_factory=lambda: uuid4().hex[:8])
    type: Literal["evaluation"] = "evaluation"
    title: str
    body: str = ""
    criteria: list[CriterionRating]
    litmus_tests: list[LitmusTest] = Field(default_factory=list)
    warnings: list[WarningSign] = Field(default_factory=list)


Finding = Annotated[
    TextFinding | TriageFinding | EvaluationFinding,
    Field(discriminator="type"),
]


class SessionState(str, Enum):
    """Possible states of a review session."""

    PENDING = "pending"
    STARTED = "started"
    READY = "ready"
    COMPLETED = "completed"


class AgentState(StrEnum):
    RUNNING = "running"
    COMPLETED = "completed"


class Agent(BaseModel):
    name: str
    description: str = ""
    state: AgentState = AgentState.RUNNING
    started_at: datetime = Field(default_factory=datetime.now)
    completed_at: datetime | None = None


class LogEntry(BaseModel):
    timestamp: datetime = Field(default_factory=datetime.now)
    agent_name: str = ""
    message: str = ""


class ResponseAction(str, Enum):
    """Actions a user can take on a finding."""

    ACCEPT = "accept"
    DROP = "drop"
    DOWNGRADE = "downgrade"
    DISCUSS = "discuss"


class UserResponse(BaseModel):
    """The user's response to a single finding."""

    action: ResponseAction | None = None
    selected: str | None = None
    answer: str | None = None
    other_text: str | None = None
    complexity: Complexity | None = None

    def merge_over(self, existing: UserResponse) -> UserResponse:
        """Merge self over existing, keeping existing values where self is None."""
        return UserResponse(**{
            field: (
                getattr(self, field)
                if getattr(self, field) is not None
                else getattr(existing, field)
            )
            for field in self.__class__.model_fields
        })


class WorkflowStep(BaseModel):
    """A single workflow step within a session.

    A step groups findings and responses for one phase of the review workflow.
    The same step_name can appear multiple times in a session (for loops);
    the sequence field provides ordering.
    """

    step_id: str = Field(default_factory=lambda: uuid4().hex)
    step_name: str
    sequence: int
    findings: list[Finding] = Field(default_factory=list)
    responses: list[UserResponse] | None = None
    agents: list[Agent] = Field(default_factory=list)
    logs: list[LogEntry] = Field(default_factory=list)
    state: SessionState = SessionState.PENDING
    created_at: datetime = Field(default_factory=datetime.now)

    @model_validator(mode="before")
    @classmethod
    def _migrate_legacy_fields(cls, data: Any) -> Any:
        """Migrate old session data that used counter fields instead of agent lists."""
        if not isinstance(data, dict):
            return data
        # Drop legacy counter fields, ensure agents list exists
        data.pop("expected_agents", None)
        data.pop("completed_agents", None)
        if "agents" not in data:
            data["agents"] = []
        if "logs" not in data:
            data["logs"] = []
        # Handle unknown state values (e.g. old data missing STARTED)
        state = data.get("state")
        if isinstance(state, str):
            valid_states = {s.value for s in SessionState}
            if state not in valid_states:
                data["state"] = SessionState.PENDING.value
        # Migrate legacy type:"choice" findings to type:"triage"
        for finding in data.get("findings", []):
            if isinstance(finding, dict) and finding.get("type") == "choice":
                logging.getLogger(__name__).warning(
                    "Migrating legacy choice finding to triage format: %s",
                    finding.get("title", "<untitled>"),
                )
                finding["type"] = "triage"
                context = finding.pop("context", None)
                if context:
                    body = finding.get("body", "")
                    finding["body"] = f"{body}\n\n{context}".strip() if body else context
        return data


class Session(BaseModel):
    """A review session containing workflow steps with findings and responses."""

    session_id: str
    title: str
    zing_file: str | None = None
    steps: list[WorkflowStep] = Field(default_factory=list)
    state: SessionState = SessionState.PENDING
    created_at: datetime = Field(default_factory=datetime.now)

    @model_validator(mode="before")
    @classmethod
    def _migrate_flat_findings(cls, data: Any) -> Any:
        """Migrate old flat-findings session format to workflow steps."""
        if isinstance(data, dict) and "findings" in data and "steps" not in data:
            step = {
                "step_name": "review",
                "sequence": 0,
                "findings": data.pop("findings", []),
                "responses": data.pop("responses", None),
                "agents": [],
                "state": data.get("state", "pending"),
                "created_at": data.get("created_at"),
            }
            data["steps"] = [step]
        # Clean up fields that moved to WorkflowStep
        if isinstance(data, dict):
            data.pop("expected_agents", None)
            data.pop("completed_agents", None)
            data.pop("findings", None)
            data.pop("responses", None)
        return data

    @property
    def current_step_name(self) -> str | None:
        """Return the name of the most recent workflow step, or None."""
        return self.steps[-1].step_name if self.steps else None

    @property
    def total_findings(self) -> int:
        """Return the total number of findings across all steps."""
        return sum(len(step.findings) for step in self.steps)


class ReviewItem(BaseModel):
    """A finding paired with its user response. Returned by wait_for_review()."""

    finding: Finding
    response: UserResponse


class ReviewResponse(BaseModel):
    """The complete review response for a workflow step."""

    session_id: str
    step_name: str
    items: list[ReviewItem]
