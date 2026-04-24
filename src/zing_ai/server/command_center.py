"""Aggregation logic for the Command Center dashboard.

Takes typed snapshots from Linear, GitHub, and the in-memory SessionManager
and produces the Kanban view rendered on ``/command-center``. Most functions
are pure (no I/O) and can be unit tested with synthetic data.
``get_live_tmux_sessions`` is the exception — it shells out to ``tmux``.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta

from zing_ai.launch import TICKET_ID_PATTERN
from zing_ai.server.models import (
    ClaudeCodeSession,
    Session,
    SessionState,
    WorkflowStep,
    ZingSession,
)
from zing_ai.server.models_external import (
    GitHubPR,
    KanbanCard,
    KanbanView,
    LinearIssue,
)

AUDIT_STEP_NAMES: frozenset[str] = frozenset(
    {
        "plan-audit",
        "build-audit",
        "pr-audit",
        "custom-audit",
    }
)


def _actionable_findings(step: WorkflowStep) -> list:
    """Return the subset of *step*'s findings that require user action.

    Evaluation findings are informational (criteria + litmus tests + warnings)
    and don't drive user input. Excluding them from the "hot" urgency gate
    prevents a hub that's really just been audited for context from screaming
    for attention.
    """
    return [f for f in step.findings if getattr(f, "type", None) != "evaluation"]


# Ticket ids look like ``BAK-1179`` / ``FRO-42``. Uses the shared pattern from
# launch.py with word boundaries and case-insensitive matching so noisy tokens
# like ``UTF-8``/``SHA-256``/``PR-1`` don't masquerade as tickets.
_TICKET_RE = re.compile(rf"\b{TICKET_ID_PATTERN}\b", re.IGNORECASE)
_PR_NUMBER_RE = re.compile(r"#(\d+)\b")


def _session_pr_number(session: Session) -> int | None:
    """Extract a PR number from a session, if available.

    For ``ClaudeCodeSession``, uses the explicit ``pr_number`` field only.
    For ``ZingSession``, parses ``#<number>`` from the title (e.g.
    ``"PR Review — #1858 feat: ..."``) or session_id (e.g.
    ``"pr-review-1858-..."``)

    Title/ID parsing is intentionally skipped for ``ClaudeCodeSession``
    because those sessions may carry a ``ticket_id`` that failed to match
    a card — falling back to title parsing would incorrectly attach them
    to an orphan PR card.
    """
    if isinstance(session, ClaudeCodeSession):
        return session.pr_number  # explicit field only, may be None
    # ZingSession: try title first: "PR Review — #1858 ..."
    match = _PR_NUMBER_RE.search(session.title)
    if match:
        return int(match.group(1))
    # Try session_id: "pr-review-1858-..."
    id_match = re.match(r"pr-review-(\d+)-", session.session_id)
    if id_match:
        return int(id_match.group(1))
    return None


def _parse_ticket_id(pr: GitHubPR) -> str | None:
    """Return the first ticket identifier referenced by a PR, if any.

    Scans ``head_ref``, then ``title``, then ``body`` (in that order) for the
    first match of ``\\b[A-Z]{2,}-\\d+\\b`` and uppercases it. Branch names
    carry the canonical ticket id in practice, so first-match ordering picks
    the right hub even when the PR body mentions a second ticket.
    """
    text = " ".join(filter(None, [pr.head_ref, pr.title, pr.body]))
    match = _TICKET_RE.search(text)
    return match.group(0).upper() if match else None


# ---------------------------------------------------------------------------
# New Kanban aggregation (Step 5)
# ---------------------------------------------------------------------------

_DONE_WINDOW = timedelta(days=7)


def _ensure_utc(dt: datetime) -> datetime:
    """Return *dt* as a UTC-aware datetime."""
    return dt.replace(tzinfo=UTC) if dt.tzinfo is None else dt


def _card_most_recent_activity(card: KanbanCard) -> datetime:
    """Return the most recent activity timestamp for sorting purposes."""
    candidates: list[datetime] = []
    if card.ticket is not None:
        candidates.append(card.ticket.updated_at)
    for pr in card.prs:
        candidates.append(pr.updated_at)
        if pr.merged_at is not None:
            candidates.append(pr.merged_at)
    for session in card.sessions:
        candidates.append(session.created_at)
        if isinstance(session, ZingSession):
            for step in session.steps:
                candidates.append(step.created_at)
    if not candidates:
        return datetime.min.replace(tzinfo=UTC)
    return max(_ensure_utc(dt) for dt in candidates)


# -- Signal helpers ----------------------------------------------------------


def _is_owned_by_user(card: KanbanCard, current_username: str) -> bool:
    """The card belongs to the user: ticket is assigned to them or they authored a PR.

    All tickets fetched from Linear are already filtered by the current viewer,
    so a non-None ticket implies ownership.  For orphan-PR cards the user must
    be the author of at least one linked PR.
    """
    if card.ticket is not None:
        return True
    return any(pr.author == current_username for pr in card.prs)


def _has_active_session(card: KanbanCard) -> bool:
    """Any ZingSession has a step in STARTED state.

    ClaudeCodeSession is intentionally excluded — it has no lifecycle
    states, so treating it as always-active would permanently pin cards
    to In Progress.  It still renders as a session indicator on the card.
    """
    return any(
        step.state == SessionState.STARTED
        for session in card.sessions
        if isinstance(session, ZingSession)
        for step in session.steps
    )


def _has_open_pr_in_progress(card: KanbanCard, current_username: str) -> bool:
    """Any linked PR is open with no pending reviews.

    For orphan-PR cards (no ticket), require the PR to be authored by
    the user so other people's un-reviewed PRs don't clutter the board.
    """
    return any(
        pr.state == "open"
        and not pr.requested_reviewers
        and pr.review_decision != "APPROVED"
        and (card.ticket is not None or pr.author == current_username)
        for pr in card.prs
    )


def _has_unaddressed_feedback(card: KanbanCard, current_username: str) -> bool:
    """User authored a PR that has reviewer feedback they haven't addressed yet.

    A reviewer's feedback is unaddressed when they submitted a review
    (appear in ``reviewers``) but have NOT been re-requested (not in
    ``requested_reviewers``), and they are a human (not a bot).

    Human vs bot detection uses ``reviewer_states``: reviewers who submitted
    ``CHANGES_REQUESTED`` or ``APPROVED`` are definitely human.  Reviewers
    who only ``COMMENTED`` are human if they appear in ``requested_reviewers``
    on any PR on the card; otherwise they are assumed to be bots.
    """
    # Build set of all logins that appear in requested_reviewers across the card.
    ever_requested: set[str] = set()
    for pr in card.prs:
        ever_requested.update(pr.requested_reviewers)

    for pr in card.prs:
        if pr.state != "open" or pr.author != current_username or pr.review_decision == "APPROVED":
            continue

        not_rerequested = set(pr.reviewers) - set(pr.requested_reviewers)
        for reviewer in not_rerequested:
            state = pr.reviewer_states.get(reviewer, "COMMENTED")
            if state in ("CHANGES_REQUESTED", "APPROVED"):
                # Definitely a human reviewer with unaddressed feedback.
                return True
            if state == "COMMENTED" and reviewer in ever_requested:
                # Comment-only review from a known requested reviewer.
                return True

    return False


def _has_pr_needing_review(card: KanbanCard, current_username: str) -> bool:
    """Any linked PR has reviewers requested and the user is involved."""
    for pr in card.prs:
        if (
            pr.state == "open"
            and pr.review_decision != "APPROVED"
            and pr.requested_reviewers
            and (current_username in pr.requested_reviewers or pr.author == current_username)
        ):
            return True
    return False


def _is_recently_done(card: KanbanCard, cutoff: datetime, current_username: str = "") -> bool:
    """Ticket completed, PR merged/approved, or user submitted a review within the cutoff."""
    if (
        card.ticket is not None
        and card.ticket.state_type == "completed"
        and _ensure_utc(card.ticket.updated_at) >= cutoff
    ):
        return True

    for pr in card.prs:
        if (
            pr.state == "merged"
            and pr.merged_at is not None
            and _ensure_utc(pr.merged_at) >= cutoff
        ):
            return True
        if pr.review_decision == "APPROVED" and _ensure_utc(pr.updated_at) >= cutoff:
            return True
        # User submitted a review — their review work is done
        if (
            pr.state == "open"
            and current_username
            and current_username in pr.reviewers
            and current_username not in pr.requested_reviewers
            and _ensure_utc(pr.updated_at) >= cutoff
        ):
            return True

    return False


# -- Filter ------------------------------------------------------------------


def _should_include_card(card: KanbanCard, current_username: str, cutoff: datetime) -> bool:
    """Return False when the card should be excluded from the board entirely."""
    # Orphan-PR cards where the user is neither author nor reviewer and has no session.
    if card.ticket is None and card.prs:
        user_involved = any(
            pr.author == current_username
            or current_username in pr.requested_reviewers
            or current_username in pr.reviewers
            for pr in card.prs
        )
        if not user_involved and not card.sessions:
            return False

    # Stale approved PRs: all PRs approved, none updated within the done window,
    # and no open ticket driving them.
    if card.prs and all(pr.review_decision == "APPROVED" for pr in card.prs):
        any_recent = any(_ensure_utc(pr.updated_at) >= cutoff for pr in card.prs)
        if not any_recent:
            has_open_ticket = card.ticket is not None and card.ticket.state_type not in (
                "completed",
                "cancelled",
            )
            if not has_open_ticket:
                return False

    return True


# -- Review grouping ---------------------------------------------------------


def _assign_review_group(card: KanbanCard, current_username: str) -> str:
    """Return the review sub-group for a needs_review card.

    - ``mine_passing``: user is a requested reviewer and CI is passing
    - ``mine_failing``: user is a requested reviewer and CI is not passing
    - ``others``: user is the author waiting on other reviewers
    """
    user_is_reviewer = any(current_username in pr.requested_reviewers for pr in card.prs)
    if user_is_reviewer:
        ci_passing = all(pr.ci_status == "success" for pr in card.prs if pr.ci_status is not None)
        return "mine_passing" if ci_passing else "mine_failing"
    return "others"


# -- Classification ----------------------------------------------------------


@dataclass(frozen=True)
class CardSignals:
    """Independent boolean signals computed from a card's state.

    Each signal answers one question about the card. The classification
    decision is a simple truth table over these signals, making it easy
    to see why a card lands in a particular column and to adjust
    priorities without accidentally breaking other cases.
    """

    owned: bool
    """Card is assigned to (or authored by) the current user."""

    has_active_session: bool
    """A ZingSession has a workflow step in STARTED state."""

    has_open_pr_no_reviewers: bool
    """An open PR exists with no requested reviewers and not yet approved."""

    has_unaddressed_feedback: bool
    """User authored a PR with reviewer feedback they haven't re-requested on."""

    has_pr_awaiting_review: bool
    """An open PR has requested reviewers and the user is involved."""

    is_recently_done: bool
    """Ticket completed, PR merged, or user submitted a review within the done window."""

    ticket_started: bool
    """The linked Linear ticket has state_type 'started'."""


def _compute_signals(
    card: KanbanCard,
    current_username: str,
    cutoff: datetime,
) -> CardSignals:
    """Compute all classification signals for *card* independently."""
    return CardSignals(
        owned=_is_owned_by_user(card, current_username),
        has_active_session=_has_active_session(card),
        has_open_pr_no_reviewers=_has_open_pr_in_progress(card, current_username),
        has_unaddressed_feedback=_has_unaddressed_feedback(card, current_username),
        has_pr_awaiting_review=_has_pr_needing_review(card, current_username),
        is_recently_done=_is_recently_done(card, cutoff, current_username),
        ticket_started=(card.ticket is not None and card.ticket.state_type == "started"),
    )


def _classify_card(
    card: KanbanCard,
    current_username: str,
    now: datetime,
) -> str | None:
    """Return the Kanban column name for *card*.

    Returns ``None`` when the card should be excluded from the board.

    Decision table (first match wins):

    =============================================  ================
    Condition                                      Column
    =============================================  ================
    Card excluded by filter                        ``None``
    User has unaddressed reviewer feedback          in_progress
    PR is awaiting review from someone              needs_review
    Owned + active session or open PR (no reviews)  in_progress
    Recently done (merged / completed / reviewed)   done
    Owned + ticket state is "started"               in_progress
    Everything else                                 todo
    =============================================  ================
    """
    cutoff = now - _DONE_WINDOW

    if not _should_include_card(card, current_username, cutoff):
        return None

    s = _compute_signals(card, current_username, cutoff)

    # User has feedback to address — they need to act.
    if s.has_unaddressed_feedback:
        return "in_progress"

    # PR is out for review — user is blocked on reviewers.
    if s.has_pr_awaiting_review:
        return "needs_review"

    # Actively working: session running or PR not yet sent for review.
    if s.owned and (s.has_active_session or s.has_open_pr_no_reviewers):
        return "in_progress"

    if s.is_recently_done:
        return "done"

    # Ticket is being worked on but no PR signal drove it elsewhere.
    if s.owned and s.ticket_started:
        return "in_progress"

    return "todo"


def _user_involved_in_done_card(card: KanbanCard, current_username: str) -> bool:
    """Return True if the user authored or actually reviewed any PR on this card.

    For ticket-only cards (no PRs), always include them (they're the user's
    assigned tickets).  Only checks ``author`` and ``reviewers`` (submitted
    reviews) — being in ``requested_reviewers`` alone is not enough, since the
    user may have been requested but never reviewed (e.g. PR was approved by
    someone else).
    """
    if not card.prs:
        return True  # ticket-only card, no PR filter needed
    return any(pr.author == current_username or current_username in pr.reviewers for pr in card.prs)


def _assign_done_group(card: KanbanCard, current_username: str) -> str:
    """Return the done sub-group for a done card.

    - ``ready_to_merge``: has an open PR authored by the user that is approved
      but not yet merged
    - ``completed``: merged PRs or completed tickets
    """
    for pr in card.prs:
        if (
            pr.state == "open"
            and pr.review_decision == "APPROVED"
            and pr.author == current_username
        ):
            return "ready_to_merge"
    return "completed"


def _todo_sort_key(card: KanbanCard) -> tuple:
    """Sort key for todo column.

    State-type bucket: other=0 (TOP), backlog=1, triage=2 (BOTTOM).
    Within bucket: priority ascending (1=urgent first, 4=low last, 0=no priority last).
    """
    if card.ticket is None:
        return (0, 0)
    state_type = card.ticket.state_type
    if state_type == "triage":
        bucket = 2
    elif state_type == "backlog":
        bucket = 1
    else:
        bucket = 0
    priority = card.ticket.priority
    # 0 = no priority → sort last within bucket
    effective_priority = priority if priority != 0 else 5
    return (bucket, effective_priority)


def aggregate(
    issues: list[LinearIssue],
    prs: list[GitHubPR],
    recent_prs: list[GitHubPR] | None = None,
    completed_issues: list[LinearIssue] | None = None,
    sessions: list[Session] | None = None,
    current_username: str | None = None,
) -> KanbanView:
    """Build the Kanban board view from external + local snapshots.

    Standalone sessions (no ``ticket_id``) are excluded.
    Cards without at least a ticket or a PR are excluded.
    """
    _recent_prs: list[GitHubPR] = recent_prs or []
    _completed_issues: list[LinearIssue] = completed_issues or []
    _sessions = sessions or []
    _username = current_username or ""

    now = datetime.now(UTC)

    # -----------------------------------------------------------------------
    # 1. Build cards keyed by ticket identifier
    # -----------------------------------------------------------------------
    cards: dict[str, KanbanCard] = {}

    # Open issues
    for issue in issues:
        cards[issue.identifier] = KanbanCard(
            key=issue.identifier,
            ticket=issue,
        )

    # Completed issues (may overlap with issues; _completed_issues wins if newer)
    for issue in _completed_issues:
        if issue.identifier not in cards:
            cards[issue.identifier] = KanbanCard(
                key=issue.identifier,
                ticket=issue,
            )
        else:
            # Keep whichever has the more recent updated_at
            existing = cards[issue.identifier]
            if existing.ticket is None or issue.updated_at > existing.ticket.updated_at:
                cards[issue.identifier].ticket = issue

    # -----------------------------------------------------------------------
    # 2. Attach open PRs; collect orphan PRs
    # -----------------------------------------------------------------------
    orphan_prs: list[GitHubPR] = []
    for pr in prs:
        pr_ticket_id = _parse_ticket_id(pr)
        if pr_ticket_id is not None and pr_ticket_id in cards:
            cards[pr_ticket_id].prs.append(pr)
        else:
            orphan_prs.append(pr)

    # -----------------------------------------------------------------------
    # 3. Attach recent (merged) PRs; new orphan merged PRs
    # -----------------------------------------------------------------------
    for pr in _recent_prs:
        pr_ticket_id = _parse_ticket_id(pr)
        if pr_ticket_id is not None and pr_ticket_id in cards:
            # Avoid duplicates if same PR already attached
            existing_numbers = {p.number for p in cards[pr_ticket_id].prs}
            if pr.number not in existing_numbers:
                cards[pr_ticket_id].prs.append(pr)
        else:
            # Check if already tracked as orphan
            orphan_numbers = {p.number for p in orphan_prs}
            if pr.number not in orphan_numbers:
                orphan_prs.append(pr)

    # -----------------------------------------------------------------------
    # 4. Attach sessions; skip standalone sessions (no ticket_id and no pr_number)
    # -----------------------------------------------------------------------
    # Sessions that have a pr_number but no ticket_id will be attached to
    # orphan PR cards (keyed as "pr-{number}") in step 5 below.
    pr_sessions: list[Session] = []
    # Index ticket cards by PR number for fallback matching.
    _card_by_pr: dict[tuple[str, int], KanbanCard] = {}
    for card in cards.values():
        for pr in card.prs:
            _card_by_pr[(pr.repo, pr.number)] = card

    for session in _sessions:
        if session.ticket_id and session.ticket_id in cards:
            cards[session.ticket_id].sessions.append(session)
            # Surface audit steps (ZingSession only — ClaudeCodeSession has no steps)
            if isinstance(session, ZingSession):
                for step in session.steps:
                    if step.step_name in AUDIT_STEP_NAMES:
                        cards[session.ticket_id].audit_steps.append(step)
        elif _session_pr_number(session) is not None:
            # Try matching to a ticket card by PR number before deferring to orphan.
            pr_num = _session_pr_number(session)
            pr_repo = getattr(session, "pr_repo", None) or ""
            matched_card = _card_by_pr.get((pr_repo, pr_num))  # type: ignore[arg-type]
            if matched_card is not None:
                matched_card.sessions.append(session)
                if isinstance(session, ZingSession):
                    for step in session.steps:
                        if step.step_name in AUDIT_STEP_NAMES:
                            matched_card.audit_steps.append(step)
            else:
                pr_sessions.append(session)

    # -----------------------------------------------------------------------
    # 5. Orphan PR cards (no matching ticket)
    # -----------------------------------------------------------------------
    orphan_cards: dict[str, KanbanCard] = {}
    for pr in orphan_prs:
        key = f"pr-{pr.repo}-{pr.number}" if pr.repo else f"pr-{pr.number}"
        orphan_cards[key] = KanbanCard(
            key=key,
            ticket=None,
            prs=[pr],
        )

    # Attach sessions that matched by PR number (from step 4).
    # ClaudeCodeSession has pr_repo for exact matching; ZingSession falls back
    # to matching by PR number suffix across all orphan card keys.
    for session in pr_sessions:
        pr_num = _session_pr_number(session)
        pr_repo = getattr(session, "pr_repo", None) or ""
        key = f"pr-{pr_repo}-{pr_num}" if pr_repo else None
        if key and key in orphan_cards:
            target_card = orphan_cards[key]
        else:
            # Fallback: match by PR number suffix (for ZingSessions without repo info)
            suffix = f"-{pr_num}"
            target_card = next((c for k, c in orphan_cards.items() if k.endswith(suffix)), None)
        if target_card is not None:
            target_card.sessions.append(session)
            if isinstance(session, ZingSession):
                for step in session.steps:
                    if step.step_name in AUDIT_STEP_NAMES:
                        target_card.audit_steps.append(step)

    # -----------------------------------------------------------------------
    # 6. Collect all cards and filter empties
    # -----------------------------------------------------------------------
    all_cards: list[KanbanCard] = [c for c in cards.values() if c.ticket is not None or c.prs] + [
        c for c in orphan_cards.values() if c.prs
    ]

    # -----------------------------------------------------------------------
    # 7. Classify and attach audit state
    # -----------------------------------------------------------------------
    view = KanbanView()
    for card in all_cards:
        # Attach audit steps that are READY with actionable findings
        ready_audit_steps = [
            step
            for step in card.audit_steps
            if step.state == SessionState.READY and _actionable_findings(step)
        ]
        card.audit_steps = ready_audit_steps

        column = _classify_card(card, _username, now)

        if column is None:
            continue
        elif column == "in_progress":
            view.in_progress.append(card)
        elif column == "needs_review":
            view.needs_review.append(card)
        elif column == "done":
            if _user_involved_in_done_card(card, _username):
                view.done.append(card)
        else:
            view.todo.append(card)

    # -----------------------------------------------------------------------
    # 8. Sort each column
    # -----------------------------------------------------------------------
    # To Do: state_type bucket asc, then priority asc (1=urgent first, 0=last)
    view.todo.sort(key=_todo_sort_key)

    # Other columns: most recent activity first (descending)
    for col in (view.in_progress, view.needs_review, view.done):
        col.sort(key=_card_most_recent_activity, reverse=True)

    # -----------------------------------------------------------------------
    # 9. Assign review groups for the needs_review column
    # -----------------------------------------------------------------------
    for card in view.needs_review:
        card.review_group = _assign_review_group(card, _username)

    # Sort needs_review by group order: mine_passing, mine_failing, others
    _GROUP_ORDER = {"mine_passing": 0, "mine_failing": 1, "others": 2}
    view.needs_review.sort(key=lambda c: _GROUP_ORDER.get(c.review_group or "", 3))

    # -----------------------------------------------------------------------
    # 10. Assign done groups and sort: ready_to_merge first, then completed
    # -----------------------------------------------------------------------
    for card in view.done:
        card.done_group = _assign_done_group(card, _username)

    _DONE_GROUP_ORDER = {"ready_to_merge": 0, "completed": 1}
    view.done.sort(key=lambda c: _DONE_GROUP_ORDER.get(c.done_group or "", 2))

    return view


def _card_display_title(card: KanbanCard) -> str:
    """Return a human-readable title for a card (ticket title or PR title)."""
    if card.ticket is not None:
        return card.ticket.title
    if card.prs:
        return card.prs[0].title
    return card.key


def _card_url(card: KanbanCard) -> str | None:
    """Return the most relevant URL for a card (ticket URL or PR URL)."""
    if card.ticket is not None:
        return card.ticket.url
    if card.prs:
        return card.prs[0].url
    return None


def generate_standup(view: KanbanView, current_username: str) -> str:
    """Generate a standup message from the current Kanban board state.

    The message lists:
    - What got done yesterday (merged PRs, completed tickets)
    - What's on the plate today (in-progress work)
    - Blockers (PRs waiting on others for review)

    Args:
        view: The current KanbanView with all columns populated.
        current_username: The GitHub username of the current user.

    Returns:
        A formatted standup message string in markdown.
    """
    now = datetime.now(UTC)
    today_start = now.replace(hour=0, minute=0, second=0, microsecond=0)
    yesterday_start = today_start - timedelta(days=1)

    # -- Done items: split into yesterday vs today --
    done_yesterday: list[str] = []
    done_today: list[str] = []

    for card in view.done:
        # Only include cards the user owns (assigned ticket or authored PR).
        # Skip cards where the user only reviewed someone else's PR.
        is_own_ticket = card.ticket is not None
        is_own_pr = any(pr.author == current_username for pr in card.prs)
        if not is_own_ticket and not is_own_pr:
            continue

        title = _card_display_title(card)
        url = _card_url(card)
        activity_time = _card_most_recent_activity(card)

        # Determine what "done" means for this card
        has_merged_pr = any(pr.state == "merged" for pr in card.prs)
        ticket_completed = card.ticket is not None and card.ticket.state_type == "completed"

        if has_merged_pr:
            label = f"Merged [{title}]({url})" if url else f"Merged {title}"
        elif ticket_completed:
            label = f"Completed [{title}]({url})" if url else f"Completed {title}"
        elif card.done_group == "ready_to_merge":
            label = f"Ready to merge [{title}]({url})" if url else f"Ready to merge {title}"
        else:
            label = f"Completed [{title}]({url})" if url else f"Completed {title}"

        if activity_time >= today_start:
            done_today.append(f"- {label}")
        elif activity_time >= yesterday_start:
            done_yesterday.append(f"- {label}")

    # -- In progress items --
    in_progress_items: list[str] = []

    for card in view.in_progress:
        title = _card_display_title(card)
        url = _card_url(card)

        # Check if the card has unaddressed feedback (returning to address comments)
        has_feedback = _has_unaddressed_feedback(card, current_username)

        if has_feedback:
            label = (
                f"Resolve comments on [{title}]({url})" if url else (f"Resolve comments on {title}")
            )
        else:
            label = f"Work on [{title}]({url})" if url else f"Work on {title}"

        in_progress_items.append(f"- {label}")

    # -- Needs review: user's own PRs waiting on others --
    # These contribute to both "done" (the work itself) and "blockers" (the review wait).
    blocker_items: list[str] = []

    for card in view.needs_review:
        if card.review_group != "others":
            continue

        # The ticket/work goes into "what got done" as "Worked on ..."
        title = _card_display_title(card)
        url = _card_url(card)
        label = f"Worked on [{title}]({url})" if url else f"Worked on {title}"
        activity_time = _card_most_recent_activity(card)
        if activity_time >= today_start:
            done_today.append(f"- {label}")
        elif activity_time >= yesterday_start:
            done_yesterday.append(f"- {label}")

        # The PR itself is a blocker — use the PR URL
        pr_url = card.prs[0].url if card.prs else _card_url(card)
        label = f"Need a review on [{title}]({pr_url})" if pr_url else (f"Need a review on {title}")
        blocker_items.append(f"- {label}")

    # -- Build message --
    sections: list[str] = []

    done_lines = done_yesterday + done_today
    if done_lines:
        sections.append("**What got done yesterday:**\n" + "\n".join(done_lines))
    else:
        sections.append("**What got done yesterday:**\n- (nothing to report)")

    if in_progress_items:
        sections.append("**What's on my plate today:**\n" + "\n".join(in_progress_items))
    else:
        sections.append("**What's on my plate today:**\n- (nothing planned)")

    if blocker_items:
        sections.append("**Blockers:**\n" + "\n".join(blocker_items))
    else:
        sections.append("**Blockers:**\n- None")

    return "\n\n".join(sections)


def get_live_tmux_sessions() -> set[str]:
    """Return the set of active tmux session names.

    Runs ``tmux list-sessions`` to get the current session list.
    Returns an empty set if tmux is not installed, not running, or has no sessions.
    """
    import subprocess

    try:
        result = subprocess.run(
            ["tmux", "list-sessions", "-F", "#{session_name}"],
            capture_output=True,
            text=True,
        )
    except FileNotFoundError:
        return set()
    if result.returncode != 0:
        return set()
    return {line for line in result.stdout.splitlines() if line}
