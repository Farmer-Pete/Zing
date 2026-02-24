"""Tests for the orchestrator ``plan`` command."""

from __future__ import annotations

import xml.etree.ElementTree as ET
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from zing_ai.orchestrator.commands.plan import (
    InvestigationArea,
    _invoke_flesh_out_with_session,
    _invoke_replan_with_session,
    _parse_identification_response,
    run_plan,
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
from zing_ai.orchestrator.xml_parser import ValidationError, write_zing_file


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _make_zing_file(tmp_path: Path, *, stage: str = "new", content: str = "# Test Project\n\nA test project.") -> Path:
    """Create a minimal zing XML file and return its path."""
    zing_dir = tmp_path / ".zing"
    zing_dir.mkdir(exist_ok=True)
    zing_path = zing_dir / "test-project.xml"
    doc = ZingDocument(
        stage=stage,
        content=content,
        plan=None,
        interactions=None,
        audit=False,
        approved=False,
    )
    write_zing_file(zing_path, doc)
    return zing_path


def _make_zing_file_with_plan(
    tmp_path: Path,
    *,
    plan_session: str = "sess-plan-001",
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
        audit=False,
        approved=False,
        plan_session=plan_session,
    )
    write_zing_file(zing_path, doc)
    return zing_path


# Sample Claude responses
IDENTIFY_RESPONSE = """\
### Area: Data model
Investigate the existing data models and their relationships.
Files:
- src/models.py
- src/database.py

### Area: API layer
Investigate the REST API endpoints and routing.
Files:
- src/api/routes.py
- src/api/handlers.py

### Area: Frontend
Investigate the frontend components and state management.
Files:
- src/ui/components.py
"""

INVESTIGATE_RESPONSE = """\
Based on my investigation, here are the key decisions:

<zing:interactions>
  <choices message="Which ORM should we use?">
    <explanation format="markdown">
The project currently uses raw SQL queries. We should adopt an ORM for better maintainability.
    </explanation>
    <choice label="SQLAlchemy" description="Full-featured ORM with great ecosystem" recommended="true" />
    <choice label="Tortoise ORM" description="Async-native ORM" recommended="false" />
  </choices>
</zing:interactions>
"""

FLESH_OUT_RESPONSE = """\
Here is the detailed plan:

<zing:steps>
  <stage label="Data layer">
    <step label="Set up SQLAlchemy">
      <instructions>Install SQLAlchemy and configure the database connection.</instructions>
      <files>
src/models.py
src/database.py
      </files>
    </step>
  </stage>
  <stage label="API layer">
    <step label="Create REST endpoints">
      <instructions>Implement the CRUD endpoints for the data models.</instructions>
      <files>
src/api/routes.py
src/api/handlers.py
      </files>
    </step>
  </stage>
</zing:steps>
"""

REPLAN_RESPONSE = """\
Updated plan reflecting the changed decisions:

<zing:steps>
  <stage label="Data layer">
    <step label="Set up Tortoise ORM">
      <instructions>Install Tortoise ORM and configure the async database connection.</instructions>
      <files>
src/models.py
src/database.py
      </files>
    </step>
  </stage>
</zing:steps>
"""

REPLAN_RESPONSE_WITH_NEW_INTERACTIONS = """\
Updated plan with new questions:

<zing:steps>
  <stage label="Data layer">
    <step label="Set up Tortoise ORM">
      <instructions>Install Tortoise ORM.</instructions>
      <files>
src/models.py
      </files>
    </step>
  </stage>
</zing:steps>

<zing:interactions>
  <choices message="Which migration tool?">
    <explanation format="markdown">Since we switched to Tortoise ORM, we need a compatible migration tool.</explanation>
    <choice label="Aerich" description="Tortoise ORM migration tool" recommended="true" />
    <choice label="Manual" description="Write migrations by hand" recommended="false" />
  </choices>
</zing:interactions>
"""


# ---------------------------------------------------------------------------
# _parse_identification_response tests
# ---------------------------------------------------------------------------


class TestParseIdentificationResponse:
    """Tests for the identification response parser."""

    def test_parses_three_areas(self) -> None:
        areas = _parse_identification_response(IDENTIFY_RESPONSE)
        assert len(areas) == 3

    def test_extracts_area_names(self) -> None:
        areas = _parse_identification_response(IDENTIFY_RESPONSE)
        assert areas[0].name == "Data model"
        assert areas[1].name == "API layer"
        assert areas[2].name == "Frontend"

    def test_extracts_descriptions(self) -> None:
        areas = _parse_identification_response(IDENTIFY_RESPONSE)
        assert "data models" in areas[0].description.lower()
        assert "api endpoints" in areas[1].description.lower()

    def test_extracts_files(self) -> None:
        areas = _parse_identification_response(IDENTIFY_RESPONSE)
        assert areas[0].files == ["src/models.py", "src/database.py"]
        assert areas[1].files == ["src/api/routes.py", "src/api/handlers.py"]
        assert areas[2].files == ["src/ui/components.py"]

    def test_handles_backtick_wrapped_files(self) -> None:
        text = """\
### Area: Test
Some description.
Files:
- `src/main.py`
- `src/utils.py`
"""
        areas = _parse_identification_response(text)
        assert areas[0].files == ["src/main.py", "src/utils.py"]

    def test_raises_on_empty_input(self) -> None:
        with pytest.raises(ValidationError, match="Could not find"):
            _parse_identification_response("")

    def test_raises_on_no_headers(self) -> None:
        with pytest.raises(ValidationError, match="Could not find"):
            _parse_identification_response("Just some text without any area headers.")

    def test_single_area(self) -> None:
        text = """\
### Area: Only one
Just one area to investigate.
Files:
- src/one.py
"""
        areas = _parse_identification_response(text)
        assert len(areas) == 1
        assert areas[0].name == "Only one"

    def test_area_with_no_files(self) -> None:
        text = """\
### Area: No files
An area with no file list.
"""
        areas = _parse_identification_response(text)
        assert len(areas) == 1
        assert areas[0].files == []

    def test_leading_text_before_first_area_is_ignored(self) -> None:
        text = """\
Some intro text that should be ignored.

### Area: Real area
Description here.
Files:
- src/real.py
"""
        areas = _parse_identification_response(text)
        assert len(areas) == 1
        assert areas[0].name == "Real area"

    def test_case_insensitive_headers(self) -> None:
        text = """\
### area: Lowercase
Description.
files:
- src/file.py
"""
        areas = _parse_identification_response(text)
        assert len(areas) == 1
        assert areas[0].name == "Lowercase"

    def test_asterisk_file_list(self) -> None:
        text = """\
### Area: Star list
Description.
Files:
* src/a.py
* src/b.py
"""
        areas = _parse_identification_response(text)
        assert areas[0].files == ["src/a.py", "src/b.py"]


# ---------------------------------------------------------------------------
# _invoke_flesh_out_with_session tests
# ---------------------------------------------------------------------------


class TestInvokeFleshOutWithSession:
    """Tests for the flesh-out helper that returns session ID."""

    def test_returns_plan_and_session_id(self) -> None:
        with patch(
            "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
            return_value=(FLESH_OUT_RESPONSE, "sess-flesh-001"),
        ):
            plan, session_id = _invoke_flesh_out_with_session(
                "test prompt",
                call_type=CallType.PLAN,
                config=ZingConfig(),
                skip_permissions=False,
            )

        assert session_id == "sess-flesh-001"
        assert len(plan.stages) == 2
        assert plan.stages[0].label == "Data layer"

    def test_retries_on_validation_error(self) -> None:
        with patch(
            "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
            side_effect=[
                ("invalid output", "sess-001"),
                (FLESH_OUT_RESPONSE, "sess-002"),
            ],
        ):
            plan, session_id = _invoke_flesh_out_with_session(
                "test prompt",
                call_type=CallType.PLAN,
                config=ZingConfig(),
                skip_permissions=False,
            )

        assert session_id == "sess-002"
        assert len(plan.stages) == 2

    def test_raises_after_max_retries(self) -> None:
        with patch(
            "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
            return_value=("always invalid", "sess-001"),
        ):
            with pytest.raises(ValidationError):
                _invoke_flesh_out_with_session(
                    "test prompt",
                    call_type=CallType.PLAN,
                    config=ZingConfig(),
                    skip_permissions=False,
                    max_retries=2,
                )


# ---------------------------------------------------------------------------
# _invoke_replan_with_session tests
# ---------------------------------------------------------------------------


class TestInvokeReplanWithSession:
    """Tests for the re-plan helper."""

    def test_returns_plan_without_new_interactions(self) -> None:
        with patch(
            "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
            return_value=(REPLAN_RESPONSE, "sess-replan-001"),
        ):
            plan, interactions, session_id = _invoke_replan_with_session(
                "replan prompt",
                call_type=CallType.PLAN,
                config=ZingConfig(),
                skip_permissions=False,
                resume_session="sess-orig",
            )

        assert session_id == "sess-replan-001"
        assert len(plan.stages) == 1
        assert plan.stages[0].steps[0].label == "Set up Tortoise ORM"
        assert interactions is None

    def test_returns_plan_with_new_interactions(self) -> None:
        with patch(
            "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
            return_value=(REPLAN_RESPONSE_WITH_NEW_INTERACTIONS, "sess-replan-002"),
        ):
            plan, interactions, session_id = _invoke_replan_with_session(
                "replan prompt",
                call_type=CallType.PLAN,
                config=ZingConfig(),
                skip_permissions=False,
                resume_session="sess-orig",
            )

        assert len(plan.stages) == 1
        assert interactions is not None
        assert len(interactions.choice_sets) == 1
        assert interactions.choice_sets[0].message == "Which migration tool?"


# ---------------------------------------------------------------------------
# run_plan first-run tests
# ---------------------------------------------------------------------------


class TestRunPlanFirstRun:
    """Tests for the first-run planning pipeline."""

    def test_full_first_run_pipeline(self, tmp_path: Path) -> None:
        """Happy path: identification -> distillation -> investigation -> flesh out -> assembly."""
        zing_path = _make_zing_file(tmp_path)
        config = ZingConfig()
        mock_plan_audit = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                side_effect=[
                    # Phase 1: Identification
                    (IDENTIFY_RESPONSE, "sess-id-001"),
                    # Phase 4: Flesh out
                    (FLESH_OUT_RESPONSE, "sess-flesh-001"),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_validated",
                side_effect=[
                    # Phase 3: Investigation (3 areas from IDENTIFY_RESPONSE)
                    Interaction(choice_sets=[
                        ChoiceSet(
                            message="Which ORM?",
                            explanation="Choose an ORM.",
                            choices=[
                                Choice(label="SQLAlchemy", description="Full ORM", recommended=True),
                                Choice(label="Raw SQL", description="No ORM", recommended=False),
                            ],
                        ),
                    ]),
                    Interaction(choice_sets=[
                        ChoiceSet(
                            message="Which framework?",
                            explanation="Choose a framework.",
                            choices=[
                                Choice(label="FastAPI", description="Modern async", recommended=True),
                                Choice(label="Flask", description="Classic", recommended=False),
                            ],
                        ),
                    ]),
                    Interaction(choice_sets=[
                        ChoiceSet(
                            message="Which UI lib?",
                            explanation="Choose a UI library.",
                            choices=[
                                Choice(label="React", description="Component-based", recommended=True),
                                Choice(label="Vue", description="Progressive", recommended=False),
                            ],
                        ),
                    ]),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
                mock_plan_audit,
            ),
        ):
            run_plan(
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
        assert root.get("audit") == "false"
        assert root.get("approved") == "false"
        assert root.get("plan-session") == "sess-flesh-001"

        # Verify plan exists
        plan_elem = root.find("plan")
        assert plan_elem is not None
        stages = plan_elem.findall("stage")
        assert len(stages) == 2

        # Verify interactions exist
        inter_elem = root.find("interactions")
        assert inter_elem is not None
        choices_elems = inter_elem.findall("choices")
        assert len(choices_elems) == 3  # One from each investigation area

        # Verify plan_audit was called
        mock_plan_audit.assert_called_once()

    def test_investigation_runs_in_parallel(self, tmp_path: Path) -> None:
        """Verify that investigation calls are made concurrently via ThreadPoolExecutor."""
        zing_path = _make_zing_file(tmp_path)
        config = ZingConfig()
        call_order: list[str] = []

        def mock_validate(prompt, validator, retry_prompt_template, **kwargs):
            call_order.append("investigate")
            return Interaction(choice_sets=[
                ChoiceSet(
                    message="Test question?",
                    explanation="Test explanation.",
                    choices=[
                        Choice(label="A", description="Option A", recommended=True),
                        Choice(label="B", description="Option B", recommended=False),
                    ],
                ),
            ])

        with (
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                side_effect=[
                    (IDENTIFY_RESPONSE, "sess-id-001"),
                    (FLESH_OUT_RESPONSE, "sess-flesh-001"),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_validated",
                side_effect=mock_validate,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
                MagicMock(),
            ),
        ):
            run_plan(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # 3 investigation calls (one per area in IDENTIFY_RESPONSE)
        assert call_order.count("investigate") == 3

    def test_distills_unique_files_across_areas(self, tmp_path: Path) -> None:
        """Files mentioned in multiple areas should only be distilled once."""
        zing_path = _make_zing_file(tmp_path)
        config = ZingConfig()

        # Create the files referenced in IDENTIFY_RESPONSE
        for f in ["src/models.py", "src/database.py", "src/api/routes.py",
                   "src/api/handlers.py", "src/ui/components.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        distill_calls: list[list[Path]] = []

        def mock_distill(file_paths, *, project_root):
            distill_calls.append(file_paths)
            return {fp: f"distilled:{fp}" for fp in file_paths}

        with (
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                side_effect=[
                    (IDENTIFY_RESPONSE, "sess-id-001"),
                    (FLESH_OUT_RESPONSE, "sess-flesh-001"),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_validated",
                return_value=Interaction(choice_sets=[
                    ChoiceSet(
                        message="Q?",
                        explanation="E.",
                        choices=[
                            Choice(label="A", description="D", recommended=True),
                            Choice(label="B", description="D", recommended=False),
                        ],
                    ),
                ]),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.distill_files",
                side_effect=mock_distill,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
                MagicMock(),
            ),
        ):
            run_plan(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # distill_files should be called once with all unique files
        assert len(distill_calls) == 1
        distilled_paths = distill_calls[0]
        assert len(distilled_paths) == 5  # 5 unique files

    def test_skip_permissions_forwarded(self, tmp_path: Path) -> None:
        """skip_permissions flag is passed through to Claude calls."""
        zing_path = _make_zing_file(tmp_path)
        config = ZingConfig()

        claude_full_calls: list[dict] = []

        def mock_full(prompt, **kwargs):
            claude_full_calls.append(kwargs)
            if not claude_full_calls[0:1] or len(claude_full_calls) == 1:
                return (IDENTIFY_RESPONSE, "sess-001")
            return (FLESH_OUT_RESPONSE, "sess-002")

        with (
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                side_effect=mock_full,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_validated",
                return_value=Interaction(choice_sets=[
                    ChoiceSet(
                        message="Q?",
                        explanation="E.",
                        choices=[
                            Choice(label="A", description="D", recommended=True),
                            Choice(label="B", description="D", recommended=False),
                        ],
                    ),
                ]),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
                MagicMock(),
            ),
        ):
            run_plan(
                zing_file="test-project.xml",
                skip_permissions=True,
                config=config,
                project_root=tmp_path,
            )

        # All claude.invoke_claude_full calls should have skip_permissions=True
        for call_kwargs in claude_full_calls:
            assert call_kwargs.get("skip_permissions") is True

    def test_plan_session_saved_in_zing_file(self, tmp_path: Path) -> None:
        """The plan session ID from flesh out should be saved."""
        zing_path = _make_zing_file(tmp_path)
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                side_effect=[
                    (IDENTIFY_RESPONSE, "sess-id-001"),
                    (FLESH_OUT_RESPONSE, "sess-flesh-saved"),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_validated",
                return_value=Interaction(choice_sets=[
                    ChoiceSet(
                        message="Q?",
                        explanation="E.",
                        choices=[
                            Choice(label="A", description="D", recommended=True),
                            Choice(label="B", description="D", recommended=False),
                        ],
                    ),
                ]),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
                MagicMock(),
            ),
        ):
            run_plan(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("plan-session") == "sess-flesh-saved"


# ---------------------------------------------------------------------------
# run_plan re-plan tests
# ---------------------------------------------------------------------------


class TestRunPlanReplan:
    """Tests for the re-plan flow."""

    def test_replan_with_changed_choices(self, tmp_path: Path) -> None:
        """Re-plan reads session, resumes Claude, and writes updated file."""
        zing_path = _make_zing_file_with_plan(tmp_path, plan_session="sess-orig-001")
        config = ZingConfig()
        mock_plan_audit = MagicMock()

        changes = [
            {
                "choice_set_message": "Which database?",
                "original_recommended": "PostgreSQL",
                "user_selected": "MongoDB",
            },
        ]

        with (
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                return_value=(REPLAN_RESPONSE, "sess-replan-001"),
            ) as mock_full,
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
                mock_plan_audit,
            ),
        ):
            run_plan(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
                replan_changes=changes,
            )

            # Verify Claude was called with session resumption
            call_kwargs = mock_full.call_args.kwargs
            assert call_kwargs["resume_session"] == "sess-orig-001"

        # Verify updated zing file
        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("stage") == "plan"
        assert root.get("plan-session") == "sess-replan-001"

        # Plan should be updated
        plan_elem = root.find("plan")
        assert plan_elem is not None
        step = plan_elem.find(".//step")
        assert step is not None
        assert "Tortoise ORM" in step.get("label", "")

        # plan_audit should be called
        mock_plan_audit.assert_called_once()

    def test_replan_merges_new_interactions(self, tmp_path: Path) -> None:
        """When re-plan returns new interactions, they are merged with existing ones."""
        zing_path = _make_zing_file_with_plan(tmp_path, plan_session="sess-orig-002")
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                return_value=(REPLAN_RESPONSE_WITH_NEW_INTERACTIONS, "sess-replan-002"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
                MagicMock(),
            ),
        ):
            run_plan(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
                replan_changes=[{
                    "choice_set_message": "Which database?",
                    "original_recommended": "PostgreSQL",
                    "user_selected": "MongoDB",
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

    def test_replan_preserves_session_id(self, tmp_path: Path) -> None:
        """The session ID should be updated to the new one from re-plan."""
        zing_path = _make_zing_file_with_plan(tmp_path, plan_session="sess-old")
        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                return_value=(REPLAN_RESPONSE, "sess-new"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
                MagicMock(),
            ),
        ):
            run_plan(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
                replan_changes=[{
                    "choice_set_message": "Q",
                    "original_recommended": "A",
                    "user_selected": "B",
                }],
            )

        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("plan-session") == "sess-new"

    def test_replan_without_session_id_warns(self, tmp_path: Path) -> None:
        """If no plan-session is in the file, re-plan should still work (no resume)."""
        # Create a zing file without plan_session
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir(exist_ok=True)
        zing_path = zing_dir / "test-project.xml"
        doc = ZingDocument(
            stage="plan",
            content="# Test",
            plan=Plan(stages=[
                Stage(label="s1", steps=[
                    Step(label="step1", instructions="Do it", files=[], done=False),
                ]),
            ]),
            interactions=None,
            audit=False,
            approved=False,
            plan_session=None,  # No session ID
        )
        write_zing_file(zing_path, doc)

        config = ZingConfig()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                return_value=(REPLAN_RESPONSE, "sess-new"),
            ) as mock_full,
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
                MagicMock(),
            ),
        ):
            run_plan(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
                replan_changes=[{
                    "choice_set_message": "Q",
                    "original_recommended": "A",
                    "user_selected": "B",
                }],
            )

            # Should still call with empty resume_session
            call_kwargs = mock_full.call_args.kwargs
            assert call_kwargs["resume_session"] == ""


# ---------------------------------------------------------------------------
# run_plan calls run_plan_audit tests
# ---------------------------------------------------------------------------


class TestRunPlanCallsAudit:
    """Tests that run_plan always calls run_plan_audit at the end."""

    def test_first_run_calls_audit(self, tmp_path: Path) -> None:
        zing_path = _make_zing_file(tmp_path)
        config = ZingConfig()
        mock_audit = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                side_effect=[
                    (IDENTIFY_RESPONSE, "sess-001"),
                    (FLESH_OUT_RESPONSE, "sess-002"),
                ],
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_validated",
                return_value=Interaction(choice_sets=[
                    ChoiceSet(
                        message="Q?",
                        explanation="E.",
                        choices=[
                            Choice(label="A", description="D", recommended=True),
                            Choice(label="B", description="D", recommended=False),
                        ],
                    ),
                ]),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
                mock_audit,
            ),
        ):
            run_plan(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        mock_audit.assert_called_once_with(
            zing_file="test-project.xml",
            skip_permissions=False,
            config=config,
            project_root=tmp_path,
        )

    def test_replan_calls_audit(self, tmp_path: Path) -> None:
        zing_path = _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()
        mock_audit = MagicMock()

        with (
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                return_value=(REPLAN_RESPONSE, "sess-001"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
                mock_audit,
            ),
        ):
            run_plan(
                zing_file="test-project.xml",
                skip_permissions=True,
                config=config,
                project_root=tmp_path,
                replan_changes=[{
                    "choice_set_message": "Q",
                    "original_recommended": "A",
                    "user_selected": "B",
                }],
            )

        mock_audit.assert_called_once_with(
            zing_file="test-project.xml",
            skip_permissions=True,
            config=config,
            project_root=tmp_path,
        )
