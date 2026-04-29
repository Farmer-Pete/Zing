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

    def test_reviewed_pill_overrides_build_audit_with_respond(self) -> None:
        """``pill == 'reviewed'`` → primary defaults to Respond.

        Reproduces frontend-v2#259: a single human reviewer leaves a
        COMMENTED review on a single-PR card.  ``_is_human_reviewer``
        conservatively classifies them as a bot (no sibling PRs to
        provide the "requested elsewhere" signal), so
        ``_pr_needs_response`` returns False — but the pill is
        "reviewed", so the user expects to Respond, not Build Audit.
        """
        pr = _make_pr(
            author="octocat",
            reviewers=["raina"],
            requested_reviewers=[],
            reviewer_states={"raina": "COMMENTED"},
            review_decision=None,
        )
        view = build_card_view(_card(prs=[pr]), "in_progress", "octocat")
        pr_view = view.pr_views[0]
        # The pill's "reviewed" label is what the user sees.
        assert pr_view.pill is not None
        self.assertEqual(pr_view.pill.label, "reviewed")
        # ``_pr_needs_response`` (conservatively) said False — the bot heuristic.
        self.assertFalse(pr_view.needs_response)
        # But the primary button should still match the pill.
        assert pr_view.primary_button is not None
        self.assertEqual(pr_view.primary_button.label, "Respond")
        self.assertEqual(pr_view.primary_button.skill, "pr-respond")

    def test_no_pill_with_no_response_keeps_build_audit(self) -> None:
        """Author + no review activity + no pill → Build Audit (unchanged)."""
        pr = _make_pr(author="octocat")  # no reviewers, no decision, no pill
        view = build_card_view(_card(prs=[pr]), "in_progress", "octocat")
        pr_view = view.pr_views[0]
        self.assertIsNone(pr_view.pill)
        assert pr_view.primary_button is not None
        self.assertEqual(pr_view.primary_button.label, "Build Audit")

    def test_approved_with_followup_commented_review_yields_respond(self) -> None:
        """``reviewDecision == APPROVED`` + COMMENTED reviewer → Respond.

        Reproduces backend-v1#1895: a reviewer approved, then left
        follow-up comments (state in ``latestReviews`` overwritten to
        COMMENTED).  ``_pr_needs_response`` short-circuits on APPROVED
        and never sees the comment activity, but the author should
        still respond to the comments rather than Build Audit.
        """
        pr = _make_pr(
            author="octocat",
            reviewers=["kyle"],
            requested_reviewers=["max"],
            reviewer_states={"kyle": "COMMENTED"},
            review_decision="APPROVED",
        )
        view = build_card_view(_card(prs=[pr]), "done", "octocat")
        pr_view = view.pr_views[0]
        # The pill correctly reads "approved" (PR-level decision wins).
        assert pr_view.pill is not None
        self.assertEqual(pr_view.pill.label, "approved")
        # _pr_needs_response = False because of the APPROVED short-circuit.
        self.assertFalse(pr_view.needs_response)
        # But Kyle's COMMENTED state means the author should still respond.
        assert pr_view.primary_button is not None
        self.assertEqual(pr_view.primary_button.label, "Respond")
        self.assertEqual(pr_view.primary_button.skill, "pr-respond")

    def test_approved_with_only_approved_reviewer_keeps_build_audit(self) -> None:
        """Pure approved PR (reviewer state APPROVED) → Build Audit.

        No follow-up activity to respond to; the approval *is* the
        response.  Author should proceed to merge / Build Audit.
        """
        pr = _make_pr(
            author="octocat",
            reviewers=["kyle"],
            requested_reviewers=[],
            reviewer_states={"kyle": "APPROVED"},
            review_decision="APPROVED",
        )
        view = build_card_view(_card(prs=[pr]), "done", "octocat")
        pr_view = view.pr_views[0]
        assert pr_view.primary_button is not None
        self.assertEqual(pr_view.primary_button.label, "Build Audit")

    def test_approved_with_outstanding_rerequest_yields_respond(self) -> None:
        """``reviewDecision == APPROVED`` + non-empty ``requested_reviewers`` → Respond.

        Reproduces backend-v1#1896: kyle approved, then the author
        re-requested max (or added max as a fresh reviewer after the
        approval).  GitHub's ``latestReviews`` excludes max, so
        ``reviewer_states`` only shows kyle's APPROVED state and the
        existing non-APPROVED-state rule can't fire.  The PR isn't
        truly awaiting merge — the author has an outstanding review
        request — so the primary should be Respond, not Build Audit.
        """
        pr = _make_pr(
            author="octocat",
            reviewers=["kyle"],
            requested_reviewers=["max"],
            reviewer_states={"kyle": "APPROVED"},
            review_decision="APPROVED",
        )
        view = build_card_view(_card(prs=[pr]), "done", "octocat")
        pr_view = view.pr_views[0]
        assert pr_view.primary_button is not None
        self.assertEqual(pr_view.primary_button.label, "Respond")
        self.assertEqual(pr_view.primary_button.skill, "pr-respond")


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


# ---------------------------------------------------------------------------
# Stage 3: rendered-HTML pins per migrated block
# ---------------------------------------------------------------------------


def _render_card(card: KanbanCard, column_cls: str = "col-progress") -> str:
    from zing_ai.server.templates import render

    return render(
        "fragments/kanban_card.html",
        card=card,
        column_cls=column_cls,
        current_username="octocat",
        live_sessions=set(),
        session_phases={},
    )


class TestRenderedHTML(unittest.TestCase):
    """Pin per-block migration outcomes by asserting on rendered HTML."""

    def test_changes_requested_pill_renders_outside_review_column(self) -> None:
        pr = _make_pr(number=1, review_decision="CHANGES_REQUESTED")
        html = _render_card(KanbanCard(key="K", prs=[pr]), column_cls="col-progress")
        self.assertIn('<span class="strip-pill strip-pill-changes">changes requested</span>', html)

    def test_approved_pill_wins_over_reviewed(self) -> None:
        pr = _make_pr(
            number=1,
            author="octocat",
            reviewers=["alice"],
            requested_reviewers=[],
            review_decision="APPROVED",
        )
        html = _render_card(KanbanCard(key="K", prs=[pr]))
        self.assertIn('<span class="strip-pill strip-pill-approved">approved</span>', html)
        self.assertNotIn("strip-pill-reviewed", html)

    def test_primary_button_respond_for_unaddressed_feedback(self) -> None:
        pr = _make_pr(
            number=1,
            author="octocat",
            review_decision="CHANGES_REQUESTED",
            reviewer_states={"alice": "CHANGES_REQUESTED"},
            reviewers=["alice"],
            requested_reviewers=[],
        )
        html = _render_card(KanbanCard(key="K", prs=[pr]))
        self.assertIn(">Respond</span>", html)
        self.assertNotIn(">Build Audit</span>", html)

    def test_primary_button_build_audit_for_author_no_feedback(self) -> None:
        pr = _make_pr(number=1, author="octocat")
        html = _render_card(KanbanCard(key="K", prs=[pr]))
        self.assertIn(">Build Audit</span>", html)

    def test_no_primary_button_for_merged_pr(self) -> None:
        pr = _make_pr(
            number=1,
            author="octocat",
            state="merged",
            merged_at=datetime(2026, 4, 1, tzinfo=UTC),
        )
        html = _render_card(KanbanCard(key="K", prs=[pr]), column_cls="col-done")
        # No Respond / Build Audit / PR Audit primary button slot for merged PRs
        self.assertNotIn(">Respond</span>", html)
        self.assertNotIn(">Build Audit</span>", html)
        self.assertNotIn(">PR Audit</span>", html)

    def test_card_dom_id_uses_lowercased_replaced_key(self) -> None:
        pr = _make_pr(number=1)
        html = _render_card(KanbanCard(key="pr-Owner/Repo-1", prs=[pr]), column_cls="col-progress")
        self.assertIn('id="card-pr-owner-repo-1"', html)

    def test_ready_to_merge_extra_class_on_done_subgroup(self) -> None:
        pr = _make_pr(number=1, author="octocat", review_decision="APPROVED")
        card = KanbanCard(key="K", prs=[pr], done_group="ready_to_merge")
        html = _render_card(card, column_cls="col-done")
        self.assertIn("card-ready-to-merge", html)

    def test_ci_failure_callout_renders_failing_check_names(self) -> None:
        pr = _make_pr(
            number=1,
            ci_checks=[
                CICheck(name="lint", status="completed", conclusion="success"),
                CICheck(name="unit-tests", status="completed", conclusion="failure"),
            ],
        )
        html = _render_card(KanbanCard(key="K", prs=[pr]))
        self.assertIn('class="ci-fail-bar"', html)
        self.assertIn("unit-tests", html)
        # Only failing checks appear in the callout
        callout_idx = html.find('class="ci-fail-bar"')
        callout_end = html.find("</div>", callout_idx)
        self.assertNotIn("lint", html[callout_idx:callout_end])

    def test_footer_note_waiting_on_others_in_review_column(self) -> None:
        pr = _make_pr(number=1)
        card = KanbanCard(key="K", prs=[pr], review_group="others")
        html = _render_card(card, column_cls="col-review")
        self.assertIn(">Waiting on others</span>", html)

    def test_footer_note_in_progress_reason_passthrough(self) -> None:
        pr = _make_pr(number=1)
        card = KanbanCard(key="K", prs=[pr], in_progress_reason="Changes requested")
        html = _render_card(card, column_cls="col-progress")
        self.assertIn(">Changes requested</span>", html)


# ---------------------------------------------------------------------------
# Stage 6: debug-tool coverage pinned to CardView's field set
# ---------------------------------------------------------------------------


def _card_view_field_names() -> set[str]:
    """Collect every field declared on a model defined in ``card_view``.

    Only recurses into sub-models that were *defined in* ``card_view``
    (StripPill, PRPrimaryButton, CICheckSummary, PRView,
    ClaudeCodeSessionView, ZingSessionView, FooterNote, CardView).
    Wrapped models from other modules (KanbanCard, GitHubPR, ClaudeCode
    Session, …) carry their own field surface and are out of scope —
    the contract this test pins is "every field added to a card_view
    model surfaces in debug-card output".
    """
    import typing

    from pydantic import BaseModel

    from zing_ai.server import card_view as cv_module

    in_scope = {
        v for v in vars(cv_module).values() if isinstance(v, type) and issubclass(v, BaseModel)
    }
    seen: set[type] = set()
    fields: set[str] = set()

    def walk(cls: type) -> None:
        if cls in seen or cls not in in_scope:
            return
        seen.add(cls)
        for name, info in cls.model_fields.items():
            fields.add(name)
            ann = info.annotation
            for sub in typing.get_args(ann) or (ann,):
                if isinstance(sub, type) and issubclass(sub, BaseModel):
                    walk(sub)

    walk(cv_module.CardView)
    return fields


class TestDebugToolCoverage(unittest.TestCase):
    """Every CardView field must surface in ``zing-ai debug-card`` output.

    Adding a field to CardView (or any nested view) and forgetting to
    wire it into the debug printer would normally pass CI silently —
    this test pins the contract by walking the model schema and
    asserting each field name appears in the formatter's output.
    """

    def _comprehensive_card_view(self):
        """Build a CardView populated enough to exercise every nested model."""
        from zing_ai.server.models import (
            ClaudeCodeSession,
            Notification,
            WorkflowStep,
        )

        ticket = _make_issue(identifier="BAK-99")
        pr = _make_pr(
            number=1,
            author="octocat",
            review_decision="CHANGES_REQUESTED",
            reviewer_states={"alice": "CHANGES_REQUESTED"},
            reviewers=["alice"],
            requested_reviewers=[],
            ci_checks=[
                CICheck(name="ci-fail", status="completed", conclusion="failure"),
                CICheck(name="ci-ok", status="completed", conclusion="success"),
            ],
        )
        zing_session = _make_session(
            session_id="zs-1",
            ticket_id="BAK-99",
            steps=[_make_workflow_step(step_name="review", state=SessionState.STARTED)],
        )
        cc_session = ClaudeCodeSession(
            session_id="cc-1",
            title="cc",
            ticket_id="BAK-99",
            terminal_session="zellij-1",
            notifications=[Notification(title="q", body="test question?")],
        )
        audit_step = WorkflowStep(step_name="plan-audit", sequence=0)
        audit_step.state = SessionState.READY
        from zing_ai.server.models import TextFinding

        audit_step.findings = [TextFinding(title="finding")]
        card = KanbanCard(
            key="BAK-99",
            ticket=ticket,
            prs=[pr],
            sessions=[zing_session, cc_session],
            audit_steps=[audit_step],
            in_progress_reason="Changes requested",
        )
        return build_card_view(card, "in_progress", "octocat")

    def test_every_card_view_field_appears_in_debug_output(self) -> None:
        from zing_ai.debug_card import _format_model

        view = self._comprehensive_card_view()
        output = "\n".join(_format_model(view))

        for name in _card_view_field_names():
            with self.subTest(field=name):
                # Each card_view field must appear as a "name:" prefix
                # somewhere in the printer output.
                self.assertIn(f"{name}:", output)
