"""Unit tests for command_center aggregation logic."""

from __future__ import annotations

import unittest
from datetime import datetime

from zing_ai.server.command_center import (
    AUDIT_STEP_NAMES,
    _parse_ticket_ids,
    aggregate,
)
from zing_ai.server.models import Session, SessionState, TextFinding, WorkflowStep
from zing_ai.server.models_external import GitHubPR, LinearIssue


def _make_pr(
    *,
    number: int = 1,
    title: str = "Title",
    head_ref: str = "feature",
    body: str | None = None,
) -> GitHubPR:
    """Build a GitHubPR with sensible defaults for ticket-parsing tests."""
    return GitHubPR(
        number=number,
        title=title,
        state="open",
        draft=False,
        head_ref=head_ref,
        base_ref="main",
        body=body,
        requested_reviewers=[],
        review_decision=None,
        mergeable_state="clean",
        ci_status=None,
        url=f"https://github.com/o/r/pull/{number}",
        updated_at=datetime(2026, 4, 16, 0, 0, 0),
    )


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


class TestParseTicketIds(unittest.TestCase):
    """_parse_ticket_ids extracts normalised ticket identifiers from PRs."""

    def test_parse_uppercase_branch(self) -> None:
        pr = _make_pr(head_ref="BAK-1179/feature")
        self.assertEqual(_parse_ticket_ids(pr), {"BAK-1179"})

    def test_parse_lowercase_branch(self) -> None:
        pr = _make_pr(head_ref="bak-1179/feature")
        self.assertEqual(_parse_ticket_ids(pr), {"BAK-1179"})

    def test_parse_body_closes_keyword(self) -> None:
        pr = _make_pr(body="Closes BAK-1179")
        self.assertEqual(_parse_ticket_ids(pr), {"BAK-1179"})

    def test_parse_multiple_tickets(self) -> None:
        pr = _make_pr(body="See FRO-892 and BAK-1179")
        self.assertEqual(_parse_ticket_ids(pr), {"FRO-892", "BAK-1179"})

    def test_parse_no_match(self) -> None:
        pr = _make_pr(title="No tickets here", head_ref="feature/cleanup", body=None)
        self.assertEqual(_parse_ticket_ids(pr), set())


class TestJoinLogic(unittest.TestCase):
    """aggregate() correctly joins issues, PRs, and sessions into Hubs."""

    def _make_issue(
        self,
        *,
        identifier: str = "BAK-1",
        title: str = "Fix bug",
        team: str = "Back End",
        assignee: str | None = "alice",
    ) -> LinearIssue:
        return LinearIssue(
            id="uuid-" + identifier,
            identifier=identifier,
            title=title,
            state="In Progress",
            assignee=assignee,
            team=team,
            url=f"https://linear.app/t/{identifier}",
            updated_at=datetime(2026, 4, 16, 0, 0, 0),
        )

    def _make_session(
        self,
        *,
        session_id: str = "sess-1",
        title: str = "Session 1",
        ticket_id: str | None = None,
        steps: list | None = None,
    ) -> Session:
        return Session(
            session_id=session_id,
            title=title,
            ticket_id=ticket_id,
            steps=steps or [],
        )

    def _make_workflow_step(self, *, step_name: str, sequence: int = 0) -> WorkflowStep:
        return WorkflowStep(step_name=step_name, sequence=sequence)

    def test_ticket_with_one_pr_and_session(self) -> None:
        """1 issue + 1 matching PR + 1 matching session -> 1 ticket-Hub containing both."""
        issue = self._make_issue(identifier="BAK-1")
        pr = _make_pr(number=10, head_ref="BAK-1/feature")
        session = self._make_session(session_id="s1", ticket_id="BAK-1")

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
        session = self._make_session(session_id="orphan-sess", title="Orphan", ticket_id=None)

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

    def test_session_with_audit_step_classified_as_audit(self) -> None:
        """Session with step_name='build-audit' -> WorkflowSteps appear in hub.audits."""
        audit_step = self._make_workflow_step(step_name="build-audit", sequence=0)
        non_audit_step = self._make_workflow_step(step_name="review", sequence=1)
        session = self._make_session(
            session_id="audit-sess",
            ticket_id=None,
            steps=[audit_step, non_audit_step],
        )

        _, hubs = aggregate(
            issues=[],
            prs=[],
            sessions=[session],
            current_username="octocat",
        )

        self.assertEqual(len(hubs), 1)
        hub = hubs[0]
        self.assertEqual(len(hub.audits), 1)
        self.assertIsInstance(hub.audits[0], WorkflowStep)
        self.assertEqual(hub.audits[0].step_name, "build-audit")
        self.assertEqual(len(hub.sessions), 0)


class TestUrgencyComputation(unittest.TestCase):
    """_compute_urgency is called inside aggregate() and sets hub.urgency correctly."""

    def _make_issue(
        self,
        *,
        identifier: str = "BAK-1",
        title: str = "Fix bug",
    ) -> LinearIssue:
        return LinearIssue(
            id="uuid-" + identifier,
            identifier=identifier,
            title=title,
            state="In Progress",
            assignee="alice",
            team="Back End",
            url=f"https://linear.app/t/{identifier}",
            updated_at=datetime(2026, 4, 16, 0, 0, 0),
        )

    def _make_session(
        self,
        *,
        session_id: str = "sess-1",
        title: str = "Session 1",
        ticket_id: str | None = None,
        steps: list | None = None,
    ) -> Session:
        return Session(
            session_id=session_id,
            title=title,
            ticket_id=ticket_id,
            steps=steps or [],
        )

    def _make_workflow_step(
        self,
        *,
        step_name: str,
        sequence: int = 0,
        state: SessionState = SessionState.PENDING,
        findings: list | None = None,
    ) -> WorkflowStep:
        step = WorkflowStep(step_name=step_name, sequence=sequence)
        step.state = state
        if findings is not None:
            step.findings = findings
        return step

    def test_hub_with_ready_audit_is_hot(self) -> None:
        """An audit step in READY state with findings -> urgency == 'hot'."""
        finding = TextFinding(title="Issue found", body="Details here")
        audit_step = self._make_workflow_step(
            step_name="build-audit",
            state=SessionState.READY,
            findings=[finding],
        )
        session = self._make_session(
            session_id="audit-sess",
            ticket_id="BAK-1",
            steps=[audit_step],
        )
        issue = self._make_issue(identifier="BAK-1")

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

        issue = self._make_issue(identifier="BAK-1")

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
        step = self._make_workflow_step(
            step_name="review",
            state=SessionState.STARTED,
        )
        session = self._make_session(
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
        step = self._make_workflow_step(
            step_name="review",
            state=SessionState.COMPLETED,
        )
        session = self._make_session(
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


if __name__ == "__main__":
    unittest.main()
