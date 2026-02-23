"""XML parser and validators for zing documents and Claude responses.

Handles two related but distinct XML formats:

1. **Zing file format** — the ``.xml`` files stored on disk, parsed via
   :func:`parse_zing_file` and serialized via :func:`write_zing_file`.

2. **Claude response format** — XML fragments embedded in Claude's text
   responses, extracted and parsed by the ``parse_*_response`` helpers.
"""

from __future__ import annotations

import re
import xml.etree.ElementTree as ET
from pathlib import Path

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

# ---------------------------------------------------------------------------
# ValidationError
# ---------------------------------------------------------------------------


class ValidationError(Exception):
    """Raised when XML is well-formed but fails business rules.

    Examples include a missing recommended choice in a choice set, or
    required elements/attributes being absent.
    """


# ---------------------------------------------------------------------------
# Zing file I/O
# ---------------------------------------------------------------------------


def parse_zing_file(path: Path) -> ZingDocument:
    """Read a ``.xml`` zing file and parse it into a :class:`ZingDocument`.

    The on-disk format uses a slightly different structure than the internal
    model XML (e.g. ``<interactions>`` with ``<choices>`` blocks rather than
    ``<interaction>`` with ``<choice-set>``).  This function bridges the two
    representations.

    Raises:
        ET.ParseError: If the file is not well-formed XML.
        FileNotFoundError: If *path* does not exist.
    """
    tree = ET.parse(path)
    root = tree.getroot()
    return _zing_element_to_document(root)


def write_zing_file(path: Path, doc: ZingDocument) -> None:
    """Serialize a :class:`ZingDocument` to a ``.xml`` file.

    The output uses the zing on-disk XML format which differs slightly from
    the internal model XML.
    """
    root = _document_to_zing_element(doc)
    tree = ET.ElementTree(root)
    ET.indent(tree, space="  ")
    tree.write(path, encoding="unicode", xml_declaration=True)


# ---------------------------------------------------------------------------
# Claude response parsers
# ---------------------------------------------------------------------------


def parse_interactions_response(text: str) -> Interaction:
    """Extract ``<zing:interactions>`` from Claude's response text.

    The Claude response may contain surrounding prose or markdown.  This
    function locates the ``<zing:interactions>...</zing:interactions>`` block,
    parses it, and returns an :class:`Interaction`.

    Raises:
        ValidationError: If the XML is missing, malformed, or fails business
            rules (e.g. no recommended choice in a choice set).
    """
    fragment = _extract_xml_fragment(text, "zing:interactions")
    try:
        elem = ET.fromstring(fragment)
    except ET.ParseError as exc:
        raise ValidationError(f"Malformed XML in zing:interactions: {exc}") from exc

    return _parse_claude_interactions(elem)


def parse_steps_response(text: str) -> Plan:
    """Extract ``<zing:steps>`` from Claude's response text.

    Returns a :class:`Plan` built from the ``<stage>``/``<step>`` tree.

    Raises:
        ValidationError: If the XML is missing or malformed.
    """
    fragment = _extract_xml_fragment(text, "zing:steps")
    try:
        elem = ET.fromstring(fragment)
    except ET.ParseError as exc:
        raise ValidationError(f"Malformed XML in zing:steps: {exc}") from exc

    return _parse_claude_steps(elem)


def parse_audit_response(text: str) -> list[AuditGroup]:
    """Extract ``<zing:audit>`` from Claude's response text.

    Each ``<group>`` contains a newline-delimited list of file paths.

    Raises:
        ValidationError: If the XML is missing or malformed.
    """
    fragment = _extract_xml_fragment(text, "zing:audit")
    try:
        elem = ET.fromstring(fragment)
    except ET.ParseError as exc:
        raise ValidationError(f"Malformed XML in zing:audit: {exc}") from exc

    return _parse_claude_audit(elem)


# ---------------------------------------------------------------------------
# Internal helpers — zing file format <-> ZingDocument
# ---------------------------------------------------------------------------


def _zing_element_to_document(root: ET.Element) -> ZingDocument:
    """Convert a ``<zing>`` element (on-disk format) to a ZingDocument."""
    stage = root.get("stage", "")
    audit = root.get("audit", "false").lower() == "true"
    approved = root.get("approved", "false").lower() == "true"
    plan_session = root.get("plan-session")
    audit_session = root.get("audit-session")

    # Content
    content: str | None = None
    content_elem = root.find("content")
    if content_elem is not None:
        content = content_elem.text

    # Plan — on-disk format matches the model's <plan><stage><step> structure,
    # except that <files> contains a newline-delimited string rather than
    # individual <file> elements.
    plan: Plan | None = None
    plan_elem = root.find("plan")
    if plan_elem is not None:
        plan = _parse_zing_plan(plan_elem)

    # Interactions — on-disk uses <interactions><choices message="..."> rather
    # than <interaction><choice-set>.
    interactions: Interaction | None = None
    inter_elem = root.find("interactions")
    if inter_elem is not None:
        interactions = _parse_zing_interactions(inter_elem)

    return ZingDocument(
        stage=stage,
        content=content,
        plan=plan,
        interactions=interactions,
        audit=audit,
        approved=approved,
        plan_session=plan_session,
        audit_session=audit_session,
    )


def _parse_zing_plan(plan_elem: ET.Element) -> Plan:
    """Parse a ``<plan>`` element from the on-disk format."""
    stages: list[Stage] = []
    for stage_elem in plan_elem.findall("stage"):
        label = stage_elem.get("label", "")
        steps: list[Step] = []
        for step_elem in stage_elem.findall("step"):
            steps.append(_parse_zing_step(step_elem))
        stages.append(Stage(label=label, steps=steps))
    return Plan(stages=stages)


def _parse_zing_step(step_elem: ET.Element) -> Step:
    """Parse a ``<step>`` from the on-disk format.

    The on-disk format stores files as a newline-delimited string inside a
    ``<files>`` element rather than individual ``<file>`` sub-elements.
    """
    label = step_elem.get("label", "")
    done = step_elem.get("done", "false").lower() == "true"

    instructions = ""
    inst_elem = step_elem.find("instructions")
    if inst_elem is not None:
        instructions = inst_elem.text or ""

    files: list[str] = []
    files_elem = step_elem.find("files")
    if files_elem is not None:
        raw = files_elem.text or ""
        files = [f.strip() for f in raw.strip().splitlines() if f.strip()]

    return Step(label=label, instructions=instructions, files=files, done=done)


def _parse_zing_interactions(inter_elem: ET.Element) -> Interaction:
    """Parse an ``<interactions>`` element from the on-disk format.

    On-disk format::

        <interactions>
          <choices message="...">
            <explanation format="markdown">markdown</explanation>
            <choice label="..." description="..." recommended="true" />
          </choices>
        </interactions>
    """
    choice_sets: list[ChoiceSet] = []
    for choices_elem in inter_elem.findall("choices"):
        message = choices_elem.get("message", "")

        explanation = ""
        exp_elem = choices_elem.find("explanation")
        if exp_elem is not None:
            explanation = exp_elem.text or ""

        choices: list[Choice] = []
        for ch in choices_elem.findall("choice"):
            choices.append(
                Choice(
                    label=ch.get("label", ""),
                    description=ch.get("description", ""),
                    recommended=ch.get("recommended", "false").lower() == "true",
                )
            )

        try:
            choice_sets.append(
                ChoiceSet(message=message, explanation=explanation, choices=choices)
            )
        except ValueError as exc:
            raise ValidationError(str(exc)) from exc

    return Interaction(choice_sets=choice_sets)


def _document_to_zing_element(doc: ZingDocument) -> ET.Element:
    """Convert a ZingDocument to the on-disk ``<zing>`` format."""
    root = ET.Element("zing")
    root.set("stage", doc.stage)
    root.set("audit", str(doc.audit).lower())
    root.set("approved", str(doc.approved).lower())

    if doc.plan_session is not None:
        root.set("plan-session", doc.plan_session)
    if doc.audit_session is not None:
        root.set("audit-session", doc.audit_session)

    if doc.content is not None:
        content_elem = ET.SubElement(root, "content")
        content_elem.text = doc.content

    if doc.plan is not None:
        root.append(_plan_to_zing_element(doc.plan))

    if doc.interactions is not None:
        root.append(_interactions_to_zing_element(doc.interactions))

    return root


def _plan_to_zing_element(plan: Plan) -> ET.Element:
    """Serialize a Plan to the on-disk format."""
    plan_elem = ET.Element("plan")
    for stage in plan.stages:
        stage_elem = ET.SubElement(plan_elem, "stage")
        stage_elem.set("label", stage.label)
        for step in stage.steps:
            step_elem = ET.SubElement(stage_elem, "step")
            step_elem.set("label", step.label)
            step_elem.set("done", str(step.done).lower())
            inst = ET.SubElement(step_elem, "instructions")
            inst.text = step.instructions
            files_elem = ET.SubElement(step_elem, "files")
            files_elem.text = "\n".join(step.files)
    return plan_elem


def _interactions_to_zing_element(interaction: Interaction) -> ET.Element:
    """Serialize an Interaction to the on-disk format."""
    inter_elem = ET.Element("interactions")
    for cs in interaction.choice_sets:
        choices_elem = ET.SubElement(inter_elem, "choices")
        choices_elem.set("message", cs.message)
        exp = ET.SubElement(choices_elem, "explanation")
        exp.set("format", "markdown")
        exp.text = cs.explanation
        for choice in cs.choices:
            ch = ET.SubElement(choices_elem, "choice")
            ch.set("label", choice.label)
            ch.set("description", choice.description)
            ch.set("recommended", str(choice.recommended).lower())
    return inter_elem


# ---------------------------------------------------------------------------
# Internal helpers — Claude response parsing
# ---------------------------------------------------------------------------


def _extract_xml_fragment(text: str, tag: str) -> str:
    """Find and extract an XML fragment delimited by *tag* from *text*.

    The ``zing:`` prefix used in Claude responses is not a real XML namespace
    — it is just a convenient prefix to avoid collisions with surrounding
    content.  Standard :mod:`xml.etree.ElementTree` rejects unbound namespace
    prefixes, so we strip the ``zing:`` prefix from both the opening and
    closing tags before returning the fragment.

    Raises :class:`ValidationError` if the tag is not found.
    """
    escaped = re.escape(tag)
    pattern = re.compile(rf"(<{escaped}[\s>].*?</{escaped}>)", re.DOTALL)
    match = pattern.search(text)
    if match is None:
        raise ValidationError(f"Could not find <{tag}> in response text")
    fragment = match.group(1)

    # Strip the "zing:" pseudo-namespace prefix so ElementTree can parse it.
    if ":" in tag:
        _, local = tag.split(":", 1)
        fragment = fragment.replace(f"<{tag}", f"<{local}", 1)
        fragment = fragment.replace(f"</{tag}>", f"</{local}>", 1)

    return fragment


def _parse_claude_interactions(elem: ET.Element) -> Interaction:
    """Parse a ``<zing:interactions>`` element from Claude's response.

    Claude uses the same format as the on-disk ``<interactions>``::

        <zing:interactions>
          <choices message="...">
            <explanation format="markdown">markdown</explanation>
            <choice label="..." description="..." recommended="true" />
          </choices>
        </zing:interactions>
    """
    choice_sets: list[ChoiceSet] = []
    for choices_elem in elem.findall("choices"):
        message = choices_elem.get("message", "")

        explanation = ""
        exp_elem = choices_elem.find("explanation")
        if exp_elem is not None:
            explanation = exp_elem.text or ""

        choices: list[Choice] = []
        for ch in choices_elem.findall("choice"):
            choices.append(
                Choice(
                    label=ch.get("label", ""),
                    description=ch.get("description", ""),
                    recommended=ch.get("recommended", "false").lower() == "true",
                )
            )

        if not choices:
            raise ValidationError("choices element must contain at least one choice")

        recommended_count = sum(1 for c in choices if c.recommended)
        if recommended_count != 1:
            raise ValidationError(
                f"ChoiceSet must have exactly one recommended choice, got {recommended_count}"
            )

        try:
            choice_sets.append(
                ChoiceSet(message=message, explanation=explanation, choices=choices)
            )
        except ValueError as exc:
            raise ValidationError(str(exc)) from exc

    if not choice_sets:
        raise ValidationError("zing:interactions must contain at least one choices element")

    return Interaction(choice_sets=choice_sets)


def _parse_claude_steps(elem: ET.Element) -> Plan:
    """Parse a ``<zing:steps>`` element from Claude's response.

    Claude format::

        <zing:steps>
          <stage label="...">
            <step label="...">
              <instructions>markdown</instructions>
              <files>file list</files>
            </step>
          </stage>
        </zing:steps>

    The ``<files>`` element contains a newline-delimited list of file paths
    (same as the on-disk format).
    """
    stages: list[Stage] = []
    for stage_elem in elem.findall("stage"):
        label = stage_elem.get("label", "")
        steps: list[Step] = []
        for step_elem in stage_elem.findall("step"):
            step_label = step_elem.get("label", "")

            instructions = ""
            inst_elem = step_elem.find("instructions")
            if inst_elem is not None:
                instructions = inst_elem.text or ""

            files: list[str] = []
            files_elem = step_elem.find("files")
            if files_elem is not None:
                raw = files_elem.text or ""
                files = [f.strip() for f in raw.strip().splitlines() if f.strip()]

            steps.append(
                Step(label=step_label, instructions=instructions, files=files, done=False)
            )
        stages.append(Stage(label=label, steps=steps))

    if not stages:
        raise ValidationError("zing:steps must contain at least one stage")

    return Plan(stages=stages)


def _parse_claude_audit(elem: ET.Element) -> list[AuditGroup]:
    """Parse a ``<zing:audit>`` element from Claude's response.

    Claude format::

        <zing:audit>
          <group>file1.py\nfile2.py</group>
          <group>file3.py</group>
        </zing:audit>

    Each ``<group>`` holds a newline-delimited list of file paths.
    """
    groups: list[AuditGroup] = []
    for group_elem in elem.findall("group"):
        raw = group_elem.text or ""
        files = [f.strip() for f in raw.strip().splitlines() if f.strip()]
        groups.append(AuditGroup(files=files))

    if not groups:
        raise ValidationError("zing:audit must contain at least one group")

    return groups
