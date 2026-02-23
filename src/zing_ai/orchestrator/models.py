"""Data models for zing XML documents.

Each model is a dataclass with ``to_xml()`` and ``from_xml()`` methods for
round-tripping through ``xml.etree.ElementTree``.
"""

from __future__ import annotations

import xml.etree.ElementTree as ET
from dataclasses import dataclass
from typing import Self

# ---------------------------------------------------------------------------
# Choice / ChoiceSet / Interaction
# ---------------------------------------------------------------------------


@dataclass
class Choice:
    """A single selectable choice within a :class:`ChoiceSet`."""

    label: str
    description: str
    recommended: bool

    def to_xml(self) -> ET.Element:
        elem = ET.SubElement(ET.Element("_"), "choice")
        # We build a detached element, so strip the dummy parent.
        elem = ET.Element("choice")
        elem.set("label", self.label)
        elem.set("recommended", str(self.recommended).lower())
        elem.text = self.description
        return elem

    @classmethod
    def from_xml(cls, element: ET.Element) -> Self:
        label = element.get("label", "")
        recommended = element.get("recommended", "false").lower() == "true"
        description = element.text or ""
        return cls(label=label, description=description, recommended=recommended)


@dataclass
class ChoiceSet:
    """A set of choices with a message and explanation.

    Exactly one choice must have ``recommended=True``.
    """

    message: str
    explanation: str
    choices: list[Choice]

    def __post_init__(self) -> None:
        self._validate_recommended()

    def _validate_recommended(self) -> None:
        count = sum(1 for c in self.choices if c.recommended)
        if count != 1:
            raise ValueError(
                f"ChoiceSet must have exactly one recommended choice, got {count}"
            )

    def to_xml(self) -> ET.Element:
        elem = ET.Element("choice-set")
        msg = ET.SubElement(elem, "message")
        msg.text = self.message
        exp = ET.SubElement(elem, "explanation")
        exp.text = self.explanation
        choices_elem = ET.SubElement(elem, "choices")
        for choice in self.choices:
            choices_elem.append(choice.to_xml())
        return elem

    @classmethod
    def from_xml(cls, element: ET.Element) -> Self:
        message = ""
        msg_elem = element.find("message")
        if msg_elem is not None:
            message = msg_elem.text or ""

        explanation = ""
        exp_elem = element.find("explanation")
        if exp_elem is not None:
            explanation = exp_elem.text or ""

        choices: list[Choice] = []
        choices_elem = element.find("choices")
        if choices_elem is not None:
            for ch in choices_elem.findall("choice"):
                choices.append(Choice.from_xml(ch))

        return cls(message=message, explanation=explanation, choices=choices)


@dataclass
class Interaction:
    """A collection of choice sets presented to the user."""

    choice_sets: list[ChoiceSet]

    def to_xml(self) -> ET.Element:
        elem = ET.Element("interaction")
        for cs in self.choice_sets:
            elem.append(cs.to_xml())
        return elem

    @classmethod
    def from_xml(cls, element: ET.Element) -> Self:
        choice_sets = [ChoiceSet.from_xml(cs) for cs in element.findall("choice-set")]
        return cls(choice_sets=choice_sets)


# ---------------------------------------------------------------------------
# Step / Stage / Plan
# ---------------------------------------------------------------------------


@dataclass
class Step:
    """A single step within a :class:`Stage`."""

    label: str
    instructions: str
    files: list[str]
    done: bool = False

    def to_xml(self) -> ET.Element:
        elem = ET.Element("step")
        elem.set("label", self.label)
        elem.set("done", str(self.done).lower())
        inst = ET.SubElement(elem, "instructions")
        inst.text = self.instructions
        files_elem = ET.SubElement(elem, "files")
        for f in self.files:
            file_elem = ET.SubElement(files_elem, "file")
            file_elem.text = f
        return elem

    @classmethod
    def from_xml(cls, element: ET.Element) -> Self:
        label = element.get("label", "")
        done = element.get("done", "false").lower() == "true"

        instructions = ""
        inst_elem = element.find("instructions")
        if inst_elem is not None:
            instructions = inst_elem.text or ""

        files: list[str] = []
        files_elem = element.find("files")
        if files_elem is not None:
            for f in files_elem.findall("file"):
                files.append(f.text or "")

        return cls(label=label, instructions=instructions, files=files, done=done)


@dataclass
class Stage:
    """A named stage containing one or more :class:`Step` items."""

    label: str
    steps: list[Step]

    def to_xml(self) -> ET.Element:
        elem = ET.Element("stage")
        elem.set("label", self.label)
        for step in self.steps:
            elem.append(step.to_xml())
        return elem

    @classmethod
    def from_xml(cls, element: ET.Element) -> Self:
        label = element.get("label", "")
        steps = [Step.from_xml(s) for s in element.findall("step")]
        return cls(label=label, steps=steps)


@dataclass
class Plan:
    """A development plan consisting of multiple :class:`Stage` items."""

    stages: list[Stage]

    def to_xml(self) -> ET.Element:
        elem = ET.Element("plan")
        for stage in self.stages:
            elem.append(stage.to_xml())
        return elem

    @classmethod
    def from_xml(cls, element: ET.Element) -> Self:
        stages = [Stage.from_xml(s) for s in element.findall("stage")]
        return cls(stages=stages)


# ---------------------------------------------------------------------------
# AuditGroup
# ---------------------------------------------------------------------------


@dataclass
class AuditGroup:
    """A group of files to be audited together."""

    files: list[str]

    def to_xml(self) -> ET.Element:
        elem = ET.Element("audit-group")
        for f in self.files:
            file_elem = ET.SubElement(elem, "file")
            file_elem.text = f
        return elem

    @classmethod
    def from_xml(cls, element: ET.Element) -> Self:
        files = [f.text or "" for f in element.findall("file")]
        return cls(files=files)


# ---------------------------------------------------------------------------
# ZingDocument
# ---------------------------------------------------------------------------


@dataclass
class ZingDocument:
    """Top-level model representing a full ``.xml`` zing file.

    The ``plan_session`` and ``audit_session`` fields store Claude session IDs
    so that sessions can be resumed instead of started fresh.
    """

    stage: str
    content: str | None
    plan: Plan | None
    interactions: Interaction | None
    audit: bool
    approved: bool
    plan_session: str | None = None
    audit_session: str | None = None

    def to_xml(self) -> ET.Element:
        root = ET.Element("zing")
        root.set("stage", self.stage)
        root.set("audit", str(self.audit).lower())
        root.set("approved", str(self.approved).lower())

        if self.plan_session is not None:
            root.set("plan-session", self.plan_session)
        if self.audit_session is not None:
            root.set("audit-session", self.audit_session)

        if self.content is not None:
            content_elem = ET.SubElement(root, "content")
            content_elem.text = self.content

        if self.plan is not None:
            root.append(self.plan.to_xml())

        if self.interactions is not None:
            root.append(self.interactions.to_xml())

        return root

    @classmethod
    def from_xml(cls, element: ET.Element) -> Self:
        stage = element.get("stage", "")
        audit = element.get("audit", "false").lower() == "true"
        approved = element.get("approved", "false").lower() == "true"
        plan_session = element.get("plan-session")
        audit_session = element.get("audit-session")

        content: str | None = None
        content_elem = element.find("content")
        if content_elem is not None:
            content = content_elem.text

        plan: Plan | None = None
        plan_elem = element.find("plan")
        if plan_elem is not None:
            plan = Plan.from_xml(plan_elem)

        interactions: Interaction | None = None
        inter_elem = element.find("interaction")
        if inter_elem is not None:
            interactions = Interaction.from_xml(inter_elem)

        return cls(
            stage=stage,
            content=content,
            plan=plan,
            interactions=interactions,
            audit=audit,
            approved=approved,
            plan_session=plan_session,
            audit_session=audit_session,
        )
