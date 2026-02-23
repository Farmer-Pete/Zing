"""Tests for the orchestrator ``build`` command."""

from __future__ import annotations

import asyncio
import xml.etree.ElementTree as ET
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

from zing_ai.orchestrator.commands.build import (
    MCP_MANDATE,
    _update_step_done,
    run_build,
)
from zing_ai.orchestrator.config import CallType, ZingConfig
from zing_ai.orchestrator.models import (
    Plan,
    Stage,
    Step,
    ZingDocument,
)
from zing_ai.orchestrator.xml_parser import parse_zing_file, write_zing_file

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _run(coro):  # type: ignore[no-untyped-def]
    """Run an async coroutine synchronously."""
    return asyncio.run(coro)


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


async def _mock_invoke_claude_gen(prompt, **kwargs):
    """Mock invoke_claude that yields a few lines."""
    yield "Line 1\n"
    yield "Line 2\n"
    yield "Done.\n"


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
# run_build tests — full pipeline
# ---------------------------------------------------------------------------


class TestRunBuildFullPipeline:
    """Tests for the full build pipeline."""

    def test_full_build_pipeline(self, tmp_path: Path) -> None:
        """Happy path: all steps executed in order, audit called at end."""
        zing_path = _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()
        mock_build_audit = AsyncMock()

        # Create referenced files so they can be "distilled"
        for f in ["src/main.py", "src/utils.py", "tests/test_main.py", "src/api.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        invoke_calls: list[dict] = []

        async def mock_invoke(prompt, **kwargs):
            invoke_calls.append({"prompt": prompt, **kwargs})
            yield "Output line\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    mock_build_audit,
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

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

        async def mock_invoke(prompt, **kwargs):
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        tree = ET.parse(zing_path)
        root = tree.getroot()
        assert root.get("stage") == "build"


# ---------------------------------------------------------------------------
# run_build tests — skipping completed steps
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

        async def mock_invoke(prompt, **kwargs):
            invoke_calls.append({"prompt": prompt, **kwargs})
            yield "Output\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        # Only 1 step invoked (the one that's not done)
        assert len(invoke_calls) == 1
        assert "Step 1.2" in invoke_calls[0]["prompt"]

    def test_all_done_skips_to_audit(self, tmp_path: Path) -> None:
        """When all steps are done, no Claude calls, but audit still runs."""
        _make_zing_file_all_done(tmp_path)
        config = ZingConfig()
        mock_build_audit = AsyncMock()

        invoke_calls: list[dict] = []

        async def mock_invoke(prompt, **kwargs):
            invoke_calls.append({"prompt": prompt, **kwargs})
            yield "Output\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    mock_build_audit,
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        # No Claude calls
        assert len(invoke_calls) == 0

        # Build audit should still be called
        mock_build_audit.assert_called_once()


# ---------------------------------------------------------------------------
# run_build tests — distillation
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

        async def mock_distill(file_paths, *, project_root):
            distill_calls.append(file_paths)
            return {fp: f"distilled:{fp.name}" for fp in file_paths}

        async def mock_invoke(prompt, **kwargs):
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    side_effect=mock_distill,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

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

        async def mock_distill(file_paths, *, project_root):
            distill_calls.append(file_paths)
            return {fp: f"distilled:{fp.name}" for fp in file_paths}

        async def mock_invoke(prompt, **kwargs):
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    side_effect=mock_distill,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

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

        async def mock_distill(file_paths, *, project_root):
            distill_calls.append(file_paths)
            return {}

        async def mock_invoke(prompt, **kwargs):
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    side_effect=mock_distill,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        # distill_files should not have been called
        assert len(distill_calls) == 0


# ---------------------------------------------------------------------------
# run_build tests — Claude invocation
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

        async def mock_invoke(prompt, **kwargs):
            invoke_kwargs_list.append(kwargs)
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

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

        async def mock_invoke(prompt, **kwargs):
            invoke_kwargs_list.append(kwargs)
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=True,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

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

        async def mock_invoke(prompt, **kwargs):
            captured_prompts.append(prompt)
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

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

        async def mock_invoke(prompt, **kwargs):
            captured_prompts.append(prompt)
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

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

        async def mock_distill(file_paths, *, project_root):
            return {fp: "def hello(): pass" for fp in file_paths}

        captured_prompts: list[str] = []

        async def mock_invoke(prompt, **kwargs):
            captured_prompts.append(prompt)
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    side_effect=mock_distill,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        assert "def hello(): pass" in captured_prompts[0]
        assert "src/a.py" in captured_prompts[0]


# ---------------------------------------------------------------------------
# run_build tests — zing document updates
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

        async def mock_invoke(prompt, **kwargs):
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

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

        async def mock_invoke(prompt, **kwargs):
            yield "done\n"

        original_write = write_zing_file

        def counting_write(path, doc):
            nonlocal write_call_count
            write_call_count += 1
            original_write(path, doc)

        async def _test() -> None:
            nonlocal write_call_count
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.write_zing_file",
                    side_effect=counting_write,
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        # 1 initial write (stage update) + 2 writes (one per step)
        assert write_call_count == 3


# ---------------------------------------------------------------------------
# run_build tests — no plan
# ---------------------------------------------------------------------------


class TestRunBuildNoPlan:
    """Tests for when the zing file has no plan."""

    def test_returns_early_with_no_plan(self, tmp_path: Path) -> None:
        """run_build should return early without calling Claude if no plan."""
        _make_zing_file_no_plan(tmp_path)
        config = ZingConfig()

        invoke_calls: list[dict] = []

        async def mock_invoke(prompt, **kwargs):
            invoke_calls.append({})
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        # No Claude calls should have been made
        assert len(invoke_calls) == 0


# ---------------------------------------------------------------------------
# run_build tests — web server
# ---------------------------------------------------------------------------


class TestRunBuildWebServer:
    """Tests for web server startup during build."""

    def test_web_server_started(self, tmp_path: Path) -> None:
        """The web server should be started in a background thread."""
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

        web_server_calls: list[dict] = []

        def mock_start_web(zing_file_path, *, port, no_browser):
            web_server_calls.append(
                {
                    "zing_file_path": zing_file_path,
                    "port": port,
                    "no_browser": no_browser,
                }
            )
            return MagicMock()

        async def mock_invoke(prompt, **kwargs):
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    side_effect=mock_start_web,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    AsyncMock(),
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        assert len(web_server_calls) == 1
        assert web_server_calls[0]["no_browser"] is True
        assert web_server_calls[0]["port"] == config.port


# ---------------------------------------------------------------------------
# run_build tests — build audit delegation
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
        mock_audit = AsyncMock()

        async def mock_invoke(prompt, **kwargs):
            yield "done\n"

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build.claude.invoke_claude",
                    side_effect=mock_invoke,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build._start_web_server_background",
                    return_value=MagicMock(),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.run_build_audit",
                    mock_audit,
                ),
            ):
                await run_build(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=True,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        mock_audit.assert_called_once_with(
            zing_file="test-project.xml",
            no_browser=True,
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
