"""Tests for Pydantic data models."""

from __future__ import annotations

import unittest

from pydantic import TypeAdapter, ValidationError

from zing_ai.server.models import (
    Category,
    Complexity,
    Confidence,
    Finding,
    Severity,
    TriageFinding,
    UserResponse,
    WorkflowStep,
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
        finding = adapter.validate_python({
            "type": "triage",
            "title": "Pick an approach",
            "options": [{"label": "A", "description": "Option A"}],
        })
        assert isinstance(finding, TriageFinding)
        assert finding.category is None
        assert finding.severity is None
        assert finding.confidence is None

    def test_triage_partial_metadata_rejected(self) -> None:
        """Partial metadata (only some of category/severity/confidence) is rejected."""
        with self.assertRaises(ValidationError):
            TriageFinding(type="triage", title="Test", severity="low")  # type: ignore[arg-type]

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


if __name__ == "__main__":
    unittest.main()
