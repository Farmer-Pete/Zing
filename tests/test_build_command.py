"""Tests for the orchestrator ``build`` command."""

from __future__ import annotations

import contextlib
import xml.etree.ElementTree as ET
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from zing_ai.orchestrator.commands.build import (
    MCP_MANDATE,
    _update_step_done,
    run_build,
)
from zing_ai.orchestrator.config import CallType, ZingConfig
from zing_ai.orchestrator.errors import PipelineError
from zing_ai.orchestrator.models import (
    Plan,
    Stage,
    Step,
    ZingDocument,
)
from zing_ai.orchestrator.ui.types import BuildProgress
from zing_ai.orchestrator.xml_parser import parse_zing_file, write_zing_file

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _make_zing_file_with_plan(
    tmp_path: Path,
    *,
    stage: str = "plan",
    plan: Plan | None = None,
    content: str = "# Test Project\n\nA test project for building.",
) -> Path:
    """Create a zing XML file with a plan and return its path."""
    zing_dir = tmp_path / ".zing"
    zing_dir.mkdir(exist_ok=True)
    zing_path = zing_dir / "test-project.xml"

    if plan is None:
        plan = Plan(
            stages=[
                Stage(
                    label="Stage 1",
                    steps=[
                        Step(
                            label="Step 1.1",
                            instructions="Implement the first feature.",
                            files=["src/main.py", "src/utils.py"],
                            done=False,
                        ),
                        Step(
                            label="Step 1.2",
                            instructions="Add tests for the first feature.",
                            files=["tests/test_main.py"],
                            done=False,
                        ),
                    ],
                ),
                Stage(
                    label="Stage 2",
                    steps=[
                        Step(
                            label="Step 2.1",
                            instructions="Implement the second feature.",
                            files=["src/api.py"],
                            done=False,
                        ),
                    ],
                ),
            ]
        )

    doc = ZingDocument(
        stage=stage,
        content=content,
        plan=plan,
        interactions=None,
        audit=False,
        approved=True,
    )
    write_zing_file(zing_path, doc)
    return zing_path


def _make_zing_file_with_partial_progress(tmp_path: Path) -> Path:
    """Create a zing file where the first step is already done."""
    plan = Plan(
        stages=[
            Stage(
                label="Stage 1",
                steps=[
                    Step(
                        label="Step 1.1",
                        instructions="Already done.",
                        files=["src/done.py"],
                        done=True,
                    ),
                    Step(
                        label="Step 1.2",
                        instructions="Not yet done.",
                        files=["src/todo.py"],
                        done=False,
                    ),
                ],
            ),
        ]
    )
    return _make_zing_file_with_plan(tmp_path, plan=plan)


def _make_zing_file_all_done(tmp_path: Path) -> Path:
    """Create a zing file where all steps are done."""
    plan = Plan(
        stages=[
            Stage(
                label="Stage 1",
                steps=[
                    Step(label="Step 1.1", instructions="Done.", files=[], done=True),
                    Step(label="Step 1.2", instructions="Done.", files=[], done=True),
                ],
            ),
        ]
    )
    return _make_zing_file_with_plan(tmp_path, plan=plan)


def _make_zing_file_no_plan(tmp_path: Path) -> Path:
    """Create a zing file with no plan."""
    zing_dir = tmp_path / ".zing"
    zing_dir.mkdir(exist_ok=True)
    zing_path = zing_dir / "test-project.xml"
    doc = ZingDocument(
        stage="new",
        content="# Test",
        plan=None,
        interactions=None,
        audit=False,
        approved=False,
    )
    write_zing_file(zing_path, doc)
    return zing_path


@contextlib.contextmanager
def _mock_invoke_claude_gen(prompt, **kwargs):
    """Mock invoke_claude context manager that yields a line iterator."""
    yield iter(["Line 1\n", "Line 2\n", "Done.\n"])


def _mock_run_with_progress(label, stages, execute_step):
    """Mock ``run_with_progress`` that invokes *execute_step* for every step.

    Iterates through all stages/steps just like the real implementation,
    calling the supplied callback so that the build logic is exercised.
    Returns a :class:`BuildProgress` with all steps marked completed.
    """
    completed: list[tuple[int, int]] = []
    for stage_idx, stage in enumerate(stages):
        for step_idx in range(len(stage.steps)):
            execute_step(stage_idx, step_idx)
            completed.append((stage_idx, step_idx))
    return BuildProgress(completed_steps=completed, failed_step=None)


# ---------------------------------------------------------------------------
# _update_step_done tests
# ---------------------------------------------------------------------------


class TestUpdateStepDone:
    """Tests for the _update_step_done helper."""

    def test_marks_matching_step_done(self) -> None:
        doc = ZingDocument(
            stage="build",
            content=None,
            plan=Plan(
                stages=[
                    Stage(
                        label="S1",
                        steps=[
                            Step(label="Step A", instructions="...", files=[], done=False),
                        ],
                    ),
                ]
            ),
            interactions=None,
            audit=False,
            approved=False,
        )
        _update_step_done(doc, "S1", "Step A")
        assert doc.plan is not None
        assert doc.plan.stages[0].steps[0].done is True

    def test_does_not_affect_other_steps(self) -> None:
        doc = ZingDocument(
            stage="build",
            content=None,
            plan=Plan(
                stages=[
                    Stage(
                        label="S1",
                        steps=[
                            Step(label="Step A", instructions="...", files=[], done=False),
                            Step(label="Step B", instructions="...", files=[], done=False),
                        ],
                    ),
                ]
            ),
            interactions=None,
            audit=False,
            approved=False,
        )
        _update_step_done(doc, "S1", "Step A")
        assert doc.plan is not None
        assert doc.plan.stages[0].steps[0].done is True
        assert doc.plan.stages[0].steps[1].done is False

    def test_no_op_when_no_plan(self) -> None:
        doc = ZingDocument(
            stage="build",
            content=None,
            plan=None,
            interactions=None,
            audit=False,
            approved=False,
        )
        _update_step_done(doc, "S1", "Step A")
        assert doc.plan is None

    def test_no_op_when_labels_dont_match(self) -> None:
        doc = ZingDocument(
            stage="build",
            content=None,
            plan=Plan(
                stages=[
                    Stage(
                        label="S1",
                        steps=[
                            Step(label="Step A", instructions="...", files=[], done=False),
                        ],
                    ),
                ]
            ),
            interactions=None,
            audit=False,
            approved=False,
        )
        _update_step_done(doc, "S1", "Step Z")
        assert doc.plan is not None
        assert doc.plan.stages[0].steps[0].done is False

    def test_matches_correct_stage(self) -> None:
        doc = ZingDocument(
            stage="build",
            content=None,
            plan=Plan(
                stages=[
                    Stage(
                        label="S1",
                        steps=[
                            Step(label="Step A", instructions="...", files=[], done=False),
                        ],
                    ),
                    Stage(
                        label="S2",
                        steps=[
                            Step(label="Step A", instructions="...", files=[], done=False),
                        ],
                    ),
                ]
            ),
            interactions=None,
            audit=False,
            approved=False,
        )
        _update_step_done(doc, "S2", "Step A")
        assert doc.plan is not None
        assert doc.plan.stages[0].steps[0].done is False
        assert doc.plan.stages[1].steps[0].done is True


# ---------------------------------------------------------------------------
# run_build tests -- full pipeline
# ---------------------------------------------------------------------------


class TestRunBuildFullPipeline:
    """Tests for the full build pipeline."""

    def test_full_build_pipeline(self, tmp_path: Path) -> None:
        """Happy path: all steps executed in order, audit called at end."""
        zing_path = _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()
        mock_build_audit = MagicMock()

        # Create referenced files so they can be "distilled"
        for f in ["src/main.py", "src/utils.py", "tests/test_main.py", "src/api.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        invoke_calls: list[dict] = []

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            invoke_calls.append({"prompt": prompt, **kwargs})
            yield iter(["Output line\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                mock_build_audit,
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # All 3 steps should have been invoked
        assert len(invoke_calls) == 3

        # Verify the zing file has all steps marked done
        doc = parse_zing_file(zing_path)
        assert doc.plan is not None
        for stage in doc.plan.stages:
            for step in stage.steps:
                assert step.done is True

        # Build audit should have been called
        mock_build_audit.assert_called_once()

    def test_stage_updated_to_build(self, tmp_path: Path) -> None:
        """The zing document stage should be set to 'build'."""
        zing_path = _make_zing_file_with_plan(tmp_path, stage="plan")
        config = ZingConfig()

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("stage") == "build"


# ---------------------------------------------------------------------------
# run_build tests -- skipping completed steps
# ---------------------------------------------------------------------------


class TestRunBuildSkipsCompletedSteps:
    """Tests that run_build skips steps where done=True."""

    def test_skips_done_steps(self, tmp_path: Path) -> None:
        """Steps with done=True should not be invoked."""
        _make_zing_file_with_partial_progress(tmp_path)
        config = ZingConfig()

        # Create the file for the pending step
        (tmp_path / "src").mkdir(parents=True, exist_ok=True)
        (tmp_path / "src" / "todo.py").write_text("# todo")

        invoke_calls: list[dict] = []

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            invoke_calls.append({"prompt": prompt, **kwargs})
            yield iter(["Output\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # Only 1 step invoked (the one that's not done)
        assert len(invoke_calls) == 1
        assert "Step 1.2" in invoke_calls[0]["prompt"]

    def test_all_done_skips_to_audit(self, tmp_path: Path) -> None:
        """When all steps are done, no Claude calls, but audit still runs."""
        _make_zing_file_all_done(tmp_path)
        config = ZingConfig()
        mock_build_audit = MagicMock()

        invoke_calls: list[dict] = []

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            invoke_calls.append({"prompt": prompt, **kwargs})
            yield iter(["Output\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                mock_build_audit,
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # No Claude calls
        assert len(invoke_calls) == 0

        # Build audit should still be called
        mock_build_audit.assert_called_once()


# ---------------------------------------------------------------------------
# run_build tests -- distillation
# ---------------------------------------------------------------------------


class TestRunBuildDistillation:
    """Tests that run_build distills files for each step."""

    def test_distills_files_per_step(self, tmp_path: Path) -> None:
        """Each step should trigger a distill_files call with its referenced files."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=["src/a.py"], done=False),
                        Step(label="Step B", instructions="Do B.", files=["src/b.py"], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        # Create referenced files
        (tmp_path / "src").mkdir(parents=True, exist_ok=True)
        (tmp_path / "src" / "a.py").write_text("# a")
        (tmp_path / "src" / "b.py").write_text("# b")

        distill_calls: list[list[Path]] = []

        def mock_distill(file_paths, *, project_root, aid_path="aid"):
            distill_calls.append(file_paths)
            return {fp: f"distilled:{fp.name}" for fp in file_paths}

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                side_effect=mock_distill,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # Two distill calls (one per step)
        assert len(distill_calls) == 2
        assert len(distill_calls[0]) == 1  # src/a.py
        assert len(distill_calls[1]) == 1  # src/b.py

    def test_skips_nonexistent_files(self, tmp_path: Path) -> None:
        """Files that don't exist should not be passed to distill_files."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(
                            label="Step A",
                            instructions="Do A.",
                            files=["src/exists.py", "src/missing.py"],
                            done=False,
                        ),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        # Only create one of the files
        (tmp_path / "src").mkdir(parents=True, exist_ok=True)
        (tmp_path / "src" / "exists.py").write_text("# exists")

        distill_calls: list[list[Path]] = []

        def mock_distill(file_paths, *, project_root, aid_path="aid"):
            distill_calls.append(file_paths)
            return {fp: f"distilled:{fp.name}" for fp in file_paths}

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                side_effect=mock_distill,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # Only the existing file should be distilled
        assert len(distill_calls) == 1
        assert len(distill_calls[0]) == 1
        assert distill_calls[0][0].name == "exists.py"

    def test_no_files_skips_distillation(self, tmp_path: Path) -> None:
        """Steps with no files should not call distill_files."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=[], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        distill_calls: list[list[Path]] = []

        def mock_distill(file_paths, *, project_root, aid_path="aid"):
            distill_calls.append(file_paths)
            return {}

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                side_effect=mock_distill,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # distill_files should not have been called
        assert len(distill_calls) == 0


# ---------------------------------------------------------------------------
# run_build tests -- Claude invocation
# ---------------------------------------------------------------------------


class TestRunBuildClaudeInvocation:
    """Tests for how Claude is invoked during build."""

    def test_uses_build_call_type(self, tmp_path: Path) -> None:
        """Claude should be invoked with CallType.BUILD."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=[], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        invoke_kwargs_list: list[dict] = []

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            invoke_kwargs_list.append(kwargs)
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert len(invoke_kwargs_list) == 1
        assert invoke_kwargs_list[0]["call_type"] == CallType.BUILD

    def test_skip_permissions_forwarded(self, tmp_path: Path) -> None:
        """skip_permissions flag should be passed to Claude."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=[], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        invoke_kwargs_list: list[dict] = []

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            invoke_kwargs_list.append(kwargs)
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=True,
                config=config,
                project_root=tmp_path,
            )

        assert invoke_kwargs_list[0]["skip_permissions"] is True

    def test_prompt_contains_step_instructions(self, tmp_path: Path) -> None:
        """The rendered prompt should include the step label and instructions."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(
                            label="Implement Widget",
                            instructions="Create the Widget class with start() and stop() methods.",
                            files=[],
                            done=False,
                        ),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        captured_prompts: list[str] = []

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            captured_prompts.append(prompt)
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert len(captured_prompts) == 1
        assert "Implement Widget" in captured_prompts[0]
        assert "Widget class with start() and stop()" in captured_prompts[0]

    def test_prompt_contains_mcp_mandate(self, tmp_path: Path) -> None:
        """The rendered prompt should contain the MCP mandate."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=[], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        captured_prompts: list[str] = []

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            captured_prompts.append(prompt)
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert "Serena" in captured_prompts[0]
        assert "search_for_pattern" in captured_prompts[0]

    def test_prompt_contains_distilled_files(self, tmp_path: Path) -> None:
        """The rendered prompt should include distilled file content."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=["src/a.py"], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        (tmp_path / "src").mkdir(parents=True, exist_ok=True)
        (tmp_path / "src" / "a.py").write_text("# a")

        def mock_distill(file_paths, *, project_root, aid_path="aid"):
            return {fp: "def hello(): pass" for fp in file_paths}

        captured_prompts: list[str] = []

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            captured_prompts.append(prompt)
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                side_effect=mock_distill,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert "def hello(): pass" in captured_prompts[0]
        assert "src/a.py" in captured_prompts[0]


# ---------------------------------------------------------------------------
# run_build tests -- zing document updates
# ---------------------------------------------------------------------------


class TestRunBuildDocumentUpdates:
    """Tests for zing document updates during build."""

    def test_step_marked_done_after_completion(self, tmp_path: Path) -> None:
        """Each step should be marked done=True after successful Claude invocation."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=[], done=False),
                    ],
                ),
            ]
        )
        zing_path = _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        doc = parse_zing_file(zing_path)
        assert doc.plan is not None
        assert doc.plan.stages[0].steps[0].done is True

    def test_zing_file_written_after_each_step(self, tmp_path: Path) -> None:
        """The zing file should be written after each step completes."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=[], done=False),
                        Step(label="Step B", instructions="Do B.", files=[], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        write_call_count = 0

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            yield iter(["done\n"])

        original_write = write_zing_file

        def counting_write(path, doc):
            nonlocal write_call_count
            write_call_count += 1
            original_write(path, doc)

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
            patch(
                "zing_ai.orchestrator.commands.build.write_zing_file",
                side_effect=counting_write,
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # 1 initial write (stage update) + 2 writes (one per step)
        assert write_call_count == 3


# ---------------------------------------------------------------------------
# run_build tests -- no plan
# ---------------------------------------------------------------------------


class TestRunBuildNoPlan:
    """Tests for when the zing file has no plan."""

    def test_returns_early_with_no_plan(self, tmp_path: Path) -> None:
        """run_build should return early without calling Claude if no plan."""
        _make_zing_file_no_plan(tmp_path)
        config = ZingConfig()

        invoke_calls: list[dict] = []

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            invoke_calls.append({})
            yield iter(["done\n"])

        mock_progress = MagicMock(
            side_effect=AssertionError("run_with_progress should not be called"),
        )

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                mock_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # No Claude calls should have been made
        assert len(invoke_calls) == 0
        # run_with_progress should not have been called
        mock_progress.assert_not_called()


# ---------------------------------------------------------------------------
# run_build tests -- build audit delegation
# ---------------------------------------------------------------------------


class TestRunBuildCallsAudit:
    """Tests that run_build calls run_build_audit at the end."""

    def test_calls_build_audit_after_all_steps(self, tmp_path: Path) -> None:
        """run_build_audit should be called with correct args after all steps complete."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=[], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()
        mock_audit = MagicMock()

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=_mock_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                mock_audit,
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=True,
                config=config,
                project_root=tmp_path,
            )

        mock_audit.assert_called_once_with(
            zing_file="test-project.xml",
            skip_permissions=True,
            config=config,
            project_root=tmp_path,
        )


# ---------------------------------------------------------------------------
# run_build tests -- run_with_progress interaction
# ---------------------------------------------------------------------------


class TestRunBuildProgressInteraction:
    """Tests that run_build correctly interacts with run_with_progress."""

    def test_run_with_progress_called_with_plan_stages(self, tmp_path: Path) -> None:
        """run_with_progress should receive the plan stages."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=[], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        captured_args: list[tuple] = []

        def capturing_run_with_progress(label, stages, execute_step):
            captured_args.append((label, stages, execute_step))
            completed: list[tuple[int, int]] = []
            for stage_idx, stage in enumerate(stages):
                for step_idx in range(len(stage.steps)):
                    execute_step(stage_idx, step_idx)
                    completed.append((stage_idx, step_idx))
            return BuildProgress(completed_steps=completed, failed_step=None)

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=capturing_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # run_with_progress should have been called once
        assert len(captured_args) == 1
        label, stages_arg, _ = captured_args[0]
        assert label == "Building"
        assert len(stages_arg) == 1
        assert stages_arg[0].label == "S1"

    def test_execute_step_returns_claude_output(self, tmp_path: Path) -> None:
        """The execute_step callback should return Claude output as a string."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=[], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        step_outputs: list[str] = []

        def capturing_run_with_progress(label, stages, execute_step):
            completed: list[tuple[int, int]] = []
            for stage_idx, stage in enumerate(stages):
                for step_idx in range(len(stage.steps)):
                    output = execute_step(stage_idx, step_idx)
                    step_outputs.append(output)
                    completed.append((stage_idx, step_idx))
            return BuildProgress(completed_steps=completed, failed_step=None)

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            yield iter(["line 1\n", "line 2\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=capturing_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert len(step_outputs) == 1
        assert step_outputs[0] == "line 1\nline 2\n"

    def test_execute_step_called_for_all_steps(self, tmp_path: Path) -> None:
        """execute_step should be called for every step in the plan."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=[], done=False),
                        Step(label="Step B", instructions="Do B.", files=[], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        executed_steps: list[tuple[int, int]] = []

        def capturing_run_with_progress(label, stages, execute_step):
            completed: list[tuple[int, int]] = []
            for stage_idx, stage in enumerate(stages):
                for step_idx in range(len(stage.steps)):
                    execute_step(stage_idx, step_idx)
                    executed_steps.append((stage_idx, step_idx))
                    completed.append((stage_idx, step_idx))
            return BuildProgress(completed_steps=completed, failed_step=None)

        @contextlib.contextmanager
        def mock_invoke(prompt, **kwargs):
            yield iter(["done\n"])

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                side_effect=mock_invoke,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.distill_files",
                return_value={},
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=capturing_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                MagicMock(),
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert executed_steps == [(0, 0), (0, 1)]


# ---------------------------------------------------------------------------
# Failed step handling tests
# ---------------------------------------------------------------------------


class TestRunBuildFailedStep:
    """Tests that run_build handles failed steps correctly."""

    def test_failed_step_stop_here_raises_pipeline_error(self, tmp_path: Path) -> None:
        """When a step fails and user selects 'Stop here', PipelineError is raised."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=[], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()
        mock_audit = MagicMock()

        def failing_run_with_progress(label, stages, execute_step):
            return BuildProgress(completed_steps=[], failed_step=(0, 0))

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=failing_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.numbered_menu",
                return_value=1,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                mock_audit,
            ),
        ):
            with pytest.raises(PipelineError) as exc_info:
                run_build(
                    zing_file="test-project.xml",
                    skip_permissions=True,
                    config=config,
                    project_root=tmp_path,
                )

            assert exc_info.value.stage == "build"

        mock_audit.assert_not_called()

    def test_failed_step_continue_calls_audit(self, tmp_path: Path) -> None:
        """When a step fails and user selects 'Continue to audit', audit is called."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(label="Step A", instructions="Do A.", files=[], done=False),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()
        mock_audit = MagicMock()

        def failing_run_with_progress(label, stages, execute_step):
            return BuildProgress(completed_steps=[], failed_step=(0, 0))

        with (
            patch(
                "zing_ai.orchestrator.commands.build.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build.run_with_progress",
                side_effect=failing_run_with_progress,
            ),
            patch(
                "zing_ai.orchestrator.commands.build.numbered_menu",
                return_value=0,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                mock_audit,
            ),
        ):
            run_build(
                zing_file="test-project.xml",
                skip_permissions=True,
                config=config,
                project_root=tmp_path,
            )

        mock_audit.assert_called_once_with(
            zing_file="test-project.xml",
            skip_permissions=True,
            config=config,
            project_root=tmp_path,
        )


# ---------------------------------------------------------------------------
# MCP_MANDATE constant tests
# ---------------------------------------------------------------------------


class TestMCPMandate:
    """Tests for the MCP mandate constant."""

    def test_contains_serena(self) -> None:
        assert "Serena" in MCP_MANDATE

    def test_contains_search_for_pattern(self) -> None:
        assert "search_for_pattern" in MCP_MANDATE

    def test_contains_find_referencing_symbols(self) -> None:
        assert "find_referencing_symbols" in MCP_MANDATE

    def test_contains_code_graph_context(self) -> None:
        assert "CodeGraphContext" in MCP_MANDATE

    def test_contains_symbol_editing(self) -> None:
        assert "replace_symbol_body" in MCP_MANDATE
        assert "insert_before_symbol" in MCP_MANDATE
        assert "insert_after_symbol" in MCP_MANDATE
