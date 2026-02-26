"""Orchestrator ``build-audit`` command -- audit build output.

Groups changed files and runs parallel audit subprocesses to review them,
then displays findings in a Textual TUI.

Pipeline:

1. Collect all files referenced in the plan's steps.
2. Distill those files via ``distiller.distill_files()``.
3. Render ``build_audit_group.md.j2`` and invoke Claude to partition files
   into audit groups (parsed via ``xml_parser.parse_audit_response()``).
4. **Phase 1 (investigation):** Push a ``ProgressScreen`` and run parallel
   review Claude calls per group via worker threads.  Each group gets its
   own subprocess entry in the TUI.
5. **Phase 2 (results):** Pop the ``ProgressScreen``, parse findings,
   group by severity, and push an ``AuditScreen`` for user triage.
6. Handle user action decisions from the ``AuditResult`` returned by
   ``AuditScreen.dismiss()``.
"""

from __future__ import annotations

import logging
import re
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING

import jinja2

from zing_ai.orchestrator import claude, project
from zing_ai.orchestrator.claude import print_line
from zing_ai.orchestrator.config import CallType, ZingConfig, resolve_aid_path
from zing_ai.orchestrator.distiller import distill_files
from zing_ai.orchestrator.models import AuditGroup
from zing_ai.orchestrator.xml_parser import parse_audit_response, parse_zing_file
from zing_ai.prompts import render_prompt

if TYPE_CHECKING:
    from zing_ai.orchestrator.tui.results import ProgressResult

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
    via a Claude call, then runs a two-phase TUI flow:

    * **Phase 1 (investigation):** A ``ProgressScreen`` shows parallel
      review Claude calls per audit group, each as a subprocess entry.
    * **Phase 2 (results):** An ``AuditScreen`` presents grouped
      findings for user triage, returning an ``AuditResult`` with
      user decisions.

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

    logger.info("Claude grouped files into %d audit groups", len(audit_groups))
    for i, group in enumerate(audit_groups):
        logger.debug("  Group %d: %d files -- %s", i + 1, len(group.files), group.files)

    # --- Phase 1: Investigation via ProgressScreen ---
    # Deferred TUI imports to avoid circular dependency (AuditScreen
    # imports Finding/FindingGroup from this module; the screens
    # __init__.py re-exports all screens, so importing any screen
    # triggers the cycle).
    from zing_ai.orchestrator.tui.app import ZingApp
    from zing_ai.orchestrator.tui.results import AuditResult

    logger.info(
        "Build-audit Phase 1: Running parallel reviews for %d groups",
        len(audit_groups),
    )

    progress_result, review_outputs = _run_review_tui(
        audit_groups=audit_groups,
        zing_content=zing_content,
        distilled_files=distilled_files,
        config=config,
        skip_permissions=skip_permissions,
        zing_dir=zing_dir,
    )

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

    # --- Phase 2: Display findings via AuditScreen ---
    logger.info("Build-audit Phase 2: Presenting %d finding groups", len(finding_groups))

    # Deferred import to avoid circular dependency (AuditScreen imports
    # Finding/FindingGroup from this module).
    from zing_ai.orchestrator.tui.screens.audit import AuditScreen

    audit_result: AuditResult = ZingApp.run_with_screen(AuditScreen(finding_groups))

    # --- Handle user decisions ---
    logger.info("Build-audit: Processing %d user decisions", len(audit_result.decisions))
    for decision in audit_result.decisions:
        action = decision.get("action", "skip")
        severity = decision.get("severity", "unknown")
        finding_index = decision.get("finding_index", -1)
        logger.info(
            "Decision for finding %d (%s): %s",
            finding_index,
            severity,
            action,
        )


def _run_review_tui(
    *,
    audit_groups: list[AuditGroup],
    zing_content: str,
    distilled_files: dict[str, str],
    config: ZingConfig,
    skip_permissions: bool,
    zing_dir: Path | None = None,
) -> tuple[ProgressResult, list[str]]:
    """Run parallel review Claude calls inside a ProgressScreen.

    Each audit group gets its own subprocess entry in the TUI.
    When all reviews complete, the screen is dismissed and the raw
    review outputs are returned.

    Parameters
    ----------
    audit_groups:
        The audit groups from the grouping step.
    zing_content:
        The zing document overview content.
    distilled_files:
        String-keyed distilled file contents.
    config:
        Zing configuration.
    skip_permissions:
        Whether to skip permission checks.

    Returns
    -------
    tuple[ProgressResult, list[str]]
        The TUI progress result and a list of raw review outputs
        (one per group, in the same order as *audit_groups*).
    """
    from zing_ai.orchestrator.tui.app import ZingApp
    from zing_ai.orchestrator.tui.screens.progress import ProgressScreen

    screen = ProgressScreen()

    # Shared state guarded by a lock so workers can safely record
    # their results without races.
    results_lock = threading.Lock()
    review_results: dict[str, str] = {}

    def _review_worker(
        group: AuditGroup,
        group_index: int,
        group_id: str,
    ) -> None:
        """Worker function executed in a thread for each audit group."""
        screen.update_status(group_id, "running")

        try:
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
                on_output=lambda line: screen.append_output(group_id, line),
                zing_dir=zing_dir,
                call_type=CallType.AUDIT,
                config=config,
                skip_permissions=skip_permissions,
            )

            screen.append_output(group_id, f"Review complete ({len(output)} chars)")
            logger.info(
                "Review group %d completed (%d chars)",
                group_index + 1,
                len(output),
            )

            with results_lock:
                review_results[group_id] = output

            screen.update_status(group_id, "success")
        except Exception as exc:
            logger.error(
                "Review failed for group %d: %s", group_index + 1, exc
            )
            screen.append_output(group_id, f"ERROR: {exc}")
            screen.update_status(group_id, "failed")

            # Store empty output so the pipeline can continue
            with results_lock:
                review_results[group_id] = ""

    # Register subprocess entries and build group ID mapping
    group_ids: list[str] = []
    for i, group in enumerate(audit_groups):
        group_id = f"review-{i}"
        group_ids.append(group_id)
        file_summary = ", ".join(group.files[:3])
        if len(group.files) > 3:
            file_summary += f" (+{len(group.files) - 3} more)"
        screen.add_subprocess(group_id, f"Review: {file_summary}")

    # Launch all workers as daemon threads
    threads: list[threading.Thread] = []
    for i, (group, group_id) in enumerate(zip(audit_groups, group_ids, strict=True)):
        t = threading.Thread(
            target=_review_worker,
            args=(group, i, group_id),
            daemon=True,
        )
        threads.append(t)

    # A coordinator thread that waits for all workers and then
    # dismisses the screen.
    def _coordinator() -> None:
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        screen.mark_all_complete()

    coordinator = threading.Thread(target=_coordinator, daemon=True)
    coordinator.start()

    # Block until the screen is dismissed.
    progress_result: ProgressResult = ZingApp.run_with_screen(screen)

    # Gather results in original group order.
    ordered_outputs: list[str] = []
    for group_id in group_ids:
        ordered_outputs.append(review_results.get(group_id, ""))

    return progress_result, ordered_outputs
