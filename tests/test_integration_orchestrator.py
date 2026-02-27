"""End-to-end integration tests for the orchestrator pipeline.

These tests exercise the interaction between multiple modules working
together.  External boundaries (Claude CLI subprocess, aid subprocess,
UI rendering) are mocked; everything else runs against real code with
``tmp_path`` for file-system state.

The UI is mocked by patching the Rich-based inline UI functions
(``run_with_progress``, ``run_parallel_investigations``,
``plan_review_menu``, ``audit_triage_menu``) to return pre-defined
result objects without rendering any UI.
"""

from __future__ import annotations

import contextlib
import subprocess
import xml.etree.ElementTree as ET
from pathlib import Path
from unittest.mock import MagicMock, patch

import jinja2
import pytest

from zing_ai.orchestrator.config import CallType, ZingConfig
from zing_ai.orchestrator.distiller import _hash_file, distill_file
from zing_ai.orchestrator.models import (
    AuditGroup,
    Choice,
    ChoiceSet,
    Interaction,
    Plan,
    Stage,
    Step,
    ZingDocument,
)
from zing_ai.orchestrator.ui.types import (
    AuditDecision,
    BuildProgress,
    InvestigationResult,
    ReviewChange,
)
from zing_ai.orchestrator.xml_parser import (
    ValidationError,
    parse_zing_file,
    write_zing_file,
)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _make_config() -> ZingConfig:
    """Build a default ZingConfig for tests."""
    return ZingConfig()


def _write_zing_doc(path: Path, doc: ZingDocument) -> None:
    """Write a ZingDocument to the given path."""
    write_zing_file(path, doc)


def _minimal_plan() -> Plan:
    """Build a minimal valid plan with one stage and one step."""
    return Plan(
        stages=[
            Stage(
                label="Setup",
                steps=[
                    Step(
                        label="Step 1.1",
                        instructions="Create the project structure.",
                        files=["src/main.py", "src/utils.py"],
                    )
                ],
            )
        ]
    )


def _minimal_interaction() -> Interaction:
    """Build a minimal valid Interaction with one choice set."""
    return Interaction(
        choice_sets=[
            ChoiceSet(
                message="Which framework?",
                explanation="We need to pick a web framework.",
                choices=[
                    Choice(label="FastAPI", description="Modern async", recommended=True),
                    Choice(label="Flask", description="Simple and mature", recommended=False),
                ],
            )
        ]
    )


def _make_zing_file(tmp_path: Path, *, stage: str = "new", with_plan: bool = False) -> Path:
    """Create a .zing directory and a sample zing XML file.

    Returns the path to the zing XML file.
    """
    zing_dir = tmp_path / ".zing"
    zing_dir.mkdir(parents=True, exist_ok=True)

    doc = ZingDocument(
        stage=stage,
        content="# Test Project\n\nBuild a todo app.",
        plan=_minimal_plan() if with_plan else None,
        interactions=_minimal_interaction() if with_plan else None,
        audit=False,
        approved=False,
        plan_session="session-123" if with_plan else None,
    )
    zing_path = zing_dir / "test-project.xml"
    write_zing_file(zing_path, doc)
    return zing_path


def _make_investigation_result(
    outputs: dict[str, str] | None = None,
    statuses: dict[str, str] | None = None,
) -> InvestigationResult:
    """Build an InvestigationResult with sensible defaults."""
    return InvestigationResult(
        outputs=outputs or {},
        statuses=statuses or {},
    )


def _make_review_result(
    action: str = "approve",
    changes: list[ReviewChange] | None = None,
) -> tuple[str, list[ReviewChange]]:
    """Build a (action, changes) tuple for plan_review_menu."""
    return (action, changes or [])


def _make_build_progress(
    completed_steps: list[tuple[int, int]] | None = None,
    failed_step: tuple[int, int] | None = None,
) -> BuildProgress:
    """Build a BuildProgress with sensible defaults."""
    return BuildProgress(
        completed_steps=completed_steps or [(0, 0)],
        failed_step=failed_step,
    )


def _make_audit_decisions(
    decisions: list[AuditDecision] | None = None,
) -> list[AuditDecision]:
    """Build a list of AuditDecision with sensible defaults."""
    return decisions or []


# ---------------------------------------------------------------------------
# Pre-built Claude XML responses
# ---------------------------------------------------------------------------


# A valid zing:interactions response from Claude
VALID_INTERACTIONS_RESPONSE = """\
Here are the investigation results:

<zing:interactions>
  <choices message="Which database?">
    <explanation format="markdown">We should pick a database engine.</explanation>
    <choice label="SQLite" description="Embedded, simple" recommended="true" />
    <choice label="PostgreSQL" description="Full-featured" recommended="false" />
  </choices>
</zing:interactions>
"""

# A valid zing:steps response from Claude
VALID_STEPS_RESPONSE = """\
Here is the development plan:

<zing:steps>
  <stage label="Foundation">
    <step label="Step 1.1">
      <instructions>Set up the project skeleton.</instructions>
      <files>src/main.py
src/config.py</files>
    </step>
    <step label="Step 1.2">
      <instructions>Add database models.</instructions>
      <files>src/models.py</files>
    </step>
  </stage>
  <stage label="Features">
    <step label="Step 2.1">
      <instructions>Implement CRUD endpoints.</instructions>
      <files>src/routes.py
src/main.py</files>
    </step>
  </stage>
</zing:steps>
"""

# An invalid (missing stages) zing:steps response
INVALID_STEPS_RESPONSE = """\
Let me think about this...

<zing:steps>
</zing:steps>
"""

# A valid identification response
VALID_IDENTIFICATION_RESPONSE = """\
### Area: Database Layer
Examine the data model and storage approach.
Files:
- `src/models.py`
- `src/db.py`

### Area: API Design
Review the REST API surface.
Files:
- `src/routes.py`
- `src/main.py`
"""

# A valid zing:audit response
VALID_AUDIT_RESPONSE = """\
<zing:audit>
  <group>src/main.py
src/config.py</group>
  <group>src/models.py</group>
</zing:audit>
"""


# ===================================================================
# Test 1: Full Pipeline Flow
# ===================================================================


class TestFullPipelineFlow:
    """Test the full pipeline flow: new -> plan -> plan-audit -> plan-review -> build -> build-audit.

    Each command calls the next, so we mock Claude at the subprocess level
    and mock UI functions to return pre-defined results.
    The chain should complete through all stages.
    """

    def test_new_chains_into_plan(self, tmp_path: Path) -> None:
        """run_new should invoke Claude interactively, find the zing file, then call run_plan."""
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()

        # Pre-create a zing XML file so that run_new can "find" it after Claude exits
        zing_file = zing_dir / "my-todo-app.xml"
        doc = ZingDocument(
            stage="new",
            content="# My Todo App\n\nA simple todo application.",
            plan=None,
            interactions=None,
            audit=False,
            approved=False,
        )
        write_zing_file(zing_file, doc)

        with (
            # Mock the subprocess.run call (Claude interactive session)
            patch(
                "zing_ai.orchestrator.commands.new.subprocess.run",
                return_value=subprocess.CompletedProcess(args=[], returncode=0),
            ) as mock_subprocess,
            # Mock time.time to return 0 so pre-created files pass the mtime filter
            patch(
                "zing_ai.orchestrator.commands.new.time.time",
                return_value=0,
            ),
            # Mock the next stage
            patch(
                "zing_ai.orchestrator.commands.plan.run_plan",
            ) as mock_run_plan,
        ):
            from zing_ai.orchestrator.commands.new import run_new

            run_new(
                zing_file=None,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )

            # subprocess.run should have been called for Claude
            mock_subprocess.assert_called_once()

            # run_plan should have been called from within run_new
            mock_run_plan.assert_called_once()
            call_kwargs = mock_run_plan.call_args.kwargs
            assert call_kwargs["zing_file"] == "my-todo-app.xml"
            assert call_kwargs["project_root"] == tmp_path

    def test_plan_chains_into_plan_audit(self, tmp_path: Path) -> None:
        """run_plan should perform identification/investigation/flesh-out then call run_plan_audit."""
        zing_path = _make_zing_file(tmp_path, stage="new")

        with (
            # Mock identification phase
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                return_value=(VALID_IDENTIFICATION_RESPONSE, "session-id1"),
            ),
            # Mock the investigation phase (returns InvestigationResult + Interactions)
            patch(
                "zing_ai.orchestrator.commands.plan._run_investigation_tui",
                return_value=(
                    _make_investigation_result(),
                    [_minimal_interaction()],
                ),
            ),
            # Mock flesh-out phase
            patch(
                "zing_ai.orchestrator.commands.plan._invoke_flesh_out_with_session",
                return_value=(_minimal_plan(), "session-plan-1"),
            ),
            # Mock distiller
            patch(
                "zing_ai.orchestrator.commands.plan.distill_files",
                return_value={},
            ),
            # Mock aid path resolution
            patch(
                "zing_ai.orchestrator.commands.plan.resolve_aid_path",
                return_value="aid",
            ),
            # Mock the next stage
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
            ) as mock_plan_audit,
        ):
            from zing_ai.orchestrator.commands.plan import run_plan

            run_plan(
                zing_file=zing_path.name,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )

            # run_plan_audit should have been called (chaining)
            mock_plan_audit.assert_called_once()
            call_kwargs = mock_plan_audit.call_args.kwargs
            assert call_kwargs["zing_file"] == zing_path.name

    def test_plan_audit_chains_into_plan_review(self, tmp_path: Path) -> None:
        """run_plan_audit should perform audit then call run_plan_review."""
        zing_path = _make_zing_file(tmp_path, stage="plan", with_plan=True)

        with (
            # Mock identification
            patch(
                "zing_ai.orchestrator.commands.plan_audit.claude.invoke_claude_full",
                return_value=(VALID_IDENTIFICATION_RESPONSE, "audit-session-1"),
            ),
            # Mock the investigation phase
            patch(
                "zing_ai.orchestrator.commands.plan_audit._run_investigation_tui",
                return_value=(
                    _make_investigation_result(),
                    [_minimal_interaction()],
                ),
            ),
            # Mock document update
            patch(
                "zing_ai.orchestrator.commands.plan_audit._invoke_update_with_session",
                return_value=(_minimal_plan(), "audit-session-2"),
            ),
            # Mock distiller
            patch(
                "zing_ai.orchestrator.commands.plan_audit.distill_files",
                return_value={},
            ),
            # Mock aid path resolution
            patch(
                "zing_ai.orchestrator.commands.plan_audit.resolve_aid_path",
                return_value="aid",
            ),
            # Mock the next stage
            patch(
                "zing_ai.orchestrator.commands.plan_review.run_plan_review",
            ) as mock_plan_review,
        ):
            from zing_ai.orchestrator.commands.plan_audit import run_plan_audit

            run_plan_audit(
                zing_file=zing_path.name,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )

            mock_plan_review.assert_called_once()

    def test_plan_review_approval_chains_into_build(self, tmp_path: Path) -> None:
        """run_plan_review with approval (no modifications) should chain into run_build."""
        zing_path = _make_zing_file(tmp_path, stage="plan", with_plan=True)

        # Mock plan_review_menu to return an "approve" result
        approve_result = _make_review_result(action="approve", changes=[])

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.plan_review_menu",
                return_value=approve_result,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_build",
            ) as mock_call_build,
        ):
            from zing_ai.orchestrator.commands.plan_review import run_plan_review

            run_plan_review(
                zing_file=zing_path.name,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )

            mock_call_build.assert_called_once()

            # Verify the zing document was marked approved
            doc = parse_zing_file(zing_path)
            assert doc.approved is True

    def test_build_chains_into_build_audit(self, tmp_path: Path) -> None:
        """run_build should iterate steps then chain into run_build_audit."""
        zing_path = _make_zing_file(tmp_path, stage="build", with_plan=True)

        # Create the files the plan references so distill doesn't skip them
        (tmp_path / "src").mkdir(parents=True, exist_ok=True)
        (tmp_path / "src" / "main.py").write_text("print('hello')")
        (tmp_path / "src" / "utils.py").write_text("def helper(): pass")

        def _mock_run_with_progress(label, stages, execute_step):
            """Iterate all steps, calling execute_step for each, then return BuildProgress."""
            completed: list[tuple[int, int]] = []
            for stage_idx, stage in enumerate(stages):
                for step_idx in range(len(stage.steps)):
                    execute_step(stage_idx, step_idx)
                    completed.append((stage_idx, step_idx))
            return BuildProgress(completed_steps=completed, failed_step=None)

        @contextlib.contextmanager
        def _mock_invoke_claude(*args, **kwargs):
            yield iter(["Build output line\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=_mock_invoke_claude,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            # Mock run_with_progress to iterate steps and return BuildProgress
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
            ) as mock_build_audit,
        ):
            from zing_ai.orchestrator.commands.build import run_build

            run_build(
                zing_file=zing_path.name,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )

            mock_build_audit.assert_called_once()

            # Verify the step was marked done
            doc = parse_zing_file(zing_path)
            assert doc.plan is not None
            assert doc.plan.stages[0].steps[0].done is True


# ===================================================================
# Test 2: Retry Logic
# ===================================================================


class TestRetryLogic:
    """Test the invoke_claude_validated retry mechanism.

    Mocks invoke_claude_full to return invalid XML on the first call
    and valid XML on the second, verifying the retry loop works.
    """

    def test_retry_on_invalid_then_valid(self) -> None:
        """invoke_claude_validated retries on ValidationError and succeeds on valid response."""
        from zing_ai.orchestrator.claude import invoke_claude_validated
        from zing_ai.orchestrator.xml_parser import parse_interactions_response

        retry_template = jinja2.Template(
            "Your response was invalid: {{ error }}. Please fix it."
        )

        call_count = 0

        def mock_invoke_full(prompt, **kwargs):
            nonlocal call_count
            call_count += 1
            if call_count == 1:
                # First call: return invalid XML (no choices)
                return (INVALID_STEPS_RESPONSE, "session-1")
            else:
                # Second call: return valid interactions
                return (VALID_INTERACTIONS_RESPONSE, "session-1")

        with patch(
            "zing_ai.orchestrator.claude.invoke_claude_full",
            side_effect=mock_invoke_full,
        ):
            result = invoke_claude_validated(
                "Test prompt",
                validator=parse_interactions_response,
                retry_prompt_template=retry_template,
                max_retries=3,
                call_type=CallType.INVESTIGATE,
                config=_make_config(),
                skip_permissions=True,
            )

            assert isinstance(result, Interaction)
            assert len(result.choice_sets) == 1
            assert result.choice_sets[0].message == "Which database?"
            # First call failed validation, second succeeded
            assert call_count == 2

    def test_retry_exhaustion_raises(self) -> None:
        """invoke_claude_validated raises ValidationError after max_retries."""
        from zing_ai.orchestrator.claude import invoke_claude_validated
        from zing_ai.orchestrator.xml_parser import parse_steps_response

        retry_template = jinja2.Template("Fix: {{ error }}")

        def mock_invoke_full(prompt, **kwargs):
            # Always return invalid response
            return (INVALID_STEPS_RESPONSE, "session-x")

        with (
            patch(
                "zing_ai.orchestrator.claude.invoke_claude_full",
                side_effect=mock_invoke_full,
            ),
            pytest.raises(ValidationError, match="at least one stage"),
        ):
            invoke_claude_validated(
                "Test prompt",
                validator=parse_steps_response,
                retry_prompt_template=retry_template,
                max_retries=2,
                call_type=CallType.PLAN,
                config=_make_config(),
                skip_permissions=True,
            )

    def test_retry_calls_on_retry_callback(self) -> None:
        """invoke_claude_validated invokes the on_retry callback before each retry."""
        from zing_ai.orchestrator.claude import invoke_claude_validated
        from zing_ai.orchestrator.xml_parser import parse_interactions_response

        retry_template = jinja2.Template("Fix: {{ error }}")
        on_retry_calls: list[tuple[int, str]] = []

        def mock_on_retry(attempt: int, error_message: str) -> None:
            on_retry_calls.append((attempt, error_message))

        call_count = 0

        def mock_invoke_full(prompt, **kwargs):
            nonlocal call_count
            call_count += 1
            if call_count <= 2:
                return (INVALID_STEPS_RESPONSE, f"session-{call_count}")
            else:
                return (VALID_INTERACTIONS_RESPONSE, f"session-{call_count}")

        with patch(
            "zing_ai.orchestrator.claude.invoke_claude_full",
            side_effect=mock_invoke_full,
        ):
            invoke_claude_validated(
                "Test prompt",
                validator=parse_interactions_response,
                retry_prompt_template=retry_template,
                max_retries=3,
                on_retry=mock_on_retry,
                call_type=CallType.INVESTIGATE,
                config=_make_config(),
                skip_permissions=True,
            )

            # on_retry should have been called for attempts 1 and 2
            assert len(on_retry_calls) == 2
            assert on_retry_calls[0][0] == 1
            assert on_retry_calls[1][0] == 2

    def test_retry_resumes_session(self) -> None:
        """invoke_claude_validated passes resume_session on retry calls."""
        from zing_ai.orchestrator.claude import invoke_claude_validated
        from zing_ai.orchestrator.xml_parser import parse_interactions_response

        retry_template = jinja2.Template("Fix: {{ error }}")
        captured_kwargs: list[dict] = []
        call_count = 0

        def mock_invoke_full(prompt, **kwargs):
            nonlocal call_count
            call_count += 1
            captured_kwargs.append(dict(kwargs))
            if call_count == 1:
                return (INVALID_STEPS_RESPONSE, "session-first")
            else:
                return (VALID_INTERACTIONS_RESPONSE, "session-second")

        with patch(
            "zing_ai.orchestrator.claude.invoke_claude_full",
            side_effect=mock_invoke_full,
        ):
            invoke_claude_validated(
                "Test prompt",
                validator=parse_interactions_response,
                retry_prompt_template=retry_template,
                max_retries=3,
                call_type=CallType.INVESTIGATE,
                config=_make_config(),
                skip_permissions=True,
            )

            # Second call should include resume_session from first call
            assert call_count == 2
            assert captured_kwargs[1].get("resume_session") == "session-first"


# ===================================================================
# Test 3: Plan-Review Re-entry Loop (via TUI)
# ===================================================================


class TestPlanReviewReentry:
    """Test the plan-review re-entry loop.

    When the user modifies choices during plan-review via the inline menu,
    the pipeline should re-enter the plan -> plan-audit -> plan-review loop
    with the changes.

    ``plan_review_menu()`` is mocked to return pre-built ``(action, changes)``
    tuples that simulate user actions.
    """

    def test_modifications_trigger_replan(self, tmp_path: Path) -> None:
        """When plan_review_menu returns action='replan', _call_replan is invoked."""
        zing_path = _make_zing_file(tmp_path, stage="plan", with_plan=True)

        # Simulate user switching from FastAPI (recommended) to Flask
        replan_changes: list[ReviewChange] = [
            ReviewChange(choice_set_id="Which framework?", selected_index=1),
        ]
        replan_result = _make_review_result(
            action="replan",
            changes=replan_changes,
        )

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.plan_review_menu",
                return_value=replan_result,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_replan",
            ) as mock_call_replan,
        ):
            from zing_ai.orchestrator.commands.plan_review import run_plan_review

            run_plan_review(
                zing_file=zing_path.name,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )

            # _call_replan should have been called with the changes
            mock_call_replan.assert_called_once()
            call_kwargs = mock_call_replan.call_args.kwargs
            changes = call_kwargs["replan_changes"]
            assert len(changes) == 1
            assert changes[0]["selected_index"] == 1

    def test_deletion_triggers_replan(self, tmp_path: Path) -> None:
        """When plan_review_menu returns action='replan', replan is triggered."""
        zing_path = _make_zing_file(tmp_path, stage="plan", with_plan=True)

        # Simulate user selecting a different option for a choice set
        replan_changes: list[ReviewChange] = [
            ReviewChange(choice_set_id="Which framework?", selected_index=0),
        ]
        replan_result = _make_review_result(
            action="replan",
            changes=replan_changes,
        )

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.plan_review_menu",
                return_value=replan_result,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_replan",
            ) as mock_call_replan,
        ):
            from zing_ai.orchestrator.commands.plan_review import run_plan_review

            run_plan_review(
                zing_file=zing_path.name,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )

            mock_call_replan.assert_called_once()
            changes = mock_call_replan.call_args.kwargs["replan_changes"]
            assert len(changes) == 1
            assert changes[0]["selected_index"] == 0

    def test_no_modifications_goes_to_build(self, tmp_path: Path) -> None:
        """When plan_review_menu returns action='approve' with no changes, the pipeline proceeds to build."""
        zing_path = _make_zing_file(tmp_path, stage="plan", with_plan=True)

        approve_result = _make_review_result(action="approve", changes=[])

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.plan_review_menu",
                return_value=approve_result,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_build",
            ) as mock_call_build,
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_replan",
            ) as mock_call_replan,
        ):
            from zing_ai.orchestrator.commands.plan_review import run_plan_review

            run_plan_review(
                zing_file=zing_path.name,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )

            mock_call_build.assert_called_once()
            mock_call_replan.assert_not_called()

            # Verify the zing document was marked approved
            doc = parse_zing_file(zing_path)
            assert doc.approved is True

    def test_no_choices_auto_approves(self, tmp_path: Path) -> None:
        """When the zing document has no choices, plan_review auto-approves without a menu."""
        # Create a zing file with plan but no interactions
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir(parents=True, exist_ok=True)
        doc = ZingDocument(
            stage="plan",
            content="# Test Project\n\nBuild a todo app.",
            plan=_minimal_plan(),
            interactions=None,
            audit=False,
            approved=False,
        )
        zing_path = zing_dir / "test-project.xml"
        write_zing_file(zing_path, doc)

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.plan_review_menu",
            ) as mock_menu,
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_build",
            ) as mock_call_build,
        ):
            from zing_ai.orchestrator.commands.plan_review import run_plan_review

            run_plan_review(
                zing_file=zing_path.name,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )

            # Menu should NOT have been called (no choices to review)
            mock_menu.assert_not_called()
            # Build should have been called (auto-approve)
            mock_call_build.assert_called_once()
            # Verify approved
            loaded = parse_zing_file(zing_path)
            assert loaded.approved is True

    def test_replan_invokes_plan_with_changes(self, tmp_path: Path) -> None:
        """The _call_replan helper calls run_plan with replan_changes kwarg."""
        zing_path = _make_zing_file(tmp_path, stage="plan", with_plan=True)
        changes = [
            {
                "choice_set_message": "Which database?",
                "original_recommended": "SQLite",
                "user_selected": "PostgreSQL",
            }
        ]

        # run_plan is imported lazily inside _call_replan, so mock at source
        with patch(
            "zing_ai.orchestrator.commands.plan.run_plan",
        ) as mock_run_plan:
            from zing_ai.orchestrator.commands.plan_review import _call_replan

            _call_replan(
                zing_path=zing_path,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
                replan_changes=changes,
            )

            mock_run_plan.assert_called_once()
            call_kwargs = mock_run_plan.call_args.kwargs
            assert call_kwargs["replan_changes"] == changes
            assert call_kwargs["zing_file"] == zing_path.name


# ===================================================================
# Test 4: Distiller Caching
# ===================================================================


class TestDistillerCaching:
    """Test the distiller's hash-based caching mechanism.

    Verifies that:
    - Cache miss triggers a subprocess call and writes a cache file
    - Modifying a file creates a different cache entry
    - Cache hit avoids subprocess call entirely
    """

    def test_cache_miss_calls_subprocess(self, tmp_path: Path) -> None:
        """First distill of a file should call the aid subprocess and create cache."""
        # Set up project structure
        project_root = tmp_path
        cache_dir = project_root / ".zing" / ".cache"

        source_file = tmp_path / "example.py"
        source_file.write_text("def hello(): pass")

        mock_result = MagicMock()
        mock_result.returncode = 0
        mock_result.stdout = b"distilled: def hello()"
        mock_result.stderr = b""

        with patch(
            "zing_ai.orchestrator.distiller.subprocess.run",
            return_value=mock_result,
        ) as mock_run:
            result = distill_file(source_file, project_root=project_root)

            assert result == "distilled: def hello()"
            mock_run.assert_called_once()

            # Verify cache file was created
            assert cache_dir.is_dir()
            cache_files = list(cache_dir.glob("*.txt"))
            assert len(cache_files) == 1

            # Verify cache file name matches the hash
            file_hash = _hash_file(source_file)
            expected_cache = cache_dir / f"{file_hash}.txt"
            assert expected_cache.is_file()
            assert expected_cache.read_text() == "distilled: def hello()"

    def test_cache_hit_skips_subprocess(self, tmp_path: Path) -> None:
        """Second distill of the same (unmodified) file should skip subprocess."""
        project_root = tmp_path
        cache_dir = project_root / ".zing" / ".cache"
        cache_dir.mkdir(parents=True)

        source_file = tmp_path / "example.py"
        source_file.write_text("def hello(): pass")

        # Pre-populate the cache
        file_hash = _hash_file(source_file)
        cache_file = cache_dir / f"{file_hash}.txt"
        cache_file.write_text("cached: def hello()")

        with patch(
            "zing_ai.orchestrator.distiller.subprocess.run",
        ) as mock_run:
            result = distill_file(source_file, project_root=project_root)

            assert result == "cached: def hello()"
            # Subprocess should NOT have been called
            mock_run.assert_not_called()

    def test_modified_file_creates_new_cache_entry(self, tmp_path: Path) -> None:
        """Modifying a file should produce a different hash and a new cache entry."""
        project_root = tmp_path
        cache_dir = project_root / ".zing" / ".cache"

        source_file = tmp_path / "example.py"
        source_file.write_text("def hello(): pass")

        call_count = 0

        def mock_subprocess_run(*args, **kwargs):
            nonlocal call_count
            call_count += 1
            result = MagicMock()
            result.returncode = 0
            result.stdout = f"distilled version {call_count}".encode()
            result.stderr = b""
            return result

        with patch(
            "zing_ai.orchestrator.distiller.subprocess.run",
            side_effect=mock_subprocess_run,
        ):
            # First distill
            result1 = distill_file(source_file, project_root=project_root)
            hash1 = _hash_file(source_file)
            assert result1 == "distilled version 1"
            assert call_count == 1

            # Modify the file
            source_file.write_text("def hello(): return 'world'")
            hash2 = _hash_file(source_file)
            assert hash1 != hash2, "Modified file should have a different hash"

            # Second distill
            result2 = distill_file(source_file, project_root=project_root)
            assert result2 == "distilled version 2"
            assert call_count == 2

            # Both cache entries should exist
            cache_files = list(cache_dir.glob("*.txt"))
            assert len(cache_files) == 2

    def test_cache_hit_after_miss_for_same_content(self, tmp_path: Path) -> None:
        """After a cache miss, re-distilling the same content hits the cache."""
        project_root = tmp_path
        source_file = tmp_path / "example.py"
        source_file.write_text("def hello(): pass")

        mock_result = MagicMock()
        mock_result.returncode = 0
        mock_result.stdout = b"distilled content"
        mock_result.stderr = b""

        with patch(
            "zing_ai.orchestrator.distiller.subprocess.run",
            return_value=mock_result,
        ) as mock_run:
            # First call: cache miss, subprocess called
            result1 = distill_file(source_file, project_root=project_root)
            assert mock_run.call_count == 1

            # Second call: cache hit, no subprocess
            result2 = distill_file(source_file, project_root=project_root)
            assert mock_run.call_count == 1  # still 1, not 2
            assert result1 == result2 == "distilled content"


# ===================================================================
# Test 5: UI Function Results Integration
# ===================================================================


class TestUIFunctionResults:
    """Test that UI function results are correctly handled by command modules.

    Mocks Rich-based UI functions to return pre-defined result objects
    and verifies each command processes them correctly.
    """

    def test_investigation_result_from_plan_investigation(self, tmp_path: Path) -> None:
        """_run_investigation_tui returns InvestigationResult and parsed Interactions."""
        # This is covered at the unit test level; here we verify the
        # integration works when mocked at the UI layer.
        zing_path = _make_zing_file(tmp_path, stage="new")

        investigation_result = _make_investigation_result(
            outputs={"investigate-0": "Choice set output"},
            statuses={"investigate-0": "success"},
        )

        with (
            patch(
                "zing_ai.orchestrator.commands.plan.claude.invoke_claude_full",
                return_value=(VALID_IDENTIFICATION_RESPONSE, "session-id1"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan._run_investigation_tui",
                return_value=(investigation_result, [_minimal_interaction()]),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan._invoke_flesh_out_with_session",
                return_value=(_minimal_plan(), "session-plan-1"),
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.plan.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_audit.run_plan_audit",
            ),
        ):
            from zing_ai.orchestrator.commands.plan import run_plan

            # Should complete without error
            run_plan(
                zing_file=zing_path.name,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )

            # Verify the zing file was written with plan data
            doc = parse_zing_file(zing_path)
            assert doc.stage == "plan"
            assert doc.plan is not None
            assert doc.interactions is not None

    def test_review_result_approve_writes_zing_file(self, tmp_path: Path) -> None:
        """plan_review_menu returning ('approve', []) writes approved=True to zing file."""
        zing_path = _make_zing_file(tmp_path, stage="plan", with_plan=True)

        approve_result = _make_review_result(action="approve", changes=[])

        with (
            patch(
                "zing_ai.orchestrator.commands.plan_review.plan_review_menu",
                return_value=approve_result,
            ),
            patch(
                "zing_ai.orchestrator.commands.plan_review._call_build",
            ),
        ):
            from zing_ai.orchestrator.commands.plan_review import run_plan_review

            run_plan_review(
                zing_file=zing_path.name,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )

            doc = parse_zing_file(zing_path)
            assert doc.approved is True

    def test_audit_decisions_processed_by_build_audit(self, tmp_path: Path) -> None:
        """AuditDecision list from audit_triage_menu is processed by run_build_audit."""
        zing_path = _make_zing_file(tmp_path, stage="build", with_plan=True)

        # Create the files referenced in the plan
        (tmp_path / "src").mkdir(parents=True, exist_ok=True)
        (tmp_path / "src" / "main.py").write_text("print('hello')")
        (tmp_path / "src" / "utils.py").write_text("def helper(): pass")

        mock_groups = [
            AuditGroup(files=["src/main.py"]),
            AuditGroup(files=["src/utils.py"]),
        ]

        review_output = (
            "FINDING|Bug|high|high|src/main.py:5|Missing error handling\n"
            "FINDING|Style|low|high|src/utils.py:1|Missing docstring\n"
        )

        audit_decisions = _make_audit_decisions(
            decisions=[
                AuditDecision(finding_index=0, category="Bug", severity="high", title="Missing error handling", action="fix"),
                AuditDecision(finding_index=1, category="Style", severity="low", title="Missing docstring", action="skip"),
            ]
        )

        # Phase 1: run_parallel_investigations returns InvestigationResult
        mock_investigation_result = InvestigationResult(
            outputs={"review-0": review_output, "review-1": review_output},
            statuses={"review-0": "success", "review-1": "success"},
        )

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={
                    tmp_path / "src" / "main.py": "distilled main",
                    tmp_path / "src" / "utils.py": "distilled utils",
                },
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=mock_groups,
            ),
            # Phase 1: mock run_parallel_investigations
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation_result,
            ),
            # Phase 2: mock audit_triage_menu
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                return_value=audit_decisions,
            ),
        ):
            from zing_ai.orchestrator.commands.build_audit import run_build_audit

            # Should complete without error -- decisions are logged
            run_build_audit(
                zing_file=zing_path.name,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )


# ===================================================================
# Test 6: Build-Audit Integration
# ===================================================================


class TestBuildAuditIntegration:
    """Test the build-audit command's file collection and grouping integration."""

    def test_build_audit_collects_files_and_groups(self, tmp_path: Path) -> None:
        """run_build_audit collects plan files, distills them, groups via Claude, and reviews."""
        zing_path = _make_zing_file(tmp_path, stage="build", with_plan=True)

        # Create the files referenced in the plan
        (tmp_path / "src").mkdir(parents=True, exist_ok=True)
        (tmp_path / "src" / "main.py").write_text("print('hello')")
        (tmp_path / "src" / "utils.py").write_text("def helper(): pass")

        mock_groups = [
            AuditGroup(files=["src/main.py"]),
            AuditGroup(files=["src/utils.py"]),
        ]

        review_output = (
            "FINDING|Bug|high|high|src/main.py:5|Missing error handling\n"
            "FINDING|Style|low|high|src/utils.py:1|Missing docstring\n"
        )

        audit_decisions = _make_audit_decisions(
            decisions=[
                AuditDecision(finding_index=0, category="Bug", severity="high", title="Missing error handling", action="fix"),
                AuditDecision(finding_index=1, category="Style", severity="low", title="Missing docstring", action="skip"),
            ]
        )

        # Phase 1: run_parallel_investigations returns InvestigationResult
        mock_investigation_result = InvestigationResult(
            outputs={"review-0": review_output, "review-1": review_output},
            statuses={"review-0": "success", "review-1": "success"},
        )

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={
                    tmp_path / "src" / "main.py": "distilled main",
                    tmp_path / "src" / "utils.py": "distilled utils",
                },
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=mock_groups,
            ),
            # Phase 1: mock run_parallel_investigations
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation_result,
            ),
            # Phase 2: mock audit_triage_menu
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                return_value=audit_decisions,
            ),
        ):
            from zing_ai.orchestrator.commands.build_audit import run_build_audit

            run_build_audit(
                zing_file=zing_path.name,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )


# ===================================================================
# Test 7: Cross-Module XML Round-Trip Integration
# ===================================================================


class TestXMLRoundTripIntegration:
    """Test that writing and reading zing documents across module boundaries preserves data."""

    def test_full_document_round_trip(self, tmp_path: Path) -> None:
        """Write a complete ZingDocument, read it back, and verify all fields."""
        zing_dir = tmp_path / ".zing"
        zing_dir.mkdir()

        plan = Plan(
            stages=[
                Stage(
                    label="Setup",
                    steps=[
                        Step(
                            label="Step 1.1",
                            instructions="Initialize project.",
                            files=["src/main.py", "tests/test_main.py"],
                            done=True,
                        ),
                        Step(
                            label="Step 1.2",
                            instructions="Add config.",
                            files=["src/config.py"],
                            done=False,
                        ),
                    ],
                ),
                Stage(
                    label="Features",
                    steps=[
                        Step(
                            label="Step 2.1",
                            instructions="Add API.",
                            files=["src/routes.py"],
                        )
                    ],
                ),
            ]
        )

        interaction = Interaction(
            choice_sets=[
                ChoiceSet(
                    message="Framework choice",
                    explanation="Pick a web framework for the API layer.",
                    choices=[
                        Choice(label="FastAPI", description="Modern async Python", recommended=True),
                        Choice(label="Flask", description="Simple WSGI", recommended=False),
                    ],
                ),
                ChoiceSet(
                    message="Database choice",
                    explanation="Pick a database engine.",
                    choices=[
                        Choice(label="SQLite", description="Embedded", recommended=False),
                        Choice(label="PostgreSQL", description="Full-featured", recommended=True),
                    ],
                ),
            ]
        )

        original_doc = ZingDocument(
            stage="plan",
            content="# Test Project\n\nBuild something great.",
            plan=plan,
            interactions=interaction,
            audit=True,
            approved=False,
            plan_session="plan-sess-123",
            audit_session="audit-sess-456",
        )

        zing_path = zing_dir / "round-trip.xml"
        write_zing_file(zing_path, original_doc)

        # Read it back
        loaded_doc = parse_zing_file(zing_path)

        # Verify all fields
        assert loaded_doc.stage == "plan"
        assert loaded_doc.content == "# Test Project\n\nBuild something great."
        assert loaded_doc.audit is True
        assert loaded_doc.approved is False
        assert loaded_doc.plan_session == "plan-sess-123"
        assert loaded_doc.audit_session == "audit-sess-456"

        # Plan
        assert loaded_doc.plan is not None
        assert len(loaded_doc.plan.stages) == 2
        assert loaded_doc.plan.stages[0].label == "Setup"
        assert len(loaded_doc.plan.stages[0].steps) == 2
        assert loaded_doc.plan.stages[0].steps[0].done is True
        assert loaded_doc.plan.stages[0].steps[1].done is False
        assert loaded_doc.plan.stages[0].steps[0].files == ["src/main.py", "tests/test_main.py"]
        assert loaded_doc.plan.stages[1].label == "Features"

        # Interactions
        assert loaded_doc.interactions is not None
        assert len(loaded_doc.interactions.choice_sets) == 2
        assert loaded_doc.interactions.choice_sets[0].message == "Framework choice"
        assert len(loaded_doc.interactions.choice_sets[0].choices) == 2
        assert loaded_doc.interactions.choice_sets[1].choices[1].recommended is True


# ===================================================================
# Test 8: Pipeline Controller Integration
# ===================================================================


class TestPipelineControllerIntegration:
    """Test the pipeline controller dispatches correctly to command modules."""

    def test_pipeline_dispatches_new_to_run_new(self, tmp_path: Path) -> None:
        """run_pipeline('new') should dispatch to the new command."""
        from zing_ai.orchestrator.pipeline import run_pipeline

        with patch(
            "zing_ai.orchestrator.commands.new.run_new",
        ) as mock_run_new:
            run_pipeline(
                "new",
                zing_file=None,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )
            mock_run_new.assert_called_once()

    def test_pipeline_dispatches_plan(self, tmp_path: Path) -> None:
        """run_pipeline('plan') should dispatch to the plan command."""
        from zing_ai.orchestrator.pipeline import run_pipeline

        with patch(
            "zing_ai.orchestrator.commands.plan.run_plan",
        ) as mock_run_plan:
            run_pipeline(
                "plan",
                zing_file="test.xml",
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )
            mock_run_plan.assert_called_once()

    def test_pipeline_dispatches_build(self, tmp_path: Path) -> None:
        """run_pipeline('build') should dispatch to the build command."""
        from zing_ai.orchestrator.pipeline import run_pipeline

        with patch(
            "zing_ai.orchestrator.commands.build.run_build",
        ) as mock_run_build:
            run_pipeline(
                "build",
                zing_file="test.xml",
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )
            mock_run_build.assert_called_once()

    def test_pipeline_dispatches_build_audit(self, tmp_path: Path) -> None:
        """run_pipeline('build-audit') should dispatch to the build-audit command."""
        from zing_ai.orchestrator.pipeline import run_pipeline

        with patch(
            "zing_ai.orchestrator.commands.build_audit.run_build_audit",
        ) as mock_run_build_audit:
            run_pipeline(
                "build-audit",
                zing_file="test.xml",
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )
            mock_run_build_audit.assert_called_once()

    def test_pipeline_invalid_stage_raises(self, tmp_path: Path) -> None:
        """run_pipeline with an invalid stage raises ValueError."""
        from zing_ai.orchestrator.pipeline import run_pipeline

        with pytest.raises(ValueError, match="Invalid start_stage"):
            run_pipeline(
                "nonexistent",
                zing_file=None,
                skip_permissions=True,
                config=_make_config(),
                project_root=tmp_path,
            )

    def test_pipeline_all_stages_recognized(self) -> None:
        """All expected stage names are present in the STAGES constant."""
        from zing_ai.orchestrator.pipeline import STAGES

        expected = {"new", "plan", "plan-audit", "plan-review", "build", "build-audit"}
        assert set(STAGES) == expected
