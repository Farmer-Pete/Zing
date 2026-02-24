"""Orchestrator ``build-audit`` command -- audit build output.

Groups changed files and runs parallel audit subprocesses to review them,
then displays findings.

Pipeline:

1. Collect all files referenced in the plan's steps.
2. Distill those files via ``distiller.distill_files()``.
3. Render ``build_audit_group.md.j2`` and invoke Claude to partition files
   into audit groups (parsed via ``xml_parser.parse_audit_response()``).
4. For each group, render ``build_audit_review.md.j2`` and invoke Claude
   in parallel (via ``ThreadPoolExecutor``), collecting findings.
5. Display findings (will be shown in the TUI in a later step).
"""

from __future__ import annotations

import logging
import re
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path

import jinja2

from zing_ai.orchestrator import claude, project
from zing_ai.orchestrator.config import CallType, ZingConfig
from zing_ai.orchestrator.distiller import distill_files
from zing_ai.orchestrator.models import AuditGroup
from zing_ai.orchestrator.xml_parser import parse_audit_response, parse_zing_file
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
    via a Claude call, runs parallel review Claude calls per group,
    then starts the web server so the user can review audit findings.

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
        distilled = distill_files(plan_file_paths, project_root=project_root)

    # Convert to string-keyed dict for templates
    distilled_files: dict[str, str] = {
        str(fp.relative_to(project_root)): content for fp, content in distilled.items()
    }
    logger.info("Distilled %d files", len(distilled_files))

    # --- Step 3: Group files via Claude ---
    logger.info("Build-audit Step 3: Grouping files via Claude")
    group_prompt = render_prompt(
        "build_audit_group.md.j2",
        file_list=plan_file_list,
        distilled_files=distilled_files,
    )

    audit_groups: list[AuditGroup] = claude.invoke_claude_validated(
        group_prompt,
        validator=parse_audit_response,
        retry_prompt_template=_RETRY_TEMPLATE,
        call_type=CallType.AUDIT,
        config=config,
        skip_permissions=skip_permissions,
    )

    logger.info("Claude grouped files into %d audit groups", len(audit_groups))
    for i, group in enumerate(audit_groups):
        logger.debug("  Group %d: %d files -- %s", i + 1, len(group.files), group.files)

    # --- Step 4: Run parallel reviews per group ---
    logger.info("Build-audit Step 4: Running parallel reviews for %d groups", len(audit_groups))

    def _review_group(args: tuple[AuditGroup, int]) -> str:
        """Run a single audit review for a group of files.

        Returns the raw review output text.
        """
        group, group_index = args
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
            call_type=CallType.AUDIT,
            config=config,
            skip_permissions=skip_permissions,
        )

        logger.info("Review group %d completed (%d chars)", group_index + 1, len(output))
        return output

    with ThreadPoolExecutor() as executor:
        review_outputs: list[str] = list(
            executor.map(_review_group, [(group, i) for i, group in enumerate(audit_groups)])
        )

    # --- Step 5: Parse and collect findings ---
    logger.info("Build-audit Step 5: Parsing findings")
    all_findings: list[Finding] = []
    for output in review_outputs:
        group_findings = parse_review_findings(output, start_index=len(all_findings))
        all_findings.extend(group_findings)

    logger.info("Collected %d total findings", len(all_findings))

    # Group findings by severity for the template
    finding_groups = group_findings_by_severity(all_findings)
    logger.info(
        "Findings by severity: %s",
        {g.severity: len(g.findings) for g in finding_groups},
    )

    # --- Step 6: Display findings ---
    # TODO: Replace with Textual TUI in a later step.
    logger.info("Build-audit Step 6: Audit complete")
    for group in finding_groups:
        for finding in group.findings:
            logger.info(
                "[%s] %s -- %s (%s)",
                finding.severity.upper(),
                finding.location,
                finding.title,
                finding.category,
            )
