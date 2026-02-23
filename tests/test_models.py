"""Tests for zing_ai.orchestrator.models."""

from __future__ import annotations

import xml.etree.ElementTree as ET

import pytest

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
# Choice
# ---------------------------------------------------------------------------


class TestChoice:
    """Tests for the Choice dataclass."""

    def test_fields(self) -> None:
        c = Choice(label="Option A", description="First option", recommended=True)
        assert c.label == "Option A"
        assert c.description == "First option"
        assert c.recommended is True

    def test_to_xml(self) -> None:
        c = Choice(label="Option A", description="First option", recommended=True)
        elem = c.to_xml()
        assert elem.tag == "choice"
        assert elem.get("label") == "Option A"
        assert elem.get("recommended") == "true"
        assert elem.text == "First option"

    def test_from_xml(self) -> None:
        elem = ET.Element("choice")
        elem.set("label", "Option B")
        elem.set("recommended", "false")
        elem.text = "Second option"
        c = Choice.from_xml(elem)
        assert c.label == "Option B"
        assert c.description == "Second option"
        assert c.recommended is False

    def test_round_trip(self) -> None:
        original = Choice(label="Pick me", description="A good choice", recommended=False)
        restored = Choice.from_xml(original.to_xml())
        assert restored == original

    def test_round_trip_recommended(self) -> None:
        original = Choice(label="Best", description="The best", recommended=True)
        restored = Choice.from_xml(original.to_xml())
        assert restored == original

    def test_empty_description(self) -> None:
        original = Choice(label="Empty", description="", recommended=True)
        restored = Choice.from_xml(original.to_xml())
        assert restored == original


# ---------------------------------------------------------------------------
# ChoiceSet
# ---------------------------------------------------------------------------


class TestChoiceSet:
    """Tests for the ChoiceSet dataclass."""

    def _make_choices(self) -> list[Choice]:
        return [
            Choice(label="A", description="Option A", recommended=True),
            Choice(label="B", description="Option B", recommended=False),
            Choice(label="C", description="Option C", recommended=False),
        ]

    def test_fields(self) -> None:
        choices = self._make_choices()
        cs = ChoiceSet(message="Pick one", explanation="Because", choices=choices)
        assert cs.message == "Pick one"
        assert cs.explanation == "Because"
        assert len(cs.choices) == 3

    def test_validation_no_recommended(self) -> None:
        choices = [
            Choice(label="A", description="a", recommended=False),
            Choice(label="B", description="b", recommended=False),
        ]
        with pytest.raises(ValueError, match="exactly one recommended"):
            ChoiceSet(message="msg", explanation="exp", choices=choices)

    def test_validation_multiple_recommended(self) -> None:
        choices = [
            Choice(label="A", description="a", recommended=True),
            Choice(label="B", description="b", recommended=True),
        ]
        with pytest.raises(ValueError, match="exactly one recommended"):
            ChoiceSet(message="msg", explanation="exp", choices=choices)

    def test_validation_single_recommended_ok(self) -> None:
        choices = [
            Choice(label="A", description="a", recommended=True),
        ]
        cs = ChoiceSet(message="msg", explanation="exp", choices=choices)
        assert len(cs.choices) == 1

    def test_to_xml(self) -> None:
        cs = ChoiceSet(
            message="Pick one",
            explanation="Because reasons",
            choices=self._make_choices(),
        )
        elem = cs.to_xml()
        assert elem.tag == "choice-set"
        assert elem.find("message") is not None
        assert elem.find("message").text == "Pick one"  # type: ignore[union-attr]
        assert elem.find("explanation") is not None
        assert elem.find("explanation").text == "Because reasons"  # type: ignore[union-attr]
        choices_elem = elem.find("choices")
        assert choices_elem is not None
        assert len(choices_elem.findall("choice")) == 3

    def test_from_xml(self) -> None:
        xml_str = """<choice-set>
            <message>Pick one</message>
            <explanation>Because</explanation>
            <choices>
                <choice label="A" recommended="true">Option A</choice>
                <choice label="B" recommended="false">Option B</choice>
            </choices>
        </choice-set>"""
        elem = ET.fromstring(xml_str)
        cs = ChoiceSet.from_xml(elem)
        assert cs.message == "Pick one"
        assert cs.explanation == "Because"
        assert len(cs.choices) == 2
        assert cs.choices[0].recommended is True

    def test_round_trip(self) -> None:
        original = ChoiceSet(
            message="Choose wisely",
            explanation="This matters",
            choices=self._make_choices(),
        )
        restored = ChoiceSet.from_xml(original.to_xml())
        assert restored == original


# ---------------------------------------------------------------------------
# Interaction
# ---------------------------------------------------------------------------


class TestInteraction:
    """Tests for the Interaction dataclass."""

    def _make_choice_set(self) -> ChoiceSet:
        return ChoiceSet(
            message="Pick one",
            explanation="Reason",
            choices=[
                Choice(label="A", description="First", recommended=True),
                Choice(label="B", description="Second", recommended=False),
            ],
        )

    def test_fields(self) -> None:
        cs = self._make_choice_set()
        interaction = Interaction(choice_sets=[cs])
        assert len(interaction.choice_sets) == 1

    def test_to_xml(self) -> None:
        interaction = Interaction(choice_sets=[self._make_choice_set()])
        elem = interaction.to_xml()
        assert elem.tag == "interaction"
        assert len(elem.findall("choice-set")) == 1

    def test_round_trip(self) -> None:
        original = Interaction(
            choice_sets=[self._make_choice_set(), self._make_choice_set()]
        )
        restored = Interaction.from_xml(original.to_xml())
        assert restored == original

    def test_empty_choice_sets(self) -> None:
        original = Interaction(choice_sets=[])
        restored = Interaction.from_xml(original.to_xml())
        assert restored == original


# ---------------------------------------------------------------------------
# Step
# ---------------------------------------------------------------------------


class TestStep:
    """Tests for the Step dataclass."""

    def test_fields(self) -> None:
        s = Step(label="Setup", instructions="Run setup", files=["a.py", "b.py"])
        assert s.label == "Setup"
        assert s.instructions == "Run setup"
        assert s.files == ["a.py", "b.py"]
        assert s.done is False

    def test_done_default(self) -> None:
        s = Step(label="S", instructions="I", files=[])
        assert s.done is False

    def test_to_xml(self) -> None:
        s = Step(label="Build", instructions="Compile it", files=["main.c"], done=True)
        elem = s.to_xml()
        assert elem.tag == "step"
        assert elem.get("label") == "Build"
        assert elem.get("done") == "true"
        assert elem.find("instructions").text == "Compile it"  # type: ignore[union-attr]
        files_elem = elem.find("files")
        assert files_elem is not None
        assert len(files_elem.findall("file")) == 1
        assert files_elem.find("file").text == "main.c"  # type: ignore[union-attr]

    def test_from_xml(self) -> None:
        xml_str = """<step label="Test" done="true">
            <instructions>Run tests</instructions>
            <files>
                <file>test_a.py</file>
                <file>test_b.py</file>
            </files>
        </step>"""
        elem = ET.fromstring(xml_str)
        s = Step.from_xml(elem)
        assert s.label == "Test"
        assert s.done is True
        assert s.instructions == "Run tests"
        assert s.files == ["test_a.py", "test_b.py"]

    def test_round_trip(self) -> None:
        original = Step(
            label="Deploy",
            instructions="Push to prod",
            files=["deploy.sh", "config.yaml"],
            done=False,
        )
        restored = Step.from_xml(original.to_xml())
        assert restored == original

    def test_round_trip_done(self) -> None:
        original = Step(label="Done step", instructions="Already done", files=[], done=True)
        restored = Step.from_xml(original.to_xml())
        assert restored == original


# ---------------------------------------------------------------------------
# Stage
# ---------------------------------------------------------------------------


class TestStage:
    """Tests for the Stage dataclass."""

    def _make_steps(self) -> list[Step]:
        return [
            Step(label="Step 1", instructions="Do first thing", files=["a.py"]),
            Step(label="Step 2", instructions="Do second thing", files=["b.py"], done=True),
        ]

    def test_fields(self) -> None:
        stage = Stage(label="Phase 1", steps=self._make_steps())
        assert stage.label == "Phase 1"
        assert len(stage.steps) == 2

    def test_to_xml(self) -> None:
        stage = Stage(label="Phase 1", steps=self._make_steps())
        elem = stage.to_xml()
        assert elem.tag == "stage"
        assert elem.get("label") == "Phase 1"
        assert len(elem.findall("step")) == 2

    def test_round_trip(self) -> None:
        original = Stage(label="Phase 1", steps=self._make_steps())
        restored = Stage.from_xml(original.to_xml())
        assert restored == original

    def test_empty_steps(self) -> None:
        original = Stage(label="Empty", steps=[])
        restored = Stage.from_xml(original.to_xml())
        assert restored == original


# ---------------------------------------------------------------------------
# Plan
# ---------------------------------------------------------------------------


class TestPlan:
    """Tests for the Plan dataclass."""

    def _make_plan(self) -> Plan:
        return Plan(
            stages=[
                Stage(
                    label="Setup",
                    steps=[
                        Step(label="Init", instructions="Initialize", files=["init.py"]),
                    ],
                ),
                Stage(
                    label="Build",
                    steps=[
                        Step(label="Compile", instructions="Build it", files=["main.py"]),
                        Step(
                            label="Test",
                            instructions="Test it",
                            files=["test_main.py"],
                            done=True,
                        ),
                    ],
                ),
            ]
        )

    def test_fields(self) -> None:
        plan = self._make_plan()
        assert len(plan.stages) == 2
        assert plan.stages[0].label == "Setup"
        assert plan.stages[1].label == "Build"

    def test_to_xml(self) -> None:
        plan = self._make_plan()
        elem = plan.to_xml()
        assert elem.tag == "plan"
        assert len(elem.findall("stage")) == 2

    def test_round_trip(self) -> None:
        original = self._make_plan()
        restored = Plan.from_xml(original.to_xml())
        assert restored == original

    def test_empty_stages(self) -> None:
        original = Plan(stages=[])
        restored = Plan.from_xml(original.to_xml())
        assert restored == original


# ---------------------------------------------------------------------------
# AuditGroup
# ---------------------------------------------------------------------------


class TestAuditGroup:
    """Tests for the AuditGroup dataclass."""

    def test_fields(self) -> None:
        ag = AuditGroup(files=["a.py", "b.py"])
        assert ag.files == ["a.py", "b.py"]

    def test_to_xml(self) -> None:
        ag = AuditGroup(files=["src/app.py", "src/utils.py"])
        elem = ag.to_xml()
        assert elem.tag == "audit-group"
        assert len(elem.findall("file")) == 2
        assert elem.findall("file")[0].text == "src/app.py"

    def test_from_xml(self) -> None:
        xml_str = """<audit-group>
            <file>a.py</file>
            <file>b.py</file>
        </audit-group>"""
        elem = ET.fromstring(xml_str)
        ag = AuditGroup.from_xml(elem)
        assert ag.files == ["a.py", "b.py"]

    def test_round_trip(self) -> None:
        original = AuditGroup(files=["x.py", "y.py", "z.py"])
        restored = AuditGroup.from_xml(original.to_xml())
        assert restored == original

    def test_empty_files(self) -> None:
        original = AuditGroup(files=[])
        restored = AuditGroup.from_xml(original.to_xml())
        assert restored == original


# ---------------------------------------------------------------------------
# ZingDocument
# ---------------------------------------------------------------------------


class TestZingDocument:
    """Tests for the ZingDocument dataclass."""

    def _make_full_document(self) -> ZingDocument:
        plan = Plan(
            stages=[
                Stage(
                    label="Phase 1",
                    steps=[
                        Step(label="Step 1", instructions="Do it", files=["a.py"]),
                    ],
                ),
            ]
        )
        interaction = Interaction(
            choice_sets=[
                ChoiceSet(
                    message="Pick one",
                    explanation="Choose wisely",
                    choices=[
                        Choice(label="A", description="Option A", recommended=True),
                        Choice(label="B", description="Option B", recommended=False),
                    ],
                ),
            ]
        )
        return ZingDocument(
            stage="planning",
            content="# My Project\n\nSome markdown content.",
            plan=plan,
            interactions=interaction,
            audit=False,
            approved=True,
            plan_session="session-abc-123",
            audit_session="session-def-456",
        )

    def test_fields(self) -> None:
        doc = self._make_full_document()
        assert doc.stage == "planning"
        assert doc.content is not None
        assert doc.plan is not None
        assert doc.interactions is not None
        assert doc.audit is False
        assert doc.approved is True
        assert doc.plan_session == "session-abc-123"
        assert doc.audit_session == "session-def-456"

    def test_defaults(self) -> None:
        doc = ZingDocument(
            stage="new",
            content=None,
            plan=None,
            interactions=None,
            audit=False,
            approved=False,
        )
        assert doc.plan_session is None
        assert doc.audit_session is None

    def test_to_xml_full(self) -> None:
        doc = self._make_full_document()
        elem = doc.to_xml()
        assert elem.tag == "zing"
        assert elem.get("stage") == "planning"
        assert elem.get("audit") == "false"
        assert elem.get("approved") == "true"
        assert elem.get("plan-session") == "session-abc-123"
        assert elem.get("audit-session") == "session-def-456"
        assert elem.find("content") is not None
        assert elem.find("plan") is not None
        assert elem.find("interaction") is not None

    def test_to_xml_minimal(self) -> None:
        doc = ZingDocument(
            stage="new",
            content=None,
            plan=None,
            interactions=None,
            audit=False,
            approved=False,
        )
        elem = doc.to_xml()
        assert elem.tag == "zing"
        assert elem.get("stage") == "new"
        assert elem.find("content") is None
        assert elem.find("plan") is None
        assert elem.find("interaction") is None
        # Session attributes should not appear when None
        assert elem.get("plan-session") is None
        assert elem.get("audit-session") is None

    def test_from_xml(self) -> None:
        xml_str = """<zing stage="building" audit="true" approved="false"
                           plan-session="s1" audit-session="s2">
            <content>Hello world</content>
            <plan>
                <stage label="S1">
                    <step label="Do" done="false">
                        <instructions>Instructions here</instructions>
                        <files><file>f.py</file></files>
                    </step>
                </stage>
            </plan>
            <interaction>
                <choice-set>
                    <message>Choose</message>
                    <explanation>Why</explanation>
                    <choices>
                        <choice label="X" recommended="true">The X</choice>
                    </choices>
                </choice-set>
            </interaction>
        </zing>"""
        elem = ET.fromstring(xml_str)
        doc = ZingDocument.from_xml(elem)
        assert doc.stage == "building"
        assert doc.audit is True
        assert doc.approved is False
        assert doc.content == "Hello world"
        assert doc.plan is not None
        assert len(doc.plan.stages) == 1
        assert doc.interactions is not None
        assert len(doc.interactions.choice_sets) == 1
        assert doc.plan_session == "s1"
        assert doc.audit_session == "s2"

    def test_round_trip_full(self) -> None:
        original = self._make_full_document()
        restored = ZingDocument.from_xml(original.to_xml())
        assert restored == original

    def test_round_trip_minimal(self) -> None:
        original = ZingDocument(
            stage="new",
            content=None,
            plan=None,
            interactions=None,
            audit=False,
            approved=False,
        )
        restored = ZingDocument.from_xml(original.to_xml())
        assert restored == original

    def test_round_trip_with_content_only(self) -> None:
        original = ZingDocument(
            stage="drafted",
            content="# Title\n\nParagraph with **bold** text.",
            plan=None,
            interactions=None,
            audit=True,
            approved=False,
        )
        restored = ZingDocument.from_xml(original.to_xml())
        assert restored == original

    def test_round_trip_sessions_none(self) -> None:
        original = ZingDocument(
            stage="planning",
            content=None,
            plan=Plan(stages=[]),
            interactions=None,
            audit=False,
            approved=False,
            plan_session=None,
            audit_session=None,
        )
        restored = ZingDocument.from_xml(original.to_xml())
        assert restored == original

    def test_full_xml_serialization(self) -> None:
        """Verify that a full document can be serialized to a string and parsed back."""
        doc = self._make_full_document()
        xml_bytes = ET.tostring(doc.to_xml(), encoding="unicode")
        elem = ET.fromstring(xml_bytes)
        restored = ZingDocument.from_xml(elem)
        assert restored == doc
