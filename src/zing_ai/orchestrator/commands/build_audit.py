"""Orchestrator ``build-audit`` command -- audit build output.

Groups changed files and runs parallel audit subprocesses to review them,
then displays findings via Rich inline menus.

Pipeline:

1. Collect all files referenced in the plan's steps.
2. Distill those files via ``distiller.distill_files()``.
3. Render ``build_audit_group.md.j2`` and invoke Claude to partition files
   into audit groups (parsed via ``xml_parser.parse_audit_response()``).
4. **Phase 1 (investigation):** Run parallel review Claude calls per group
   via ``run_parallel_investigations()`` with Rich Live spinners.
5. **Phase 2 (triage):** Present findings grouped by severity via
   ``audit_triage_menu()`` for user triage decisions.
6. Handle user action decisions from the returned ``list[AuditDecision]``.
"""

from __future__ import annotations

import logging
import re
from dataclasses import dataclass, field
from pathlib import Path

import jinja2

from zing_ai.orchestrator import claude, project
from zing_ai.orchestrator.claude import print_line
from zing_ai.orchestrator.config import CallType, ZingConfig, resolve_aid_path
from zing_ai.orchestrator.distiller import distill_files
from zing_ai.orchestrator.errors import PipelineError
from zing_ai.orchestrator.models import AuditGroup
from zing_ai.orchestrator.xml_parser import (
    ValidationError,
    parse_audit_response,
    parse_zing_file,
)
from zing_ai.prompts import render_prompt

logger = logging.getLogger(__name__)

# Simple retry template for invoke_claude_validated.
_RETRY_TEMPLATE = jinja2.Template(
    "Your previous response was invalid: {{ error }}. "
    "Please produce a corrected response following the original instructions."
)

# Regex for parsing ``FINDING|...`` lines from the review output.
_FINDING_RE = re.compile(
    r"^FINDING\|"
    r"(?P<category>[^|]+)\|"
    r"(?P<severity>[^|]+)\|"
    r"(?P<confidence>[^|]+)\|"
    r"(?P<location>[^|]+)\|"
    r"(?P<description>.+)$",
    re.MULTILINE,
)


# ---------------------------------------------------------------------------
# Data structures for parsed findings
# ---------------------------------------------------------------------------


@dataclass
class Finding:
    """A single audit finding parsed from review output."""

    index: int
    category: str
    severity: str
    confidence: str
    location: str
    title: str  # Human-readable summary (derived from description)


@dataclass
class FindingGroup:
    """A group of findings at a given severity level for template rendering."""

    severity: str
    findings: list[Finding] = field(default_factory=list)


# ---------------------------------------------------------------------------
# Parsing helpers
# ---------------------------------------------------------------------------


def parse_review_findings(text: str, *, start_index: int = 0) -> list[Finding]:
    """Parse ``FINDING|...`` lines from a review response.

    Parameters
    ----------
    text:
        The raw review response text from Claude.
    start_index:
        The starting index for numbering findings (for global uniqueness
        across multiple review groups).

    Returns
    -------
    list[Finding]
        Parsed findings.  Returns an empty list if the response contains
        ``NO_FINDINGS`` or has no matching lines.
    """
    if "NO_FINDINGS" in text:
        return []

    findings: list[Finding] = []
    for match in _FINDING_RE.finditer(text):
        findings.append(
            Finding(
                index=start_index + len(findings),
                category=match.group("category").strip(),
                severity=match.group("severity").strip(),
                confidence=match.group("confidence").strip(),
                location=match.group("location").strip(),
                title=match.group("description").strip(),
            )
        )
    return findings


def group_findings_by_severity(findings: list[Finding]) -> list[FindingGroup]:
    """Group findings by severity for template rendering.

    Returns groups ordered by severity: critical, high, medium, low.
    Only includes groups that have at least one finding.

    Parameters
    ----------
    findings:
        Flat list of all findings.

    Returns
    -------
    list[FindingGroup]
        Grouped findings, ordered by decreasing severity.
    """
    severity_order = ["critical", "high", "medium", "low"]
    by_severity: dict[str, list[Finding]] = {s: [] for s in severity_order}

    for finding in findings:
        key = finding.severity.lower()
        if key in by_severity:
            by_severity[key].append(finding)
        else:
            # Unknown severity -- bucket under "medium"
            by_severity["medium"].append(finding)

    groups: list[FindingGroup] = []
    for severity in severity_order:
        if by_severity[severity]:
            groups.append(FindingGroup(severity=severity, findings=by_severity[severity]))

    return groups


# ---------------------------------------------------------------------------
# File collection helper
# ---------------------------------------------------------------------------


def collect_plan_files(doc_path: Path) -> list[str]:
    """Collect all unique file paths referenced across a plan's steps.

    Reads the zing document at *doc_path* and iterates through every stage
    and step, gathering file references into a deduplicated, sorted list.

    Parameters
    ----------
    doc_path:
        Path to the zing XML file.

    Returns
    -------
    list[str]
        Sorted, deduplicated list of relative file paths from the plan.
    """
    doc = parse_zing_file(doc_path)
    if doc.plan is None:
        return []

    all_files: set[str] = set()
    for stage in doc.plan.stages:
        for step in stage.steps:
            all_files.update(step.files)
    return sorted(all_files)


# ---------------------------------------------------------------------------
# Main entry point
# ---------------------------------------------------------------------------


def run_build_audit(
    *,
    zing_file: str | None,
    skip_permissions: bool,
    config: ZingConfig,
    project_root: Path,
) -> None:
    """Run the ``build-audit`` orchestrator command.

    Collects files referenced in the plan, distills them, groups them
    via a Claude call, then runs a two-phase inline flow:

    * **Phase 1 (investigation):** ``run_parallel_investigations()``
      runs parallel review Claude calls per audit group with Rich Live
      spinners.
    * **Phase 2 (triage):** ``audit_triage_menu()`` presents grouped
      findings for user triage, returning ``list[AuditDecision]``.

    Parameters
    ----------
    zing_file:
        Optional zing file name to audit.
    skip_permissions:
        If ``True``, pass ``--dangerously-skip-permissions`` to all Claude
        calls.
    config:
        Parsed ``.zing.toml`` configuration.
    project_root:
        Path to the project root directory.
    """
    # Resolve the aid binary path (fail fast if missing)
    aid_path = resolve_aid_path(config)

    # Resolve the zing file
    zing_path = project.resolve_zing_file(zing_file, project_root)
    logger.info("Running build-audit with zing file: %s", zing_path)

    doc = parse_zing_file(zing_path)
    zing_content = doc.content or ""

    # --- Step 1: Collect all files referenced in the plan ---
    logger.info("Build-audit Step 1: Collecting plan files")
    plan_file_list = collect_plan_files(zing_path)

    if not plan_file_list:
        logger.warning("No files referenced in plan; nothing to audit")
        return

    logger.info("Collected %d unique files from plan", len(plan_file_list))

    # Resolve to absolute paths, filtering to existing files
    plan_file_paths = [
        project_root / f for f in plan_file_list if (project_root / f).is_file()
    ]

    # --- Step 2: Distill all plan files ---
    logger.info("Build-audit Step 2: Distilling %d files", len(plan_file_paths))
    distilled: dict[Path, str] = {}
    if plan_file_paths:
        distilled = distill_files(plan_file_paths, project_root=project_root, aid_path=aid_path)

    # Convert to string-keyed dict for templates
    distilled_files: dict[str, str] = {
        str(fp.relative_to(project_root)): content for fp, content in distilled.items()
    }
    logger.info("Distilled %d files", len(distilled_files))

    zing_dir = project.ensure_zing_dir(project_root)

    # --- Step 3: Group files via Claude ---
    logger.info("Build-audit Step 3: Grouping files via Claude")
    group_prompt = render_prompt(
        "build_audit_group.md.j2",
        file_list=plan_file_list,
        distilled_files=distilled_files,
    )

    try:
        audit_groups: list[AuditGroup] = claude.invoke_claude_validated(
            group_prompt,
            validator=parse_audit_response,
            retry_prompt_template=_RETRY_TEMPLATE,
            on_output=print_line,
            zing_dir=zing_dir,
            call_type=CallType.AUDIT,
            config=config,
            skip_permissions=skip_permissions,
        )
    except ValidationError as exc:
        raise PipelineError(
            stage="build-audit",
            message=f"Audit grouping failed after max retries: {exc}",
        ) from exc

    logger.info("Claude grouped files into %d audit groups", len(audit_groups))
    for i, group in enumerate(audit_groups):
        logger.debug("  Group %d: %d files -- %s", i + 1, len(group.files), group.files)

    # --- Phase 1: Investigation via run_parallel_investigations ---
    # Deferred imports to avoid circular dependency (ui.menus imports
    # Finding/FindingGroup from this module).
    from zing_ai.orchestrator.ui import console
    from zing_ai.orchestrator.ui.menus import audit_triage_menu
    from zing_ai.orchestrator.ui.progress import run_parallel_investigations
    from zing_ai.orchestrator.ui.types import AuditDecision, InvestigationEntry, InvestigationResult

    logger.info(
        "Build-audit Phase 1: Running parallel reviews for %d groups",
        len(audit_groups),
    )

    # Build investigation entries from audit groups
    entries: list[InvestigationEntry] = []
    for i, group in enumerate(audit_groups):
        file_summary = ", ".join(group.files[:3])
        if len(group.files) > 3:
            file_summary += f" (+{len(group.files) - 3} more)"
        entries.append(InvestigationEntry(id=f"review-{i}", label=f"Review: {file_summary}"))

    # Build a run_fn that maps entry IDs to Claude review calls
    def _review_run_fn(entry_id: str) -> str:
        idx = int(entry_id.split("-")[1])
        group = audit_groups[idx]

        # Build distilled code context for this group's files
        group_distilled: dict[str, str] = {}
        for f in group.files:
            if f in distilled_files:
                group_distilled[f] = distilled_files[f]

        review_prompt = render_prompt(
            "build_audit_review.md.j2",
            zing_content=zing_content,
            group_files=group.files,
            distilled_code=group_distilled,
        )

        output, _session_id = claude.invoke_claude_full(
            review_prompt,
            on_output=lambda line: None,
            zing_dir=zing_dir,
            call_type=CallType.AUDIT,
            config=config,
            skip_permissions=skip_permissions,
        )

        logger.info(
            "Review group %d completed (%d chars)",
            idx + 1,
            len(output),
        )
        return output

    investigation_result: InvestigationResult = run_parallel_investigations(
        entries=entries,
        run_fn=_review_run_fn,
    )

    # Gather results in original group order
    review_outputs: list[str] = []
    for entry in entries:
        review_outputs.append(investigation_result.outputs.get(entry["id"], ""))

    # --- Parse and collect findings ---
    logger.info("Build-audit: Parsing findings from %d review outputs", len(review_outputs))
    all_findings: list[Finding] = []
    for output in review_outputs:
        group_findings = parse_review_findings(output, start_index=len(all_findings))
        all_findings.extend(group_findings)

    logger.info("Collected %d total findings", len(all_findings))

    # Group findings by severity
    finding_groups = group_findings_by_severity(all_findings)
    logger.info(
        "Findings by severity: %s",
        {g.severity: len(g.findings) for g in finding_groups},
    )

    # --- Phase 2: Triage findings via audit_triage_menu ---
    logger.info("Build-audit Phase 2: Presenting %d finding groups", len(finding_groups))

    decisions: list[AuditDecision] = audit_triage_menu(finding_groups)

    # --- Handle user decisions ---
    logger.info("Build-audit: Processing %d user decisions", len(decisions))
    for decision in decisions:
        action = decision.get("action", "skip")
        severity = decision.get("severity", "unknown")
        finding_index = decision.get("finding_index", -1)
        logger.info(
            "Decision for finding %d (%s): %s",
            finding_index,
            severity,
            action,
        )

    # --- Write action files ---
    # Build a lookup from finding index to Finding for confidence/location.
    finding_by_index: dict[int, Finding] = {f.index: f for f in all_findings}

    # Group decisions by action
    fix_decisions = [d for d in decisions if d.get("action") == "fix"]
    discuss_decisions = [d for d in decisions if d.get("action") == "discuss"]

    zing_file_stem = zing_path.stem

    def _write_findings_markdown(
        path: Path,
        title: str,
        action_decisions: list[AuditDecision],
    ) -> None:
        """Write a markdown file with one heading per finding."""
        lines: list[str] = [f"# {title}", ""]
        for i, dec in enumerate(action_decisions, 1):
            fidx = dec.get("finding_index", -1)
            finding = finding_by_index.get(fidx)
            confidence = finding.confidence if finding else "unknown"
            location = finding.location if finding else "unknown"

            lines.append(f"## Finding {i}: {dec.get('title', 'Untitled')}")
            lines.append("")
            lines.append(f"- **Category:** {dec.get('category', 'unknown')}")
            lines.append(f"- **Severity:** {dec.get('severity', 'unknown')}")
            lines.append(f"- **Confidence:** {confidence}")
            lines.append(f"- **Location:** {location}")
            lines.append("")
        path.write_text("\n".join(lines))

    if fix_decisions:
        fix_path = zing_dir / f"build-audit-fixes-{zing_file_stem}.md"
        _write_findings_markdown(fix_path, "Build Audit Fixes", fix_decisions)
        logger.info("Wrote %d fix findings to %s", len(fix_decisions), fix_path)
        console.print(f"[green]Wrote {len(fix_decisions)} fix finding(s) to {fix_path.name}[/green]")

    if discuss_decisions:
        discuss_path = zing_dir / f"build-audit-discuss-{zing_file_stem}.md"
        _write_findings_markdown(discuss_path, "Build Audit Discuss", discuss_decisions)
        logger.info("Wrote %d discuss findings to %s", len(discuss_decisions), discuss_path)
        console.print(f"[yellow]Wrote {len(discuss_decisions)} discuss finding(s) to {discuss_path.name}[/yellow]")

    skip_count = len([d for d in decisions if d.get("action") == "skip"])
    if skip_count:
        logger.info("Skipped %d findings", skip_count)
        console.print(f"[dim]Skipped {skip_count} finding(s)[/dim]")


