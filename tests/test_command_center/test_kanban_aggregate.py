"""Unit tests for the new KanbanView-based aggregate() function (Step 5)."""

from __future__ import annotations

import unittest
from datetime import UTC, datetime, timedelta

from tests.test_command_center.conftest import (
    make_claude_code_session as _make_cc_session,
)
from tests.test_command_center.conftest import (
    make_issue as _make_issue,
)
from tests.test_command_center.conftest import (
    make_pr as _make_pr,
)
from tests.test_command_center.conftest import (
    make_session as _make_session,
)
from tests.test_command_center.conftest import (
    make_workflow_step as _make_workflow_step,
)
from zing_ai.server.command_center import aggregate
from zing_ai.server.models import SessionState, TextFinding
from zing_ai.server.models_external import KanbanCard, KanbanView

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

NOW = datetime.now(UTC)
RECENT = NOW - timedelta(days=3)
OLD = NOW - timedelta(days=10)  # outside the 7-day done window


def _agg(
    issues=None,
    prs=None,
    recent_prs=None,
    completed_issues=None,
    sessions=None,
    current_username="octocat",
) -> KanbanView:
    """Convenience wrapper for the new 6-arg aggregate() call."""
    return aggregate(
        issues=issues or [],
        prs=prs or [],
        recent_prs=recent_prs or [],
        completed_issues=completed_issues or [],
        sessions=sessions or [],
        current_username=current_username,
    )


def _all_cards(view: KanbanView) -> list[KanbanCard]:
    return view.todo + view.in_progress + view.needs_review + view.done


# ---------------------------------------------------------------------------
# Basic grouping
# ---------------------------------------------------------------------------


class TestAggregateEmptyInputs(unittest.TestCase):
    """aggregate() returns an empty KanbanView for empty inputs."""

    def test_empty_returns_kanban_view(self) -> None:
        view = _agg()
        self.assertIsInstance(view, KanbanView)
        self.assertEqual(view.todo, [])
        self.assertEqual(view.in_progress, [])
        self.assertEqual(view.needs_review, [])
        self.assertEqual(view.done, [])

    def test_empty_total_cards_is_zero(self) -> None:
        view = _agg()
        self.assertEqual(len(_all_cards(view)), 0)


class TestAggregateGrouping(unittest.TestCase):
    """Issues + PRs + sessions are grouped into KanbanCards correctly."""

    def test_issue_creates_one_card(self) -> None:
        issue = _make_issue(identifier="BAK-1")
        view = _agg(issues=[issue])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(cards[0].key, "BAK-1")
        self.assertIs(cards[0].ticket, issue)

    def test_pr_linked_to_ticket_joins_card(self) -> None:
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(number=10, head_ref="BAK-1/feature")
        view = _agg(issues=[issue], prs=[pr])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(len(cards[0].prs), 1)
        self.assertEqual(cards[0].prs[0].number, 10)

    def test_orphan_pr_becomes_its_own_card(self) -> None:
        pr = _make_pr(number=99, head_ref="feature/no-ticket")
        view = _agg(prs=[pr])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(cards[0].key, "pr-org/repo-99")
        self.assertIsNone(cards[0].ticket)

    def test_session_with_ticket_id_joins_card(self) -> None:
        issue = _make_issue(identifier="BAK-1")
        session = _make_session(session_id="s1", ticket_id="BAK-1")
        view = _agg(issues=[issue], sessions=[session])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(len(cards[0].sessions), 1)

    def test_standalone_session_excluded(self) -> None:
        """Sessions with no ticket_id must not produce a card."""
        session = _make_session(session_id="orphan", ticket_id=None)
        view = _agg(sessions=[session])
        self.assertEqual(len(_all_cards(view)), 0)

    def test_two_issues_produce_two_cards(self) -> None:
        i1 = _make_issue(identifier="BAK-1")
        i2 = _make_issue(identifier="BAK-2")
        view = _agg(issues=[i1, i2])
        self.assertEqual(len(_all_cards(view)), 2)

    def test_recent_pr_merged_joins_ticket_card(self) -> None:
        """A recently merged PR that references a ticket joins the ticket card."""
        issue = _make_issue(identifier="BAK-1")
        merged_pr = _make_pr(
            number=5,
            head_ref="BAK-1/fix",
            state="merged",
            updated_at=RECENT,
        )
        merged_pr.merged_at = RECENT
        view = _agg(issues=[issue], recent_prs=[merged_pr])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(len(cards[0].prs), 1)

    def test_completed_issue_creates_card(self) -> None:
        """A completed issue (from completed_issues param) creates a card."""
        issue = _make_issue(
            identifier="BAK-2",
            state="Done",
            state_type="completed",
            updated_at=RECENT,
        )
        view = _agg(completed_issues=[issue])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(cards[0].key, "BAK-2")


# ---------------------------------------------------------------------------
# PR-session attachment by (pr_repo, pr_number)
# ---------------------------------------------------------------------------


class TestSessionPRAttachment(unittest.TestCase):
    """ZingSessions only attach to the PR matching both pr_number AND pr_repo.

    Regression test: PR numbers are not unique across repositories, so a
    session for ``frontend-v1#175`` must never attach to
    ``turngate-integrations#175`` (and vice versa). Without explicit
    pr_repo the session does not attach at all.
    """

    def test_session_attaches_to_matching_repo_orphan_pr(self) -> None:
        pr = _make_pr(number=175, head_ref="feature/no-ticket", repo="org/frontend")
        session = _make_session(
            session_id="pr-review-175-feature-abc123",
            ticket_id=None,
            pr_number=175,
            pr_repo="org/frontend",
        )
        view = _agg(prs=[pr], sessions=[session])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(len(cards[0].sessions), 1)
        self.assertEqual(cards[0].sessions[0].session_id, "pr-review-175-feature-abc123")

    def test_session_does_not_cross_attach_to_different_repo_with_same_pr_number(
        self,
    ) -> None:
        """A session for ``org/frontend#175`` must NOT attach to ``org/backend#175``."""
        pr = _make_pr(number=175, head_ref="feature/no-ticket", repo="org/backend")
        session = _make_session(
            session_id="pr-review-175-frontend-stuff",
            ticket_id=None,
            pr_number=175,
            pr_repo="org/frontend",
        )
        view = _agg(prs=[pr], sessions=[session])
        cards = _all_cards(view)
        # The orphan PR card exists with NO sessions attached, and because
        # the user isn't involved with that PR (default username 'octocat')
        # the card is filtered out entirely.
        for card in cards:
            self.assertEqual(card.sessions, [])

    def test_legacy_session_without_pr_repo_does_not_attach(self) -> None:
        """ZingSessions with pr_number but no pr_repo are dropped, not cross-attached.

        Pre-fix, these would attach to any orphan PR card whose key ended
        in ``-{pr_number}`` — leaking sessions across repos.
        """
        pr = _make_pr(number=175, head_ref="feature/no-ticket", repo="org/backend")
        session = _make_session(
            session_id="pr-review-175-old-session",
            ticket_id=None,
            pr_number=175,
            pr_repo=None,  # legacy: no repo recorded
        )
        view = _agg(prs=[pr], sessions=[session])
        for card in _all_cards(view):
            self.assertEqual(card.sessions, [])

    def test_session_attaches_to_ticket_card_pr_with_matching_repo(self) -> None:
        """When a ticket card carries the PR, the session still requires repo match."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(number=42, head_ref="BAK-1/feature", repo="org/backend")
        session = _make_session(
            session_id="pr-review-42-bak-1",
            ticket_id=None,
            pr_number=42,
            pr_repo="org/backend",
        )
        view = _agg(issues=[issue], prs=[pr], sessions=[session])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(cards[0].key, "BAK-1")
        self.assertEqual(len(cards[0].sessions), 1)


# ---------------------------------------------------------------------------
# Column classification
# ---------------------------------------------------------------------------


class TestColumnClassification(unittest.TestCase):
    """Cards are placed in the correct Kanban column."""

    def test_todo_column_default(self) -> None:
        """An open issue with no sessions/PRs falls into todo."""
        issue = _make_issue(identifier="BAK-1", state_type="unstarted")
        view = _agg(issues=[issue])
        self.assertEqual(len(view.todo), 1)
        self.assertEqual(len(view.in_progress), 0)

    def test_in_progress_when_session_step_started(self) -> None:
        """A session with a STARTED step places the card in in_progress."""
        issue = _make_issue(identifier="BAK-1")
        step = _make_workflow_step(step_name="build", state=SessionState.STARTED)
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[step])
        view = _agg(issues=[issue], sessions=[session])
        self.assertEqual(len(view.in_progress), 1)
        self.assertEqual(len(view.todo), 0)
        # No PR-feedback reason for a session-driven in_progress card.
        self.assertIsNone(view.in_progress[0].in_progress_reason)

    def test_in_progress_when_open_pr_no_reviewers(self) -> None:
        """An open PR with no requested_reviewers → in_progress."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(number=1, head_ref="BAK-1/feature", state="open", requested_reviewers=[])
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.in_progress), 1)

    def test_needs_review_when_pr_has_reviewers(self) -> None:
        """Open PR with requested_reviewers and non-APPROVED decision → needs_review."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=1,
            head_ref="BAK-1/feature",
            state="open",
            requested_reviewers=["octocat"],
            review_decision=None,
        )
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.needs_review), 1)
        self.assertEqual(len(view.in_progress), 0)

    def test_needs_review_when_user_is_author_and_others_reviewing(self) -> None:
        """PR where user is author and others are reviewers → needs_review."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=1,
            head_ref="BAK-1/feature",
            state="open",
            requested_reviewers=["alice"],
            review_decision=None,
        )
        pr.author = "octocat"
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.needs_review), 1)

    def test_needs_review_when_re_requested_after_prior_approval(self) -> None:
        """Re-request after approval surfaces the card in needs_review.

        Scenario: PR was approved by reviewer A, author then re-requests
        review from the current user (B).  GitHub keeps ``reviewDecision``
        as ``APPROVED`` even though B is now in ``requested_reviewers`` and
        has not submitted a review.  The card must land in ``needs_review``
        rather than vanishing — being explicitly re-requested is a "please
        look again" signal that overrides the overall approval status.
        """
        pr = _make_pr(
            number=295,
            head_ref="some-branch",
            state="open",
            author="someone-else",
            requested_reviewers=["octocat"],
            reviewers=[],
            review_decision="APPROVED",
            updated_at=RECENT,
        )
        view = _agg(prs=[pr])
        self.assertEqual(len(view.needs_review), 1)
        self.assertEqual(len(view.done), 0)

    def test_not_needs_review_when_another_human_already_approved(self) -> None:
        """User is in requested_reviewers but a co-reviewer has already approved.

        Scenario (PR turngate/frontend-v2#327): author requests two reviewers
        (the current user + one other), a third human approves first, the
        author leaves both requests in place because the approver isn't a
        codeowner.  Branch protection's "1 approving review" rule is already
        satisfied by the third human; the request from the current user is a
        courtesy ask, not a blocker.  Card must NOT land in needs_review.
        """
        pr = _make_pr(
            number=327,
            head_ref="some-branch",
            state="open",
            author="some-author",
            requested_reviewers=["octocat", "another-reviewer"],
            reviewers=["kyle"],
            reviewer_states={"kyle": "APPROVED"},
            review_decision=None,
            updated_at=RECENT,
        )
        view = _agg(prs=[pr])
        self.assertEqual(len(view.needs_review), 0)

    def test_needs_review_when_only_bot_approved(self) -> None:
        """A bot approval does not demote a real review request.

        The "other human approved" exception must skip bot accounts so that
        a Renovate / dependabot auto-approval doesn't suppress a legitimate
        human review request from the current user.
        """
        pr = _make_pr(
            number=42,
            head_ref="renovate/some-dep",
            state="open",
            author="some-author",
            requested_reviewers=["octocat"],
            reviewers=["renovate[bot]"],
            reviewer_states={"renovate[bot]": "APPROVED"},
            review_decision=None,
            updated_at=RECENT,
        )
        view = _agg(prs=[pr])
        self.assertEqual(len(view.needs_review), 1)

    def test_done_when_ticket_completed_recently(self) -> None:
        """Ticket with state_type='completed' updated within 7 days → done."""
        issue = _make_issue(
            identifier="BAK-1",
            state_type="completed",
            updated_at=RECENT,
        )
        view = _agg(issues=[issue])
        self.assertEqual(len(view.done), 1)
        self.assertEqual(len(view.todo), 0)

    def test_excluded_when_ticket_completed_but_old(self) -> None:
        """Ticket with state_type='completed' updated > 7 days ago → excluded.

        No positive rule fires for an old completed ticket (not unstarted,
        not started, not recently_done), so it falls out of the board
        entirely rather than being swept into todo.
        """
        issue = _make_issue(
            identifier="BAK-1",
            state_type="completed",
            updated_at=OLD,
        )
        view = _agg(issues=[issue])
        self.assertEqual(len(view.done), 0)
        self.assertEqual(len(view.todo), 0)
        self.assertEqual(len(_all_cards(view)), 0)

    def test_done_when_pr_merged_recently(self) -> None:
        """A card with a recently merged PR → done."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=5,
            head_ref="BAK-1/fix",
            state="merged",
            updated_at=RECENT,
        )
        pr.merged_at = RECENT
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.done), 1)

    def test_todo_when_pr_merged_old(self) -> None:
        """A merged PR older than 7 days doesn't count for done."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(number=5, head_ref="BAK-1/fix", state="merged", updated_at=OLD)
        pr.merged_at = OLD
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.done), 0)

    def test_done_when_merged_pr_approved_recently(self) -> None:
        """An APPROVED merged PR updated within 7 days → done."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=5,
            head_ref="BAK-1/fix",
            state="merged",
            updated_at=RECENT,
            review_decision="APPROVED",
        )
        pr.merged_at = RECENT
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.done), 1)


# ---------------------------------------------------------------------------
# Column priority rule
# ---------------------------------------------------------------------------


class TestColumnPriorityRule(unittest.TestCase):
    """Priority: unaddressed feedback > needs_review > active session > done > todo."""

    def test_needs_review_beats_active_session(self) -> None:
        """A card with both a STARTED session and pending reviewers → needs_review.

        Once a PR is out for review the user is blocked on reviewers; the
        active session is stale context and should not pin the card to
        in_progress.
        """
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=1,
            head_ref="BAK-1/feature",
            state="open",
            requested_reviewers=["octocat"],
        )
        step = _make_workflow_step(step_name="build", state=SessionState.STARTED)
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[step])
        view = _agg(issues=[issue], prs=[pr], sessions=[session])
        self.assertEqual(len(view.needs_review), 1)
        self.assertEqual(len(view.in_progress), 0)

    def test_needs_review_beats_started_ticket_and_session(self) -> None:
        """Started ticket + active session + PR with reviewers → needs_review.

        Reproduces the BAK-1233 scenario: ticket is "started" in Linear,
        a Claude session is running, but the PR is waiting on reviewers.
        The card should show as "Waiting on others", not "In Progress".
        """
        issue = _make_issue(identifier="BAK-1233", state_type="started")
        pr = _make_pr(
            number=1878,
            head_ref="BAK-1233/fix",
            state="open",
            author="octocat",
            requested_reviewers=["reviewer-1"],
            review_decision=None,
        )
        step = _make_workflow_step(step_name="build", state=SessionState.STARTED)
        session = _make_session(session_id="s1", ticket_id="BAK-1233", steps=[step])
        view = _agg(issues=[issue], prs=[pr], sessions=[session])
        self.assertEqual(len(view.needs_review), 1)
        self.assertEqual(view.needs_review[0].review_group, "others")
        self.assertEqual(len(view.in_progress), 0)

    def test_unaddressed_feedback_beats_needs_review(self) -> None:
        """A card with unaddressed feedback AND other pending reviewers → in_progress.

        The user needs to act on the feedback before reviews can proceed.
        """
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=1,
            head_ref="BAK-1/feature",
            state="open",
            author="octocat",
            reviewers=["alice"],
            requested_reviewers=["bob"],
            reviewer_states={"alice": "CHANGES_REQUESTED"},
            review_decision=None,
        )
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.in_progress), 1)
        self.assertEqual(len(view.needs_review), 0)

    def test_review_decision_changes_requested_overrides_latest_reviews(self) -> None:
        """PR-level reviewDecision=CHANGES_REQUESTED → in_progress even when
        latestReviews shows the reviewer's most recent state is COMMENTED.

        Reproduces backend-v1#1885: the reviewer first submitted
        CHANGES_REQUESTED, then later posted inline COMMENTED reviews.
        GitHub's ``latestReviews`` keeps only the most recent review per
        user (COMMENTED), but the PR-level ``reviewDecision`` still
        reflects the unresolved change request.
        """
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=1,
            head_ref="BAK-1/feature",
            state="open",
            author="octocat",
            reviewers=["kyle"],
            requested_reviewers=["max"],
            reviewer_states={"kyle": "COMMENTED"},
            review_decision="CHANGES_REQUESTED",
        )
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.in_progress), 1)
        self.assertEqual(len(view.needs_review), 0)
        self.assertEqual(view.in_progress[0].in_progress_reason, "Changes requested")

    def test_changes_requested_empty_states_lands_in_needs_review(self) -> None:
        """Realistic PR 1885 scenario: empty latestReviews + re-requested reviewers.

        Live GitHub returned ``reviewDecision: CHANGES_REQUESTED`` and
        ``latestReviews: []`` because both reviewers were currently
        re-requested.  The card must land in ``needs_review`` (waiting on
        others), not ``in_progress``.
        """
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=1,
            head_ref="BAK-1/feature",
            state="open",
            author="octocat",
            reviewers=[],
            requested_reviewers=["kyle", "max"],
            reviewer_states={},
            review_decision="CHANGES_REQUESTED",
        )
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.in_progress), 0)
        self.assertEqual(len(view.needs_review), 1)

    def test_changes_requested_rerequested_lands_in_needs_review(self) -> None:
        """reviewDecision=CHANGES_REQUESTED but the changes-requester has been
        re-requested → needs_review (waiting on others), not in_progress.

        Reproduces the case where the author addressed feedback and clicked
        "re-request review" on the same reviewer.  GitHub does not reset
        ``reviewDecision`` until the reviewer submits a new review, so the
        PR-level decision remains ``CHANGES_REQUESTED`` even though the
        author is now blocked on the reviewer.
        """
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=1,
            head_ref="BAK-1/feature",
            state="open",
            author="octocat",
            reviewers=["kyle"],
            requested_reviewers=["kyle"],
            reviewer_states={"kyle": "CHANGES_REQUESTED"},
            review_decision="CHANGES_REQUESTED",
        )
        view = _agg(issues=[issue], prs=[pr], current_username="octocat")
        self.assertEqual(len(view.in_progress), 0)
        self.assertEqual(len(view.needs_review), 1)

    def test_human_comment_review_counts_as_unaddressed(self) -> None:
        """Human reviewer's COMMENTED review (no re-request) → in_progress.

        The reviewer's humanity is established via a sibling PR on the
        same card where they appear in ``requested_reviewers``.
        """
        issue = _make_issue(identifier="BAK-1")
        pr_with_comment = _make_pr(
            number=1,
            head_ref="BAK-1/feature",
            state="open",
            author="octocat",
            reviewers=["alice"],
            requested_reviewers=[],
            reviewer_states={"alice": "COMMENTED"},
            review_decision=None,
        )
        sibling_pr = _make_pr(
            number=2,
            head_ref="BAK-1/feature-2",
            state="open",
            author="octocat",
            requested_reviewers=["alice"],
            review_decision=None,
        )
        view = _agg(issues=[issue], prs=[pr_with_comment, sibling_pr])
        self.assertEqual(len(view.in_progress), 1)
        self.assertEqual(len(view.needs_review), 0)
        self.assertEqual(view.in_progress[0].in_progress_reason, "Reviewer feedback")

    def test_bot_comment_does_not_block_needs_review(self) -> None:
        """A bot's COMMENTED review on a PR awaiting human review → needs_review.

        Bots like Greptile/CodeRabbit only ever leave COMMENT reviews and
        are never requested as reviewers.  Their comments must not hijack
        the card to in_progress when a human review is still pending.
        """
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(
            number=1,
            head_ref="BAK-1/feature",
            state="open",
            author="octocat",
            reviewers=["greptile-bot"],
            requested_reviewers=["alice"],
            reviewer_states={"greptile-bot": "COMMENTED"},
            review_decision=None,
        )
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.needs_review), 1)
        self.assertEqual(len(view.in_progress), 0)

    def test_done_beats_stale_session(self) -> None:
        """A merged PR moves card to done even if a session step is still STARTED."""
        issue = _make_issue(identifier="BAK-1", state_type="completed", updated_at=RECENT)
        pr = _make_pr(number=5, head_ref="BAK-1/fix", state="merged", updated_at=RECENT)
        pr.merged_at = RECENT
        step = _make_workflow_step(step_name="build", state=SessionState.STARTED)
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[step])
        view = _agg(issues=[issue], prs=[pr], sessions=[session])
        self.assertEqual(len(view.done), 1)
        self.assertEqual(len(view.in_progress), 0)

    def test_needs_review_beats_done(self) -> None:
        """A card with pending reviews and a recently merged PR → needs_review."""
        issue = _make_issue(identifier="BAK-1", state_type="completed", updated_at=RECENT)
        pr = _make_pr(
            number=5,
            head_ref="BAK-1/fix",
            state="open",
            requested_reviewers=["octocat"],
            review_decision=None,
        )
        view = _agg(issues=[issue], prs=[pr])
        self.assertEqual(len(view.needs_review), 1)
        self.assertEqual(len(view.done), 0)

    def test_done_beats_todo(self) -> None:
        """A recently completed ticket → done, not todo."""
        issue = _make_issue(
            identifier="BAK-1",
            state_type="completed",
            updated_at=RECENT,
        )
        view = _agg(issues=[issue])
        self.assertEqual(len(view.done), 1)
        self.assertEqual(len(view.todo), 0)


# ---------------------------------------------------------------------------
# To Do sort order
# ---------------------------------------------------------------------------


class TestTodoSortOrder(unittest.TestCase):
    """To Do column: only unstarted tickets land here, sorted by priority asc.

    Triage and backlog tickets are excluded from the board entirely; see
    ``TestBacklogExclusion`` and ``TestTriageExclusion`` for those behaviours.
    """

    def test_triage_ticket_is_excluded_not_sorted(self) -> None:
        """Triage tickets are not in todo at all — they're excluded."""
        unstarted = _make_issue(identifier="BAK-1", state_type="unstarted", priority=0)
        triage = _make_issue(identifier="BAK-2", state_type="triage", priority=0)
        view = _agg(issues=[unstarted, triage])
        self.assertEqual([c.key for c in view.todo], ["BAK-1"])

    def test_priority_urgent_first(self) -> None:
        """priority=1 (urgent) sorts before priority=4 (low)."""
        urgent = _make_issue(identifier="BAK-1", state_type="unstarted", priority=1)
        low = _make_issue(identifier="BAK-2", state_type="unstarted", priority=4)
        view = _agg(issues=[urgent, low])
        self.assertEqual(view.todo[0].key, "BAK-1")  # urgent first
        self.assertEqual(view.todo[1].key, "BAK-2")

    def test_no_priority_sorts_last(self) -> None:
        """priority=0 (no priority) sorts after priority=4 (low)."""
        no_priority = _make_issue(identifier="BAK-1", state_type="unstarted", priority=0)
        low = _make_issue(identifier="BAK-2", state_type="unstarted", priority=4)
        view = _agg(issues=[no_priority, low])
        self.assertEqual(view.todo[0].key, "BAK-2")  # explicit priority first
        self.assertEqual(view.todo[1].key, "BAK-1")  # no-priority last


# ---------------------------------------------------------------------------
# Backlog exclusion
# ---------------------------------------------------------------------------


class TestBacklogExclusion(unittest.TestCase):
    """Backlog tickets are excluded from the board regardless of attached state."""

    def test_plain_backlog_ticket_excluded(self) -> None:
        backlog = _make_issue(identifier="BAK-1", state_type="backlog")
        view = _agg(issues=[backlog])
        self.assertEqual(_all_cards(view), [])

    def test_backlog_ticket_with_active_session_excluded(self) -> None:
        """Stale started session on a backlog ticket does not bring it back."""
        active_step = _make_workflow_step(step_name="build", state=SessionState.STARTED)
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[active_step])
        backlog = _make_issue(identifier="BAK-1", state_type="backlog")
        view = _agg(issues=[backlog], sessions=[session])
        self.assertEqual(_all_cards(view), [])

    def test_backlog_ticket_with_open_pr_excluded(self) -> None:
        """An open PR linked to a backlog ticket does not pull the card onto the board."""
        pr = _make_pr(number=1, state="open", head_ref="bak-1-feature")
        backlog = _make_issue(identifier="BAK-1", state_type="backlog")
        view = _agg(issues=[backlog], prs=[pr])
        self.assertEqual(_all_cards(view), [])


# ---------------------------------------------------------------------------
# Triage exclusion
# ---------------------------------------------------------------------------


class TestTriageExclusion(unittest.TestCase):
    """Triage tickets are excluded from the board regardless of attached state.

    Triage means the ticket is undecided — it still needs product-side
    attention before it belongs on an engineering board. Symmetric to the
    backlog exclusion.
    """

    def test_plain_triage_ticket_excluded(self) -> None:
        triage = _make_issue(identifier="BAK-1", state_type="triage")
        view = _agg(issues=[triage])
        self.assertEqual(_all_cards(view), [])

    def test_triage_ticket_with_open_pr_excluded(self) -> None:
        """An open PR linked to a triage ticket does not pull the card onto the board."""
        pr = _make_pr(number=1, state="open", head_ref="bak-1-feature")
        triage = _make_issue(identifier="BAK-1", state_type="triage")
        view = _agg(issues=[triage], prs=[pr])
        self.assertEqual(_all_cards(view), [])


# ---------------------------------------------------------------------------
# Duplicate exclusion
# ---------------------------------------------------------------------------


class TestDuplicateExclusion(unittest.TestCase):
    """Duplicate tickets are excluded from the board regardless of attached state.

    Duplicates are resolved by merging into another ticket. A stale session or
    a merged PR linked to the duplicate must not bring it back onto the board.
    """

    def test_plain_duplicate_ticket_excluded(self) -> None:
        dup = _make_issue(identifier="BAK-1", state_type="duplicate")
        view = _agg(issues=[dup])
        self.assertEqual(_all_cards(view), [])

    def test_duplicate_ticket_with_active_session_excluded(self) -> None:
        """Stale started session on a duplicate ticket does not bring it back."""
        active_step = _make_workflow_step(step_name="build", state=SessionState.STARTED)
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[active_step])
        dup = _make_issue(identifier="BAK-1", state_type="duplicate")
        view = _agg(issues=[dup], sessions=[session])
        self.assertEqual(_all_cards(view), [])

    def test_duplicate_ticket_with_open_pr_excluded(self) -> None:
        """An open PR linked to a duplicate ticket does not pull the card onto the board."""
        pr = _make_pr(number=1, state="open", head_ref="bak-1-feature")
        dup = _make_issue(identifier="BAK-1", state_type="duplicate")
        view = _agg(issues=[dup], prs=[pr])
        self.assertEqual(_all_cards(view), [])


# ---------------------------------------------------------------------------
# Catch-all exclusion (no positive column rule fires)
# ---------------------------------------------------------------------------


class TestNoPositiveMatchExcluded(unittest.TestCase):
    """Cards that don't match any positive column rule are dropped (no fallthrough).

    Regression: previously these landed in todo via a default fallthrough,
    polluting the column with stale shapes.
    """

    def test_orphan_merged_pr_user_reviewed_outside_window_excluded(self) -> None:
        """The PR #1938 case: merged orphan PR user reviewed, merged_at > 7 days ago."""
        pr = _make_pr(
            number=1938,
            head_ref="kyle/feature",
            state="merged",
            author="someone-else",
            reviewers=["octocat"],
            reviewer_states={"octocat": "DISMISSED"},
            review_decision="REVIEW_REQUIRED",
            updated_at=OLD,
            merged_at=OLD,
        )
        view = _agg(prs=[], recent_prs=[pr], current_username="octocat")
        self.assertEqual(_all_cards(view), [])

    def test_orphan_closed_not_merged_pr_excluded(self) -> None:
        """A closed-not-merged orphan PR authored by the user is excluded.

        Not in todo (no ticket), not in any other column. Used to fall
        through to todo as a catch-all.
        """
        pr = _make_pr(
            number=42,
            head_ref="abandoned",
            state="closed",
            author="octocat",
            updated_at=OLD,
        )
        view = _agg(prs=[pr], current_username="octocat")
        self.assertEqual(_all_cards(view), [])


# ---------------------------------------------------------------------------
# Audit step attachment
# ---------------------------------------------------------------------------


class TestAuditStepAttachment(unittest.TestCase):
    """Cards with READY audit steps and actionable findings get audit_steps set."""

    def test_ready_audit_step_with_findings_attached(self) -> None:
        finding = TextFinding(title="Issue found", body="Details here")
        audit_step = _make_workflow_step(
            step_name="build-audit",
            state=SessionState.READY,
            findings=[finding],
        )
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[audit_step])
        issue = _make_issue(identifier="BAK-1")
        view = _agg(issues=[issue], sessions=[session])
        cards = _all_cards(view)
        self.assertEqual(len(cards), 1)
        self.assertEqual(len(cards[0].audit_steps), 1)
        self.assertEqual(cards[0].audit_steps[0].step_name, "build-audit")

    def test_completed_audit_step_not_attached(self) -> None:
        """Only READY audit steps with findings are attached, not COMPLETED."""
        finding = TextFinding(title="Issue found", body="Details here")
        audit_step = _make_workflow_step(
            step_name="build-audit",
            state=SessionState.COMPLETED,
            findings=[finding],
        )
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[audit_step])
        issue = _make_issue(identifier="BAK-1")
        view = _agg(issues=[issue], sessions=[session])
        cards = _all_cards(view)
        self.assertEqual(len(cards[0].audit_steps), 0)

    def test_evaluation_only_findings_not_attached(self) -> None:
        """Evaluation findings are informational — don't attach as actionable."""
        from zing_ai.server.models import CriterionRating, EvaluationFinding, Rating

        eval_finding = EvaluationFinding(
            title="Audit complete",
            criteria=[CriterionRating(name="crit", rating=Rating.STRONG, justification="n/a")],
        )
        audit_step = _make_workflow_step(
            step_name="plan-audit",
            state=SessionState.READY,
            findings=[eval_finding],
        )
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[audit_step])
        issue = _make_issue(identifier="BAK-1")
        view = _agg(issues=[issue], sessions=[session])
        cards = _all_cards(view)
        self.assertEqual(len(cards[0].audit_steps), 0)

    def test_multiple_audit_types_all_attached(self) -> None:
        """plan-audit and build-audit both get surfaced in audit_steps."""
        finding = TextFinding(title="Issue found")
        plan_step = _make_workflow_step(
            step_name="plan-audit",
            state=SessionState.READY,
            findings=[finding],
        )
        build_step = _make_workflow_step(
            step_name="build-audit",
            state=SessionState.READY,
            findings=[finding],
        )
        session = _make_session(session_id="s1", ticket_id="BAK-1", steps=[plan_step, build_step])
        issue = _make_issue(identifier="BAK-1")
        view = _agg(issues=[issue], sessions=[session])
        cards = _all_cards(view)
        self.assertEqual(len(cards[0].audit_steps), 2)


# ---------------------------------------------------------------------------
# Filtering
# ---------------------------------------------------------------------------


class TestFiltering(unittest.TestCase):
    """Cards without at least a ticket or a PR are excluded."""

    def test_card_with_only_session_excluded(self) -> None:
        """A session attached to a ticket card — but the ticket isn't in issues.
        The card is excluded because it has no ticket and no PR.
        """
        # Session has ticket_id but no issue is provided → session is skipped
        session = _make_session(session_id="s1", ticket_id="BAK-99")
        view = _agg(sessions=[session])
        self.assertEqual(len(_all_cards(view)), 0)

    def test_orphan_pr_card_included(self) -> None:
        """Orphan PR cards (no ticket) are included since they have a PR."""
        pr = _make_pr(number=7, head_ref="feature/misc", state="open")
        view = _agg(prs=[pr])
        self.assertEqual(len(_all_cards(view)), 1)


class TestClaudeCodeSessionClassification(unittest.TestCase):
    """ClaudeCodeSession should NOT affect column classification."""

    def test_claude_code_session_does_not_pin_to_in_progress(self) -> None:
        """A card with a ClaudeCodeSession should NOT be pinned to in_progress."""
        issue = _make_issue(identifier="BAK-10", state_type="unstarted")
        session = _make_cc_session(ticket_id="BAK-10")
        view = _agg(issues=[issue], sessions=[session])
        # Card should be in todo, not in_progress
        todo_keys = [c.key for c in view.todo]
        ip_keys = [c.key for c in view.in_progress]
        self.assertIn("BAK-10", todo_keys)
        self.assertNotIn("BAK-10", ip_keys)

    def test_completed_ticket_with_claude_session_goes_to_done(self) -> None:
        """Completed ticket with stale ClaudeCodeSession should be in done, not in_progress."""
        issue = _make_issue(
            identifier="BAK-11",
            state_type="completed",
            updated_at=RECENT,
        )
        session = _make_cc_session(ticket_id="BAK-11")
        view = _agg(issues=[], completed_issues=[issue], sessions=[session])
        done_keys = [c.key for c in view.done]
        ip_keys = [c.key for c in view.in_progress]
        self.assertIn("BAK-11", done_keys)
        self.assertNotIn("BAK-11", ip_keys)


class TestReviewerDoneClassification(unittest.TestCase):
    """PRs where the user submitted a review should be classified as done."""

    def test_user_reviewed_pr_is_done(self) -> None:
        """Open PR where user submitted review and is not re-requested goes to done."""
        pr = _make_pr(
            number=42,
            head_ref="feat/thing",
            state="open",
            author="other-dev",
            reviewers=["octocat"],
            requested_reviewers=["someone-else"],
            review_decision="CHANGES_REQUESTED",
            updated_at=RECENT,
        )
        view = _agg(prs=[pr], current_username="octocat")
        done_keys = [c.key for c in view.done]
        self.assertTrue(any("42" in k for k in done_keys))

    def test_user_in_requested_reviewers_not_done(self) -> None:
        """User still in requested_reviewers should NOT be in done via reviewer path."""
        pr = _make_pr(
            number=43,
            head_ref="feat/other",
            state="open",
            author="other-dev",
            reviewers=[],
            requested_reviewers=["octocat"],
            review_decision="REVIEW_REQUIRED",
            updated_at=RECENT,
        )
        view = _agg(prs=[pr], current_username="octocat")
        done_keys = [c.key for c in view.done]
        self.assertFalse(any("43" in k for k in done_keys))


class TestShouldIncludeCardReviewers(unittest.TestCase):
    """_should_include_card should include orphan PRs where user submitted a review."""

    def test_orphan_pr_included_when_user_is_reviewer(self) -> None:
        """Orphan PR where user is in reviewers (submitted review) is included."""
        pr = _make_pr(
            number=50,
            head_ref="feat/x",
            state="open",
            author="other-dev",
            reviewers=["octocat"],
            updated_at=RECENT,
        )
        view = _agg(prs=[pr], current_username="octocat")
        all_keys = [c.key for c in _all_cards(view)]
        self.assertTrue(any("50" in k for k in all_keys))

    def test_orphan_pr_excluded_when_user_not_involved(self) -> None:
        """Orphan PR where user is not author, reviewer, or requested is excluded."""
        pr = _make_pr(
            number=51,
            head_ref="feat/y",
            state="open",
            author="other-dev",
            reviewers=["someone-else"],
            updated_at=RECENT,
        )
        view = _agg(prs=[pr], current_username="octocat")
        all_keys = [c.key for c in _all_cards(view)]
        self.assertFalse(any("51" in k for k in all_keys))


class TestSessionPrNumberLinking(unittest.TestCase):
    """Sessions link to orphan PR cards by explicit (pr_repo, pr_number)."""

    def test_zing_session_linked_by_explicit_pr_fields(self) -> None:
        """ZingSession with explicit pr_number + pr_repo attaches to orphan PR card."""
        pr = _make_pr(number=42, head_ref="feat/thing", author="octocat")
        session = _make_session(
            session_id="pr-review-42-feat-thing-abc123",
            title="PR Review \u2014 #42 feat: thing",
            pr_number=42,
            pr_repo="org/repo",
        )
        view = _agg(prs=[pr], sessions=[session], current_username="octocat")
        cards_with_sessions = [c for c in _all_cards(view) if c.sessions]
        self.assertEqual(len(cards_with_sessions), 1)
        self.assertEqual(len(cards_with_sessions[0].sessions), 1)

    def test_claude_code_session_linked_by_pr_number(self) -> None:
        """ClaudeCodeSession with explicit pr_number attaches to orphan PR card."""
        pr = _make_pr(number=99, head_ref="fix/bug", author="octocat")
        session = _make_cc_session(
            session_id="cc-99",
            title="PR #99 Review",
            pr_number=99,
            pr_repo="org/repo",
        )
        view = _agg(prs=[pr], sessions=[session], current_username="octocat")
        cards_with_sessions = [c for c in _all_cards(view) if c.sessions]
        self.assertEqual(len(cards_with_sessions), 1)


class TestDoneGrouping(unittest.TestCase):
    """Done column should group cards into ready_to_merge and completed."""

    def test_approved_open_pr_authored_by_user_is_ready_to_merge(self) -> None:
        """Open PR authored by the user and approved → ready_to_merge."""
        pr = _make_pr(
            number=60,
            head_ref="feat/approved",
            state="open",
            author="octocat",
            reviewers=["other-dev"],
            review_decision="APPROVED",
            updated_at=RECENT,
        )
        view = _agg(prs=[pr], current_username="octocat")
        done_cards = view.done
        ready = [c for c in done_cards if c.done_group == "ready_to_merge"]
        self.assertEqual(len(ready), 1)

    def test_approved_open_pr_by_other_is_completed(self) -> None:
        """Open PR authored by someone else and approved → completed, not ready_to_merge."""
        pr = _make_pr(
            number=65,
            head_ref="feat/other-approved",
            state="open",
            author="other-dev",
            reviewers=["octocat"],
            review_decision="APPROVED",
            updated_at=RECENT,
        )
        view = _agg(prs=[pr], current_username="octocat")
        done_cards = view.done
        ready = [c for c in done_cards if c.done_group == "ready_to_merge"]
        self.assertEqual(len(ready), 0)
        completed = [c for c in done_cards if c.done_group == "completed"]
        self.assertEqual(len(completed), 1)

    def test_merged_pr_is_completed(self) -> None:
        """Merged PR gets completed group."""
        pr = _make_pr(
            number=61,
            head_ref="feat/merged",
            state="merged",
            author="octocat",
            merged_at=RECENT,
            updated_at=RECENT,
        )
        view = _agg(recent_prs=[pr], current_username="octocat")
        done_cards = view.done
        completed = [c for c in done_cards if c.done_group == "completed"]
        self.assertEqual(len(completed), 1)


class TestPrNeedsResponse(unittest.TestCase):
    """Direct unit tests for the per-PR response predicate.

    The aggregation tests above cover the card-level outcomes, but the
    template now also calls this predicate directly to choose between
    "Respond" and "Build Audit" as the primary PR button.
    """

    def _card(self, *prs) -> KanbanCard:  # noqa: ANN001, ANN202
        return KanbanCard(key="BAK-1", ticket=_make_issue(identifier="BAK-1"), prs=list(prs))

    def test_changes_requested_by_author_returns_true(self) -> None:
        from zing_ai.server.command_center import _pr_needs_response

        pr = _make_pr(author="octocat", review_decision="CHANGES_REQUESTED")
        self.assertTrue(_pr_needs_response(self._card(pr), pr, "octocat"))

    def test_human_commented_review_returns_true(self) -> None:
        """Human reviewer's COMMENTED review (no re-request) must trigger Respond."""
        from zing_ai.server.command_center import _pr_needs_response

        pr_with_comment = _make_pr(
            number=1,
            author="octocat",
            reviewers=["alice"],
            requested_reviewers=[],
            reviewer_states={"alice": "COMMENTED"},
            review_decision=None,
        )
        sibling = _make_pr(
            number=2,
            author="octocat",
            requested_reviewers=["alice"],
            review_decision=None,
        )
        card = self._card(pr_with_comment, sibling)
        self.assertTrue(_pr_needs_response(card, pr_with_comment, "octocat"))

    def test_bot_commented_review_returns_false(self) -> None:
        """Greptile-style bot drive-by COMMENT must not trigger Respond."""
        from zing_ai.server.command_center import _pr_needs_response

        pr = _make_pr(
            author="octocat",
            reviewers=["greptile-bot"],
            requested_reviewers=[],
            reviewer_states={"greptile-bot": "COMMENTED"},
            review_decision=None,
        )
        self.assertFalse(_pr_needs_response(self._card(pr), pr, "octocat"))

    def test_rerequested_after_comment_returns_false(self) -> None:
        """Comment was addressed and reviewer was re-requested → no longer pending."""
        from zing_ai.server.command_center import _pr_needs_response

        pr = _make_pr(
            author="octocat",
            reviewers=["alice"],
            requested_reviewers=["alice"],
            reviewer_states={"alice": "COMMENTED"},
            review_decision=None,
        )
        self.assertFalse(_pr_needs_response(self._card(pr), pr, "octocat"))

    def test_changes_requested_rerequested_returns_false(self) -> None:
        """CHANGES_REQUESTED reviewer was re-requested → user is now waiting.

        GitHub keeps ``reviewDecision`` at ``CHANGES_REQUESTED`` even after
        the author re-requests review — it only flips when the reviewer
        submits a new review.  Once every explicit changes-requester is
        back in ``requested_reviewers``, the author has done their part.
        """
        from zing_ai.server.command_center import _pr_needs_response

        pr = _make_pr(
            author="octocat",
            reviewers=["alice"],
            requested_reviewers=["alice"],
            reviewer_states={"alice": "CHANGES_REQUESTED"},
            review_decision="CHANGES_REQUESTED",
        )
        self.assertFalse(_pr_needs_response(self._card(pr), pr, "octocat"))

    def test_changes_requested_partial_rerequest_returns_true(self) -> None:
        """One CR-er re-requested, another still pending → still needs response."""
        from zing_ai.server.command_center import _pr_needs_response

        pr = _make_pr(
            author="octocat",
            reviewers=["alice", "bob"],
            requested_reviewers=["alice"],
            reviewer_states={
                "alice": "CHANGES_REQUESTED",
                "bob": "CHANGES_REQUESTED",
            },
            review_decision="CHANGES_REQUESTED",
        )
        self.assertTrue(_pr_needs_response(self._card(pr), pr, "octocat"))

    def test_changes_requested_empty_states_with_pending_request_returns_false(self) -> None:
        """Realistic re-requested scenario per the actual GitHub API.

        Reproduces backend-v1#1885 as observed live:
        ``reviewDecision == 'CHANGES_REQUESTED'`` but ``latestReviews`` is
        empty because GitHub excludes reviewers currently in
        ``reviewRequests``.  Empty ``reviewer_states`` plus non-empty
        ``requested_reviewers`` proves every CR-er has been re-requested,
        so the author is waiting for the re-review.
        """
        from zing_ai.server.command_center import _pr_needs_response

        pr = _make_pr(
            author="octocat",
            reviewers=[],  # latestReviews is empty
            requested_reviewers=["kyle", "max"],  # both currently re-requested
            reviewer_states={},
            review_decision="CHANGES_REQUESTED",
        )
        self.assertFalse(_pr_needs_response(self._card(pr), pr, "octocat"))

    def test_approved_returns_false(self) -> None:
        from zing_ai.server.command_center import _pr_needs_response

        pr = _make_pr(
            author="octocat",
            reviewers=["alice"],
            reviewer_states={"alice": "APPROVED"},
            review_decision="APPROVED",
        )
        self.assertFalse(_pr_needs_response(self._card(pr), pr, "octocat"))

    def test_non_author_returns_false(self) -> None:
        """Predicate is from the author's POV — reviewers don't get Respond."""
        from zing_ai.server.command_center import _pr_needs_response

        pr = _make_pr(
            author="someone-else",
            reviewers=["octocat"],
            reviewer_states={"octocat": "COMMENTED"},
            review_decision=None,
        )
        self.assertFalse(_pr_needs_response(self._card(pr), pr, "octocat"))

    def test_closed_pr_returns_false(self) -> None:
        from zing_ai.server.command_center import _pr_needs_response

        pr = _make_pr(
            author="octocat",
            state="closed",
            review_decision="CHANGES_REQUESTED",
        )
        self.assertFalse(_pr_needs_response(self._card(pr), pr, "octocat"))


class TestKanbanCardRendering(unittest.TestCase):
    """Template-level checks for the in-progress PR primary button.

    These pin the wiring between ``_pr_needs_response`` and the
    Respond/Build Audit branches in ``kanban_card.html``.
    """

    def _render(self, card: KanbanCard, *, current_username: str = "octocat") -> str:
        from zing_ai.server.templates import render

        return render(
            "fragments/kanban_card.html",
            card=card,
            column_cls="col-progress",
            current_username=current_username,
            live_sessions=set(),
            session_phases={},
        )

    def test_human_commented_review_renders_respond_button(self) -> None:
        """In-progress own-PR with a human's COMMENTED review → Respond, not Build Audit."""
        pr_with_comment = _make_pr(
            number=1,
            author="octocat",
            reviewers=["alice"],
            requested_reviewers=[],
            reviewer_states={"alice": "COMMENTED"},
            review_decision=None,
        )
        sibling = _make_pr(
            number=2,
            author="octocat",
            requested_reviewers=["alice"],
            review_decision=None,
        )
        card = KanbanCard(
            key="BAK-1",
            ticket=_make_issue(identifier="BAK-1"),
            prs=[pr_with_comment, sibling],
        )
        html = self._render(card)
        # The Respond skill appears (primary button + kebab Respond section).
        self.assertIn("pr-respond", html)
        # And the user-facing label is "Respond", not "Build Audit", on the primary slot.
        self.assertIn(">Respond</span>", html)

    def test_changes_requested_still_renders_respond(self) -> None:
        """Existing CHANGES_REQUESTED behaviour must be preserved."""
        pr = _make_pr(
            number=7,
            author="octocat",
            review_decision="CHANGES_REQUESTED",
        )
        card = KanbanCard(key="BAK-1", ticket=_make_issue(identifier="BAK-1"), prs=[pr])
        html = self._render(card)
        self.assertIn("pr-respond", html)
        self.assertIn(">Respond</span>", html)

    def test_no_review_renders_build_audit(self) -> None:
        """In-progress own-PR with no review activity → Build Audit primary."""
        pr = _make_pr(number=3, author="octocat", review_decision=None)
        card = KanbanCard(key="BAK-1", ticket=_make_issue(identifier="BAK-1"), prs=[pr])
        html = self._render(card)
        self.assertIn(">Build Audit</span>", html)
        # No Respond branch when there's no review activity.
        self.assertNotIn("pr-respond", html)
        self.assertNotIn(">Respond</span>", html)


if __name__ == "__main__":
    unittest.main()
