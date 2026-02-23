"""Tests for the orchestrator ``build-audit`` command."""

from __future__ import annotations

import asyncio
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

from zing_ai.orchestrator.commands.build_audit import (
    Finding,
    FindingGroup,
    collect_plan_files,
    group_findings_by_severity,
    parse_review_findings,
    run_build_audit,
)
from zing_ai.orchestrator.config import CallType, ZingConfig
from zing_ai.orchestrator.models import (
    AuditGroup,
    Plan,
    Stage,
    Step,
    ZingDocument,
)
from zing_ai.orchestrator.xml_parser import write_zing_file

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _run(coro):  # type: ignore[no-untyped-def]
    """Run an async coroutine synchronously."""
    return asyncio.run(coro)


def _make_zing_file_with_plan(
    tmp_path: Path,
    *,
    plan: Plan | None = None,
    content: str = "# Test Project\n\nA test project for auditing.",
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
                            instructions="Implement auth.",
                            files=["src/auth.py", "src/models.py"],
                            done=True,
                        ),
                        Step(
                            label="Step 1.2",
                            instructions="Add tests.",
                            files=["tests/test_auth.py"],
                            done=True,
                        ),
                    ],
                ),
                Stage(
                    label="Stage 2",
                    steps=[
                        Step(
                            label="Step 2.1",
                            instructions="Build API.",
                            files=["src/api.py", "src/models.py"],
                            done=True,
                        ),
                    ],
                ),
            ]
        )

    doc = ZingDocument(
        stage="build",
        content=content,
        plan=plan,
        interactions=None,
        audit=False,
        approved=True,
    )
    write_zing_file(zing_path, doc)
    return zing_path


def _make_zing_file_no_plan(tmp_path: Path) -> Path:
    """Create a zing file with no plan."""
    zing_dir = tmp_path / ".zing"
    zing_dir.mkdir(exist_ok=True)
    zing_path = zing_dir / "test-project.xml"
    doc = ZingDocument(
        stage="build",
        content="# Test",
        plan=None,
        interactions=None,
        audit=False,
        approved=False,
    )
    write_zing_file(zing_path, doc)
    return zing_path


def _make_zing_file_no_files(tmp_path: Path) -> Path:
    """Create a zing file with a plan that has no file references."""
    plan = Plan(
        stages=[
            Stage(
                label="S1",
                steps=[
                    Step(label="Step A", instructions="Do something.", files=[], done=True),
                ],
            ),
        ]
    )
    return _make_zing_file_with_plan(tmp_path, plan=plan)


# Mock Claude response for the grouping step -- valid <zing:audit> XML
MOCK_GROUPING_RESPONSE = """\
Here are the file groupings:

<zing:audit>
  <group>src/auth.py
src/models.py</group>
  <group>src/api.py</group>
  <group>tests/test_auth.py</group>
</zing:audit>
"""

# Mock Claude response for a review step with findings
MOCK_REVIEW_WITH_FINDINGS = """\
Here are my findings:

FINDING|Logic Errors and Bugs|high|high|src/auth.py:42|Null check missing -- req.user can be undefined when session expires
FINDING|Error Handling|medium|medium|src/auth.py:15|Exception is caught but silently swallowed -- should at least log a warning
FINDING|Security|critical|high|src/auth.py:78|Password comparison uses == instead of constant-time comparison
"""

# Mock Claude response for a review step with no findings
MOCK_REVIEW_NO_FINDINGS = """\
After reviewing the code, I found no issues.

NO_FINDINGS
"""

# Mock Claude response for a review step with a single finding
MOCK_REVIEW_SINGLE_FINDING = """\
FINDING|Performance|low|medium|src/api.py:20|Could use list comprehension instead of loop for better readability
"""


# ---------------------------------------------------------------------------
# parse_review_findings tests
# ---------------------------------------------------------------------------


class TestParseReviewFindings:
    """Tests for the FINDING| line parser."""

    def test_parses_multiple_findings(self) -> None:
        findings = parse_review_findings(MOCK_REVIEW_WITH_FINDINGS)
        assert len(findings) == 3

    def test_finding_fields_parsed_correctly(self) -> None:
        findings = parse_review_findings(MOCK_REVIEW_WITH_FINDINGS)
        f = findings[0]
        assert f.category == "Logic Errors and Bugs"
        assert f.severity == "high"
        assert f.confidence == "high"
        assert f.location == "src/auth.py:42"
        assert "Null check missing" in f.title

    def test_finding_indexes_sequential(self) -> None:
        findings = parse_review_findings(MOCK_REVIEW_WITH_FINDINGS)
        assert [f.index for f in findings] == [0, 1, 2]

    def test_start_index_offset(self) -> None:
        findings = parse_review_findings(MOCK_REVIEW_WITH_FINDINGS, start_index=10)
        assert [f.index for f in findings] == [10, 11, 12]

    def test_no_findings_returns_empty(self) -> None:
        findings = parse_review_findings(MOCK_REVIEW_NO_FINDINGS)
        assert findings == []

    def test_empty_text_returns_empty(self) -> None:
        findings = parse_review_findings("")
        assert findings == []

    def test_text_without_findings_returns_empty(self) -> None:
        findings = parse_review_findings("Some random text with no FINDING lines.")
        assert findings == []

    def test_single_finding(self) -> None:
        findings = parse_review_findings(MOCK_REVIEW_SINGLE_FINDING)
        assert len(findings) == 1
        assert findings[0].category == "Performance"
        assert findings[0].severity == "low"

    def test_critical_severity_parsed(self) -> None:
        findings = parse_review_findings(MOCK_REVIEW_WITH_FINDINGS)
        critical = [f for f in findings if f.severity == "critical"]
        assert len(critical) == 1
        assert "Password comparison" in critical[0].title


# ---------------------------------------------------------------------------
# group_findings_by_severity tests
# ---------------------------------------------------------------------------


class TestGroupFindingsBySeverity:
    """Tests for severity-based grouping."""

    def test_groups_by_severity(self) -> None:
        findings = parse_review_findings(MOCK_REVIEW_WITH_FINDINGS)
        groups = group_findings_by_severity(findings)
        severities = [g.severity for g in groups]
        assert "critical" in severities
        assert "high" in severities
        assert "medium" in severities

    def test_severity_order(self) -> None:
        findings = parse_review_findings(MOCK_REVIEW_WITH_FINDINGS)
        groups = group_findings_by_severity(findings)
        severities = [g.severity for g in groups]
        # critical < high < medium (no low in this data)
        assert severities == ["critical", "high", "medium"]

    def test_empty_findings_returns_empty(self) -> None:
        groups = group_findings_by_severity([])
        assert groups == []

    def test_omits_empty_severity_groups(self) -> None:
        findings = [
            Finding(index=0, category="Bug", severity="low", confidence="high",
                    location="a.py:1", title="minor issue"),
        ]
        groups = group_findings_by_severity(findings)
        assert len(groups) == 1
        assert groups[0].severity == "low"

    def test_unknown_severity_bucketed_to_medium(self) -> None:
        findings = [
            Finding(index=0, category="Bug", severity="unknown", confidence="high",
                    location="a.py:1", title="weird issue"),
        ]
        groups = group_findings_by_severity(findings)
        assert len(groups) == 1
        assert groups[0].severity == "medium"
        assert len(groups[0].findings) == 1


# ---------------------------------------------------------------------------
# collect_plan_files tests
# ---------------------------------------------------------------------------


class TestCollectPlanFiles:
    """Tests for the plan file collector."""

    def test_collects_all_unique_files(self, tmp_path: Path) -> None:
        zing_path = _make_zing_file_with_plan(tmp_path)
        files = collect_plan_files(zing_path)
        # src/auth.py, src/api.py, src/models.py, tests/test_auth.py
        assert len(files) == 4
        assert "src/auth.py" in files
        assert "src/api.py" in files
        assert "src/models.py" in files
        assert "tests/test_auth.py" in files

    def test_deduplicates_files(self, tmp_path: Path) -> None:
        """src/models.py appears in two steps but should only appear once."""
        zing_path = _make_zing_file_with_plan(tmp_path)
        files = collect_plan_files(zing_path)
        assert files.count("src/models.py") == 1

    def test_returns_sorted(self, tmp_path: Path) -> None:
        zing_path = _make_zing_file_with_plan(tmp_path)
        files = collect_plan_files(zing_path)
        assert files == sorted(files)

    def test_no_plan_returns_empty(self, tmp_path: Path) -> None:
        zing_path = _make_zing_file_no_plan(tmp_path)
        files = collect_plan_files(zing_path)
        assert files == []

    def test_no_files_in_plan(self, tmp_path: Path) -> None:
        zing_path = _make_zing_file_no_files(tmp_path)
        files = collect_plan_files(zing_path)
        assert files == []


# ---------------------------------------------------------------------------
# run_build_audit tests -- full pipeline
# ---------------------------------------------------------------------------


class TestRunBuildAuditFullPipeline:
    """Tests for the full build-audit pipeline with mocked Claude subprocess."""

    def test_full_pipeline_with_findings(self, tmp_path: Path) -> None:
        """Happy path: groups files, reviews in parallel, collects findings."""
        zing_path = _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        # Create referenced files so they pass the exists check
        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        web_server_calls: list[dict] = []

        def mock_start_web(zing_file_path, *, port, no_browser, finding_groups=None):
            web_server_calls.append({
                "zing_file_path": zing_file_path,
                "port": port,
                "no_browser": no_browser,
                "finding_groups": finding_groups,
            })
            return MagicMock()

        # Track invoke_claude_full calls to return different review outputs
        review_call_count = 0

        async def mock_invoke_full(prompt, **kwargs):
            nonlocal review_call_count
            review_call_count += 1
            # Return different findings for different groups
            if review_call_count == 1:
                return (MOCK_REVIEW_WITH_FINDINGS, "session-1")
            elif review_call_count == 2:
                return (MOCK_REVIEW_SINGLE_FINDING, "session-2")
            else:
                return (MOCK_REVIEW_NO_FINDINGS, "session-3")

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                    new_callable=AsyncMock,
                    return_value=[
                        AuditGroup(files=["src/auth.py", "src/models.py"]),
                        AuditGroup(files=["src/api.py"]),
                        AuditGroup(files=["tests/test_auth.py"]),
                    ],
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_full",
                    side_effect=mock_invoke_full,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit._start_web_server_background",
                    side_effect=mock_start_web,
                ),
            ):
                await run_build_audit(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        # Web server should have been started
        assert len(web_server_calls) == 1
        finding_groups = web_server_calls[0]["finding_groups"]
        assert isinstance(finding_groups, list)

        # Should have findings from first two reviews (third had NO_FINDINGS)
        total = sum(len(g.findings) for g in finding_groups)
        assert total == 4  # 3 from first review + 1 from second

    def test_parallel_reviews_called_for_each_group(self, tmp_path: Path) -> None:
        """Each audit group should trigger a separate Claude review call."""
        zing_path = _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        # Create referenced files
        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        invoke_full_calls: list[dict] = []

        async def mock_invoke_full(prompt, **kwargs):
            invoke_full_calls.append({"prompt": prompt, **kwargs})
            return (MOCK_REVIEW_NO_FINDINGS, "session-x")

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                    new_callable=AsyncMock,
                    return_value=[
                        AuditGroup(files=["src/auth.py"]),
                        AuditGroup(files=["src/api.py"]),
                    ],
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_full",
                    side_effect=mock_invoke_full,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit._start_web_server_background",
                    return_value=MagicMock(),
                ),
            ):
                await run_build_audit(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        # Two audit groups -> two review calls
        assert len(invoke_full_calls) == 2

    def test_uses_audit_call_type(self, tmp_path: Path) -> None:
        """Both grouping and review should use CallType.AUDIT."""
        zing_path = _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        validated_kwargs: list[dict] = []
        full_kwargs: list[dict] = []

        async def mock_validated(prompt, validator, retry_prompt_template, **kwargs):
            validated_kwargs.append(kwargs)
            return [AuditGroup(files=["src/auth.py"])]

        async def mock_invoke_full(prompt, **kwargs):
            full_kwargs.append(kwargs)
            return (MOCK_REVIEW_NO_FINDINGS, "session-x")

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                    side_effect=mock_validated,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_full",
                    side_effect=mock_invoke_full,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit._start_web_server_background",
                    return_value=MagicMock(),
                ),
            ):
                await run_build_audit(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        assert validated_kwargs[0]["call_type"] == CallType.AUDIT
        assert full_kwargs[0]["call_type"] == CallType.AUDIT

    def test_skip_permissions_forwarded(self, tmp_path: Path) -> None:
        """skip_permissions should be forwarded to all Claude calls."""
        zing_path = _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        validated_kwargs: list[dict] = []
        full_kwargs: list[dict] = []

        async def mock_validated(prompt, validator, retry_prompt_template, **kwargs):
            validated_kwargs.append(kwargs)
            return [AuditGroup(files=["src/auth.py"])]

        async def mock_invoke_full(prompt, **kwargs):
            full_kwargs.append(kwargs)
            return (MOCK_REVIEW_NO_FINDINGS, "session-x")

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                    side_effect=mock_validated,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_full",
                    side_effect=mock_invoke_full,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit._start_web_server_background",
                    return_value=MagicMock(),
                ),
            ):
                await run_build_audit(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=True,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        assert validated_kwargs[0]["skip_permissions"] is True
        assert full_kwargs[0]["skip_permissions"] is True


# ---------------------------------------------------------------------------
# run_build_audit tests -- no files to audit
# ---------------------------------------------------------------------------


class TestRunBuildAuditNoFiles:
    """Tests for when the plan has no files to audit."""

    def test_no_plan_starts_empty_server(self, tmp_path: Path) -> None:
        """When there is no plan, web server should start with empty findings."""
        _make_zing_file_no_plan(tmp_path)
        config = ZingConfig()

        web_server_calls: list[dict] = []

        def mock_start_web(zing_file_path, *, port, no_browser, finding_groups=None):
            web_server_calls.append({"finding_groups": finding_groups})
            return MagicMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build_audit._start_web_server_background",
                    side_effect=mock_start_web,
                ),
            ):
                await run_build_audit(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        assert len(web_server_calls) == 1
        assert web_server_calls[0]["finding_groups"] == []

    def test_no_files_in_plan_starts_empty_server(self, tmp_path: Path) -> None:
        """When the plan has no file references, server starts with empty findings."""
        _make_zing_file_no_files(tmp_path)
        config = ZingConfig()

        web_server_calls: list[dict] = []

        def mock_start_web(zing_file_path, *, port, no_browser, finding_groups=None):
            web_server_calls.append({"finding_groups": finding_groups})
            return MagicMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build_audit._start_web_server_background",
                    side_effect=mock_start_web,
                ),
            ):
                await run_build_audit(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        assert len(web_server_calls) == 1
        assert web_server_calls[0]["finding_groups"] == []


# ---------------------------------------------------------------------------
# run_build_audit tests -- distillation
# ---------------------------------------------------------------------------


class TestRunBuildAuditDistillation:
    """Tests for file distillation in the build-audit pipeline."""

    def test_distills_plan_files(self, tmp_path: Path) -> None:
        """All plan files that exist on disk should be distilled."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        # Create some of the referenced files
        for f in ["src/auth.py", "src/models.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        distill_calls: list[list[Path]] = []

        async def mock_distill(file_paths, *, project_root):
            distill_calls.append(file_paths)
            return {fp: f"distilled:{fp.name}" for fp in file_paths}

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                    new_callable=AsyncMock,
                    return_value=[AuditGroup(files=["src/auth.py"])],
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_full",
                    new_callable=AsyncMock,
                    return_value=(MOCK_REVIEW_NO_FINDINGS, "session"),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.distill_files",
                    side_effect=mock_distill,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit._start_web_server_background",
                    return_value=MagicMock(),
                ),
            ):
                await run_build_audit(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        assert len(distill_calls) == 1
        # Only existing files should be distilled (2 out of 4)
        assert len(distill_calls[0]) == 2

    def test_skips_nonexistent_files(self, tmp_path: Path) -> None:
        """Files that don't exist on disk should be skipped during distillation."""
        plan = Plan(
            stages=[
                Stage(
                    label="S1",
                    steps=[
                        Step(
                            label="Step A",
                            instructions="Do A.",
                            files=["src/exists.py", "src/missing.py"],
                            done=True,
                        ),
                    ],
                ),
            ]
        )
        _make_zing_file_with_plan(tmp_path, plan=plan)
        config = ZingConfig()

        (tmp_path / "src").mkdir(parents=True, exist_ok=True)
        (tmp_path / "src" / "exists.py").write_text("# exists")

        distill_calls: list[list[Path]] = []

        async def mock_distill(file_paths, *, project_root):
            distill_calls.append(file_paths)
            return {fp: f"distilled:{fp.name}" for fp in file_paths}

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                    new_callable=AsyncMock,
                    return_value=[AuditGroup(files=["src/exists.py"])],
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_full",
                    new_callable=AsyncMock,
                    return_value=(MOCK_REVIEW_NO_FINDINGS, "session"),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.distill_files",
                    side_effect=mock_distill,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit._start_web_server_background",
                    return_value=MagicMock(),
                ),
            ):
                await run_build_audit(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        assert len(distill_calls) == 1
        assert len(distill_calls[0]) == 1
        assert distill_calls[0][0].name == "exists.py"


# ---------------------------------------------------------------------------
# run_build_audit tests -- web server
# ---------------------------------------------------------------------------


class TestRunBuildAuditWebServer:
    """Tests for web server startup and state."""

    def test_web_server_started_with_findings(self, tmp_path: Path) -> None:
        """The web server should be started with finding_groups on app state."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        web_server_calls: list[dict] = []

        def mock_start_web(zing_file_path, *, port, no_browser, finding_groups=None):
            web_server_calls.append({
                "port": port,
                "no_browser": no_browser,
                "finding_groups": finding_groups,
            })
            return MagicMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                    new_callable=AsyncMock,
                    return_value=[AuditGroup(files=["src/auth.py"])],
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_full",
                    new_callable=AsyncMock,
                    return_value=(MOCK_REVIEW_WITH_FINDINGS, "session"),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit._start_web_server_background",
                    side_effect=mock_start_web,
                ),
            ):
                await run_build_audit(
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
        finding_groups = web_server_calls[0]["finding_groups"]
        assert isinstance(finding_groups, list)
        assert len(finding_groups) > 0

    def test_web_server_port_from_config(self, tmp_path: Path) -> None:
        """The web server should use the port from config."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()
        config.port = 9999

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        web_server_calls: list[dict] = []

        def mock_start_web(zing_file_path, *, port, no_browser, finding_groups=None):
            web_server_calls.append({"port": port})
            return MagicMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                    new_callable=AsyncMock,
                    return_value=[AuditGroup(files=["src/auth.py"])],
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_full",
                    new_callable=AsyncMock,
                    return_value=(MOCK_REVIEW_NO_FINDINGS, "session"),
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit._start_web_server_background",
                    side_effect=mock_start_web,
                ),
            ):
                await run_build_audit(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        assert web_server_calls[0]["port"] == 9999


# ---------------------------------------------------------------------------
# run_build_audit tests -- finding indexes are globally unique
# ---------------------------------------------------------------------------


class TestRunBuildAuditFindingIndexes:
    """Tests that finding indexes are globally unique across groups."""

    def test_finding_indexes_unique_across_groups(self, tmp_path: Path) -> None:
        """Findings from different groups should have unique, sequential indexes."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        call_count = 0

        async def mock_invoke_full(prompt, **kwargs):
            nonlocal call_count
            call_count += 1
            if call_count == 1:
                return (MOCK_REVIEW_WITH_FINDINGS, "s1")  # 3 findings (0,1,2)
            else:
                return (MOCK_REVIEW_SINGLE_FINDING, "s2")  # 1 finding (3)

        web_server_calls: list[dict] = []

        def mock_start_web(zing_file_path, *, port, no_browser, finding_groups=None):
            web_server_calls.append({"finding_groups": finding_groups})
            return MagicMock()

        async def _test() -> None:
            with (
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                    new_callable=AsyncMock,
                    return_value=[
                        AuditGroup(files=["src/auth.py"]),
                        AuditGroup(files=["src/api.py"]),
                    ],
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_full",
                    side_effect=mock_invoke_full,
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit.distill_files",
                    return_value={},
                ),
                patch(
                    "zing_ai.orchestrator.commands.build_audit._start_web_server_background",
                    side_effect=mock_start_web,
                ),
            ):
                await run_build_audit(
                    zing_file="test-project.xml",
                    no_browser=True,
                    skip_permissions=False,
                    config=config,
                    project_root=tmp_path,
                )

        _run(_test())

        # Collect all finding indexes
        all_indexes = []
        for group in web_server_calls[0]["finding_groups"]:
            for f in group.findings:
                all_indexes.append(f.index)

        # Should be unique
        assert len(all_indexes) == len(set(all_indexes))
        # Should be sequential 0,1,2,3
        assert sorted(all_indexes) == [0, 1, 2, 3]
