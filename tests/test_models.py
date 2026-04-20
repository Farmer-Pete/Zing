"""Tests for Pydantic data models."""

from __future__ import annotations

import unittest

from pydantic import TypeAdapter, ValidationError

from zing_ai.server.models import (
    Category,
    ClaudeCodeSession,
    Complexity,
    Confidence,
    Finding,
    Notification,
    Session,
    SessionState,
    Severity,
    TriageFinding,
    UserResponse,
    WorkflowStep,
    ZingSession,
)


class TestComplexityEnum(unittest.TestCase):
    """Test the Complexity enum values."""

    def test_simple_value(self) -> None:
        assert Complexity.SIMPLE == "simple"

    def test_standard_value(self) -> None:
        assert Complexity.STANDARD == "standard"

    def test_complex_value(self) -> None:
        assert Complexity.COMPLEX == "complex"

    def test_all_values(self) -> None:
        assert set(Complexity) == {Complexity.SIMPLE, Complexity.STANDARD, Complexity.COMPLEX}


class TestTriageFindingComplexity(unittest.TestCase):
    """Test TriageFinding complexity field behavior."""

    def _make_triage(self, **overrides: object) -> TriageFinding:
        defaults: dict[str, object] = {
            "title": "Test finding",
            "body": "Details",
            "category": Category.CORRECTNESS,
            "severity": Severity.HIGH,
            "confidence": Confidence.HIGH,
        }
        defaults.update(overrides)
        return TriageFinding(**defaults)  # type: ignore[arg-type]

    def test_explicit_simple(self) -> None:
        finding = self._make_triage(complexity=Complexity.SIMPLE)
        assert finding.complexity == Complexity.SIMPLE

    def test_explicit_standard(self) -> None:
        finding = self._make_triage(complexity=Complexity.STANDARD)
        assert finding.complexity == Complexity.STANDARD

    def test_explicit_complex(self) -> None:
        finding = self._make_triage(complexity=Complexity.COMPLEX)
        assert finding.complexity == Complexity.COMPLEX

    def test_explicit_string_value(self) -> None:
        """Passing a raw string should coerce to the enum."""
        finding = self._make_triage(complexity="simple")
        assert finding.complexity == Complexity.SIMPLE

    def test_default_is_standard(self) -> None:
        """Omitting complexity should default to STANDARD."""
        finding = self._make_triage()
        assert finding.complexity == Complexity.STANDARD

    def test_serialization_roundtrip(self) -> None:
        finding = self._make_triage(complexity=Complexity.COMPLEX)
        data = finding.model_dump()
        assert data["complexity"] == "complex"
        restored = TriageFinding.model_validate(data)
        assert restored.complexity == Complexity.COMPLEX


class TestFindingTypeAdapterComplexity(unittest.TestCase):
    """Test that TypeAdapter(Finding) handles the complexity field."""

    adapter = TypeAdapter(Finding)

    def test_triage_with_complexity(self) -> None:
        data = {
            "type": "triage",
            "title": "Bug",
            "category": "correctness",
            "severity": "high",
            "confidence": "high",
            "complexity": "simple",
        }
        finding = self.adapter.validate_python(data)
        assert isinstance(finding, TriageFinding)
        assert finding.complexity == Complexity.SIMPLE

    def test_triage_without_complexity_defaults(self) -> None:
        """Backward compatibility: old serialized data without complexity still parses."""
        data = {
            "type": "triage",
            "title": "Bug",
            "category": "correctness",
            "severity": "high",
            "confidence": "high",
        }
        finding = self.adapter.validate_python(data)
        assert isinstance(finding, TriageFinding)
        assert finding.complexity == Complexity.STANDARD

    def test_text_finding_unaffected(self) -> None:
        """Non-triage finding types should still parse without issues."""
        data = {"type": "text", "title": "Question"}
        finding = self.adapter.validate_python(data)
        assert finding.type == "text"

    def test_json_roundtrip(self) -> None:
        data = {
            "type": "triage",
            "title": "Issue",
            "category": "security",
            "severity": "critical",
            "confidence": "medium",
            "complexity": "complex",
        }
        finding = self.adapter.validate_python(data)
        json_bytes = self.adapter.dump_json(finding)
        restored = self.adapter.validate_json(json_bytes)
        assert isinstance(restored, TriageFinding)
        assert restored.complexity == Complexity.COMPLEX


class TestUserResponseComplexity(unittest.TestCase):
    """Test UserResponse complexity field."""

    def test_default_is_none(self) -> None:
        resp = UserResponse()
        assert resp.complexity is None

    def test_explicit_complexity(self) -> None:
        resp = UserResponse(complexity=Complexity.SIMPLE)
        assert resp.complexity == Complexity.SIMPLE

    def test_string_coercion(self) -> None:
        resp = UserResponse(complexity="complex")  # type: ignore[arg-type]
        assert resp.complexity == Complexity.COMPLEX


class TestTriageMetadataValidation(unittest.TestCase):
    """Test TriageFinding optional metadata and validation."""

    def test_triage_without_metadata_defaults(self) -> None:
        """TriageFinding without category/severity/confidence defaults all to None."""
        adapter = TypeAdapter(Finding)
        finding = adapter.validate_python(
            {
                "type": "triage",
                "title": "Pick an approach",
                "options": [{"label": "A", "description": "Option A"}],
            }
        )
        assert isinstance(finding, TriageFinding)
        assert finding.category is None
        assert finding.severity is None
        assert finding.confidence is None

    def test_triage_partial_metadata_rejected(self) -> None:
        """Partial metadata (only some of category/severity/confidence) is rejected."""
        # One of three set
        with self.assertRaises(ValidationError):
            TriageFinding(type="triage", title="Test", severity="low")  # type: ignore[arg-type]
        # Two of three set (severity + category, missing confidence)
        with self.assertRaises(ValidationError):
            TriageFinding(type="triage", title="Test", severity="low", category="correctness")  # type: ignore[arg-type]
        # Two of three set (severity + confidence, missing category)
        with self.assertRaises(ValidationError):
            TriageFinding(type="triage", title="Test", severity="low", confidence="high")  # type: ignore[arg-type]

    def test_choice_migration_shim(self) -> None:
        """Persisted type:'choice' findings are migrated to type:'triage'."""
        step_data = {
            "step_name": "review",
            "sequence": 0,
            "findings": [
                {
                    "type": "choice",
                    "title": "Pick one",
                    "body": "Some body",
                    "context": "Referenced in plan section 3.2",
                    "options": [
                        {"label": "A", "description": "Option A"},
                        {"label": "B", "description": "Option B"},
                    ],
                }
            ],
        }
        step = WorkflowStep.model_validate(step_data)
        assert len(step.findings) == 1
        finding = step.findings[0]
        assert isinstance(finding, TriageFinding)
        assert finding.type == "triage"
        assert "Referenced in plan section 3.2" in finding.body
        assert "Some body" in finding.body
        assert len(finding.options) == 2  # type: ignore[arg-type]

    def test_choice_migration_shim_context_only(self) -> None:
        """Legacy choice finding with context but no body uses context as body."""
        step_data = {
            "step_name": "review",
            "sequence": 0,
            "findings": [
                {
                    "type": "choice",
                    "title": "Pick one",
                    "context": "Some context",
                    "options": [
                        {"label": "A", "description": "Option A"},
                        {"label": "B", "description": "Option B"},
                    ],
                }
            ],
        }
        step = WorkflowStep.model_validate(step_data)
        finding = step.findings[0]
        assert isinstance(finding, TriageFinding)
        assert finding.body == "Some context"

    def test_choice_migration_shim_body_only(self) -> None:
        """Legacy choice finding with body but no context preserves body."""
        step_data = {
            "step_name": "review",
            "sequence": 0,
            "findings": [
                {
                    "type": "choice",
                    "title": "Pick one",
                    "body": "Just a body",
                    "options": [
                        {"label": "A", "description": "Option A"},
                        {"label": "B", "description": "Option B"},
                    ],
                }
            ],
        }
        step = WorkflowStep.model_validate(step_data)
        finding = step.findings[0]
        assert isinstance(finding, TriageFinding)
        assert finding.body == "Just a body"

    def test_choice_migration_shim_no_body_no_context(self) -> None:
        """Legacy choice finding with neither body nor context gets empty body."""
        step_data = {
            "step_name": "review",
            "sequence": 0,
            "findings": [
                {
                    "type": "choice",
                    "title": "Pick one",
                    "options": [
                        {"label": "A", "description": "Option A"},
                        {"label": "B", "description": "Option B"},
                    ],
                }
            ],
        }
        step = WorkflowStep.model_validate(step_data)
        finding = step.findings[0]
        assert isinstance(finding, TriageFinding)
        assert finding.body == ""


class TestNotificationModel(unittest.TestCase):
    """Test Notification model creation, defaults, and serialization."""

    def test_required_title(self) -> None:
        """Notification requires a title."""
        n = Notification(title="Hello")
        assert n.title == "Hello"

    def test_id_auto_generated(self) -> None:
        """id is auto-generated as an 8-char hex string."""
        n = Notification(title="Test")
        assert isinstance(n.id, str)
        assert len(n.id) == 8
        # Should be valid hex
        int(n.id, 16)

    def test_id_unique(self) -> None:
        """Two notifications get different ids."""
        n1 = Notification(title="A")
        n2 = Notification(title="B")
        assert n1.id != n2.id

    def test_body_defaults_to_empty(self) -> None:
        """body defaults to empty string."""
        n = Notification(title="Test")
        assert n.body == ""

    def test_url_defaults_to_none(self) -> None:
        """url defaults to None."""
        n = Notification(title="Test")
        assert n.url is None

    def test_created_at_auto_set(self) -> None:
        """created_at is auto-set to a datetime."""
        from datetime import datetime

        n = Notification(title="Test")
        assert isinstance(n.created_at, datetime)

    def test_explicit_values(self) -> None:
        """Explicit body and url are preserved."""
        n = Notification(title="Alert", body="Details here", url="/session/s1")
        assert n.title == "Alert"
        assert n.body == "Details here"
        assert n.url == "/session/s1"

    def test_model_dump(self) -> None:
        """model_dump() serializes all fields."""
        n = Notification(title="Alert", body="Details", url="/s1")
        data = n.model_dump()
        assert data["title"] == "Alert"
        assert data["body"] == "Details"
        assert data["url"] == "/s1"
        assert "id" in data
        assert "created_at" in data

    def test_model_dump_defaults(self) -> None:
        """model_dump() includes default values."""
        n = Notification(title="Test")
        data = n.model_dump()
        assert data["body"] == ""
        assert data["url"] is None


class TestSessionDiscriminatedUnion(unittest.TestCase):
    """Test the Session discriminated union and backward compatibility."""

    _adapter = TypeAdapter(Session)

    def test_old_session_json_loads_as_zing_session(self) -> None:
        """Old session JSON without session_type defaults to ZingSession."""
        data = {
            "session_id": "legacy-session-abc123",
            "title": "Legacy session",
            "ticket_id": "ENG-42",
            "steps": [],
            "notifications": [],
        }
        session = self._adapter.validate_python(data)
        assert isinstance(session, ZingSession)
        assert session.session_type == "zing"
        assert session.session_id == "legacy-session-abc123"
        assert session.ticket_id == "ENG-42"

    def test_zing_session_explicit_type(self) -> None:
        """ZingSession with explicit session_type='zing' parses correctly."""
        data = {
            "session_id": "zing-session-001",
            "title": "A zing session",
            "session_type": "zing",
        }
        session = self._adapter.validate_python(data)
        assert isinstance(session, ZingSession)
        assert session.session_type == "zing"

    def test_claude_code_session_loads_correctly(self) -> None:
        """ClaudeCodeSession with session_type='claude_code' parses correctly."""
        data = {
            "session_id": "claude-session-001",
            "title": "Claude Code session",
            "session_type": "claude_code",
            "ticket_id": "FRO-99",
            "worktree_path": "/tmp/worktree",
            "skill": "zing:build",
        }
        session = self._adapter.validate_python(data)
        assert isinstance(session, ClaudeCodeSession)
        assert session.session_type == "claude_code"
        assert session.worktree_path == "/tmp/worktree"
        assert session.skill == "zing:build"
        assert session.ticket_id == "FRO-99"

    def test_claude_code_session_defaults(self) -> None:
        """ClaudeCodeSession optional fields default to None."""
        session = ClaudeCodeSession(session_id="s1", title="T")
        assert session.worktree_path is None
        assert session.skill is None
        assert session.ticket_id is None

    def test_session_json_roundtrip_zing(self) -> None:
        """ZingSession round-trips through JSON serialization."""
        original = ZingSession(session_id="zing-rt", title="Roundtrip", ticket_id="ENG-1")
        json_bytes = self._adapter.dump_json(original)
        restored = self._adapter.validate_json(json_bytes)
        assert isinstance(restored, ZingSession)
        assert restored.session_id == "zing-rt"
        assert restored.ticket_id == "ENG-1"
        assert restored.session_type == "zing"

    def test_session_json_roundtrip_claude_code(self) -> None:
        """ClaudeCodeSession round-trips through JSON serialization."""
        original = ClaudeCodeSession(
            session_id="cc-rt",
            title="Claude RT",
            ticket_id="FRO-5",
            worktree_path="/tmp/wt",
            skill="zing:plan",
        )
        json_bytes = self._adapter.dump_json(original)
        restored = self._adapter.validate_json(json_bytes)
        assert isinstance(restored, ClaudeCodeSession)
        assert restored.session_id == "cc-rt"
        assert restored.worktree_path == "/tmp/wt"
        assert restored.skill == "zing:plan"

    def test_zing_session_migration_flat_findings(self) -> None:
        """Old flat-findings format migrates to steps on ZingSession."""
        from datetime import datetime

        data = {
            "session_id": "old-flat",
            "title": "Old format",
            "created_at": datetime.now().isoformat(),
            "findings": [{"type": "text", "title": "A finding"}],
        }
        session = self._adapter.validate_python(data)
        assert isinstance(session, ZingSession)
        assert len(session.steps) == 1
        assert session.steps[0].step_name == "review"


class TestClaudeCodeSessionState(unittest.TestCase):
    """Tests for ClaudeCodeSession.state lifecycle behavior."""

    def test_state_started_when_no_tmux(self) -> None:
        """State is STARTED when tmux_session is None."""
        session = ClaudeCodeSession(session_id="s1", title="Test")
        assert session.state == SessionState.STARTED

    def test_state_stopped_when_tmux_not_alive(self) -> None:
        """State is STOPPED when tmux_session is set but not alive."""
        session = ClaudeCodeSession(session_id="s2", title="Test", tmux_session="zing-fro-123")
        assert session.state == SessionState.STOPPED

    def test_state_started_when_tmux_alive(self) -> None:
        """State is STARTED when tmux_session is set and alive."""
        session = ClaudeCodeSession(session_id="s3", title="Test", tmux_session="zing-fro-123")
        session._tmux_alive = True
        assert session.state == SessionState.STARTED

    def test_tmux_alive_not_in_model_dump(self) -> None:
        """_tmux_alive is a PrivateAttr and does not appear in model_dump."""
        session = ClaudeCodeSession(session_id="s4", title="Test")
        dump = session.model_dump()
        assert "_tmux_alive" not in dump

    def test_tmux_session_in_model_dump(self) -> None:
        """tmux_session appears in model_dump."""
        session = ClaudeCodeSession(session_id="s5", title="Test", tmux_session="zing-x")
        dump = session.model_dump()
        assert dump["tmux_session"] == "zing-x"


if __name__ == "__main__":
    unittest.main()
