"""Pydantic data models for the Zing batch review server."""

from __future__ import annotations

from datetime import datetime
from enum import Enum, StrEnum
from typing import Annotated, Literal
from uuid import uuid4

from pydantic import BaseModel, Field


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


class ChoiceFinding(BaseModel):
    """A multiple-choice question (plan audit improvements, design decisions)."""

    id: str = Field(default_factory=lambda: uuid4().hex[:8])
    type: Literal["choice"] = "choice"
    title: str
    body: str = ""
    options: list[ChoiceOption]


class TriageFinding(BaseModel):
    """A code review finding for triage (accept/drop/downgrade/discuss)."""

    id: str = Field(default_factory=lambda: uuid4().hex[:8])
    type: Literal["triage"] = "triage"
    title: str
    body: str = ""
    category: Category
    severity: Severity
    confidence: Confidence
    location: Location | None = None
    options: list[ChoiceOption] | None = None


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
    TextFinding | ChoiceFinding | TriageFinding | EvaluationFinding,
    Field(discriminator="type"),
]


class SessionState(str, Enum):
    """Possible states of a review session."""

    PENDING = "pending"
    READY = "ready"
    COMPLETED = "completed"


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


class Session(BaseModel):
    """A review session containing findings and their responses."""

    session_id: str
    title: str
    zing_file: str
    expected_agents: int
    completed_agents: int = 0
    state: SessionState = SessionState.PENDING
    findings: list[Finding] = Field(default_factory=list)
    responses: list[UserResponse] | None = None
    created_at: datetime = Field(default_factory=datetime.now)


class ReviewItem(BaseModel):
    """A finding paired with its user response. Returned by wait_for_review()."""

    finding: Finding
    response: UserResponse


class ReviewResponse(BaseModel):
    """The complete review response for a session."""

    session_id: str
    items: list[ReviewItem]
