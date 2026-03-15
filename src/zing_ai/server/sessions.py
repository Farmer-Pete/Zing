"""Session management for the Zing batch review server.

Manages review sessions with in-memory caching backed by JSON file persistence.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
from collections.abc import Callable
from datetime import datetime
from pathlib import Path
from typing import Any

from pydantic import TypeAdapter

from zing_ai.server.models import (
    Agent,
    AgentState,
    Finding,
    LogEntry,
    Notification,
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
_FINDING_ADAPTER = TypeAdapter(Finding)


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
        self._listeners: list[Callable[[str, str], None]] = []
        self._auto_completed_steps: set[str] = set()
        self._load_existing_sessions()

    def add_listener(self, callback: Callable[[str, str], None]) -> None:
        """Register a callback that fires on session state changes."""
        self._listeners.append(callback)

    def _notify(self, event_type: str, session_id: str) -> None:
        """Notify all registered listeners of a state change."""
        for listener in self._listeners:
            listener(event_type, session_id)

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
        """Write a session to disk as JSON atomically (write-then-rename)."""
        path = self._session_path(session.session_id)
        tmp_path = path.with_suffix(".json.tmp")
        tmp_path.write_text(session.model_dump_json(indent=2), encoding="utf-8")
        tmp_path.replace(path)
        logger.debug("Persisted session %s to %s", session.session_id, path)

    def _load_existing_sessions(self) -> None:
        """Load all existing session JSON files into memory on startup."""
        for path in self._data_dir.glob("*.json"):
            try:
                data = json.loads(path.read_text(encoding="utf-8"))
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
        """Update session-level state based on the highest-priority step state.

        Priority order: COMPLETED > READY > STARTED > PENDING.
        """
        if not session.steps:
            session.state = SessionState.PENDING
            return
        priority = {
            SessionState.PENDING: 0,
            SessionState.STARTED: 1,
            SessionState.READY: 2,
            SessionState.COMPLETED: 3,
        }
        best = max(session.steps, key=lambda s: priority.get(s.state, 0))
        session.state = best.state

    def create_session(
        self,
        session_id: str,
        title: str,
        zing_file: str | None = None,
        steps: list[str] | None = None,
    ) -> Session:
        """Create a new review session.

        Args:
            session_id: Unique identifier for the session.
            title: Human-readable title for the session.
            zing_file: Absolute path to the zing file, or None if no zing doc.
            steps: Optional list of step names to pre-create as PENDING steps.

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
            if not zing_file.endswith(".md"):
                logger.warning("Rejected zing_file (not markdown): %s", zing_file)
                msg = f"zing_file must be a markdown file (.md), got: {zing_file}"
                raise ValueError(msg)
        session = Session(session_id=session_id, title=title, zing_file=zing_file)
        if steps:
            for i, step_name in enumerate(steps):
                step = WorkflowStep(step_name=step_name, sequence=i)
                session.steps.append(step)
                self._steps_by_id[step.step_id] = (session_id, i)
                key = self._event_key(session_id, step.step_id)
                self._events[key] = asyncio.Event()
        self._sessions[session_id] = session
        notif = Notification(title=f"New session: {title}")
        session.notifications.append(notif)
        self._persist(session)
        self._notify("session_created", session_id)
        self._notify(f"notification_added:{notif.id}", session_id)
        logger.info("Created session %s: %s", session_id, title)
        return session

    def update_session(
        self,
        session_id: str,
        zing_file: str | None = None,
        title: str | None = None,
    ) -> Session:
        """Update fields on an existing session.

        Either parameter can be ``None`` to skip updating that field.

        Args:
            session_id: The session to update.
            zing_file: If not None, set the session's zing_file (must be absolute and exist).
            title: If not None, set the session's title.

        Returns:
            The updated Session.
        """
        session = self._get_session_or_raise(session_id)
        if zing_file is not None:
            if not os.path.isabs(zing_file):
                logger.warning("Rejected zing_file (not absolute): %s", zing_file)
                msg = f"zing_file must be an absolute path, got: {zing_file}"
                raise ValueError(msg)
            if not os.path.exists(zing_file):
                logger.warning("Rejected zing_file (does not exist): %s", zing_file)
                msg = f"zing_file path does not exist: {zing_file}"
                raise ValueError(msg)
            if not zing_file.endswith(".md"):
                logger.warning("Rejected zing_file (not markdown): %s", zing_file)
                msg = f"zing_file must be a markdown file (.md), got: {zing_file}"
                raise ValueError(msg)
            session.zing_file = zing_file
        if title is not None:
            session.title = title
        self._persist(session)
        self._notify("session_updated", session_id)
        logger.info("Updated session %s", session_id)
        return session

    def start_step(
        self,
        session_id: str,
        step_id: str,
    ) -> WorkflowStep:
        """Transition an existing workflow step from PENDING to STARTED.

        Args:
            session_id: The session containing the step.
            step_id: The ID of the pre-created step to start.

        Returns:
            The started WorkflowStep.

        Raises:
            KeyError: If the session or step does not exist.
            ValueError: If the step is not in PENDING state.
        """
        self._get_session_or_raise(session_id)
        session, step = self.get_step_by_id(step_id)
        if session.session_id != session_id:
            raise KeyError(f"Step '{step_id}' does not belong to session '{session_id}'")
        if step.state != SessionState.PENDING:
            msg = (
                f"Step '{step_id}' is in state '{step.state.value}', "
                f"expected '{SessionState.PENDING.value}'"
            )
            raise ValueError(msg)
        # Auto-complete any prior steps that are still in-progress
        for prior in session.steps:
            if prior.step_id == step_id:
                break
            if prior.state in (SessionState.STARTED, SessionState.READY):
                prior.state = SessionState.COMPLETED
                self._auto_completed_steps.add(prior.step_id)
                key = self._event_key(session_id, prior.step_id)
                event = self._events.get(key)
                if event:
                    event.set()
                logger.info(
                    "Auto-completed step '%s' (id=%s) — new step starting",
                    prior.step_name,
                    prior.step_id,
                )

        step.state = SessionState.STARTED
        self._update_session_state(session)
        self._persist(session)
        self._notify("step_started", session_id)
        logger.info(
            "Started step '%s' (seq=%d, id=%s) in session %s",
            step.step_name,
            step.sequence,
            step.step_id,
            session_id,
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

        finding = _FINDING_ADAPTER.validate_python(finding_data)

        # Deduplicate by (type, title) — skip if an identical pair already exists
        for existing in step.findings:
            if existing.type == finding.type and existing.title == finding.title:
                logger.info(
                    "Skipped duplicate %s finding '%s' on step '%s' (id=%s)",
                    finding.type,
                    finding.title,
                    step.step_name,
                    step_id,
                )
                return existing

        step.findings.append(finding)
        self._persist(session)
        self._notify("finding_added", session_id)
        logger.info(
            "Added %s finding to step '%s' (id=%s, total: %d)",
            finding_data.get("type", "unknown"),
            step.step_name,
            step_id,
            len(step.findings),
        )
        return finding

    def add_log(
        self, session_id: str, step_id: str, agent_name: str, message: str
    ) -> LogEntry:
        """Append a log entry to a workflow step.

        Args:
            session_id: The session ID that the step must belong to.
            step_id: The UUID of the workflow step.
            agent_name: Name of the agent producing the log.
            message: The log message text.

        Returns:
            The created LogEntry object.

        Raises:
            KeyError: If no step with that ID exists.
            ValueError: If the step doesn't belong to the session.
        """
        session, step = self.get_step_by_id(step_id)
        if session.session_id != session_id:
            msg = (
                f"Step '{step_id}' belongs to session '{session.session_id}', "
                f"not '{session_id}'"
            )
            raise ValueError(msg)

        entry = LogEntry(agent_name=agent_name, message=message)
        step.logs.append(entry)
        self._persist(session)
        self._notify("log_added", session_id)
        logger.info(
            "Added log entry from '%s' to step '%s' (id=%s, total: %d)",
            agent_name,
            step.step_name,
            step_id,
            len(step.logs),
        )
        return entry

    def start_agent(
        self,
        session_id: str,
        step_id: str,
        name: str,
        description: str = "",
    ) -> Agent:
        """Register and start an agent for a workflow step.

        Creates an Agent in RUNNING state and appends it to the step's agent list.

        Args:
            session_id: The session containing the step.
            step_id: The UUID of the workflow step.
            name: Unique name identifying the agent within this step.
            description: Optional description of what the agent does.

        Returns:
            The newly created Agent.

        Raises:
            KeyError: If the session or step does not exist.
            ValueError: If the step doesn't belong to the session.
        """
        session, step = self.get_step_by_id(step_id)
        if session.session_id != session_id:
            msg = (
                f"Step '{step_id}' belongs to session '{session.session_id}', "
                f"not '{session_id}'"
            )
            raise ValueError(msg)
        agent = Agent(name=name, description=description, state=AgentState.RUNNING)
        step.agents.append(agent)
        self._persist(session)
        self._notify("agent_started", session_id)
        logger.info(
            "Started agent '%s' for step '%s' (id=%s) in session %s",
            name,
            step.step_name,
            step_id,
            session_id,
        )
        return agent

    def stop_agent(
        self,
        session_id: str,
        step_id: str,
        name: str,
    ) -> WorkflowStep:
        """Stop a running agent.

        Sets the agent state to COMPLETED and records its completion time.
        The step remains in STARTED state so that the parent process can still
        submit findings via ``add_finding`` after all agents finish.  The step
        transitions to READY later when ``mark_step_ready`` is called.

        Args:
            session_id: The session containing the step.
            step_id: The UUID of the workflow step.
            name: The name of the agent to stop.

        Returns:
            The updated WorkflowStep.

        Raises:
            KeyError: If the session, step, or agent does not exist.
            ValueError: If the step doesn't belong to the session, or the
                agent is already COMPLETED.
        """
        session, step = self.get_step_by_id(step_id)
        if session.session_id != session_id:
            msg = (
                f"Step '{step_id}' belongs to session '{session.session_id}', "
                f"not '{session_id}'"
            )
            raise ValueError(msg)
        agent = None
        for a in step.agents:
            if a.name == name:
                agent = a
                break
        if agent is None:
            raise KeyError(f"No agent named '{name}' found in step '{step_id}'")
        if agent.state == AgentState.COMPLETED:
            raise ValueError(f"Agent '{name}' is already completed in step '{step_id}'")
        agent.state = AgentState.COMPLETED
        agent.completed_at = datetime.now()
        logger.info(
            "Stopped agent '%s' for step '%s' (id=%s) in session %s",
            name,
            step.step_name,
            step_id,
            session_id,
        )
        all_done = all(a.state == AgentState.COMPLETED for a in step.agents)
        if all_done:
            logger.info(
                "All agents completed for step '%s' (id=%s) — "
                "awaiting finding submission before transitioning to READY",
                step.step_name,
                step_id,
            )
        self._persist(session)
        self._notify("agent_stopped", session_id)
        if all_done:
            self._notify("agents_done", session_id)
        return step


    def mark_step_ready(self, session_id: str, step_id: str) -> WorkflowStep:
        """Transition a step from STARTED to READY.

        Called by the parent process after all agent findings have been
        submitted.  This makes the step visible to the review UI.

        Args:
            session_id: The session containing the step.
            step_id: The UUID of the workflow step.

        Returns:
            The updated WorkflowStep.

        Raises:
            KeyError: If the session or step does not exist.
            ValueError: If the step doesn't belong to the session, or is not
                in STARTED state.
        """
        session, step = self.get_step_by_id(step_id)
        if session.session_id != session_id:
            msg = (
                f"Step '{step_id}' belongs to session '{session.session_id}', "
                f"not '{session_id}'"
            )
            raise ValueError(msg)
        if step.state != SessionState.STARTED:
            msg = (
                f"Step '{step.step_name}' (id={step_id}) is in state "
                f"'{step.state.value}', expected 'started'"
            )
            raise ValueError(msg)
        step.state = SessionState.READY
        self._update_session_state(session)
        notif = Notification(title=f"Review ready: {step.step_name}")
        session.notifications.append(notif)
        self._persist(session)
        self._notify("step_ready", session_id)
        self._notify(f"notification_added:{notif.id}", session_id)
        logger.info(
            "Step '%s' (id=%s) transitioned to READY",
            step.step_name,
            step_id,
        )
        return step

    def save_response(
        self,
        session_id: str,
        step_id: str,
        finding_id: str,
        response: UserResponse,
    ) -> None:
        """Auto-save a single response for incremental persistence.

        Finds the finding by ID within the step, lazily initializes
        ``step.responses`` if needed, and stores the response at the
        matching index.  Does **not** set any event or change step state —
        that only happens on submit.

        Args:
            session_id: The session that owns the step.
            step_id: The UUID of the workflow step.
            finding_id: The ``id`` of the finding to save a response for.
            response: The user response to persist.

        Raises:
            KeyError: If no step with that ID exists.
            ValueError: If the step doesn't belong to the session, or no
                finding with the given ID exists in the step.
        """
        session, step = self.get_step_by_id(step_id)
        if session.session_id != session_id:
            msg = (
                f"Step '{step_id}' belongs to session '{session.session_id}', "
                f"not '{session_id}'"
            )
            raise ValueError(msg)

        # Find the index of the matching finding
        finding_index: int | None = None
        for i, finding in enumerate(step.findings):
            if finding.id == finding_id:
                finding_index = i
                break
        if finding_index is None:
            msg = (
                f"No finding with id '{finding_id}' in step '{step.step_name}' "
                f"(id={step_id})"
            )
            raise ValueError(msg)

        # Lazily initialize responses list with empty UserResponse objects
        if step.responses is None:
            step.responses = [UserResponse() for _ in step.findings]
        # Extend if findings were added since responses was initialized
        while len(step.responses) < len(step.findings):
            step.responses.append(UserResponse())

        step.responses[finding_index] = response
        self._persist(session)
        logger.debug(
            "Auto-saved response for finding '%s' in step '%s' (id=%s)",
            finding_id,
            step.step_name,
            step_id,
        )

    def submit_responses(
        self,
        session_id: str,
        step_id: str,
        responses: list[UserResponse],
    ) -> ReviewResponse:
        """Submit final user responses for a workflow step and mark it as completed.

        The submitted responses list is the final version and overwrites any
        responses that were previously auto-saved via ``save_response``.
        The step transitions to COMPLETED and the wait event is set so that
        any ``wait_for_review`` caller is unblocked.

        Args:
            session_id: The session to submit responses for.
            step_id: The UUID of the workflow step.
            responses: Final list of user responses, one per finding.

        Returns:
            A ReviewResponse pairing findings with responses.

        Raises:
            KeyError: If the session or step does not exist.
            ValueError: If the step doesn't belong to the session, or
                response count doesn't match finding count.
        """
        session, step = self.get_step_by_id(step_id)
        if session.session_id != session_id:
            msg = (
                f"Step '{step_id}' belongs to session '{session.session_id}', "
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
        self._notify("review_submitted", session_id)

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

    def _build_review_response(
        self,
        session_id: str,
        step: WorkflowStep,
        auto_completed: bool = False,
    ) -> ReviewResponse:
        """Build a ReviewResponse from a step's findings and responses."""
        responses = step.responses or [UserResponse() for _ in step.findings]
        # Pad responses if findings were added after responses was initialized
        while len(responses) < len(step.findings):
            responses.append(UserResponse())
        items = [
            ReviewItem(finding=finding, response=response)
            for finding, response in zip(step.findings, responses, strict=True)
        ]
        return ReviewResponse(
            session_id=session_id,
            step_name=step.step_name,
            items=items,
            auto_completed=auto_completed,
        )

    async def wait_for_review(self, session_id: str, step_id: str) -> ReviewResponse:
        """Transition a step to READY and block until the review is submitted.

        If the step is already COMPLETED (review previously submitted), returns
        the existing responses immediately without waiting.

        If the step is in STARTED state, it is first transitioned to READY so
        the review UI can show findings and accept user input.

        Args:
            session_id: The session to wait for.
            step_id: The UUID of the workflow step to wait for.

        Returns:
            The ReviewResponse once submitted.

        Raises:
            KeyError: If the session or step does not exist, or was cleaned up
                while waiting.
        """
        _session, step = self.get_step_by_id(step_id)

        # If a review was already submitted, return it immediately
        if step.state == SessionState.COMPLETED:
            return self._build_review_response(session_id, step)

        # Transition to READY if still in STARTED (findings have been submitted)
        if step.state == SessionState.STARTED:
            self.mark_step_ready(session_id, step_id)
            _session, step = self.get_step_by_id(step_id)

        # Wait for user to submit the review
        key = self._event_key(session_id, step_id)
        if key not in self._events:
            self._events[key] = asyncio.Event()
        await self._events[key].wait()
        # Re-fetch after await — session may have been cleaned up while waiting
        try:
            _session, step = self.get_step_by_id(step_id)
        except KeyError:
            msg = f"Session was cleaned up while waiting for review (step_id={step_id!r})"
            raise KeyError(msg) from None
        was_auto_completed = step_id in self._auto_completed_steps
        self._auto_completed_steps.discard(step_id)

        return self._build_review_response(
            session_id, step, auto_completed=was_auto_completed,
        )

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
                key = self._event_key(session_id, step.step_id)
                event = self._events.pop(key, None)
                if event:
                    event.set()  # unblock any wait_for_review callers
        path = self._session_path(session_id)
        if path.exists():
            path.unlink()
        self._notify("session_cleaned_up", session_id)
        logger.info("Cleaned up session %s", session_id)

    def _get_session_or_raise(self, session_id: str) -> Session:
        """Return a session or raise KeyError."""
        session = self._sessions.get(session_id)
        if session is None:
            raise KeyError(f"Session not found: {session_id}")
        return session

    def add_notification(
        self, session_id: str, title: str, body: str = "", url: str | None = None
    ) -> Notification:
        """Create a notification, append it to the session, persist, and notify."""
        session = self._get_session_or_raise(session_id)
        notification = Notification(title=title, body=body, url=url)
        session.notifications.append(notification)
        self._persist(session)
        self._notify(f"notification_added:{notification.id}", session_id)
        return notification
