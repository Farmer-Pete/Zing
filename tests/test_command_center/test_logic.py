"""Unit tests for command_center aggregation logic."""

from __future__ import annotations

import unittest
from datetime import UTC, datetime

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
from zing_ai.server.command_center import (
    AUDIT_STEP_NAMES,
    _parse_ticket_id,
    aggregate,
)
from zing_ai.server.models import SessionState, TextFinding


class TestAggregateEmptyInputs(unittest.TestCase):
    """The aggregate() entry point returns empty collections for empty inputs."""

    def test_aggregate_empty_inputs(self) -> None:
        inbox, hubs = aggregate(
            issues=[],
            prs=[],
            sessions=[],
            current_username="octocat",
        )
        self.assertEqual(inbox, [])
        self.assertEqual(hubs, [])


class TestAuditStepNames(unittest.TestCase):
    """AUDIT_STEP_NAMES covers the four audit types."""

    def test_audit_step_names_contents(self) -> None:
        self.assertEqual(
            AUDIT_STEP_NAMES,
            frozenset({"plan-audit", "build-audit", "pr-audit", "custom-audit"}),
        )


class TestParseTicketId(unittest.TestCase):
    """_parse_ticket_id returns the first normalised ticket identifier on a PR."""

    def test_parse_uppercase_branch(self) -> None:
        pr = _make_pr(head_ref="BAK-1179/feature")
        self.assertEqual(_parse_ticket_id(pr), "BAK-1179")

    def test_parse_lowercase_branch(self) -> None:
        pr = _make_pr(head_ref="bak-1179/feature")
        self.assertEqual(_parse_ticket_id(pr), "BAK-1179")

    def test_parse_body_closes_keyword(self) -> None:
        pr = _make_pr(body="Closes BAK-1179")
        self.assertEqual(_parse_ticket_id(pr), "BAK-1179")

    def test_parse_first_match_wins_when_multiple_tickets(self) -> None:
        """Branch name takes precedence over body mentions — first match wins."""
        pr = _make_pr(head_ref="FRO-892/something", body="See BAK-1179 and ENG-42")
        self.assertEqual(_parse_ticket_id(pr), "FRO-892")

    def test_parse_no_match(self) -> None:
        pr = _make_pr(title="No tickets here", head_ref="feature/cleanup", body=None)
        self.assertIsNone(_parse_ticket_id(pr))

    def test_parse_single_letter_prefix_rejected(self) -> None:
        """Single-letter prefixes aren't real Linear keys; X-5 should not match."""
        pr = _make_pr(head_ref="feature/X-5", title="tweak X-5 handling", body=None)
        self.assertIsNone(_parse_ticket_id(pr))

    def test_parse_noisy_body_does_not_beat_branch_match(self) -> None:
        """A branch-name ticket wins even when the body has noisy tokens like UTF-8.

        First-match ordering means the branch is scanned first; downstream the
        parser returns ``BAK-1179`` rather than ``UTF-8`` from the body.
        """
        pr = _make_pr(
            head_ref="BAK-1179/fix-encoding",
            title="fix UTF-8 handling",
            body="Supports UTF-8 and SHA-256",
        )
        self.assertEqual(_parse_ticket_id(pr), "BAK-1179")

    def test_parse_empty_body_and_no_branch_match(self) -> None:
        """An empty body and a non-ticket head_ref must yield None, not raise."""
        pr = _make_pr(head_ref="feature/no-ticket", title="cleanup", body="")
        self.assertIsNone(_parse_ticket_id(pr))

    def test_parse_none_body_is_tolerated(self) -> None:
        """`GitHubPR.body is None` shouldn't break the parser (filter handles None)."""
        pr = _make_pr(head_ref="feature/misc", title="cleanup", body=None)
        self.assertIsNone(_parse_ticket_id(pr))

    def test_parse_unicode_adjacency(self) -> None:
        """Emoji/CJK characters adjacent to a ticket id must still match."""
        pr = _make_pr(head_ref="main", title="fix BAK-1179 \U0001f680", body="closes BAK-1179")
        self.assertEqual(_parse_ticket_id(pr), "BAK-1179")


class TestJoinLogic(unittest.TestCase):
    """aggregate() correctly joins issues, PRs, and sessions into Hubs."""

    def test_ticket_with_one_pr_and_session(self) -> None:
        """1 issue + 1 matching PR + 1 matching session -> 1 ticket-Hub containing both."""
        issue = _make_issue(identifier="BAK-1")
        pr = _make_pr(number=10, head_ref="BAK-1/feature")
        session = _make_session(session_id="s1", ticket_id="BAK-1")

        _, hubs = aggregate(
            issues=[issue],
            prs=[pr],
            sessions=[session],
            current_username="octocat",
        )

        self.assertEqual(len(hubs), 1)
        hub = hubs[0]
        self.assertEqual(hub.kind, "ticket")
        self.assertEqual(hub.id, "BAK-1")
        self.assertEqual(len(hub.prs), 1)
        self.assertEqual(hub.prs[0].number, 10)
        self.assertEqual(len(hub.sessions), 1)
        self.assertEqual(hub.sessions[0].session_id, "s1")

    def test_two_distinct_issues_produce_two_hubs(self) -> None:
        """Contract test: ticket_hubs is keyed by issue.identifier.

        Regression guard against accidental key changes (e.g. switching to
        issue.id) that would collapse distinct tickets into one hub.
        """
        i1 = _make_issue(identifier="BAK-1")
        i2 = _make_issue(identifier="BAK-2")
        _, hubs = aggregate(
            issues=[i1, i2],
            prs=[],
            sessions=[],
            current_username="octocat",
        )
        self.assertEqual(len(hubs), 2)
        self.assertEqual({h.id for h in hubs}, {"BAK-1", "BAK-2"})

    def test_orphan_pr_becomes_pr_hub(self) -> None:
        """A PR with no matching ticket -> Hub(kind='pr')."""
        pr = _make_pr(number=99, head_ref="feature/no-ticket")

        _, hubs = aggregate(
            issues=[],
            prs=[pr],
            sessions=[],
            current_username="octocat",
        )

        self.assertEqual(len(hubs), 1)
        hub = hubs[0]
        self.assertEqual(hub.kind, "pr")
        self.assertEqual(hub.id, "pr-99")
        self.assertEqual(len(hub.prs), 1)

    def test_orphan_session_becomes_session_hub(self) -> None:
        """A session with no ticket_id -> Hub(kind='session')."""
        session = _make_session(session_id="orphan-sess", title="Orphan", ticket_id=None)

        _, hubs = aggregate(
            issues=[],
            prs=[],
            sessions=[session],
            current_username="octocat",
        )

        self.assertEqual(len(hubs), 1)
        hub = hubs[0]
        self.assertEqual(hub.kind, "session")
        self.assertEqual(hub.id, "session-orphan-sess")
        self.assertEqual(len(hub.sessions), 1)

    def test_session_with_mixed_steps_surfaces_in_both_spokes(self) -> None:
        """Session with mixed audit + non-audit steps appears in BOTH hub.sessions and hub.audits.

        Regression: the original behaviour partitioned mutually — an audit
        session would be stripped to just its audit steps and the rest of the
        session (plus the parent Session itself) was silently dropped. The
        typical Zing workflow ``['plan', 'plan-audit', 'build', 'build-audit']``
        must surface both the parent session and its audit steps so
        ``_compute_urgency`` can observe STARTED non-audit work and the UI
        can render both spokes.
        """
        plan_step = _make_workflow_step(step_name="plan", sequence=0)
        plan_audit = _make_workflow_step(step_name="plan-audit", sequence=1)
        build_step = _make_workflow_step(step_name="build", sequence=2)
        build_audit = _make_workflow_step(step_name="build-audit", sequence=3)
        session = _make_session(
            session_id="mixed-sess",
            ticket_id=None,
            steps=[plan_step, plan_audit, build_step, build_audit],
        )

        _, hubs = aggregate(
            issues=[],
            prs=[],
            sessions=[session],
            current_username="octocat",
        )

        self.assertEqual(len(hubs), 1)
        hub = hubs[0]
        # Parent session retained — Sessions spoke shows overall progress.
        self.assertEqual(len(hub.sessions), 1)
        self.assertIs(hub.sessions[0], session)
        # Audit steps also surfaced — Audits spoke shows audit-specific state.
        self.assertEqual(len(hub.audits), 2)
        self.assertEqual({s.step_name for s in hub.audits}, {"plan-audit", "build-audit"})

    def test_session_with_only_non_audit_steps(self) -> None:
        """Session with no audit steps appears in hub.sessions, not in hub.audits."""
        plan_step = _make_workflow_step(step_name="plan", sequence=0)
        session = _make_session(
            session_id="plan-sess",
            ticket_id=None,
            steps=[plan_step],
        )

        _, hubs = aggregate(
            issues=[],
            prs=[],
            sessions=[session],
            current_username="octocat",
        )

        self.assertEqual(len(hubs), 1)
        hub = hubs[0]
        self.assertEqual(len(hub.sessions), 1)
        self.assertEqual(len(hub.audits), 0)


class TestUrgencyComputation(unittest.TestCase):
    """_compute_urgency is called inside aggregate() and sets hub.urgency correctly."""

    def test_hub_with_ready_audit_is_hot(self) -> None:
        """An audit step in READY state with findings -> urgency == 'hot'."""
        finding = TextFinding(title="Issue found", body="Details here")
        audit_step = _make_workflow_step(
            step_name="build-audit",
            state=SessionState.READY,
            findings=[finding],
        )
        session = _make_session(
            session_id="audit-sess",
            ticket_id="BAK-1",
            steps=[audit_step],
        )
        issue = _make_issue(identifier="BAK-1")

        _, hubs = aggregate(
            issues=[issue],
            prs=[],
            sessions=[session],
            current_username="octocat",
        )

        self.assertEqual(len(hubs), 1)
        self.assertEqual(hubs[0].urgency, "hot")

    def test_hub_with_pr_review_requested_is_hot(self) -> None:
        """PR with current_username in requested_reviewers and not APPROVED -> urgency == 'hot'."""
        pr = _make_pr(number=42, head_ref="BAK-1/feature")
        pr.requested_reviewers = ["octocat"]
        pr.review_decision = None

        issue = _make_issue(identifier="BAK-1")

        _, hubs = aggregate(
            issues=[issue],
            prs=[pr],
            sessions=[],
            current_username="octocat",
        )

        self.assertEqual(len(hubs), 1)
        self.assertEqual(hubs[0].urgency, "hot")

    def test_hub_with_running_session_is_active(self) -> None:
        """A session step in STARTED state -> urgency == 'active' (not hot)."""
        step = _make_workflow_step(
            step_name="review",
            state=SessionState.STARTED,
        )
        session = _make_session(
            session_id="active-sess",
            ticket_id=None,  # orphan session hub
            steps=[step],
        )

        _, hubs = aggregate(
            issues=[],
            prs=[],
            sessions=[session],
            current_username="octocat",
        )

        self.assertEqual(len(hubs), 1)
        self.assertEqual(hubs[0].urgency, "active")

    def test_hub_with_completed_state_is_cool(self) -> None:
        """A hub with no hot or active signals -> urgency == 'cool'."""
        step = _make_workflow_step(
            step_name="review",
            state=SessionState.COMPLETED,
        )
        session = _make_session(
            session_id="cool-sess",
            ticket_id=None,
            steps=[step],
        )

        _, hubs = aggregate(
            issues=[],
            prs=[],
            sessions=[session],
            current_username="octocat",
        )

        self.assertEqual(len(hubs), 1)
        self.assertEqual(hubs[0].urgency, "cool")

    def test_empty_hub_is_cool(self) -> None:
        """A fresh ticket hub with no PRs/sessions/audits is the common initial state.

        Regression: the cool branch used to be implicitly covered by hubs
        with COMPLETED steps; this asserts the truly-empty case too.
        """
        issue = _make_issue(identifier="ENG-1")
        _, hubs = aggregate(
            issues=[issue],
            prs=[],
            sessions=[],
            current_username="octocat",
        )
        self.assertEqual(len(hubs), 1)
        self.assertEqual(hubs[0].urgency, "cool")


class TestInboxItems(unittest.TestCase):
    """_derive_inbox_items builds the prioritised action list."""

    def test_audit_ready_creates_inbox_item(self) -> None:
        """An audit step in READY state with findings generates a high-priority inbox item."""
        from zing_ai.server.models import TextFinding

        finding = TextFinding(title="Issue found", body="Details here")
        audit_step = _make_workflow_step(
            step_name="build-audit",
            state=SessionState.READY,
            findings=[finding],
        )
        session = _make_session(
            session_id="audit-sess",
            ticket_id="BAK-1",
            steps=[audit_step],
        )
        issue = _make_issue(identifier="BAK-1")

        inbox, _ = aggregate(
            issues=[issue],
            prs=[],
            sessions=[session],
            current_username="octocat",
        )

        self.assertEqual(len(inbox), 1)
        item = inbox[0]
        self.assertEqual(item.priority, "high")
        self.assertIn("1", item.action_text)
        self.assertIn("audit", item.action_text.lower())
        self.assertEqual(item.hub_id, "BAK-1")
        self.assertEqual(item.hub_label, "BAK-1")
        self.assertEqual(item.detail_text, "build-audit")
        self.assertIn("audit-sess", item.target_url)

    def test_pr_review_requested_creates_inbox_item(self) -> None:
        """A PR with current_username in requested_reviewers yields a medium inbox item."""
        pr = _make_pr(number=42, head_ref="feature/no-ticket")
        pr.requested_reviewers = ["octocat"]
        pr.review_decision = None

        inbox, _ = aggregate(
            issues=[],
            prs=[pr],
            sessions=[],
            current_username="octocat",
        )

        self.assertEqual(len(inbox), 1)
        item = inbox[0]
        self.assertEqual(item.priority, "medium")
        self.assertIn("42", item.action_text)
        self.assertEqual(item.target_url, pr.url)
        self.assertEqual(item.hub_id, "pr-42")
        self.assertEqual(item.hub_label, "Standalone")

    def test_pr_inbox_item_with_tz_aware_updated_at_does_not_crash(self) -> None:
        """Regression: real GitHub timestamps are tz-aware; _format_time_waiting must not raise."""

        pr = _make_pr(number=77, head_ref="feature/tz-bug")
        pr.requested_reviewers = ["octocat"]
        pr.review_decision = None
        # Mirror what github_client._map_pr produces from a real GitHub API response.
        pr.updated_at = datetime(2026, 4, 16, 0, 0, 0, tzinfo=UTC)

        inbox, _ = aggregate(
            issues=[],
            prs=[pr],
            sessions=[],
            current_username="octocat",
        )

        self.assertEqual(len(inbox), 1)
        item = inbox[0]
        # time_waiting must be a non-empty string ("Xm" / "Xh" / "Xd" / "—") rather than raising.
        self.assertIsInstance(item.time_waiting, str)
        self.assertNotEqual(item.time_waiting, "")

    def test_inbox_sorted_high_priority_first(self) -> None:
        """High-priority audit items appear before medium-priority PR review items."""
        from zing_ai.server.models import TextFinding

        finding = TextFinding(title="Issue found", body="Details here")
        audit_step = _make_workflow_step(
            step_name="build-audit",
            state=SessionState.READY,
            findings=[finding],
        )
        session = _make_session(
            session_id="audit-sess",
            ticket_id=None,
            steps=[audit_step],
        )

        # PR review item — older so would sort first by time if priority were equal
        pr = _make_pr(number=99, head_ref="feature/no-ticket")
        pr.requested_reviewers = ["octocat"]
        pr.review_decision = "REVIEW_REQUIRED"
        pr.updated_at = datetime(2025, 1, 1, 0, 0, 0)  # very old

        inbox, _ = aggregate(
            issues=[],
            prs=[pr],
            sessions=[session],
            current_username="octocat",
        )

        self.assertGreaterEqual(len(inbox), 2)
        self.assertEqual(inbox[0].priority, "high")
        self.assertEqual(inbox[1].priority, "medium")

    def test_empty_inbox_when_no_actions_pending(self) -> None:
        """No READY audit steps and no review requests means the inbox stays empty."""
        from zing_ai.server.models import TextFinding

        # Audit step is COMPLETED (not READY)
        finding = TextFinding(title="Issue found", body="Details here")
        audit_step = _make_workflow_step(
            step_name="build-audit",
            state=SessionState.COMPLETED,
            findings=[finding],
        )
        session = _make_session(
            session_id="done-sess",
            ticket_id=None,
            steps=[audit_step],
        )

        # PR where current user is NOT in requested_reviewers
        pr = _make_pr(number=7, head_ref="feature/no-ticket")
        pr.requested_reviewers = ["someone-else"]

        inbox, _ = aggregate(
            issues=[],
            prs=[pr],
            sessions=[session],
            current_username="octocat",
        )

        self.assertEqual(inbox, [])


if __name__ == "__main__":
    unittest.main()
