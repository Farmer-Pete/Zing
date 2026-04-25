"""Attention queue logic for the Command Center."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Literal

from zing_ai.server.models import ClaudeCodeSession, Session, SessionState, ZingSession


@dataclass
class AttentionItem:
    """An item requiring user attention in the Command Center."""

    action_type: Literal["findings", "attach", "questions"]
    session_id: str
    ticket_id: str | None
    title: str
    description: str  # "5 findings from build-audit" or question preview
    step_name: str | None
    finding_count: int
    wait_seconds: int  # seconds since step became READY or notification was created
    card_key: str | None  # for linking to the card


def build_attention_queue(sessions: list[Session], now: datetime) -> list[AttentionItem]:
    """Build a list of items requiring user attention, sorted by wait time descending.

    For each ZingSession: if any step is in READY state, creates an AttentionItem.
    Classifies by step name: action_type="questions" if step_name == "plan",
    otherwise action_type="findings".

    For each ClaudeCodeSession: if has pending_question (notification with
    answered_at is None), creates an AttentionItem with action_type="attach".

    Returns items sorted by wait_seconds descending (longest wait first).
    """
    items: list[AttentionItem] = []

    for session in sessions:
        if isinstance(session, ZingSession):
            for step in session.steps:
                if step.state == SessionState.READY:
                    wait_seconds = int((now - step.created_at).total_seconds())
                    action_type: Literal["findings", "attach", "questions"] = (
                        "questions" if step.step_name == "plan" else "findings"
                    )
                    finding_count = len(step.findings)
                    description = f"{finding_count} findings from {step.step_name}"
                    items.append(
                        AttentionItem(
                            action_type=action_type,
                            session_id=session.session_id,
                            ticket_id=session.ticket_id,
                            title=session.title,
                            description=description,
                            step_name=step.step_name,
                            finding_count=finding_count,
                            wait_seconds=wait_seconds,
                            card_key=session.session_id,
                        )
                    )
        elif isinstance(session, ClaudeCodeSession):
            notification = session.pending_question
            if notification is not None:
                wait_seconds = int((now - notification.created_at).total_seconds())
                items.append(
                    AttentionItem(
                        action_type="attach",
                        session_id=session.session_id,
                        ticket_id=session.ticket_id,
                        title=session.title,
                        description=notification.body,
                        step_name=None,
                        finding_count=0,
                        wait_seconds=wait_seconds,
                        card_key=session.session_id,
                    )
                )

    items.sort(key=lambda item: item.wait_seconds, reverse=True)
    return items
