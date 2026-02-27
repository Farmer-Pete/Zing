"""Tests for the orchestrator ``plan-audit`` command."""

from __future__ import annotations

import xml.etree.ElementTree as ET
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from zing_ai.orchestrator.commands.plan_audit import (
    _invoke_reaudit_with_session,
    _invoke_update_with_session,
    run_plan_audit,
)
from zing_ai.orchestrator.config import CallType, ZingConfig
from zing_ai.orchestrator.models import (
    Choice,
    ChoiceSet,
    Interaction,
    Plan,
    Stage,
    Step,
    ZingDocument,
)
from zing_ai.orchestrator.errors import PipelineError
from zing_ai.orchestrator.ui.types import InvestigationResult
from zing_ai.orchestrator.xml_parser import ValidationError, write_zing_file


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _make_zing_file_with_plan(
    tmp_path: Path,
    *,
    plan_session: str = "sess-plan-001",
    audit_session: str | None = None,
    audit: bool = False,
    with_interactions: bool = True,
) -> Path:
    """Create a zing file with an existing plan and session ID."""
    zing_dir = tmp_path / ".zing"
    zing_dir.mkdir(exist_ok=True)
    zing_path = zing_dir / "test-project.xml"

    plan = Plan(stages=[
        Stage(label="stage-1", steps=[
            Step(label="step-1", instructions="Do something", files=["src/main.py"], done=False),
        ]),
    ])

    interaction: Interaction | None = None
    if with_interactions:
        interaction = Interaction(choice_sets=[
            ChoiceSet(
                message="Which database?",
                explanation="We need to choose a database.",
                choices=[
                    Choice(label="PostgreSQL", description="Relational DB", recommended=True),
                    Choice(label="MongoDB", description="Document DB", recommended=False),
                ],
            ),
        ])

    doc = ZingDocument(
        stage="plan",
        content="# Test Project\n\nA test project.",
        plan=plan,
        interactions=interaction,
        audit=audit,
        approved=False,
        plan_session=plan_session,
        audit_session=audit_session,
    )
    write_zing_file(zing_path, doc)
    return zing_path


def _make_investigation_result(
    xml_outputs: list[str],
) -> InvestigationResult:
    """Build a mock return value for ``run_parallel_investigations``."""
    outputs = {f"investigate-{i}": xml for i, xml in enumerate(xml_outputs)}
    statuses = {f"investigate-{i}": "success" for i in range(len(xml_outputs))}
    return InvestigationResult(outputs=outputs, statuses=statuses)


# Sample Claude responses -- audit identification
AUDIT_IDENTIFY_RESPONSE = """\
### Area: Design Fundamentals
Evaluate the architecture for soundness and appropriate scoping.
Files:
- src/models.py
- src/database.py

### Area: Robustness & Safety
Evaluate failure modes, edge cases, and security.
Files:
- src/api/routes.py
- src/api/handlers.py

### Area: Plan Executability
Evaluate whether steps are atomic and independently verifiable.
Files:
- src/ui/components.py
"""

# Sample Claude response -- audit investigation
AUDIT_INVESTIGATE_RESPONSE = """\
Based on my evaluation, here are the key findings:

<zing:interactions>
  <choices message="Should we add input validation to the API layer?">
    <explanation format="markdown">
The API layer currently lacks input validation. This is a significant gap.
    </explanation>
    <choice label="Add validation middleware" description="Add Pydantic validation" recommended="true" />
    <choice label="Keep as-is" description="No validation needed yet" recommended="false" />
  </choices>
</zing:interactions>
"""

# Sample Claude response -- document update (returns updated plan)
AUDIT_UPDATE_RESPONSE = """\
Here is the updated plan incorporating audit findings:

<zing:steps>
  <stage label="Data layer">
    <step label="Set up SQLAlchemy with validation">
      <instructions>Install SQLAlchemy and add input validation middleware.</instructions>
      <files>
src/models.py
src/database.py
src/validation.py
      </files>
    </step>
  </stage>
  <stage label="API layer">
    <step label="Create validated REST endpoints">
      <instructions>Implement the CRUD endpoints with Pydantic validation.</instructions>
      <files>
src/api/routes.py
src/api/handlers.py
      </files>
    </step>
  </stage>
</zing:steps>
"""

# Sample Claude response -- re-audit (updated plan only)
REAUDIT_RESPONSE = """\
Updated plan reflecting the changed audit decisions:

<zing:steps>
  <stage label="Data layer">
    <step label="Set up SQLAlchemy without validation">
      <instructions>Install SQLAlchemy without validation middleware.</instructions>
      <files>
src/models.py
src/database.py
      </files>
    </step>
  </stage>
</zing:steps>
"""

# Sample Claude response -- re-audit with new interactions
REAUDIT_RESPONSE_WITH_NEW_INTERACTIONS = """\
Updated plan with new questions:

<zing:steps>
  <stage label="Data layer">
    <step label="Set up SQLAlchemy">
      <instructions>Install SQLAlchemy.</instructions>
      <files>
src/models.py
      </files>
    </step>
  </stage>
</zing:steps>

<zing:interactions>
  <choices message="Should we add rate limiting?">
    <explanation format="markdown">Since validation was removed, rate limiting becomes more important.</explanation>
    <choice label="Add rate limiting" description="Protect against abuse" recommended="true" />
    <choice label="Skip rate limiting" description="Not needed yet" recommended="false" />
  </choices>
</zing:interactions>
"""

# Three XML investigation outputs matching the three areas in AUDIT_IDENTIFY_RESPONSE.
_THREE_AREA_XML_OUTPUTS = [
    """\
<zing:interactions>
  <choices message="Should we add validation?">
    <explanation format="markdown">Validation is important.</explanation>
    <choice label="Add validation" description="Add it" recommended="true" />
    <choice label="Skip" description="No validation" recommended="false" />
  </choices>
</zing:interactions>
""",
    """\
<zing:interactions>
  <choices message="Should we add error handling?">
    <explanation format="markdown">Error handling is important.</explanation>
    <choice label="Add error handling" description="Add it" recommended="true" />
    <choice label="Skip" description="No error handling" recommended="false" />
  </choices>
</zing:interactions>
""",
    """\
<zing:interactions>
  <choices message="Are steps atomic?">
    <explanation format="markdown">Steps should be atomic.</explanation>
    <choice label="Steps are atomic" description="No changes" recommended="true" />
    <choice label="Split steps" description="Make more granular" recommended="false" />
  </choices>
</zing:interactions>
""",
]

# A single-choice-set XML output used in many tests (repeated 3 times for 3 areas).
_SINGLE_CHOICE_XML = """\
<zing:interactions>
  <choices message="Q?">
    <explanation format="markdown">E.</explanation>
    <choice label="A" description="D" recommended="true" />
    <choice label="B" description="D" recommended="false" />
  </choices>
</zing:interactions>
"""


# ---------------------------------------------------------------------------
# _invoke_update_with_session tests
# ---------------------------------------------------------------------------


class TestInvokeUpdateWithSession:
    """Tests for the document-update helper that returns session ID."""

    def test_returns_plan_and_session_id(self) -> None:
        with patch(
            "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
            return_value=(AUDIT_UPDATE_RESPONSE, "sess-audit-001"),
        ):
            plan, session_id = _invoke_update_with_session(
                "test prompt",
                call_type=CallType.AUDIT,
                config=ZingConfig(),
                skip_permissions=False,
            )

        assert session_id == "sess-audit-001"
        assert len(plan.stages) == 2
        assert plan.stages[0].label == "Data layer"

    def test_retries_on_validation_error(self) -> None:
        with patch(
            "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
            side_effect=[
                ("invalid output", "sess-001"),
                (AUDIT_UPDATE_RESPONSE, "sess-002"),
            ],
        ):
            plan, session_id = _invoke_update_with_session(
                "test prompt",
                call_type=CallType.AUDIT,
                config=ZingConfig(),
                skip_permissions=False,
            )

        assert session_id == "sess-002"
        assert len(plan.stages) == 2

    def test_raises_after_max_retries(self) -> None:
        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                return_value=("always invalid", "sess-001"),
            ),
            pytest.raises(ValidationError),
        ):
            _invoke_update_with_session(
                "test prompt",
                call_type=CallType.AUDIT,
                config=ZingConfig(),
                skip_permissions=False,
                max_retries=2,
            )


# ---------------------------------------------------------------------------
# _invoke_reaudit_with_session tests
# ---------------------------------------------------------------------------


class TestInvokeReauditWithSession:
    """Tests for the re-audit helper."""

    def test_returns_plan_without_new_interactions(self) -> None:
        with patch(
            "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
            return_value=(REAUDIT_RESPONSE, "sess-reaudit-001"),
        ):
            plan, interactions, session_id = _invoke_reaudit_with_session(
                "reaudit prompt",
                call_type=CallType.AUDIT,
                config=ZingConfig(),
                skip_permissions=False,
                resume_session="sess-orig",
            )

        assert session_id == "sess-reaudit-001"
        assert len(plan.stages) == 1
        assert plan.stages[0].steps[0].label == "Set up SQLAlchemy without validation"
        assert interactions is None

    def test_returns_plan_with_new_interactions(self) -> None:
        with patch(
            "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
            return_value=(REAUDIT_RESPONSE_WITH_NEW_INTERACTIONS, "sess-reaudit-002"),
        ):
            plan, interactions, session_id = _invoke_reaudit_with_session(
                "reaudit prompt",
                call_type=CallType.AUDIT,
                config=ZingConfig(),
                skip_permissions=False,
                resume_session="sess-orig",
            )

        assert len(plan.stages) == 1
        assert interactions is not None
        assert len(interactions.choice_sets) == 1
        assert interactions.choice_sets[0].message == "Should we add rate limiting?"

    def test_retries_on_validation_error(self) -> None:
        with patch(
            "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
            side_effect=[
                ("invalid output", "sess-001"),
                (REAUDIT_RESPONSE, "sess-002"),
            ],
        ):
            plan, interactions, session_id = _invoke_reaudit_with_session(
                "reaudit prompt",
                call_type=CallType.AUDIT,
                config=ZingConfig(),
                skip_permissions=False,
                resume_session="sess-orig",
            )

        assert session_id == "sess-002"
        assert len(plan.stages) == 1


# ---------------------------------------------------------------------------
# run_plan_audit first-run tests
# ---------------------------------------------------------------------------


class TestRunPlanAuditFirstRun:
    """Tests for the first-run audit pipeline."""

    def test_full_first_run_pipeline(self, tmp_path: Path) -> None:
        """Happy path: identification -> distillation -> investigation (TUI) -> update -> assembly."""
        zing_path = _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()
        mock_plan_review = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                side_effect=[
                    # Phase 1: Identification
                    (AUDIT_IDENTIFY_RESPONSE, "sess-id-001"),
                    # Phase 4: Document update
                    (AUDIT_UPDATE_RESPONSE, "sess-audit-001"),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_parallel_investigations",
                return_value=_make_investigation_result(_THREE_AREA_XML_OUTPUTS),
            ) as mock_investigations,
            patch(
                "zing_ai.orchestrator.commands.plan_audit.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                mock_plan_review,
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # Verify the zing file was written
        assert zing_path.is_file()

        # Parse and verify the written zing file
        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("stage") == "plan"
        assert root.get("audit") == "true"
        assert root.get("approved") == "false"
        assert root.get("audit-session") == "sess-audit-001"
        # Plan session should be preserved from original file
        assert root.get("plan-session") == "sess-plan-001"

        # Verify plan exists and was updated
        plan_elem = root.find("plan")
        assert plan_elem is not None
        stages = plan_elem.findall("stage")
        assert len(stages) == 2  # From AUDIT_UPDATE_RESPONSE

        # Verify interactions exist -- original (1) + audit investigation (3) = 4
        inter_elem = root.find("interactions")
        assert inter_elem is not None
        choices_elems = inter_elem.findall("choices")
        assert len(choices_elems) == 4

        # Verify plan_review was called
        mock_plan_review.assert_called_once()

        # Verify the parallel investigations were invoked
        mock_investigations.assert_called_once()

    def test_investigation_dispatches_via_tui(self, tmp_path: Path) -> None:
        """Verify that run_parallel_investigations is called with correct entries."""
        zing_path = _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()
        captured_entries: list = []

        def mock_run_parallel(entries, run_fn):
            captured_entries.extend(entries)
            return _make_investigation_result([_SINGLE_CHOICE_XML] * 3)

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                side_effect=[
                    (AUDIT_IDENTIFY_RESPONSE, "sess-id-001"),
                    (AUDIT_UPDATE_RESPONSE, "sess-audit-001"),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_parallel_investigations",
                side_effect=mock_run_parallel,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                MagicMock(),
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # Verify 3 entries were passed (one per area)
        assert len(captured_entries) == 3
        assert captured_entries[0]["label"] == "Investigate: Design Fundamentals"
        assert captured_entries[1]["label"] == "Investigate: Robustness & Safety"
        assert captured_entries[2]["label"] == "Investigate: Plan Executability"

    def test_audit_flag_set_to_true(self, tmp_path: Path) -> None:
        """The audit flag should be set to True in the zing document."""
        zing_path = _make_zing_file_with_plan(tmp_path, audit=False)
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                side_effect=[
                    (AUDIT_IDENTIFY_RESPONSE, "sess-id-001"),
                    (AUDIT_UPDATE_RESPONSE, "sess-audit-001"),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_parallel_investigations",
                return_value=_make_investigation_result([_SINGLE_CHOICE_XML] * 3),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                MagicMock(),
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("audit") == "true"

    def test_audit_session_saved_in_zing_file(self, tmp_path: Path) -> None:
        """The audit session ID from document update should be saved."""
        zing_path = _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                side_effect=[
                    (AUDIT_IDENTIFY_RESPONSE, "sess-id-001"),
                    (AUDIT_UPDATE_RESPONSE, "sess-audit-saved"),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_parallel_investigations",
                return_value=_make_investigation_result([_SINGLE_CHOICE_XML] * 3),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                MagicMock(),
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("audit-session") == "sess-audit-saved"

    def test_plan_session_preserved(self, tmp_path: Path) -> None:
        """The plan session ID should be preserved from the original file."""
        zing_path = _make_zing_file_with_plan(tmp_path, plan_session="sess-plan-original")
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                side_effect=[
                    (AUDIT_IDENTIFY_RESPONSE, "sess-id-001"),
                    (AUDIT_UPDATE_RESPONSE, "sess-audit-001"),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_parallel_investigations",
                return_value=_make_investigation_result([_SINGLE_CHOICE_XML] * 3),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                MagicMock(),
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("plan-session") == "sess-plan-original"

    def test_first_run_without_existing_interactions(self, tmp_path: Path) -> None:
        """First audit run when no prior interactions exist."""
        zing_path = _make_zing_file_with_plan(tmp_path, with_interactions=False)
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                side_effect=[
                    (AUDIT_IDENTIFY_RESPONSE, "sess-id-001"),
                    (AUDIT_UPDATE_RESPONSE, "sess-audit-001"),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_parallel_investigations",
                return_value=_make_investigation_result([_SINGLE_CHOICE_XML] * 3),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                MagicMock(),
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        tree = ET.parse(zing_path)
        root = tree.getroot()

        # Only audit investigation interactions (3 areas, 1 each)
        inter_elem = root.find("interactions")
        assert inter_elem is not None
        choices_elems = inter_elem.findall("choices")
        assert len(choices_elems) == 3

    def test_uses_audit_call_type(self, tmp_path: Path) -> None:
        """All Claude calls should use CallType.AUDIT."""
        zing_path = _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        claude_full_calls: list[dict] = []

        def mock_full(prompt, **kwargs):
            claude_full_calls.append(kwargs)
            if len(claude_full_calls) == 1:
                return (AUDIT_IDENTIFY_RESPONSE, "sess-001")
            return (AUDIT_UPDATE_RESPONSE, "sess-002")

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                side_effect=mock_full,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_parallel_investigations",
                return_value=_make_investigation_result([_SINGLE_CHOICE_XML] * 3),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                MagicMock(),
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # All invoke_claude_full calls should use AUDIT call type
        for call_kwargs in claude_full_calls:
            assert call_kwargs.get("call_type") == CallType.AUDIT


# ---------------------------------------------------------------------------
# run_plan_audit re-audit tests
# ---------------------------------------------------------------------------


class TestRunPlanAuditReaudit:
    """Tests for the re-audit flow."""

    def test_reaudit_with_changed_choices(self, tmp_path: Path) -> None:
        """Re-audit reads audit session, resumes Claude, and writes updated file."""
        zing_path = _make_zing_file_with_plan(
            tmp_path, audit_session="sess-audit-orig-001", audit=True
        )
        config = ZingConfig()
        mock_plan_review = MagicMock()

        changes = [
            {
                "choice_set_message": "Should we add validation?",
                "original_recommended": "Add validation middleware",
                "user_selected": "Keep as-is",
            },
        ]

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                return_value=(REAUDIT_RESPONSE, "sess-reaudit-001"),
            ) as mock_full,
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                mock_plan_review,
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
                reaudit_changes=changes,
            )

            # Verify Claude was called with session resumption
            call_kwargs = mock_full.call_args.kwargs
            assert call_kwargs["resume_session"] == "sess-audit-orig-001"

        # Verify updated zing file
        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("stage") == "plan"
        assert root.get("audit") == "true"
        assert root.get("audit-session") == "sess-reaudit-001"

        # Plan should be updated
        plan_elem = root.find("plan")
        assert plan_elem is not None
        step = plan_elem.find(".//step")
        assert step is not None
        assert "without validation" in step.get("label", "")

        # plan_review should be called
        mock_plan_review.assert_called_once()

    def test_reaudit_merges_new_interactions(self, tmp_path: Path) -> None:
        """When re-audit returns new interactions, they are merged with existing ones."""
        zing_path = _make_zing_file_with_plan(
            tmp_path, audit_session="sess-audit-orig-002", audit=True
        )
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                return_value=(REAUDIT_RESPONSE_WITH_NEW_INTERACTIONS, "sess-reaudit-002"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                MagicMock(),
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
                reaudit_changes=[{
                    "choice_set_message": "Should we add validation?",
                    "original_recommended": "Add validation",
                    "user_selected": "Keep as-is",
                }],
            )

        # Parse the updated zing file
        tree = ET.parse(zing_path)
        root = tree.getroot()

        inter_elem = root.find("interactions")
        assert inter_elem is not None
        choices_elems = inter_elem.findall("choices")
        # Original had 1 choice set + 1 new = 2
        assert len(choices_elems) == 2

    def test_reaudit_preserves_audit_session_id(self, tmp_path: Path) -> None:
        """The audit session ID should be updated to the new one from re-audit."""
        zing_path = _make_zing_file_with_plan(
            tmp_path, audit_session="sess-old-audit", audit=True
        )
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                return_value=(REAUDIT_RESPONSE, "sess-new-audit"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                MagicMock(),
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
                reaudit_changes=[{
                    "choice_set_message": "Q",
                    "original_recommended": "A",
                    "user_selected": "B",
                }],
            )

        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("audit-session") == "sess-new-audit"

    def test_reaudit_without_session_id_warns(self, tmp_path: Path) -> None:
        """If no audit-session is in the file, re-audit should still work (no resume)."""
        # Create a zing file without audit_session
        zing_path = _make_zing_file_with_plan(
            tmp_path, audit_session=None, audit=True
        )
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                return_value=(REAUDIT_RESPONSE, "sess-new"),
            ) as mock_full,
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                MagicMock(),
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
                reaudit_changes=[{
                    "choice_set_message": "Q",
                    "original_recommended": "A",
                    "user_selected": "B",
                }],
            )

            # Should still call with empty resume_session
            call_kwargs = mock_full.call_args.kwargs
            assert call_kwargs["resume_session"] == ""

    def test_reaudit_preserves_plan_session(self, tmp_path: Path) -> None:
        """The plan session ID should be preserved during re-audit."""
        zing_path = _make_zing_file_with_plan(
            tmp_path, plan_session="sess-plan-keep", audit_session="sess-audit-old", audit=True
        )
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                return_value=(REAUDIT_RESPONSE, "sess-audit-new"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                MagicMock(),
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
                reaudit_changes=[{
                    "choice_set_message": "Q",
                    "original_recommended": "A",
                    "user_selected": "B",
                }],
            )

        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("plan-session") == "sess-plan-keep"
        assert root.get("audit-session") == "sess-audit-new"


# ---------------------------------------------------------------------------
# run_plan_audit calls run_plan_review tests
# ---------------------------------------------------------------------------


class TestRunPlanAuditCallsReview:
    """Tests that run_plan_audit always calls run_plan_review at the end."""

    def test_first_run_calls_review(self, tmp_path: Path) -> None:
        zing_path = _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()
        mock_review = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                side_effect=[
                    (AUDIT_IDENTIFY_RESPONSE, "sess-001"),
                    (AUDIT_UPDATE_RESPONSE, "sess-002"),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_parallel_investigations",
                return_value=_make_investigation_result([_SINGLE_CHOICE_XML] * 3),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                mock_review,
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        mock_review.assert_called_once_with(
            zing_file="test-project.xml",
            skip_permissions=False,
            config=config,
            project_root=tmp_path,
        )

    def test_reaudit_calls_review(self, tmp_path: Path) -> None:
        zing_path = _make_zing_file_with_plan(
            tmp_path, audit_session="sess-audit-001", audit=True
        )
        config = ZingConfig()
        mock_review = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                return_value=(REAUDIT_RESPONSE, "sess-001"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
                mock_review,
            ),
        ):
            run_plan_audit(
                zing_file="test-project.xml",
                skip_permissions=True,
                config=config,
                project_root=tmp_path,
                reaudit_changes=[{
                    "choice_set_message": "Q",
                    "original_recommended": "A",
                    "user_selected": "B",
                }],
            )

        mock_review.assert_called_once_with(
            zing_file="test-project.xml",
            skip_permissions=True,
            config=config,
            project_root=tmp_path,
        )


# ---------------------------------------------------------------------------
# run_plan_audit PipelineError tests
# ---------------------------------------------------------------------------


class TestRunPlanAuditPipelineError:
    """Tests that run_plan_audit raises PipelineError on failure paths."""

    def test_file_not_found_raises_pipeline_error(self, tmp_path: Path) -> None:
        """FileNotFoundError from resolve_zing_file should be converted to PipelineError."""
        config = ZingConfig()

        with patch(
            "zing_ai.orchestrator.commands.plan_audit.project.resolve_zing_file",
            side_effect=FileNotFoundError("test-project.xml not found"),
        ):
            with pytest.raises(PipelineError, match="plan-audit") as exc_info:
                run_plan_audit(
                    zing_file="test-project.xml",
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

            assert exc_info.value.stage == "plan-audit"
            assert exc_info.value.__cause__ is not None
            assert isinstance(exc_info.value.__cause__, FileNotFoundError)
