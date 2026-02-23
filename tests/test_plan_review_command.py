"""Tests for the orchestrator ``plan-review`` command."""

from __future__ import annotations

import asyncio
import xml.etree.ElementTree as ET
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from zing_ai.orchestrator.commands.plan_review import (
    ReviewState,
    _compute_changes,
    _start_review_server,
    run_plan_review,
)
from zing_ai.orchestrator.config import ZingConfig
from zing_ai.orchestrator.models import (
    Choice,
    ChoiceSet,
    Interaction,
    Plan,
    Stage,
    Step,
    ZingDocument,
)
from zing_ai.orchestrator.xml_parser import write_zing_file

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _run(coro):  # type: ignore[no-untyped-def]
    """Run an async coroutine synchronously."""
    return asyncio.run(coro)


def _make_choice_sets() -> list[ChoiceSet]:
    """Create a list of sample choice sets for testing."""
    return [
        ChoiceSet(
            message="Which database?",
            explanation="We need to choose a database.",
            choices=[
                Choice(label="PostgreSQL", description="Relational DB", recommended=True),
                Choice(label="MongoDB", description="Document DB", recommended=False),
                Choice(label="SQLite", description="Embedded DB", recommended=False),
            ],
        ),
        ChoiceSet(
            message="Which framework?",
            explanation="We need to choose a framework.",
            choices=[
                Choice(label="FastAPI", description="Modern async", recommended=True),
                Choice(label="Flask", description="Classic", recommended=False),
            ],
        ),
    ]


def _make_zing_file_with_choices(
    tmp_path: Path,
    *,
    choice_sets: list[ChoiceSet] | None = None,
    plan_session: str = "sess-plan-001",
) -> Path:
    """Create a zing file with plan and interactions."""
    zing_dir = tmp_path / ".zing"
    zing_dir.mkdir(exist_ok=True)
    zing_path = zing_dir / "test-project.xml"

    plan = Plan(stages=[
        Stage(label="stage-1", steps=[
            Step(label="step-1", instructions="Do something", files=["src/main.py"], done=False),
        ]),
    ])

    if choice_sets is None:
        choice_sets = _make_choice_sets()

    interaction = Interaction(choice_sets=choice_sets)

    doc = ZingDocument(
        stage="plan",
        content="# Test Project\n\nA test project.",
        plan=plan,
        interactions=interaction,
        audit=True,
        approved=False,
        plan_session=plan_session,
        audit_session="sess-audit-001",
    )
    write_zing_file(zing_path, doc)
    return zing_path


def _make_zing_file_no_choices(tmp_path: Path) -> Path:
    """Create a zing file without any interactions."""
    zing_dir = tmp_path / ".zing"
    zing_dir.mkdir(exist_ok=True)
    zing_path = zing_dir / "test-project.xml"

    plan = Plan(stages=[
        Stage(label="stage-1", steps=[
            Step(label="step-1", instructions="Do something", files=["src/main.py"], done=False),
        ]),
    ])

    doc = ZingDocument(
        stage="plan",
        content="# Test Project\n\nA test project.",
        plan=plan,
        interactions=None,
        audit=True,
        approved=False,
    )
    write_zing_file(zing_path, doc)
    return zing_path


# ---------------------------------------------------------------------------
# ReviewState tests
# ---------------------------------------------------------------------------


class TestReviewState:
    """Tests for the ReviewState dataclass."""

    def test_default_has_no_modifications(self) -> None:
        state = ReviewState(choice_sets=_make_choice_sets())
        assert state.has_modifications is False

    def test_selecting_recommended_is_not_a_modification(self) -> None:
        state = ReviewState(
            choice_sets=_make_choice_sets(),
            user_selections={0: 0},  # Index 0 is recommended for first choice set
        )
        assert state.has_modifications is False

    def test_selecting_non_recommended_is_a_modification(self) -> None:
        state = ReviewState(
            choice_sets=_make_choice_sets(),
            user_selections={0: 1},  # Index 1 is MongoDB (not recommended)
        )
        assert state.has_modifications is True

    def test_deletion_is_a_modification(self) -> None:
        state = ReviewState(
            choice_sets=_make_choice_sets(),
            user_selections={0: None},  # None means deleted
        )
        assert state.has_modifications is True

    def test_empty_selections_no_modification(self) -> None:
        state = ReviewState(
            choice_sets=_make_choice_sets(),
            user_selections={},
        )
        assert state.has_modifications is False

    def test_multiple_selections_mixed(self) -> None:
        """One recommended, one non-recommended -> has modifications."""
        state = ReviewState(
            choice_sets=_make_choice_sets(),
            user_selections={0: 0, 1: 1},  # 0: recommended, 1: non-recommended
        )
        assert state.has_modifications is True

    def test_all_recommended_no_modification(self) -> None:
        """All selections are recommended -> no modifications."""
        state = ReviewState(
            choice_sets=_make_choice_sets(),
            user_selections={0: 0, 1: 0},  # Both recommended indices
        )
        assert state.has_modifications is False

    def test_decision_event_initially_not_set(self) -> None:
        state = ReviewState(choice_sets=_make_choice_sets())
        assert not state.decision_event.is_set()

    def test_out_of_range_selection_no_modification(self) -> None:
        """Selections referencing out-of-range indices are not modifications."""
        state = ReviewState(
            choice_sets=_make_choice_sets(),
            user_selections={99: 0},  # choice_set index 99 doesn't exist
        )
        assert state.has_modifications is False


# ---------------------------------------------------------------------------
# _compute_changes tests
# ---------------------------------------------------------------------------


class TestComputeChanges:
    """Tests for the change diff computation."""

    def test_no_selections_no_changes(self) -> None:
        changes = _compute_changes(_make_choice_sets(), {})
        assert changes == []

    def test_recommended_selection_no_change(self) -> None:
        changes = _compute_changes(_make_choice_sets(), {0: 0})
        assert changes == []

    def test_non_recommended_selection_produces_change(self) -> None:
        changes = _compute_changes(_make_choice_sets(), {0: 1})
        assert len(changes) == 1
        assert changes[0]["choice_set_message"] == "Which database?"
        assert changes[0]["original_recommended"] == "PostgreSQL"
        assert changes[0]["user_selected"] == "MongoDB"
        assert "deleted" not in changes[0]

    def test_deletion_produces_change(self) -> None:
        changes = _compute_changes(_make_choice_sets(), {0: None})
        assert len(changes) == 1
        assert changes[0]["choice_set_message"] == "Which database?"
        assert changes[0]["original_recommended"] == "PostgreSQL"
        assert changes[0]["deleted"] is True

    def test_multiple_changes(self) -> None:
        changes = _compute_changes(_make_choice_sets(), {0: 1, 1: None})
        assert len(changes) == 2
        # Changes should be sorted by index
        assert changes[0]["choice_set_message"] == "Which database?"
        assert changes[0]["user_selected"] == "MongoDB"
        assert changes[1]["choice_set_message"] == "Which framework?"
        assert changes[1]["deleted"] is True

    def test_out_of_range_index_ignored(self) -> None:
        changes = _compute_changes(_make_choice_sets(), {99: 1})
        assert changes == []

    def test_negative_index_ignored(self) -> None:
        changes = _compute_changes(_make_choice_sets(), {-1: 0})
        assert changes == []

    def test_third_choice_selection(self) -> None:
        """Selecting the third choice (SQLite) in the first set."""
        changes = _compute_changes(_make_choice_sets(), {0: 2})
        assert len(changes) == 1
        assert changes[0]["user_selected"] == "SQLite"

    def test_out_of_range_choice_index_ignored(self) -> None:
        """Out-of-range choice index within a valid set is ignored."""
        changes = _compute_changes(_make_choice_sets(), {0: 99})
        assert changes == []


# ---------------------------------------------------------------------------
# run_plan_review approval tests
# ---------------------------------------------------------------------------


class TestRunPlanReviewApproval:
    """Tests for the approval flow (no modifications)."""

    def test_approval_sets_approved_true(self, tmp_path: Path) -> None:
        """Approve with no changes -> writes approved=True to zing file."""
        zing_path = _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()
        mock_build = AsyncMock()

        def mock_start_server(zing_file_path, review_state, *, port, no_browser):
            # Simulate user approving immediately (no changes)
            review_state.approved = True
            review_state.decision_event.set()
            return MagicMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.plan_review._start_review_server",
                    side_effect=mock_start_server,
                ),
                patch(
                    "zing_ai.orchestrator.commands.plan_review._call_build",
                    mock_build,
                ),
            ):
                await run_plan_review(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        # Verify approved=True in the zing file
        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("approved") == "true"

        # Verify run_build was called
        mock_build.assert_called_once()

    def test_approval_calls_build_with_correct_args(self, tmp_path: Path) -> None:
        """Approved plan calls _call_build with correct arguments."""
        zing_path = _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()
        mock_build = AsyncMock()

        def mock_start_server(zing_file_path, review_state, *, port, no_browser):
            review_state.approved = True
            review_state.decision_event.set()
            return MagicMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.plan_review._start_review_server",
                    side_effect=mock_start_server,
                ),
                patch(
                    "zing_ai.orchestrator.commands.plan_review._call_build",
                    mock_build,
                ),
            ):
                await run_plan_review(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=True,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        call_kwargs = mock_build.call_args.kwargs
        assert call_kwargs["zing_path"] == zing_path
        assert call_kwargs["no_browser"] is True
        assert call_kwargs["skip_permissions"] is True
        assert call_kwargs["config"] is config
        assert call_kwargs["project_root"] == tmp_path

    def test_web_server_started_with_review_state(self, tmp_path: Path) -> None:
        """The web server should be started with ReviewState on app.state."""
        zing_path = _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()

        server_calls: list[dict] = []

        def mock_start_server(zing_file_path, review_state, *, port, no_browser):
            server_calls.append({
                "zing_file_path": zing_file_path,
                "review_state": review_state,
                "port": port,
                "no_browser": no_browser,
            })
            # Approve immediately
            review_state.approved = True
            review_state.decision_event.set()
            return MagicMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.plan_review._start_review_server",
                    side_effect=mock_start_server,
                ),
                patch(
                    "zing_ai.orchestrator.commands.plan_review._call_build",
                    AsyncMock(),
                ),
            ):
                await run_plan_review(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        assert len(server_calls) == 1
        assert server_calls[0]["zing_file_path"] == zing_path
        assert server_calls[0]["port"] == config.port
        assert server_calls[0]["no_browser"] is True
        assert isinstance(server_calls[0]["review_state"], ReviewState)
        # Review state should have the choice sets from the zing file
        assert len(server_calls[0]["review_state"].choice_sets) == 2


# ---------------------------------------------------------------------------
# run_plan_review modification tests
# ---------------------------------------------------------------------------


class TestRunPlanReviewModifications:
    """Tests for the modification flow (user changes choices)."""

    def test_modification_calls_replan(self, tmp_path: Path) -> None:
        """Modifying choices -> calls _call_replan with change diff."""
        _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()
        mock_replan = AsyncMock()

        def mock_start_server(zing_file_path, review_state, *, port, no_browser):
            # Simulate user changing first choice from PostgreSQL to MongoDB
            review_state.user_selections[0] = 1
            review_state.approved = True
            review_state.decision_event.set()
            return MagicMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.plan_review._start_review_server",
                    side_effect=mock_start_server,
                ),
                patch(
                    "zing_ai.orchestrator.commands.plan_review._call_replan",
                    mock_replan,
                ),
            ):
                await run_plan_review(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        mock_replan.assert_called_once()
        call_kwargs = mock_replan.call_args.kwargs
        changes = call_kwargs["replan_changes"]
        assert len(changes) == 1
        assert changes[0]["choice_set_message"] == "Which database?"
        assert changes[0]["original_recommended"] == "PostgreSQL"
        assert changes[0]["user_selected"] == "MongoDB"

    def test_deletion_calls_replan(self, tmp_path: Path) -> None:
        """Deleting a choice set -> calls _call_replan with deleted diff."""
        _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()
        mock_replan = AsyncMock()

        def mock_start_server(zing_file_path, review_state, *, port, no_browser):
            # Simulate user deleting the second choice set
            review_state.user_selections[1] = None
            review_state.approved = True
            review_state.decision_event.set()
            return MagicMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.plan_review._start_review_server",
                    side_effect=mock_start_server,
                ),
                patch(
                    "zing_ai.orchestrator.commands.plan_review._call_replan",
                    mock_replan,
                ),
            ):
                await run_plan_review(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        mock_replan.assert_called_once()
        call_kwargs = mock_replan.call_args.kwargs
        changes = call_kwargs["replan_changes"]
        assert len(changes) == 1
        assert changes[0]["choice_set_message"] == "Which framework?"
        assert changes[0]["deleted"] is True

    def test_modification_does_not_set_approved(self, tmp_path: Path) -> None:
        """Modifying choices should NOT set approved=True in the zing file."""
        zing_path = _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()

        def mock_start_server(zing_file_path, review_state, *, port, no_browser):
            review_state.user_selections[0] = 1
            review_state.approved = True
            review_state.decision_event.set()
            return MagicMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.plan_review._start_review_server",
                    side_effect=mock_start_server,
                ),
                patch(
                    "zing_ai.orchestrator.commands.plan_review._call_replan",
                    AsyncMock(),
                ),
            ):
                await run_plan_review(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        # The zing file should NOT have approved=True (modifications go through replan)
        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("approved") == "false"

    def test_replan_called_with_correct_args(self, tmp_path: Path) -> None:
        """Verify _call_replan receives all expected arguments."""
        zing_path = _make_zing_file_with_choices(tmp_path)
        config = ZingConfig()
        mock_replan = AsyncMock()

        def mock_start_server(zing_file_path, review_state, *, port, no_browser):
            review_state.user_selections[0] = 2  # Select SQLite
            review_state.approved = True
            review_state.decision_event.set()
            return MagicMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.plan_review._start_review_server",
                    side_effect=mock_start_server,
                ),
                patch(
                    "zing_ai.orchestrator.commands.plan_review._call_replan",
                    mock_replan,
                ),
            ):
                await run_plan_review(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=True,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        call_kwargs = mock_replan.call_args.kwargs
        assert call_kwargs["zing_path"] == zing_path
        assert call_kwargs["no_browser"] is True
        assert call_kwargs["skip_permissions"] is True
        assert call_kwargs["config"] is config
        assert call_kwargs["project_root"] == tmp_path
        assert len(call_kwargs["replan_changes"]) == 1
        assert call_kwargs["replan_changes"][0]["user_selected"] == "SQLite"


# ---------------------------------------------------------------------------
# run_plan_review no-choices tests
# ---------------------------------------------------------------------------


class TestRunPlanReviewNoChoices:
    """Tests for when the zing document has no choices."""

    def test_no_choices_auto_approves(self, tmp_path: Path) -> None:
        """When no choices exist, the plan is auto-approved."""
        zing_path = _make_zing_file_no_choices(tmp_path)
        config = ZingConfig()
        mock_build = AsyncMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.plan_review._call_build",
                    mock_build,
                ),
            ):
                await run_plan_review(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        # Verify approved=True in the zing file
        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("approved") == "true"

        # Verify build was called
        mock_build.assert_called_once()

    def test_no_choices_does_not_start_server(self, tmp_path: Path) -> None:
        """When no choices exist, the web server should not be started."""
        _make_zing_file_no_choices(tmp_path)
        config = ZingConfig()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.plan_review._start_review_server",
                    side_effect=AssertionError("Server should not be started"),
                ),
                patch(
                    "zing_ai.orchestrator.commands.plan_review._call_build",
                    AsyncMock(),
                ),
            ):
                await run_plan_review(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        # Should not raise -- server start should never be called
        _run(_test())


# ---------------------------------------------------------------------------
# _start_review_server tests
# ---------------------------------------------------------------------------


class TestStartReviewServer:
    """Tests for the review server startup."""

    def test_attaches_review_state_to_app(self) -> None:
        """Verify that review state is stored on app.state.review."""
        review_state = ReviewState(choice_sets=_make_choice_sets())
        captured_app = None

        def mock_start_server(app, *, port, no_browser):
            nonlocal captured_app
            captured_app = app

        with (
            patch(
                "zing_ai.orchestrator.web.app.create_app",
            ) as mock_create,
            patch(
                "zing_ai.orchestrator.web.app.start_server",
                side_effect=mock_start_server,
            ),
        ):
            mock_app = MagicMock()
            mock_create.return_value = mock_app

            _start_review_server(
                Path("test.xml"),
                review_state,
                port=8741,
                no_browser=True,
            )

            # Wait briefly for the thread to start
            import time
            time.sleep(0.1)

        # The app should have had review state assigned
        assert mock_app.state.review is review_state


# ---------------------------------------------------------------------------
# Route integration tests (routes use app.state.review)
# ---------------------------------------------------------------------------


class TestReviewRouteIntegration:
    """Tests that routes correctly interact with ReviewState via app.state."""

    def _make_app_with_review(self, choice_sets=None):
        """Create a FastAPI app with a ReviewState attached."""
        from zing_ai.orchestrator.web.app import create_app

        app = create_app(zing_file=None)
        if choice_sets is None:
            choice_sets = _make_choice_sets()
        app.state.review = ReviewState(choice_sets=choice_sets)
        return app

    @pytest.mark.anyio()
    async def test_review_page_shows_choices(self) -> None:
        """GET /review should render the choice sets from ReviewState."""
        from httpx import ASGITransport, AsyncClient

        app = self._make_app_with_review()
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.get("/review")
        assert response.status_code == 200
        assert "Which database?" in response.text
        assert "Which framework?" in response.text

    @pytest.mark.anyio()
    async def test_review_update_stores_selection(self) -> None:
        """POST /review/update should store the user's selection."""
        from httpx import ASGITransport, AsyncClient

        app = self._make_app_with_review()
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post(
                "/review/update",
                json={"choice_set_index": 0, "selected_choice_index": 1},
            )
        assert response.status_code == 200
        assert response.json()["ok"] is True
        assert app.state.review.user_selections[0] == 1

    @pytest.mark.anyio()
    async def test_review_update_stores_deletion(self) -> None:
        """POST /review/update with null should store a deletion."""
        from httpx import ASGITransport, AsyncClient

        app = self._make_app_with_review()
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post(
                "/review/update",
                json={"choice_set_index": 1, "selected_choice_index": None},
            )
        assert response.status_code == 200
        assert app.state.review.user_selections[1] is None

    @pytest.mark.anyio()
    async def test_review_approve_sets_event(self) -> None:
        """POST /review/approve should set the decision event."""
        from httpx import ASGITransport, AsyncClient

        app = self._make_app_with_review()
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post("/review/approve")
        assert response.status_code == 200
        assert app.state.review.approved is True
        assert app.state.review.decision_event.is_set()

    @pytest.mark.anyio()
    async def test_review_approve_returns_build_when_no_modifications(self) -> None:
        """POST /review/approve with no changes returns next_stage=build."""
        from httpx import ASGITransport, AsyncClient

        app = self._make_app_with_review()
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post("/review/approve")
        assert response.json()["next_stage"] == "build"

    @pytest.mark.anyio()
    async def test_review_approve_returns_replan_when_modifications(self) -> None:
        """POST /review/approve after modifications returns next_stage=replan."""
        from httpx import ASGITransport, AsyncClient

        app = self._make_app_with_review()
        # Simulate a modification before approving
        app.state.review.user_selections[0] = 1
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post("/review/approve")
        assert response.json()["next_stage"] == "replan"

    @pytest.mark.anyio()
    async def test_review_page_without_review_state(self) -> None:
        """GET /review without ReviewState should render empty choices."""
        from httpx import ASGITransport, AsyncClient

        from zing_ai.orchestrator.web.app import create_app

        app = create_app(zing_file=None)
        # No review state attached
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.get("/review")
        assert response.status_code == 200
        assert "No choices to review" in response.text

    @pytest.mark.anyio()
    async def test_review_update_without_review_state(self) -> None:
        """POST /review/update without ReviewState should still return ok."""
        from httpx import ASGITransport, AsyncClient

        from zing_ai.orchestrator.web.app import create_app

        app = create_app(zing_file=None)
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post(
                "/review/update",
                json={"choice_set_index": 0, "selected_choice_index": 1},
            )
        assert response.status_code == 200
        assert response.json()["ok"] is True

    @pytest.mark.anyio()
    async def test_review_approve_without_review_state(self) -> None:
        """POST /review/approve without ReviewState should still return ok."""
        from httpx import ASGITransport, AsyncClient

        from zing_ai.orchestrator.web.app import create_app

        app = create_app(zing_file=None)
        transport = ASGITransport(app=app)
        async with AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post("/review/approve")
        assert response.status_code == 200
        assert response.json()["ok"] is True
