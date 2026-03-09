"""Session management for the Zing batch review server.

Manages review sessions with in-memory caching backed by JSON file persistence.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
from pathlib import Path
from typing import Any

from zing_ai.server.models import (
    Finding,
    ReviewItem,
    ReviewResponse,
    Session,
    SessionState,
    UserResponse,
    WorkflowStep,
)

_LOG_LEVEL = os.environ.get("ZING_LOG_LEVEL", "INFO").upper()
logger = logging.getLogger("zing_ai.server")
logger.setLevel(getattr(logging, _LOG_LEVEL, logging.INFO))

if not logger.handlers:
    _handler = logging.StreamHandler()
    _handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(name)s: %(message)s"))
    logger.addHandler(_handler)

_DEFAULT_DATA_DIR = Path.home() / ".local" / "share" / "zing-ai" / "sessions"
_SAFE_SESSION_ID = re.compile(r"^[a-zA-Z0-9][a-zA-Z0-9_-]*$")


class SessionManager:
    """Manages review sessions with in-memory cache and JSON file persistence."""

    def __init__(self, data_dir: Path | None = None) -> None:
        """Initialize the session manager.

        Args:
            data_dir: Directory for session JSON files. Defaults to
                ~/.local/share/zing-ai/sessions/
        """
        self._data_dir = data_dir or _DEFAULT_DATA_DIR
        self._data_dir.mkdir(parents=True, exist_ok=True)
        self._sessions: dict[str, Session] = {}
        self._events: dict[str, asyncio.Event] = {}
        self._steps_by_id: dict[str, tuple[str, int]] = {}
        self._load_existing_sessions()

    @staticmethod
    def _validate_session_id(session_id: str) -> None:
        """Raise ValueError if session_id contains unsafe characters."""
        if not _SAFE_SESSION_ID.match(session_id):
            msg = (
                f"Invalid session_id {session_id!r}: must contain only "
                "alphanumeric characters, hyphens, and underscores"
            )
            raise ValueError(msg)

    def _event_key(self, session_id: str, step_id: str) -> str:
        """Return the event key for a session + step combination."""
        return f"{session_id}:{step_id}"

    def _session_path(self, session_id: str) -> Path:
        """Return the JSON file path for a session."""
        self._validate_session_id(session_id)
        return self._data_dir / f"{session_id}.json"

    def _persist(self, session: Session) -> None:
        """Write a session to disk as JSON."""
        path = self._session_path(session.session_id)
        path.write_text(session.model_dump_json(indent=2))
        logger.debug("Persisted session %s to %s", session.session_id, path)

    def _load_existing_sessions(self) -> None:
        """Load all existing session JSON files into memory on startup."""
        for path in self._data_dir.glob("*.json"):
            try:
                data = json.loads(path.read_text())
                session = Session.model_validate(data)
                self._sessions[session.session_id] = session
                # Index steps by step_id and create events
                for i, step in enumerate(session.steps):
                    self._steps_by_id[step.step_id] = (session.session_id, i)
                for step in session.steps:
                    key = self._event_key(session.session_id, step.step_id)
                    self._events[key] = asyncio.Event()
                    if step.state == SessionState.COMPLETED:
                        self._events[key].set()
                logger.info(
                    "Loaded session %s from disk (state=%s, steps=%d)",
                    session.session_id,
                    session.state.value,
                    len(session.steps),
                )
            except Exception:
                logger.exception("Failed to load session from %s", path)

    def _update_session_state(self, session: Session) -> None:
        """Update session-level state based on the latest step."""
        if not session.steps:
            session.state = SessionState.PENDING
            return
        latest = session.steps[-1]
        session.state = latest.state

    def create_session(
        self,
        session_id: str,
        title: str,
        zing_file: str | None = None,
    ) -> Session:
        """Create a new review session.

        Args:
            session_id: Unique identifier for the session.
            title: Human-readable title for the session.
            zing_file: Absolute path to the zing file, or None if no zing doc.

        Returns:
            The newly created Session.
        """
        self._validate_session_id(session_id)
        if session_id in self._sessions:
            msg = f"Session already exists: {session_id}"
            raise ValueError(msg)
        if zing_file is not None:
            if not os.path.isabs(zing_file):
                logger.warning("Rejected zing_file (not absolute): %s", zing_file)
                msg = f"zing_file must be an absolute path, got: {zing_file}"
                raise ValueError(msg)
            if not os.path.exists(zing_file):
                logger.warning("Rejected zing_file (does not exist): %s", zing_file)
                msg = f"zing_file path does not exist: {zing_file}"
                raise ValueError(msg)
        session = Session(session_id=session_id, title=title, zing_file=zing_file)
        self._sessions[session_id] = session
        self._persist(session)
        logger.info("Created session %s: %s", session_id, title)
        return session

    def start_step(
        self,
        session_id: str,
        step_name: str,
        expected_agents: int,
    ) -> WorkflowStep:
        """Start a new workflow step within a session.

        The same step_name can be used multiple times (for loops). Each call
        creates a new step with an incrementing sequence number.

        Args:
            session_id: The session to add the step to.
            step_name: Name of the workflow step.
            expected_agents: Number of agents expected to report findings.

        Returns:
            The newly created WorkflowStep.

        Raises:
            KeyError: If the session does not exist.
        """
        session = self._get_session_or_raise(session_id)
        step = WorkflowStep(
            step_name=step_name,
            sequence=len(session.steps),
            expected_agents=expected_agents,
        )
        session.steps.append(step)
        self._steps_by_id[step.step_id] = (session_id, len(session.steps) - 1)
        session.state = SessionState.PENDING
        # Create a fresh event for this step
        key = self._event_key(session_id, step.step_id)
        self._events[key] = asyncio.Event()
        self._persist(session)
        logger.info(
            "Started step '%s' (seq=%d, id=%s) in session %s (expecting %d agents)",
            step_name,
            step.sequence,
            step.step_id,
            session_id,
            expected_agents,
        )
        return step

    def get_latest_step(self, session_id: str, step_name: str) -> WorkflowStep:
        """Return the most recent step with the given name.

        Args:
            session_id: The session to search.
            step_name: The step name to find.

        Returns:
            The most recent WorkflowStep with the given name.

        Raises:
            KeyError: If no step with that name exists in the session.
        """
        session = self._get_session_or_raise(session_id)
        for step in reversed(session.steps):
            if step.step_name == step_name:
                return step
        raise KeyError(f"No step '{step_name}' found in session '{session_id}'")

    def get_step_by_id(self, step_id: str) -> tuple[Session, WorkflowStep]:
        """Return the session and step for a given step_id.

        Args:
            step_id: The UUID of the step.

        Returns:
            Tuple of (Session, WorkflowStep).

        Raises:
            KeyError: If no step with that ID exists.
        """
        if step_id not in self._steps_by_id:
            raise KeyError(f"No step with id '{step_id}' found")
        session_id, step_index = self._steps_by_id[step_id]
        session = self._get_session_or_raise(session_id)
        return session, session.steps[step_index]

    def add_finding(
        self, session_id: str, step_id: str, finding_data: dict[str, Any]
    ) -> Finding:
        """Append a finding to a workflow step.

        Args:
            session_id: The session ID that the step must belong to.
            step_id: The UUID of the workflow step.
            finding_data: Dictionary of finding data (must include 'type' discriminator).

        Returns:
            The validated Finding object.

        Raises:
            KeyError: If no step with that ID exists.
            ValueError: If the step doesn't belong to the session, or is in
                READY or COMPLETED state.
        """
        session, step = self.get_step_by_id(step_id)
        if session.session_id != session_id:
            msg = (
                f"Step '{step_id}' belongs to session '{session.session_id}', "
                f"not '{session_id}'"
            )
            raise ValueError(msg)
        if step.state in (SessionState.READY, SessionState.COMPLETED):
            msg = (
                f"Step '{step.step_name}' (id={step_id}) is in state "
                f"'{step.state.value}' and cannot accept new findings"
            )
            raise ValueError(msg)

        from pydantic import TypeAdapter

        adapter = TypeAdapter(Finding)
        finding = adapter.validate_python(finding_data)
        step.findings.append(finding)
        self._persist(session)
        logger.info(
            "Added %s finding to step '%s' (id=%s, total: %d)",
            finding_data.get("type", "unknown"),
            step.step_name,
            step_id,
            len(step.findings),
        )
        return finding

    def mark_agent_complete(self, session_id: str, step_id: str) -> WorkflowStep:
        """Mark one agent as complete for a workflow step.

        If all expected agents are done, transitions the step state to READY.

        Args:
            session_id: The session ID that the step must belong to.
            step_id: The UUID of the workflow step.

        Returns:
            The updated WorkflowStep.

        Raises:
            KeyError: If no step with that ID exists.
            ValueError: If the step doesn't belong to the session, or is in
                READY or COMPLETED state.
        """
        session, step = self.get_step_by_id(step_id)
        if session.session_id != session_id:
            msg = (
                f"Step '{step_id}' belongs to session '{session.session_id}', "
                f"not '{session_id}'"
            )
            raise ValueError(msg)
        if step.state in (SessionState.READY, SessionState.COMPLETED):
            msg = (
                f"Step '{step.step_name}' (id={step_id}) is in state "
                f"'{step.state.value}' and cannot accept agent-complete signals"
            )
            raise ValueError(msg)
        step.completed_agents += 1
        logger.info(
            "Agent completed for step '%s' (id=%s, %d/%d)",
            step.step_name,
            step_id,
            step.completed_agents,
            step.expected_agents,
        )
        if step.completed_agents >= step.expected_agents:
            step.state = SessionState.READY
            self._update_session_state(session)
            logger.info(
                "Step '%s' (id=%s) is now READY for review",
                step.step_name,
                step_id,
            )
        self._persist(session)
        return step

    def submit_responses(
        self,
        session_id: str,
        step_id: str,
        responses: list[UserResponse],
    ) -> ReviewResponse:
        """Store user responses for a workflow step and mark it as completed.

        Args:
            session_id: The session to submit responses for.
            step_id: The UUID of the workflow step.
            responses: List of user responses, one per finding.

        Returns:
            A ReviewResponse pairing findings with responses.

        Raises:
            KeyError: If the session or step does not exist.
            ValueError: If response count doesn't match finding count.
        """
        session, step = self.get_step_by_id(step_id)
        if session.session_id != session_id:
            msg = (
                f"Step '{step_id}' belongs to session '{_session_from_step.session_id}', "
                f"not '{session_id}'"
            )
            raise ValueError(msg)

        if len(responses) != len(step.findings):
            msg = (
                f"Expected {len(step.findings)} responses but got {len(responses)} "
                f"for session {session_id} step '{step.step_name}' (id={step_id})"
            )
            raise ValueError(msg)

        step.responses = responses
        step.state = SessionState.COMPLETED
        self._update_session_state(session)
        self._persist(session)

        key = self._event_key(session_id, step_id)
        event = self._events.get(key)
        if event:
            event.set()

        items = [
            ReviewItem(finding=finding, response=response)
            for finding, response in zip(step.findings, responses, strict=True)
        ]
        logger.info(
            "Step '%s' (id=%s) in session %s completed with %d responses",
            step.step_name,
            step_id,
            session_id,
            len(responses),
        )
        return ReviewResponse(session_id=session_id, step_name=step.step_name, items=items)

    async def wait_for_review(self, session_id: str, step_id: str) -> ReviewResponse:
        """Block until a workflow step's review is submitted.

        If the step is already completed, returns immediately.

        Args:
            session_id: The session to wait for.
            step_id: The UUID of the workflow step to wait for.

        Returns:
            The ReviewResponse once submitted.

        Raises:
            KeyError: If the session or step does not exist.
        """
        _session, step = self.get_step_by_id(step_id)

        if step.state != SessionState.COMPLETED:
            key = self._event_key(session_id, step_id)
            if key not in self._events:
                self._events[key] = asyncio.Event()
            await self._events[key].wait()
            # Re-fetch after await in case step was updated
            _session, step = self.get_step_by_id(step_id)

        responses = step.responses or []
        items = [
            ReviewItem(finding=finding, response=response)
            for finding, response in zip(step.findings, responses, strict=True)
        ]
        return ReviewResponse(session_id=session_id, step_name=step.step_name, items=items)

    def get_session(self, session_id: str) -> Session | None:
        """Return a session by ID, or None if not found."""
        return self._sessions.get(session_id)

    def list_sessions(self) -> list[Session]:
        """Return all sessions with metadata."""
        return list(self._sessions.values())

    def cleanup_session(self, session_id: str) -> None:
        """Remove a session from memory and disk.

        Args:
            session_id: The session to remove.
        """
        session = self._sessions.pop(session_id, None)
        if session:
            for step in session.steps:
                self._steps_by_id.pop(step.step_id, None)
                self._events.pop(self._event_key(session_id, step.step_id), None)
        path = self._session_path(session_id)
        if path.exists():
            path.unlink()
        logger.info("Cleaned up session %s", session_id)

    def _get_session_or_raise(self, session_id: str) -> Session:
        """Return a session or raise KeyError."""
        session = self._sessions.get(session_id)
        if session is None:
            raise KeyError(f"Session not found: {session_id}")
        return session
