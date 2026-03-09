"""Session management for the Zing batch review server.

Manages review sessions with in-memory caching backed by JSON file persistence.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
from pathlib import Path
from typing import Any

from zing_ai.server.models import (
    Finding,
    ReviewItem,
    ReviewResponse,
    Session,
    SessionState,
    UserResponse,
)

_LOG_LEVEL = os.environ.get("ZING_LOG_LEVEL", "INFO").upper()
logger = logging.getLogger("zing_ai.server")
logger.setLevel(getattr(logging, _LOG_LEVEL, logging.INFO))

if not logger.handlers:
    _handler = logging.StreamHandler()
    _handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(name)s: %(message)s"))
    logger.addHandler(_handler)

_DEFAULT_DATA_DIR = Path.home() / ".local" / "share" / "zing-ai" / "sessions"


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
        self._load_existing_sessions()

    def _session_path(self, session_id: str) -> Path:
        """Return the JSON file path for a session."""
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
                self._events[session.session_id] = asyncio.Event()
                if session.state == SessionState.COMPLETED:
                    self._events[session.session_id].set()
                logger.info("Loaded session %s from disk (state=%s)", session.session_id, session.state.value)
            except Exception:
                logger.exception("Failed to load session from %s", path)

    def create_session(
        self,
        session_id: str,
        title: str,
        zing_file: str,
        expected_agents: int,
    ) -> Session:
        """Create a new review session.

        Args:
            session_id: Unique identifier for the session.
            title: Human-readable title for the session.
            zing_file: Path to the zing file being reviewed.
            expected_agents: Number of agents expected to report findings.

        Returns:
            The newly created Session.
        """
        session = Session(
            session_id=session_id,
            title=title,
            zing_file=zing_file,
            expected_agents=expected_agents,
        )
        self._sessions[session_id] = session
        self._events[session_id] = asyncio.Event()
        self._persist(session)
        logger.info("Created session %s: %s (expecting %d agents)", session_id, title, expected_agents)
        return session

    def add_finding(self, session_id: str, finding_data: dict[str, Any]) -> Finding:
        """Append a finding to a session's findings list.

        Args:
            session_id: The session to add the finding to.
            finding_data: Dictionary of finding data (must include 'type' discriminator).

        Returns:
            The validated Finding object.

        Raises:
            KeyError: If the session does not exist.
        """
        session = self._get_session_or_raise(session_id)
        from pydantic import TypeAdapter

        adapter = TypeAdapter(Finding)
        finding = adapter.validate_python(finding_data)
        session.findings.append(finding)
        self._persist(session)
        logger.info("Added %s finding to session %s (total: %d)", finding_data.get("type", "unknown"), session_id, len(session.findings))
        return finding

    def mark_agent_complete(self, session_id: str) -> Session:
        """Mark one agent as complete for a session.

        If all expected agents are done, transitions state to READY.

        Args:
            session_id: The session to update.

        Returns:
            The updated Session.

        Raises:
            KeyError: If the session does not exist.
        """
        session = self._get_session_or_raise(session_id)
        session.completed_agents += 1
        logger.info(
            "Agent completed for session %s (%d/%d)",
            session_id,
            session.completed_agents,
            session.expected_agents,
        )
        if session.completed_agents >= session.expected_agents:
            session.state = SessionState.READY
            logger.info("Session %s is now READY for review", session_id)
        self._persist(session)
        return session

    def submit_responses(
        self,
        session_id: str,
        responses: list[UserResponse],
    ) -> ReviewResponse:
        """Store user responses and mark session as completed.

        Args:
            session_id: The session to submit responses for.
            responses: List of user responses, one per finding.

        Returns:
            A ReviewResponse pairing findings with responses.

        Raises:
            KeyError: If the session does not exist.
        """
        session = self._get_session_or_raise(session_id)
        session.responses = responses
        session.state = SessionState.COMPLETED
        self._persist(session)

        event = self._events.get(session_id)
        if event:
            event.set()

        items = [
            ReviewItem(finding=finding, response=response)
            for finding, response in zip(session.findings, responses, strict=False)
        ]
        logger.info("Session %s completed with %d responses", session_id, len(responses))
        return ReviewResponse(session_id=session_id, items=items)

    async def wait_for_review(self, session_id: str) -> ReviewResponse:
        """Block until a session's review is submitted.

        Args:
            session_id: The session to wait for.

        Returns:
            The ReviewResponse once submitted.

        Raises:
            KeyError: If the session does not exist.
        """
        self._get_session_or_raise(session_id)
        event = self._events.get(session_id)
        if event:
            await event.wait()

        session = self._sessions[session_id]
        responses = session.responses or []
        items = [
            ReviewItem(finding=finding, response=response)
            for finding, response in zip(session.findings, responses, strict=False)
        ]
        return ReviewResponse(session_id=session_id, items=items)

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
        self._sessions.pop(session_id, None)
        self._events.pop(session_id, None)
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
