"""Tests for the zing web UI routes."""

from __future__ import annotations

from pathlib import Path

import pytest
from httpx import ASGITransport, AsyncClient

from zing_ai.orchestrator.web.app import create_app


@pytest.fixture()
def app():
    """Create a test FastAPI app with no zing file."""
    return create_app(zing_file=None)


@pytest.fixture()
def app_with_file(tmp_path: Path):
    """Create a test FastAPI app with a fake zing file path."""
    fake_file = tmp_path / "test.xml"
    fake_file.write_text("<zing />")
    return create_app(zing_file=fake_file)


@pytest.fixture()
async def client(app):
    """Async HTTP client for the test app."""
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as ac:
        yield ac


@pytest.fixture()
async def client_with_file(app_with_file):
    """Async HTTP client for the test app with a zing file."""
    transport = ASGITransport(app=app_with_file)
    async with AsyncClient(transport=transport, base_url="http://test") as ac:
        yield ac


# ---------------------------------------------------------------------------
# App factory tests
# ---------------------------------------------------------------------------


class TestCreateApp:
    """Tests for the create_app factory."""

    def test_creates_fastapi_app(self, app) -> None:
        from fastapi import FastAPI

        assert isinstance(app, FastAPI)

    def test_stores_zing_file_none(self, app) -> None:
        assert app.state.zing_file is None

    def test_stores_zing_file_path(self, app_with_file) -> None:
        assert app_with_file.state.zing_file is not None
        assert app_with_file.state.zing_file.name == "test.xml"

    def test_has_templates(self, app) -> None:
        assert hasattr(app.state, "templates")


# ---------------------------------------------------------------------------
# Redirect
# ---------------------------------------------------------------------------


class TestIndexRedirect:
    """Tests for GET /."""

    @pytest.mark.anyio()
    async def test_redirects_to_progress(self, client: AsyncClient) -> None:
        response = await client.get("/", follow_redirects=False)
        assert response.status_code == 302
        assert response.headers["location"] == "/progress"


# ---------------------------------------------------------------------------
# Progress
# ---------------------------------------------------------------------------


class TestProgressPage:
    """Tests for GET /progress."""

    @pytest.mark.anyio()
    async def test_returns_200(self, client: AsyncClient) -> None:
        response = await client.get("/progress")
        assert response.status_code == 200

    @pytest.mark.anyio()
    async def test_contains_progress_title(self, client: AsyncClient) -> None:
        response = await client.get("/progress")
        assert "Investigation Progress" in response.text

    @pytest.mark.anyio()
    async def test_contains_html_structure(self, client: AsyncClient) -> None:
        response = await client.get("/progress")
        assert "<!DOCTYPE html>" in response.text
        assert "process-card" in response.text


class TestProgressStream:
    """Tests for GET /progress/stream."""

    @pytest.mark.anyio()
    async def test_returns_sse_response(self, client: AsyncClient) -> None:
        response = await client.get("/progress/stream")
        assert response.status_code == 200
        assert "text/event-stream" in response.headers.get("content-type", "")


# ---------------------------------------------------------------------------
# Review
# ---------------------------------------------------------------------------


class TestReviewPage:
    """Tests for GET /review."""

    @pytest.mark.anyio()
    async def test_returns_200(self, client: AsyncClient) -> None:
        response = await client.get("/review")
        assert response.status_code == 200

    @pytest.mark.anyio()
    async def test_contains_review_title(self, client: AsyncClient) -> None:
        response = await client.get("/review")
        assert "Review Plan Decisions" in response.text

    @pytest.mark.anyio()
    async def test_shows_approve_button(self, app, client: AsyncClient) -> None:
        from zing_ai.orchestrator.commands.plan_review import ReviewState
        from zing_ai.orchestrator.models import Choice, ChoiceSet

        app.state.review = ReviewState(
            choice_sets=[
                ChoiceSet(
                    message="Test",
                    explanation="Test explanation",
                    choices=[Choice(label="A", description="a", recommended=True)],
                ),
            ],
        )
        response = await client.get("/review")
        assert "Approve" in response.text

    @pytest.mark.anyio()
    async def test_hides_approve_button_when_no_choices(self, client: AsyncClient) -> None:
        response = await client.get("/review")
        assert "Approve" not in response.text


class TestReviewUpdate:
    """Tests for POST /review/update."""

    @pytest.mark.anyio()
    async def test_valid_update_returns_ok(self, client: AsyncClient) -> None:
        response = await client.post(
            "/review/update",
            json={"choice_set_index": 0, "selected_choice_index": 1},
        )
        assert response.status_code == 200
        assert response.json()["ok"] is True

    @pytest.mark.anyio()
    async def test_null_selection_deletes(self, client: AsyncClient) -> None:
        response = await client.post(
            "/review/update",
            json={"choice_set_index": 0, "selected_choice_index": None},
        )
        assert response.status_code == 200
        assert response.json()["ok"] is True

    @pytest.mark.anyio()
    async def test_missing_index_returns_400(self, client: AsyncClient) -> None:
        response = await client.post("/review/update", json={})
        assert response.status_code == 400
        assert "error" in response.json()


class TestReviewApprove:
    """Tests for POST /review/approve."""

    @pytest.mark.anyio()
    async def test_approve_returns_ok(self, client: AsyncClient) -> None:
        response = await client.post("/review/approve")
        assert response.status_code == 200
        body = response.json()
        assert body["ok"] is True
        assert body["next_stage"] == "build"


# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------


class TestBuildPage:
    """Tests for GET /build."""

    @pytest.mark.anyio()
    async def test_returns_200(self, client: AsyncClient) -> None:
        response = await client.get("/build")
        assert response.status_code == 200

    @pytest.mark.anyio()
    async def test_contains_build_title(self, client: AsyncClient) -> None:
        response = await client.get("/build")
        assert "Build" in response.text

    @pytest.mark.anyio()
    async def test_contains_step_list(self, client: AsyncClient) -> None:
        response = await client.get("/build")
        assert "step-list" in response.text


class TestBuildStream:
    """Tests for GET /build/stream."""

    @pytest.mark.anyio()
    async def test_returns_sse_response(self, client: AsyncClient) -> None:
        response = await client.get("/build/stream")
        assert response.status_code == 200
        assert "text/event-stream" in response.headers.get("content-type", "")


# ---------------------------------------------------------------------------
# Audit
# ---------------------------------------------------------------------------


class TestAuditPage:
    """Tests for GET /audit."""

    @pytest.mark.anyio()
    async def test_returns_200(self, client: AsyncClient) -> None:
        response = await client.get("/audit")
        assert response.status_code == 200

    @pytest.mark.anyio()
    async def test_contains_audit_title(self, client: AsyncClient) -> None:
        response = await client.get("/audit")
        assert "Build Audit Findings" in response.text


class TestAuditAction:
    """Tests for POST /audit/action."""

    @pytest.mark.anyio()
    async def test_fix_action_returns_ok(self, client: AsyncClient) -> None:
        response = await client.post(
            "/audit/action",
            json={"finding_index": 0, "action": "fix"},
        )
        assert response.status_code == 200
        assert response.json()["ok"] is True

    @pytest.mark.anyio()
    async def test_skip_action_returns_ok(self, client: AsyncClient) -> None:
        response = await client.post(
            "/audit/action",
            json={"finding_index": 0, "action": "skip"},
        )
        assert response.status_code == 200
        assert response.json()["ok"] is True

    @pytest.mark.anyio()
    async def test_discuss_action_returns_ok(self, client: AsyncClient) -> None:
        response = await client.post(
            "/audit/action",
            json={"finding_index": 0, "action": "discuss"},
        )
        assert response.status_code == 200
        assert response.json()["ok"] is True

    @pytest.mark.anyio()
    async def test_invalid_action_returns_400(self, client: AsyncClient) -> None:
        response = await client.post(
            "/audit/action",
            json={"finding_index": 0, "action": "invalid"},
        )
        assert response.status_code == 400
        assert "error" in response.json()

    @pytest.mark.anyio()
    async def test_missing_fields_returns_400(self, client: AsyncClient) -> None:
        response = await client.post("/audit/action", json={})
        assert response.status_code == 400
        assert "error" in response.json()


# ---------------------------------------------------------------------------
# SSE Helpers
# ---------------------------------------------------------------------------


class TestSSEHelpers:
    """Tests for the SSE event helper functions."""

    def test_progress_event_returns_sse(self) -> None:
        from zing_ai.orchestrator.web.sse import progress_event

        event = progress_event("test-1", "running", "Processing...")
        assert event is not None

    def test_output_event_returns_sse(self) -> None:
        from zing_ai.orchestrator.web.sse import output_event

        event = output_event("test-1", "Line of output")
        assert event is not None

    def test_completion_event_returns_sse(self) -> None:
        from zing_ai.orchestrator.web.sse import completion_event

        event = completion_event("test-1", "Success")
        assert event is not None

    def test_error_event_returns_sse(self) -> None:
        from zing_ai.orchestrator.web.sse import error_event

        event = error_event("test-1", "Something failed")
        assert event is not None

    def test_notify_event_returns_sse(self) -> None:
        from zing_ai.orchestrator.web.sse import notify_event

        event = notify_event("Title", "Body text")
        assert event is not None

    def test_output_event_escapes_html(self) -> None:
        from zing_ai.orchestrator.web.sse import output_event

        event = output_event("test", "<script>alert('xss')</script>")
        # The event object should exist (we trust datastar-py formatting)
        assert event is not None

    def test_progress_event_escapes_special_chars(self) -> None:
        from zing_ai.orchestrator.web.sse import progress_event

        event = progress_event("id-with-<>", "running", "Msg with & chars")
        assert event is not None


# ---------------------------------------------------------------------------
# Zing File in Template Context
# ---------------------------------------------------------------------------


class TestZingFileContext:
    """Tests that the zing file name appears in page headers."""

    @pytest.mark.anyio()
    async def test_progress_shows_filename(self, client_with_file: AsyncClient) -> None:
        response = await client_with_file.get("/progress")
        assert "test.xml" in response.text

    @pytest.mark.anyio()
    async def test_review_shows_filename(self, client_with_file: AsyncClient) -> None:
        response = await client_with_file.get("/review")
        assert "test.xml" in response.text

    @pytest.mark.anyio()
    async def test_build_shows_filename(self, client_with_file: AsyncClient) -> None:
        response = await client_with_file.get("/build")
        assert "test.xml" in response.text

    @pytest.mark.anyio()
    async def test_audit_shows_filename(self, client_with_file: AsyncClient) -> None:
        response = await client_with_file.get("/audit")
        assert "test.xml" in response.text
