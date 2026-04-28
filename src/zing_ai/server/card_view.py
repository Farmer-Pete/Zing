"""Single-source-of-truth view model for a Kanban card.

A :class:`CardView` captures every value any renderer (the Jinja
fragment ``kanban_card.html``, the ``zing-ai debug-card`` CLI, future
API consumers) needs to draw a single card.  All inline first-match
rules and Jinja-namespace counters that previously lived in the
template are computed once here, in :func:`build_card_view`.

Adding a new card-displayed value is a single change: add a field to
the appropriate sub-view, populate it in the builder, and the debug
tool surfaces it automatically via Pydantic introspection.

This module is intentionally *additive* — existing callers can keep
threading ``card`` / ``column_cls`` / ``current_username`` through the
template until each block is migrated.  Nothing in this module mutates
:class:`KanbanCard`.
"""

from __future__ import annotations

from pydantic import BaseModel, Field

from zing_ai.server.command_center import (
    _pr_needs_response,
    _user_involved_in_done_card,
)
from zing_ai.server.models import (
    ClaudeCodeSession,
    SessionState,
    ZingSession,
)
from zing_ai.server.models_external import (
    CICheck,
    GitHubPR,
    KanbanCard,
    KanbanColumn,
)

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

_COLUMN_CLS: dict[KanbanColumn, str] = {
    "todo": "col-todo",
    "in_progress": "col-progress",
    "needs_review": "col-review",
    "done": "col-done",
}

_COLUMN_FROM_CLS: dict[str, KanbanColumn] = {v: k for k, v in _COLUMN_CLS.items()}


def column_from_cls(column_cls: str) -> KanbanColumn:
    """Reverse-map a CSS column class (``col-progress``) to its column literal.

    Exposed as a Jinja global so templates that already have ``column_cls``
    in scope can call :func:`build_card_view` without threading the column
    literal through the include chain.
    """
    return _COLUMN_FROM_CLS[column_cls]


# ---------------------------------------------------------------------------
# Sub-views
# ---------------------------------------------------------------------------


class StripPill(BaseModel):
    """A status pill rendered next to a PR strip (e.g. ``approved``)."""

    label: str
    css_class: str  # ``strip-pill-merged`` / ``strip-pill-approved`` / etc.


class PRPrimaryButton(BaseModel):
    """Primary-action button rendered for a single PR."""

    label: str  # ``Respond`` / ``PR Audit`` / ``Build Audit``
    skill: str  # ``pr-respond`` / ``pr-audit`` / ``build-audit``


class CICheckSummary(BaseModel):
    """Aggregate counts and the failure list for a PR's CI checks."""

    passing: int
    failing: int
    pending: int  # ``in_progress`` / ``queued``
    other: int  # ``neutral`` / ``cancelled`` / ``skipped``
    failing_checks: list[CICheck] = Field(default_factory=list)


class PRView(BaseModel):
    """Render-ready view of one PR on a card."""

    pr: GitHubPR
    is_author: bool
    needs_response: bool
    pill: StripPill | None
    primary_button: PRPrimaryButton | None  # ``None`` when merged
    ci: CICheckSummary


class ClaudeCodeSessionView(BaseModel):
    """Render-ready view of a ClaudeCodeSession strip."""

    session: ClaudeCodeSession
    is_alive: bool
    is_starting: bool
    status_label: str  # ``Running: <title>`` / ``Stopped: <title>`` / ``<title>``
    status_label_class: str  # ``""`` or ``"strip-session-label-dead"``
    dot_class: str  # ``strip-session-alive`` / ``-starting`` / ``-dead``
    dot_title: str  # ``Running`` / ``Starting`` / ``Stopped``
    pending_question_text: str | None  # truncated to 60 chars


class ZingSessionView(BaseModel):
    """Render-ready view of a ZingSession strip."""

    session: ZingSession
    dot_class: str  # ``strip-zing-amber`` / ``strip-zing-red`` / etc.
    state_label: str  # ``Claude session running`` / etc.


class FooterNote(BaseModel):
    """The small note rendered below findings (e.g. ``Waiting on others``)."""

    text: str


# ---------------------------------------------------------------------------
# CardView
# ---------------------------------------------------------------------------


class CardView(BaseModel):
    """Everything a renderer needs to draw a card in one column."""

    card: KanbanCard
    column: KanbanColumn
    column_cls: str
    current_username: str

    pr_views: list[PRView] = Field(default_factory=list)
    claude_code_session_views: list[ClaudeCodeSessionView] = Field(default_factory=list)
    zing_session_views: list[ZingSessionView] = Field(default_factory=list)

    total_findings: int
    has_active_action: bool
    footer_note: FooterNote | None
    card_dom_id: str
    extra_card_classes: list[str] = Field(default_factory=list)
    excluded_from_done_view: bool


# ---------------------------------------------------------------------------
# Builder helpers
# ---------------------------------------------------------------------------


def _strip_pill(pr: GitHubPR, column_cls: str, current_username: str) -> StripPill | None:
    """Pill rule from ``kanban_card.html`` lines 90-95 — first match wins."""
    if pr.merged_at is not None:
        return StripPill(label="merged", css_class="strip-pill-merged")
    if pr.draft:
        return StripPill(label="draft", css_class="strip-pill-draft")
    if pr.review_decision == "APPROVED":
        return StripPill(label="approved", css_class="strip-pill-approved")
    if pr.review_decision == "CHANGES_REQUESTED" and column_cls != "col-review":
        return StripPill(label="changes requested", css_class="strip-pill-changes")
    if (
        pr.state == "open"
        and pr.author == current_username
        and column_cls != "col-review"
        and any(r not in pr.requested_reviewers for r in pr.reviewers)
    ):
        return StripPill(label="reviewed", css_class="strip-pill-reviewed")
    return None


def _primary_button(
    pr: GitHubPR,
    column_cls: str,
    is_author: bool,
    needs_response: bool,
) -> PRPrimaryButton | None:
    """Primary-button rule.

    Defaults to Respond whenever the author has *any* reviewer with a
    non-APPROVED state that hasn't been re-requested.  Catches:

    * frontend-v2#259 — single COMMENTED reviewer on a single-PR card.
      ``_pr_needs_response`` returns False here because
      ``_is_human_reviewer`` conservatively classifies a lone COMMENTED
      reviewer as a bot, but the user expects Respond.
    * backend-v1#1895 — ``reviewDecision == APPROVED`` with a reviewer
      whose latest state is COMMENTED (they left follow-up comments
      after their approval).  ``_pr_needs_response`` short-circuits on
      ``APPROVED`` so it can't speak to this case.

    Falls through to Build Audit only when every live reviewer is in
    ``APPROVED`` state — i.e. the PR is genuinely awaiting merge.
    """
    if pr.merged_at is not None:
        return None
    if column_cls == "col-review" and not is_author:
        return PRPrimaryButton(label="PR Audit", skill="pr-audit")
    if needs_response:
        return PRPrimaryButton(label="Respond", skill="pr-respond")
    if is_author and any(
        state != "APPROVED"
        for login, state in pr.reviewer_states.items()
        if login not in pr.requested_reviewers
    ):
        return PRPrimaryButton(label="Respond", skill="pr-respond")
    if is_author:
        return PRPrimaryButton(label="Build Audit", skill="build-audit")
    return PRPrimaryButton(label="PR Audit", skill="pr-audit")


def _ci_summary(pr: GitHubPR) -> CICheckSummary:
    """Bucket :attr:`GitHubPR.ci_checks` by conclusion."""
    passing = failing = pending = other = 0
    failing_checks: list[CICheck] = []
    for check in pr.ci_checks:
        c = check.conclusion
        if c == "success":
            passing += 1
        elif c == "failure":
            failing += 1
            failing_checks.append(check)
        elif c in ("neutral", "cancelled", "skipped"):
            other += 1
        else:
            pending += 1
    return CICheckSummary(
        passing=passing,
        failing=failing,
        pending=pending,
        other=other,
        failing_checks=failing_checks,
    )


def _build_pr_view(
    card: KanbanCard, pr: GitHubPR, column_cls: str, current_username: str
) -> PRView:
    is_author = pr.author == current_username
    needs_response = _pr_needs_response(card, pr, current_username)
    return PRView(
        pr=pr,
        is_author=is_author,
        needs_response=needs_response,
        pill=_strip_pill(pr, column_cls, current_username),
        primary_button=_primary_button(pr, column_cls, is_author, needs_response),
        ci=_ci_summary(pr),
    )


def _build_claude_code_view(session: ClaudeCodeSession) -> ClaudeCodeSessionView:
    state = session.state
    is_alive = state == SessionState.STARTED
    is_starting = state == SessionState.STARTING
    has_terminal = session.terminal_session is not None

    if is_alive:
        status_label = f"Running: {session.title}"
        status_label_class = ""
        dot_class = "strip-session-alive"
        dot_title = "Running"
    elif is_starting:
        status_label = f"Starting: {session.title}"
        status_label_class = ""
        dot_class = "strip-session-starting"
        dot_title = "Starting"
    elif has_terminal:
        status_label = f"Stopped: {session.title}"
        status_label_class = "strip-session-label-dead"
        dot_class = "strip-session-dead"
        dot_title = "Stopped"
    else:
        status_label = session.title
        status_label_class = ""
        dot_class = "strip-session-dead"
        dot_title = "Stopped"

    pending = session.pending_question
    pending_text: str | None = None
    if pending is not None:
        body = pending.body
        pending_text = body if len(body) <= 60 else body[:60] + "..."

    return ClaudeCodeSessionView(
        session=session,
        is_alive=is_alive,
        is_starting=is_starting,
        status_label=status_label,
        status_label_class=status_label_class,
        dot_class=dot_class,
        dot_title=dot_title,
        pending_question_text=pending_text,
    )


_ZING_DOT_CLS: dict[SessionState, str] = {
    SessionState.STARTED: "strip-zing-amber",
    SessionState.PENDING: "strip-zing-red",
    SessionState.COMPLETED: "strip-zing-green",
}

_ZING_STATE_LABEL: dict[SessionState, str] = {
    SessionState.STARTED: "Claude session running",
    SessionState.PENDING: "Waiting to start",
    SessionState.READY: "Ready for review",
    SessionState.COMPLETED: "Session completed",
}


def _build_zing_view(session: ZingSession) -> ZingSessionView:
    state = session.state
    dot_class = _ZING_DOT_CLS.get(state, "strip-zing-cyan")
    state_label = _ZING_STATE_LABEL.get(state, state.value)
    return ZingSessionView(
        session=session,
        dot_class=dot_class,
        state_label=state_label,
    )


def _has_active_action(card: KanbanCard) -> bool:
    """Replicates the ``_active_action`` namespace from ``kanban_card.html`` L34-49."""
    for session in card.sessions:
        if isinstance(session, ZingSession) and any(
            step.state == SessionState.READY for step in session.steps
        ):
            return True
        if isinstance(session, ClaudeCodeSession) and session.pending_question is not None:
            return True
    return False


def _total_findings(card: KanbanCard) -> int:
    """Sum of findings across all audit steps on the card."""
    return sum(len(step.findings) for step in card.audit_steps)


def _footer_note(card: KanbanCard, column_cls: str) -> FooterNote | None:
    """Footer note rule from ``kanban_card.html`` lines 355-361."""
    if column_cls == "col-review" and card.review_group == "others":
        return FooterNote(text="Waiting on others")
    if column_cls == "col-progress" and card.in_progress_reason:
        return FooterNote(text=card.in_progress_reason)
    return None


def _card_dom_id(card_key: str) -> str:
    """Mirror ``id="card-{{ card.key | lower | replace('/', '-') }}"`` in the template."""
    return f"card-{card_key.lower().replace('/', '-')}"


def _extra_card_classes(column: KanbanColumn, card: KanbanCard) -> list[str]:
    """``card-ready-to-merge`` is set by ``kanban_column_done.html`` for that subgroup."""
    if column == "done" and card.done_group == "ready_to_merge":
        return ["card-ready-to-merge"]
    return []


# ---------------------------------------------------------------------------
# Public factory
# ---------------------------------------------------------------------------


def build_card_view(
    card: KanbanCard,
    column: KanbanColumn,
    current_username: str,
) -> CardView:
    """Compute display state for *card* in *column* exactly once.

    Single source of truth for strip-pill rules, primary-button rules,
    CI bucketing, session-status labels, footer-note rules, DOM id
    derivation, and column CSS class selection.

    Pure function; safe to call repeatedly.
    """
    column_cls = _COLUMN_CLS[column]

    pr_views = [_build_pr_view(card, pr, column_cls, current_username) for pr in card.prs]

    claude_code_session_views: list[ClaudeCodeSessionView] = []
    zing_session_views: list[ZingSessionView] = []
    for session in card.sessions:
        if isinstance(session, ClaudeCodeSession):
            claude_code_session_views.append(_build_claude_code_view(session))
        elif isinstance(session, ZingSession):
            zing_session_views.append(_build_zing_view(session))

    excluded_from_done_view = column == "done" and not _user_involved_in_done_card(
        card, current_username
    )

    return CardView(
        card=card,
        column=column,
        column_cls=column_cls,
        current_username=current_username,
        pr_views=pr_views,
        claude_code_session_views=claude_code_session_views,
        zing_session_views=zing_session_views,
        total_findings=_total_findings(card),
        has_active_action=_has_active_action(card),
        footer_note=_footer_note(card, column_cls),
        card_dom_id=_card_dom_id(card.key),
        extra_card_classes=_extra_card_classes(column, card),
        excluded_from_done_view=excluded_from_done_view,
    )
