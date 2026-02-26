"""Tests for the orchestrator ``build-audit`` command."""

from __future__ import annotations

from pathlib import Path
from unittest.mock import MagicMock, patch

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
from zing_ai.orchestrator.ui.types import AuditDecision, InvestigationResult
from zing_ai.orchestrator.xml_parser import write_zing_file

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


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
# run_build_audit tests -- full pipeline (two-phase TUI)
# ---------------------------------------------------------------------------


class TestRunBuildAuditFullPipeline:
    """Tests for the full build-audit pipeline with mocked UI.

    Phase 1 is mocked via ``run_parallel_investigations`` returning an
    ``InvestigationResult``.  Phase 2 is mocked via
    ``audit_triage_menu`` returning ``list[AuditDecision]``.
    """

    def test_full_pipeline_with_findings(self, tmp_path: Path) -> None:
        """Happy path: groups files, reviews via investigations, triages via menu."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        # Create referenced files so they pass the exists check
        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        # Phase 1 mock: run_parallel_investigations returns InvestigationResult
        mock_investigation_result = InvestigationResult(
            outputs={
                "review-0": MOCK_REVIEW_WITH_FINDINGS,
                "review-1": MOCK_REVIEW_SINGLE_FINDING,
                "review-2": MOCK_REVIEW_NO_FINDINGS,
            },
            statuses={"review-0": "success", "review-1": "success", "review-2": "success"},
        )

        # Phase 2 mock: audit_triage_menu returns list[AuditDecision]
        mock_decisions: list[AuditDecision] = [
            AuditDecision(finding_index=2, category="Security", severity="critical", title="Password comparison", action="fix"),
            AuditDecision(finding_index=0, category="Logic Errors and Bugs", severity="high", title="Null check missing", action="fix"),
            AuditDecision(finding_index=1, category="Error Handling", severity="medium", title="Exception swallowed", action="skip"),
            AuditDecision(finding_index=3, category="Performance", severity="low", title="List comprehension", action="skip"),
        ]

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=[
                    AuditGroup(files=["src/auth.py", "src/models.py"]),
                    AuditGroup(files=["src/api.py"]),
                    AuditGroup(files=["tests/test_auth.py"]),
                ],
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation_result,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                return_value=mock_decisions,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={},
            ),
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

    def test_phase1_returns_investigation_result(self, tmp_path: Path) -> None:
        """Phase 1 should call run_parallel_investigations and use its result."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        mock_investigation = InvestigationResult(
            outputs={"review-0": MOCK_REVIEW_NO_FINDINGS},
            statuses={"review-0": "success"},
        )

        run_investigations_mock = MagicMock(
            return_value=mock_investigation,
        )

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=[AuditGroup(files=["src/auth.py"])],
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                run_investigations_mock,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                return_value=[],
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={},
            ),
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # run_parallel_investigations should have been called exactly once
        run_investigations_mock.assert_called_once()
        call_kwargs = run_investigations_mock.call_args.kwargs
        assert len(call_kwargs["entries"]) == 1
        assert call_kwargs["entries"][0]["id"] == "review-0"
        assert callable(call_kwargs["run_fn"])

    def test_phase2_audit_triage_menu_receives_finding_groups(self, tmp_path: Path) -> None:
        """Phase 2 should pass grouped findings to audit_triage_menu."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        mock_investigation = InvestigationResult(
            outputs={"review-0": MOCK_REVIEW_WITH_FINDINGS},
            statuses={"review-0": "success"},
        )

        # Capture the finding_groups passed to audit_triage_menu
        triage_calls: list = []

        def mock_triage(finding_groups):
            triage_calls.append(finding_groups)
            return [
                AuditDecision(finding_index=2, category="Security", severity="critical", title="Password comparison", action="fix"),
                AuditDecision(finding_index=0, category="Logic Errors and Bugs", severity="high", title="Null check", action="skip"),
                AuditDecision(finding_index=1, category="Error Handling", severity="medium", title="Exception swallowed", action="skip"),
            ]

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=[AuditGroup(files=["src/auth.py"])],
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                side_effect=mock_triage,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={},
            ),
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        # audit_triage_menu should have been called with finding groups
        assert len(triage_calls) == 1
        finding_groups = triage_calls[0]
        # The finding groups should cover 3 findings
        total_findings = sum(len(g.findings) for g in finding_groups)
        assert total_findings == 3
        # Verify severity order: critical, high, medium
        severities = [g.severity for g in finding_groups]
        assert severities == ["critical", "high", "medium"]

    def test_uses_audit_call_type(self, tmp_path: Path) -> None:
        """Grouping should use CallType.AUDIT."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        validated_kwargs: list[dict] = []

        def mock_validated(prompt, validator, retry_prompt_template, **kwargs):
            validated_kwargs.append(kwargs)
            return [AuditGroup(files=["src/auth.py"])]

        mock_investigation = InvestigationResult(outputs={}, statuses={})

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                side_effect=mock_validated,
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                return_value=[],
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={},
            ),
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert validated_kwargs[0]["call_type"] == CallType.AUDIT

    def test_skip_permissions_forwarded(self, tmp_path: Path) -> None:
        """skip_permissions should be forwarded to grouping call."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        validated_kwargs: list[dict] = []

        def mock_validated(prompt, validator, retry_prompt_template, **kwargs):
            validated_kwargs.append(kwargs)
            return [AuditGroup(files=["src/auth.py"])]

        mock_investigation = InvestigationResult(outputs={}, statuses={})

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                side_effect=mock_validated,
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                return_value=[],
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={},
            ),
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=True,
                config=config,
                project_root=tmp_path,
            )

        # Grouping call should have skip_permissions=True
        assert validated_kwargs[0]["skip_permissions"] is True


# ---------------------------------------------------------------------------
# run_build_audit tests -- finding grouping logic
# ---------------------------------------------------------------------------


class TestRunBuildAuditFindingGrouping:
    """Tests that findings are correctly grouped by severity before audit_triage_menu."""

    def test_findings_grouped_by_severity(self, tmp_path: Path) -> None:
        """Findings from review outputs should be grouped by severity."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        mock_investigation = InvestigationResult(
            outputs={
                "review-0": MOCK_REVIEW_WITH_FINDINGS,
                "review-1": MOCK_REVIEW_SINGLE_FINDING,
            },
            statuses={"review-0": "success", "review-1": "success"},
        )

        # Capture the finding_groups passed to audit_triage_menu
        triage_calls: list = []

        def mock_triage(finding_groups):
            triage_calls.append(finding_groups)
            return []

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=[
                    AuditGroup(files=["src/auth.py"]),
                    AuditGroup(files=["src/api.py"]),
                ],
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                side_effect=mock_triage,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={},
            ),
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert len(triage_calls) == 1
        finding_groups = triage_calls[0]

        # MOCK_REVIEW_WITH_FINDINGS has: 1 high, 1 medium, 1 critical
        # MOCK_REVIEW_SINGLE_FINDING has: 1 low
        # Expect 4 groups: critical, high, medium, low
        severities = [g.severity for g in finding_groups]
        assert severities == ["critical", "high", "medium", "low"]

    def test_no_findings_yields_empty_groups(self, tmp_path: Path) -> None:
        """When all reviews produce NO_FINDINGS, audit_triage_menu gets empty groups."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        mock_investigation = InvestigationResult(
            outputs={"review-0": MOCK_REVIEW_NO_FINDINGS},
            statuses={"review-0": "success"},
        )

        triage_calls: list = []

        def mock_triage(finding_groups):
            triage_calls.append(finding_groups)
            return []

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=[AuditGroup(files=["src/auth.py"])],
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                side_effect=mock_triage,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={},
            ),
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert len(triage_calls) == 1
        assert triage_calls[0] == []

    def test_finding_indexes_unique_across_groups(self, tmp_path: Path) -> None:
        """Findings from different review outputs should have unique, sequential indexes."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        mock_investigation = InvestigationResult(
            outputs={
                "review-0": MOCK_REVIEW_WITH_FINDINGS,
                "review-1": MOCK_REVIEW_SINGLE_FINDING,
            },
            statuses={"review-0": "success", "review-1": "success"},
        )

        triage_calls: list = []

        def mock_triage(finding_groups):
            triage_calls.append(finding_groups)
            return []

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=[
                    AuditGroup(files=["src/auth.py"]),
                    AuditGroup(files=["src/api.py"]),
                ],
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                side_effect=mock_triage,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={},
            ),
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert len(triage_calls) == 1
        all_indexes = []
        for group in triage_calls[0]:
            for finding in group.findings:
                all_indexes.append(finding.index)
        # 3 from MOCK_REVIEW_WITH_FINDINGS (0,1,2) + 1 from MOCK_REVIEW_SINGLE_FINDING (3)
        assert sorted(all_indexes) == [0, 1, 2, 3]
        assert len(all_indexes) == len(set(all_indexes))  # all unique


# ---------------------------------------------------------------------------
# run_build_audit tests -- action dispatch from AuditResult
# ---------------------------------------------------------------------------


class TestRunBuildAuditActionDispatch:
    """Tests that user decisions from audit_triage_menu are processed correctly."""

    def test_decisions_logged(self, tmp_path: Path) -> None:
        """User decisions from audit_triage_menu should be processed."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        mock_investigation = InvestigationResult(
            outputs={"review-0": MOCK_REVIEW_WITH_FINDINGS},
            statuses={"review-0": "success"},
        )
        mock_decisions: list[AuditDecision] = [
            AuditDecision(finding_index=0, category="Security", severity="critical", title="Issue A", action="fix"),
            AuditDecision(finding_index=1, category="Logic", severity="high", title="Issue B", action="skip"),
            AuditDecision(finding_index=2, category="Error Handling", severity="medium", title="Issue C", action="discuss"),
        ]

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=[AuditGroup(files=["src/auth.py"])],
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                return_value=mock_decisions,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={},
            ),
        ):
            # Should not raise -- decisions are processed via logging
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

    def test_empty_decisions_handled(self, tmp_path: Path) -> None:
        """When audit_triage_menu returns no decisions, the command completes normally."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        mock_investigation = InvestigationResult(
            outputs={"review-0": MOCK_REVIEW_NO_FINDINGS},
            statuses={"review-0": "success"},
        )

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=[AuditGroup(files=["src/auth.py"])],
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                return_value=[],
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={},
            ),
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

    def test_fix_action_dispatched(self, tmp_path: Path) -> None:
        """Decisions with action='fix' should be recorded in the result."""
        _make_zing_file_with_plan(tmp_path)
        config = ZingConfig()

        for f in ["src/auth.py", "src/models.py", "src/api.py", "tests/test_auth.py"]:
            fp = tmp_path / f
            fp.parent.mkdir(parents=True, exist_ok=True)
            fp.write_text(f"# {f}")

        mock_investigation = InvestigationResult(
            outputs={"review-0": MOCK_REVIEW_WITH_FINDINGS},
            statuses={"review-0": "success"},
        )

        fix_decisions: list[AuditDecision] = [
            AuditDecision(finding_index=0, category="Security", severity="critical", title="Issue A", action="fix"),
            AuditDecision(finding_index=1, category="Logic", severity="high", title="Issue B", action="fix"),
        ]

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=[AuditGroup(files=["src/auth.py"])],
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                return_value=fix_decisions,
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                return_value={},
            ),
        ):
            # The command should complete without error, processing fix actions
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )


# ---------------------------------------------------------------------------
# run_build_audit tests -- no files to audit
# ---------------------------------------------------------------------------


class TestRunBuildAuditNoFiles:
    """Tests for when the plan has no files to audit."""

    def test_no_plan_returns_early(self, tmp_path: Path) -> None:
        """When there is no plan, run_build_audit should return early."""
        _make_zing_file_no_plan(tmp_path)
        config = ZingConfig()

        with patch(
            "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
            return_value="aid",
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

    def test_no_files_in_plan_returns_early(self, tmp_path: Path) -> None:
        """When the plan has no file references, run_build_audit should return early."""
        _make_zing_file_no_files(tmp_path)
        config = ZingConfig()

        with patch(
            "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
            return_value="aid",
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )


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

        def mock_distill(file_paths, *, project_root, aid_path="aid"):
            distill_calls.append(file_paths)
            return {fp: f"distilled:{fp.name}" for fp in file_paths}

        mock_investigation = InvestigationResult(outputs={}, statuses={})

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=[AuditGroup(files=["src/auth.py"])],
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                return_value=[],
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                side_effect=mock_distill,
            ),
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

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

        def mock_distill(file_paths, *, project_root, aid_path="aid"):
            distill_calls.append(file_paths)
            return {fp: f"distilled:{fp.name}" for fp in file_paths}

        mock_investigation = InvestigationResult(outputs={}, statuses={})

        with (
            patch(
                "zing_ai.orchestrator.commands.build_audit.claude.invoke_claude_validated",
                return_value=[AuditGroup(files=["src/exists.py"])],
            ),
            patch(
                "zing_ai.orchestrator.ui.progress.run_parallel_investigations",
                return_value=mock_investigation,
            ),
            patch(
                "zing_ai.orchestrator.ui.menus.audit_triage_menu",
                return_value=[],
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.resolve_aid_path",
                return_value="aid",
            ),
            patch(
                "zing_ai.orchestrator.commands.build_audit.distill_files",
                side_effect=mock_distill,
            ),
        ):
            run_build_audit(
                zing_file="test-project.xml",
                skip_permissions=False,
                config=config,
                project_root=tmp_path,
            )

        assert len(distill_calls) == 1
        assert len(distill_calls[0]) == 1
        assert distill_calls[0][0].name == "exists.py"
