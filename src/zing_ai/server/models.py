"""Pydantic data models for the Zing batch review server."""

from __future__ import annotations

import logging
from datetime import datetime
from enum import StrEnum
from typing import Annotated, Any, Literal
from uuid import uuid4

from pydantic import BaseModel, ConfigDict, Discriminator, Field, PrivateAttr, Tag, model_validator

from zing_ai.server.signals import to_signal_key as _to_signal_key


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


class SessionState(StrEnum):
    """Possible states of a review session."""

    PENDING = "pending"
    STARTED = "started"
    READY = "ready"
    COMPLETED = "completed"
    STOPPED = "stopped"


_STATE_PRIORITY: dict[str, int] = {
    SessionState.PENDING: 0,
    SessionState.STARTED: 1,
    SessionState.READY: 2,
}


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


class QuestionOption(BaseModel):
    """A single labeled option in a structured AskUserQuestion notification."""

    label: str
    description: str = ""


class QuestionData(BaseModel):
    """Structured payload for an AskUserQuestion-style notification.

    Carries the question text plus optional UI hints (header, options,
    multi-select) so the drawer can render real labelled choices instead of a
    JSON dump of the raw tool input.
    """

    question: str
    header: str = ""
    multi_select: bool = False
    options: list[QuestionOption] = Field(default_factory=list)


class Notification(BaseModel):
    """A notification record stored per-session."""

    id: str = Field(default_factory=lambda: uuid4().hex[:8])
    title: str
    body: str = ""
    question: QuestionData | None = None
    url: str | None = None
    created_at: datetime = Field(default_factory=datetime.now)
    answered_at: datetime | None = None


class ResponseAction(StrEnum):
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
        return UserResponse(
            **{
                field: (
                    getattr(self, field)
                    if getattr(self, field) is not None
                    else getattr(existing, field)
                )
                for field in self.__class__.model_fields
            }
        )


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


class SessionBase(BaseModel):
    """Shared base for all session types."""

    model_config = ConfigDict(extra="ignore")

    session_id: str
    title: str
    ticket_id: str | None = None
    created_at: datetime = Field(default_factory=datetime.now)

    @property
    def signal_key(self) -> str:
        """Return :attr:`session_id` sanitised for use as a Datastar signal name.

        Same purpose as :attr:`KanbanCard.signal_key` — see that property's
        docstring for the rationale.
        """
        return _to_signal_key(self.session_id)


class ZingSession(SessionBase):
    """A review session containing workflow steps with findings and responses."""

    session_type: Literal["zing"] = "zing"
    zing_file: str | None = None
    steps: list[WorkflowStep] = Field(default_factory=list)
    notifications: list[Notification] = Field(default_factory=list)

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
    def state(self) -> SessionState:
        """Compute session state from step states.

        The session is COMPLETED only when all steps are COMPLETED.
        Otherwise, the highest-priority non-COMPLETED state is used.
        """
        if not self.steps:
            return SessionState.PENDING
        if all(s.state == SessionState.COMPLETED for s in self.steps):
            return SessionState.COMPLETED
        non_completed = [s for s in self.steps if s.state != SessionState.COMPLETED]
        highest_priority_step = max(non_completed, key=lambda s: _STATE_PRIORITY.get(s.state, 0))
        return highest_priority_step.state

    @property
    def current_step_name(self) -> str | None:
        """Return the name of the most recent workflow step, or None."""
        return self.steps[-1].step_name if self.steps else None

    @property
    def total_findings(self) -> int:
        """Return the total number of findings across all steps."""
        return sum(len(step.findings) for step in self.steps)


class ClaudeCodeSession(SessionBase):
    """An interactive Claude Code session launched from the Command Center."""

    session_type: Literal["claude_code"] = "claude_code"
    worktree_path: str | None = None
    skill: str | None = None
    pr_number: int | None = None
    pr_repo: str | None = None
    terminal_session: str | None = None
    notifications: list[Notification] = Field(default_factory=list)
    _session_alive: bool = PrivateAttr(default=False)

    @model_validator(mode="before")
    @classmethod
    def _migrate_tmux_session(cls, data: dict) -> dict:
        """Backward compat: load old JSON files with 'tmux_session' key."""
        if isinstance(data, dict) and "tmux_session" in data and "terminal_session" not in data:
            data["terminal_session"] = data.pop("tmux_session")
        return data

    @property
    def pending_question(self) -> Notification | None:
        """Return the last unanswered notification, or None."""
        return next((n for n in reversed(self.notifications) if n.answered_at is None), None)

    @property
    def state(self) -> SessionState:
        """Return the session state.

        If *terminal_session* is set and the session is not alive, return STOPPED.
        Otherwise return STARTED.
        """
        if self.terminal_session is not None and not self._session_alive:
            return SessionState.STOPPED
        return SessionState.STARTED


def _session_discriminator(data: Any) -> str:
    """Return session_type, defaulting to 'zing' for old JSON files that lack it."""
    if isinstance(data, dict):
        return data.get("session_type", "zing")
    if hasattr(data, "session_type"):
        return data.session_type
    return "zing"


Session = Annotated[
    Annotated[ZingSession, Tag("zing")] | Annotated[ClaudeCodeSession, Tag("claude_code")],
    Discriminator(_session_discriminator),
]


class ReviewItem(BaseModel):
    """A finding paired with its user response. Returned by wait_for_review()."""

    finding: Finding
    response: UserResponse


class ReviewResponse(BaseModel):
    """The complete review response for a workflow step."""

    session_id: str
    step_name: str
    items: list[ReviewItem]
    auto_completed: bool = False
