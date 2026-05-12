"""Resolve a session_id to its on-disk plan markdown + viz JSON."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from fastapi import HTTPException

from zing_ai.server.models import ZingSession
from zing_ai.server.sessions import SessionManager


def load_plan_for_session(session_id: str, sm: SessionManager) -> tuple[str, dict[str, Any]]:
    """Return (markdown_text, viz_graph) for the given session.

    Raises HTTPException(404) if the session is unknown, is not a ZingSession,
    has no zing_file, or either the markdown or the .viz.json sibling does
    not exist on disk.

    Trusts the session_update invariant (Q13) that ``sess.zing_file`` is an
    absolute path that exists at the time of write — the existence check here
    is a defensive 404 against deletion after the fact, not a re-validation
    of the boundary contract.
    """
    sess = sm.get_session(session_id)
    if not isinstance(sess, ZingSession) or sess.zing_file is None:
        raise HTTPException(404, f"no plan for session {session_id}")
    md_path = Path(sess.zing_file)
    viz_path = md_path.with_name(md_path.stem + ".viz.json")
    if not md_path.exists():
        raise HTTPException(404, f"plan markdown missing: {md_path}")
    if not viz_path.exists():
        raise HTTPException(404, f"viz JSON missing: {viz_path}")
    return md_path.read_text(), json.loads(viz_path.read_text())
