"""Unit tests for :mod:`zing_ai.server.card_view`.

Pin every display rule the builder consolidates: strip-pill selection,
primary-button selection, CI bucketing, session-status labels, footer
note, DOM id derivation, ``card-ready-to-merge`` extra class, and the
done-view exclusion flag.
"""

from __future__ import annotations

import unittest
from datetime import UTC, datetime

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
from zing_ai.server.card_view import (
    _COLUMN_CLS,
    build_card_view,
)
from zing_ai.server.models import SessionState, TextFinding
from zing_ai.server.models_external import CICheck, KanbanCard, KanbanColumn


def _card(
    *,
    key: str = "BAK-1",
    ticket=None,
    prs=None,
    sessions=None,
    audit_steps=None,
    review_group=None,
    done_group=None,
    in_progress_reason=None,
):
    return KanbanCard(
        key=key,
        ticket=ticket,
        prs=prs or [],
        sessions=sessions or [],
        audit_steps=audit_steps or [],
        review_group=review_group,
        done_group=done_group,
        in_progress_reason=in_progress_reason,
    )


# ---------------------------------------------------------------------------
# Column class mapping
# ---------------------------------------------------------------------------


class TestColumnClsMapping(unittest.TestCase):
    def test_each_column_maps_to_expected_css_class(self) -> None:
        self.assertEqual(_COLUMN_CLS["todo"], "col-todo")
        self.assertEqual(_COLUMN_CLS["in_progress"], "col-progress")
        self.assertEqual(_COLUMN_CLS["needs_review"], "col-review")
        self.assertEqual(_COLUMN_CLS["done"], "col-done")

    def test_builder_writes_column_cls_onto_view(self) -> None:
        ticket = _make_issue(identifier="BAK-1")
        view = build_card_view(_card(ticket=ticket), "in_progress", "octocat")
        self.assertEqual(view.column_cls, "col-progress")
        self.assertEqual(view.column, "in_progress")


# ---------------------------------------------------------------------------
# Strip pill (kanban_card.html L90-95 first-match)
# ---------------------------------------------------------------------------


class TestStripPill(unittest.TestCase):
    def _pill(self, pr, column: KanbanColumn = "in_progress", username: str = "octocat"):
        view = build_card_view(_card(prs=[pr]), column, username)
        return view.pr_views[0].pill

    def test_merged_wins_over_everything(self) -> None:
        pr = _make_pr(
            merged_at=datetime(2026, 4, 1, tzinfo=UTC),
            draft=True,
            review_decision="APPROVED",
        )
        pill = self._pill(pr)
        assert pill is not None
        self.assertEqual(pill.label, "merged")
        self.assertEqual(pill.css_class, "strip-pill-merged")

    def test_draft_wins_over_review_decision(self) -> None:
        pr = _make_pr(draft=True, review_decision="APPROVED")
        pill = self._pill(pr)
        assert pill is not None
        self.assertEqual(pill.label, "draft")

    def test_approved(self) -> None:
        pr = _make_pr(review_decision="APPROVED")
        pill = self._pill(pr)
        assert pill is not None
        self.assertEqual(pill.label, "approved")

    def test_changes_requested_outside_review_column(self) -> None:
        pr = _make_pr(review_decision="CHANGES_REQUESTED")
        pill = self._pill(pr, column="in_progress")
        assert pill is not None
        self.assertEqual(pill.label, "changes requested")

    def test_changes_requested_suppressed_in_review_column(self) -> None:
        pr = _make_pr(review_decision="CHANGES_REQUESTED")
        pill = self._pill(pr, column="needs_review")
        # The "reviewed" branch only fires for the author with non-rerequested
        # reviewers; this PR has none, so we expect no pill at all.
        self.assertIsNone(pill)

    def test_reviewed_when_author_has_non_rerequested_reviewer(self) -> None:
        pr = _make_pr(
            author="octocat",
            reviewers=["alice"],
            requested_reviewers=[],
        )
        pill = self._pill(pr, column="in_progress", username="octocat")
        assert pill is not None
        self.assertEqual(pill.label, "reviewed")

    def test_reviewed_suppressed_when_all_reviewers_rerequested(self) -> None:
        pr = _make_pr(
            author="octocat",
            reviewers=["alice"],
            requested_reviewers=["alice"],
        )
        pill = self._pill(pr, column="in_progress", username="octocat")
        self.assertIsNone(pill)

    def test_reviewed_suppressed_in_review_column(self) -> None:
        pr = _make_pr(
            author="octocat",
            reviewers=["alice"],
            requested_reviewers=[],
        )
        pill = self._pill(pr, column="needs_review", username="octocat")
        self.assertIsNone(pill)

    def test_no_pill_when_nothing_applies(self) -> None:
        pr = _make_pr(author="alice")  # not author, no decision, not draft
        pill = self._pill(pr, column="todo", username="octocat")
        self.assertIsNone(pill)


# ---------------------------------------------------------------------------
# Primary button (kanban_card.html L117-125)
# ---------------------------------------------------------------------------


class TestPrimaryButton(unittest.TestCase):
    def _btn(self, pr, column: KanbanColumn = "in_progress", username: str = "octocat"):
        view = build_card_view(_card(prs=[pr]), column, username)
        return view.pr_views[0].primary_button

    def test_no_button_for_merged_pr(self) -> None:
        pr = _make_pr(merged_at=datetime(2026, 4, 1, tzinfo=UTC))
        self.assertIsNone(self._btn(pr))

    def test_review_column_non_author_gets_pr_audit(self) -> None:
        pr = _make_pr(author="alice")
        btn = self._btn(pr, column="needs_review", username="octocat")
        assert btn is not None
        self.assertEqual(btn.label, "PR Audit")
        self.assertEqual(btn.skill, "pr-audit")

    def test_needs_response_yields_respond(self) -> None:
        pr = _make_pr(
            author="octocat",
            review_decision="CHANGES_REQUESTED",
            reviewer_states={"alice": "CHANGES_REQUESTED"},
            reviewers=["alice"],
            requested_reviewers=[],
        )
        btn = self._btn(pr, column="in_progress", username="octocat")
        assert btn is not None
        self.assertEqual(btn.label, "Respond")
        self.assertEqual(btn.skill, "pr-respond")

    def test_author_no_response_needed_yields_build_audit(self) -> None:
        pr = _make_pr(author="octocat")
        btn = self._btn(pr, column="in_progress", username="octocat")
        assert btn is not None
        self.assertEqual(btn.label, "Build Audit")
        self.assertEqual(btn.skill, "build-audit")

    def test_non_author_outside_review_column_yields_pr_audit(self) -> None:
        pr = _make_pr(author="alice")
        btn = self._btn(pr, column="in_progress", username="octocat")
        assert btn is not None
        self.assertEqual(btn.label, "PR Audit")
        self.assertEqual(btn.skill, "pr-audit")


# ---------------------------------------------------------------------------
# CI summary
# ---------------------------------------------------------------------------


class TestCISummary(unittest.TestCase):
    def test_buckets_by_conclusion(self) -> None:
        checks = [
            CICheck(name="a", status="completed", conclusion="success"),
            CICheck(name="b", status="completed", conclusion="success"),
            CICheck(name="c", status="completed", conclusion="failure"),
            CICheck(name="d", status="completed", conclusion="skipped"),
            CICheck(name="e", status="completed", conclusion="cancelled"),
            CICheck(name="f", status="completed", conclusion="neutral"),
            CICheck(name="g", status="in_progress", conclusion=None),
            CICheck(name="h", status="queued", conclusion=None),
        ]
        pr = _make_pr(ci_checks=checks)
        view = build_card_view(_card(prs=[pr]), "in_progress", "octocat")
        ci = view.pr_views[0].ci
        self.assertEqual(ci.passing, 2)
        self.assertEqual(ci.failing, 1)
        self.assertEqual(ci.other, 3)
        self.assertEqual(ci.pending, 2)
        self.assertEqual([c.name for c in ci.failing_checks], ["c"])

    def test_empty_checks_yields_zero_summary(self) -> None:
        pr = _make_pr(ci_checks=[])
        view = build_card_view(_card(prs=[pr]), "in_progress", "octocat")
        ci = view.pr_views[0].ci
        self.assertEqual((ci.passing, ci.failing, ci.pending, ci.other), (0, 0, 0, 0))
        self.assertEqual(ci.failing_checks, [])


# ---------------------------------------------------------------------------
# Sessions
# ---------------------------------------------------------------------------


class TestZingSessionView(unittest.TestCase):
    def test_dot_class_and_label_per_state(self) -> None:
        cases = [
            (SessionState.STARTED, "strip-zing-amber", "Claude session running"),
            (SessionState.PENDING, "strip-zing-red", "Waiting to start"),
            (SessionState.READY, "strip-zing-cyan", "Ready for review"),
            (SessionState.COMPLETED, "strip-zing-green", "Session completed"),
        ]
        for state, dot_cls, label in cases:
            with self.subTest(state=state):
                step = _make_workflow_step(step_name="review", state=state)
                session = _make_session(steps=[step])
                view = build_card_view(_card(sessions=[session]), "in_progress", "octocat")
                zv = view.zing_session_views[0]
                self.assertEqual(zv.dot_class, dot_cls)
                self.assertEqual(zv.state_label, label)


class TestClaudeCodeSessionView(unittest.TestCase):
    def test_no_terminal_session_means_alive(self) -> None:
        # state property returns STARTED when terminal_session is None
        session = _make_cc_session(title="Foo")
        view = build_card_view(_card(sessions=[session]), "in_progress", "octocat")
        cv = view.claude_code_session_views[0]
        self.assertTrue(cv.is_alive)
        self.assertEqual(cv.status_label, "Running: Foo")


# ---------------------------------------------------------------------------
# Total findings + active action
# ---------------------------------------------------------------------------


class TestAggregates(unittest.TestCase):
    def test_total_findings_sums_audit_steps(self) -> None:
        step1 = _make_workflow_step(
            step_name="plan-audit",
            findings=[TextFinding(title="x"), TextFinding(title="y")],
        )
        step2 = _make_workflow_step(
            step_name="build-audit",
            findings=[TextFinding(title="z")],
        )
        view = build_card_view(_card(audit_steps=[step1, step2]), "in_progress", "x")
        self.assertEqual(view.total_findings, 3)

    def test_has_active_action_zing_ready_step(self) -> None:
        step = _make_workflow_step(step_name="review", state=SessionState.READY)
        session = _make_session(steps=[step])
        view = build_card_view(_card(sessions=[session]), "in_progress", "x")
        self.assertTrue(view.has_active_action)

    def test_has_active_action_false_when_no_ready_step(self) -> None:
        step = _make_workflow_step(step_name="review", state=SessionState.PENDING)
        session = _make_session(steps=[step])
        view = build_card_view(_card(sessions=[session]), "in_progress", "x")
        self.assertFalse(view.has_active_action)


# ---------------------------------------------------------------------------
# Footer note
# ---------------------------------------------------------------------------


class TestFooterNote(unittest.TestCase):
    def test_waiting_on_others_in_review_column_with_others_group(self) -> None:
        view = build_card_view(_card(review_group="others"), "needs_review", "octocat")
        assert view.footer_note is not None
        self.assertEqual(view.footer_note.text, "Waiting on others")

    def test_in_progress_reason_passthrough(self) -> None:
        view = build_card_view(
            _card(in_progress_reason="Changes requested"), "in_progress", "octocat"
        )
        assert view.footer_note is not None
        self.assertEqual(view.footer_note.text, "Changes requested")

    def test_no_note_when_neither_condition_holds(self) -> None:
        view = build_card_view(_card(review_group="mine_passing"), "needs_review", "octocat")
        self.assertIsNone(view.footer_note)


# ---------------------------------------------------------------------------
# Card DOM id + extra classes
# ---------------------------------------------------------------------------


class TestCardDomIdAndExtras(unittest.TestCase):
    def test_dom_id_lowercases_and_replaces_slashes(self) -> None:
        view = build_card_view(_card(key="pr-Owner/Repo-42"), "todo", "x")
        self.assertEqual(view.card_dom_id, "card-pr-owner-repo-42")

    def test_card_ready_to_merge_extra_class_only_in_done_subgroup(self) -> None:
        view = build_card_view(_card(done_group="ready_to_merge"), "done", "x")
        self.assertEqual(view.extra_card_classes, ["card-ready-to-merge"])

    def test_no_extras_for_completed_done(self) -> None:
        view = build_card_view(_card(done_group="completed"), "done", "x")
        self.assertEqual(view.extra_card_classes, [])

    def test_no_extras_outside_done_column(self) -> None:
        view = build_card_view(_card(done_group="ready_to_merge"), "in_progress", "x")
        self.assertEqual(view.extra_card_classes, [])


# ---------------------------------------------------------------------------
# excluded_from_done_view
# ---------------------------------------------------------------------------


class TestExcludedFromDoneView(unittest.TestCase):
    def test_done_card_with_no_user_involvement_is_excluded(self) -> None:
        # PR card (no ticket); user is neither author nor in reviewers.
        pr = _make_pr(author="alice", reviewers=["bob"])
        view = build_card_view(_card(key="pr-1", prs=[pr]), "done", "octocat")
        self.assertTrue(view.excluded_from_done_view)

    def test_done_card_authored_by_user_is_included(self) -> None:
        pr = _make_pr(author="octocat")
        view = build_card_view(_card(key="pr-1", prs=[pr]), "done", "octocat")
        self.assertFalse(view.excluded_from_done_view)

    def test_done_ticket_only_card_is_always_included(self) -> None:
        ticket = _make_issue(identifier="BAK-1")
        view = build_card_view(_card(ticket=ticket), "done", "octocat")
        self.assertFalse(view.excluded_from_done_view)

    def test_non_done_columns_never_excluded(self) -> None:
        pr = _make_pr(author="alice", reviewers=["bob"])
        view = build_card_view(_card(prs=[pr]), "in_progress", "octocat")
        self.assertFalse(view.excluded_from_done_view)


# ---------------------------------------------------------------------------
# Stage 2: Jinja-global plumbing
# ---------------------------------------------------------------------------


class TestJinjaGlobals(unittest.TestCase):
    """``build_card_view`` and ``column_from_cls`` are reachable from templates.

    The ``{% set card_view = build_card_view(...) %}`` line in
    ``kanban_card.html`` would raise during render if either global went
    missing. Pin the contract here.
    """

    def test_kanban_card_renders_with_card_view_global(self) -> None:
        from zing_ai.server.templates import render

        ticket = _make_issue(identifier="BAK-1")
        card = KanbanCard(key="BAK-1", ticket=ticket)
        html = render(
            "fragments/kanban_card.html",
            card=card,
            column_cls="col-progress",
            current_username="octocat",
            live_sessions=set(),
            session_phases={},
        )
        # Card renders without raising; the ticket id appears as expected.
        self.assertIn("BAK-1", html)

    def test_column_from_cls_round_trip(self) -> None:
        from zing_ai.server.card_view import _COLUMN_CLS, column_from_cls

        for column, cls in _COLUMN_CLS.items():
            self.assertEqual(column_from_cls(cls), column)
