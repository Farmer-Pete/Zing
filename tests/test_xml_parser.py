"""Tests for zing_ai.orchestrator.xml_parser."""

from __future__ import annotations

import textwrap

import pytest

from zing_ai.orchestrator.models import (
    Choice,
    ChoiceSet,
    Interaction,
    Plan,
    Stage,
    Step,
    ZingDocument,
)
from zing_ai.orchestrator.xml_parser import (
    ValidationError,
    parse_audit_response,
    parse_interactions_response,
    parse_steps_response,
    parse_zing_file,
    write_zing_file,
)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _make_full_document() -> ZingDocument:
    """Create a fully-populated ZingDocument for testing."""
    plan = Plan(
        stages=[
            Stage(
                label="Setup",
                steps=[
                    Step(
                        label="Initialize project",
                        instructions="Run the init script to set up the project.",
                        files=["init.py", "config.yaml"],
                        done=False,
                    ),
                ],
            ),
            Stage(
                label="Build",
                steps=[
                    Step(
                        label="Compile sources",
                        instructions="Build all source files.",
                        files=["src/main.py", "src/utils.py"],
                        done=True,
                    ),
                    Step(
                        label="Run tests",
                        instructions="Execute the test suite.",
                        files=["tests/test_main.py"],
                        done=False,
                    ),
                ],
            ),
        ]
    )
    interaction = Interaction(
        choice_sets=[
            ChoiceSet(
                message="Which framework should we use?",
                explanation="We need to pick a web framework for the API layer.",
                choices=[
                    Choice(label="FastAPI", description="Modern async framework", recommended=True),
                    Choice(label="Flask", description="Simple and lightweight", recommended=False),
                    Choice(label="Django", description="Batteries included", recommended=False),
                ],
            ),
        ]
    )
    return ZingDocument(
        stage="plan",
        content="# My Project\n\nA project description with **markdown**.",
        plan=plan,
        interactions=interaction,
        audit=True,
        approved=False,
        plan_session="session-plan-001",
        audit_session="session-audit-002",
    )


def _make_minimal_document() -> ZingDocument:
    """Create a minimal ZingDocument with no optional fields."""
    return ZingDocument(
        stage="new",
        content=None,
        plan=None,
        interactions=None,
        audit=False,
        approved=False,
    )


# ===========================================================================
# parse_zing_file / write_zing_file
# ===========================================================================


class TestParseAndWriteZingFile:
    """Tests for reading and writing .xml zing files."""

    def test_round_trip_full_document(self, tmp_path: ...) -> None:
        """Write a full document, read it back, and verify all fields match."""
        original = _make_full_document()
        path = tmp_path / "test.xml"
        write_zing_file(path, original)
        restored = parse_zing_file(path)

        assert restored.stage == original.stage
        assert restored.audit == original.audit
        assert restored.approved == original.approved
        assert restored.plan_session == original.plan_session
        assert restored.audit_session == original.audit_session
        assert restored.content == original.content

    def test_round_trip_plan(self, tmp_path: ...) -> None:
        """Verify plan stages and steps survive a round trip."""
        original = _make_full_document()
        path = tmp_path / "test.xml"
        write_zing_file(path, original)
        restored = parse_zing_file(path)

        assert restored.plan is not None
        assert original.plan is not None
        assert len(restored.plan.stages) == len(original.plan.stages)
        for r_stage, o_stage in zip(restored.plan.stages, original.plan.stages, strict=True):
            assert r_stage.label == o_stage.label
            assert len(r_stage.steps) == len(o_stage.steps)
            for r_step, o_step in zip(r_stage.steps, o_stage.steps, strict=True):
                assert r_step.label == o_step.label
                assert r_step.instructions == o_step.instructions
                assert r_step.files == o_step.files
                assert r_step.done == o_step.done

    def test_round_trip_interactions(self, tmp_path: ...) -> None:
        """Verify interactions (choice sets) survive a round trip."""
        original = _make_full_document()
        path = tmp_path / "test.xml"
        write_zing_file(path, original)
        restored = parse_zing_file(path)

        assert restored.interactions is not None
        assert original.interactions is not None
        assert len(restored.interactions.choice_sets) == len(
            original.interactions.choice_sets
        )
        for r_cs, o_cs in zip(
            restored.interactions.choice_sets,
            original.interactions.choice_sets,
            strict=True,
        ):
            assert r_cs.message == o_cs.message
            assert r_cs.explanation == o_cs.explanation
            assert len(r_cs.choices) == len(o_cs.choices)
            for r_ch, o_ch in zip(r_cs.choices, o_cs.choices, strict=True):
                assert r_ch.label == o_ch.label
                assert r_ch.description == o_ch.description
                assert r_ch.recommended == o_ch.recommended

    def test_round_trip_minimal(self, tmp_path: ...) -> None:
        """A minimal document with no content/plan/interactions round-trips."""
        original = _make_minimal_document()
        path = tmp_path / "test.xml"
        write_zing_file(path, original)
        restored = parse_zing_file(path)

        assert restored.stage == "new"
        assert restored.content is None
        assert restored.plan is None
        assert restored.interactions is None
        assert restored.audit is False
        assert restored.approved is False
        assert restored.plan_session is None
        assert restored.audit_session is None

    def test_round_trip_content_only(self, tmp_path: ...) -> None:
        """Document with only content (no plan/interactions) round-trips."""
        original = ZingDocument(
            stage="drafted",
            content="# Title\n\nSome **bold** text.\n\n- Item 1\n- Item 2",
            plan=None,
            interactions=None,
            audit=False,
            approved=False,
        )
        path = tmp_path / "test.xml"
        write_zing_file(path, original)
        restored = parse_zing_file(path)
        assert restored.content == original.content

    def test_round_trip_empty_plan(self, tmp_path: ...) -> None:
        """A document with an empty plan (no stages) round-trips."""
        original = ZingDocument(
            stage="planning",
            content=None,
            plan=Plan(stages=[]),
            interactions=None,
            audit=False,
            approved=False,
        )
        path = tmp_path / "test.xml"
        write_zing_file(path, original)
        restored = parse_zing_file(path)
        assert restored.plan is not None
        assert restored.plan.stages == []

    def test_write_creates_valid_xml(self, tmp_path: ...) -> None:
        """The written file starts with an XML declaration."""
        doc = _make_full_document()
        path = tmp_path / "test.xml"
        write_zing_file(path, doc)
        raw = path.read_text()
        assert raw.startswith("<?xml")

    def test_parse_nonexistent_file_raises(self, tmp_path: ...) -> None:
        """Attempting to parse a nonexistent file raises FileNotFoundError."""
        with pytest.raises(FileNotFoundError):
            parse_zing_file(tmp_path / "does_not_exist.xml")

    def test_parse_malformed_xml_raises(self, tmp_path: ...) -> None:
        """Attempting to parse malformed XML raises ParseError."""
        path = tmp_path / "bad.xml"
        path.write_text("<zing><unclosed>")
        import xml.etree.ElementTree as ET

        with pytest.raises(ET.ParseError):
            parse_zing_file(path)

    def test_round_trip_multiple_choice_sets(self, tmp_path: ...) -> None:
        """Multiple choice sets within interactions survive a round trip."""
        doc = ZingDocument(
            stage="plan",
            content=None,
            plan=None,
            interactions=Interaction(
                choice_sets=[
                    ChoiceSet(
                        message="First question",
                        explanation="Explanation 1",
                        choices=[
                            Choice(label="A", description="Opt A", recommended=True),
                            Choice(label="B", description="Opt B", recommended=False),
                        ],
                    ),
                    ChoiceSet(
                        message="Second question",
                        explanation="Explanation 2",
                        choices=[
                            Choice(label="X", description="Opt X", recommended=False),
                            Choice(label="Y", description="Opt Y", recommended=True),
                        ],
                    ),
                ]
            ),
            audit=False,
            approved=False,
        )
        path = tmp_path / "test.xml"
        write_zing_file(path, doc)
        restored = parse_zing_file(path)
        assert restored.interactions is not None
        assert len(restored.interactions.choice_sets) == 2
        assert restored.interactions.choice_sets[0].message == "First question"
        assert restored.interactions.choice_sets[1].message == "Second question"

    def test_round_trip_step_with_no_files(self, tmp_path: ...) -> None:
        """A step with an empty file list round-trips correctly."""
        doc = ZingDocument(
            stage="plan",
            content=None,
            plan=Plan(
                stages=[
                    Stage(
                        label="S1",
                        steps=[
                            Step(label="No files", instructions="Do something", files=[]),
                        ],
                    ),
                ]
            ),
            interactions=None,
            audit=False,
            approved=False,
        )
        path = tmp_path / "test.xml"
        write_zing_file(path, doc)
        restored = parse_zing_file(path)
        assert restored.plan is not None
        assert restored.plan.stages[0].steps[0].files == []


# ===========================================================================
# parse_interactions_response
# ===========================================================================


class TestParseInteractionsResponse:
    """Tests for parsing <zing:interactions> from Claude response text."""

    def test_basic_extraction(self) -> None:
        """Extract interactions from text with surrounding prose."""
        text = textwrap.dedent("""\
            Here is my analysis of the project.

            <zing:interactions>
              <choices message="Which database should we use?">
                <explanation format="markdown">We need a database for persistence.</explanation>
                <choice label="PostgreSQL" description="Relational, robust" recommended="true" />
                <choice label="SQLite" description="Simple, file-based" recommended="false" />
              </choices>
            </zing:interactions>

            Let me know if you have questions!
        """)
        result = parse_interactions_response(text)
        assert len(result.choice_sets) == 1
        cs = result.choice_sets[0]
        assert cs.message == "Which database should we use?"
        assert "persistence" in cs.explanation
        assert len(cs.choices) == 2
        assert cs.choices[0].label == "PostgreSQL"
        assert cs.choices[0].recommended is True
        assert cs.choices[1].label == "SQLite"
        assert cs.choices[1].recommended is False

    def test_multiple_choice_sets(self) -> None:
        """Multiple <choices> blocks within one <zing:interactions>."""
        text = textwrap.dedent("""\
            <zing:interactions>
              <choices message="Question 1">
                <explanation format="markdown">Exp 1</explanation>
                <choice label="A" description="a" recommended="true" />
                <choice label="B" description="b" recommended="false" />
              </choices>
              <choices message="Question 2">
                <explanation format="markdown">Exp 2</explanation>
                <choice label="X" description="x" recommended="false" />
                <choice label="Y" description="y" recommended="true" />
              </choices>
            </zing:interactions>
        """)
        result = parse_interactions_response(text)
        assert len(result.choice_sets) == 2
        assert result.choice_sets[0].message == "Question 1"
        assert result.choice_sets[1].message == "Question 2"

    def test_missing_tag_raises(self) -> None:
        """If <zing:interactions> is not found, raise ValidationError."""
        with pytest.raises(ValidationError, match="Could not find"):
            parse_interactions_response("No XML here, just plain text.")

    def test_malformed_xml_raises(self) -> None:
        """If the XML within the tag is malformed, raise ValidationError."""
        text = "<zing:interactions><choices><unclosed></zing:interactions>"
        with pytest.raises(ValidationError, match="Malformed XML"):
            parse_interactions_response(text)

    def test_no_recommended_choice_raises(self) -> None:
        """If no choice has recommended=true, raise ValidationError."""
        text = textwrap.dedent("""\
            <zing:interactions>
              <choices message="Pick one">
                <explanation format="markdown">Pick</explanation>
                <choice label="A" description="a" recommended="false" />
                <choice label="B" description="b" recommended="false" />
              </choices>
            </zing:interactions>
        """)
        with pytest.raises(ValidationError, match="exactly one recommended"):
            parse_interactions_response(text)

    def test_multiple_recommended_raises(self) -> None:
        """If more than one choice has recommended=true, raise ValidationError."""
        text = textwrap.dedent("""\
            <zing:interactions>
              <choices message="Pick one">
                <explanation format="markdown">Pick</explanation>
                <choice label="A" description="a" recommended="true" />
                <choice label="B" description="b" recommended="true" />
              </choices>
            </zing:interactions>
        """)
        with pytest.raises(ValidationError, match="exactly one recommended"):
            parse_interactions_response(text)

    def test_empty_choices_raises(self) -> None:
        """A <choices> block with no <choice> elements should raise."""
        text = textwrap.dedent("""\
            <zing:interactions>
              <choices message="Pick one">
                <explanation format="markdown">Pick</explanation>
              </choices>
            </zing:interactions>
        """)
        with pytest.raises(ValidationError, match="at least one choice"):
            parse_interactions_response(text)

    def test_no_choices_element_raises(self) -> None:
        """<zing:interactions> with no <choices> child should raise."""
        text = "<zing:interactions></zing:interactions>"
        with pytest.raises(ValidationError, match="at least one choices"):
            parse_interactions_response(text)

    def test_surrounded_by_markdown(self) -> None:
        """Extraction works when surrounded by markdown code fences."""
        text = textwrap.dedent("""\
            Here's what I recommend:

            ```xml
            Some other XML
            ```

            <zing:interactions>
              <choices message="Approach?">
                <explanation format="markdown">Consider this.</explanation>
                <choice label="Option A" description="First" recommended="true" />
              </choices>
            </zing:interactions>

            That should work well.
        """)
        result = parse_interactions_response(text)
        assert result.choice_sets[0].choices[0].label == "Option A"

    def test_choice_description_preserved(self) -> None:
        """Choice descriptions from the description attribute are preserved."""
        text = textwrap.dedent("""\
            <zing:interactions>
              <choices message="msg">
                <explanation format="markdown">exp</explanation>
                <choice label="L" description="A detailed description" recommended="true" />
              </choices>
            </zing:interactions>
        """)
        result = parse_interactions_response(text)
        assert result.choice_sets[0].choices[0].description == "A detailed description"


# ===========================================================================
# parse_steps_response
# ===========================================================================


class TestParseStepsResponse:
    """Tests for parsing <zing:steps> from Claude response text."""

    def test_basic_extraction(self) -> None:
        """Extract a plan from a response with surrounding text."""
        text = textwrap.dedent("""\
            I've analyzed the project and here's my plan:

            <zing:steps>
              <stage label="Foundation">
                <step label="Set up project structure">
                  <instructions>Create the directory layout and initial files.</instructions>
                  <files>src/main.py
            src/utils.py</files>
                </step>
              </stage>
            </zing:steps>

            This should get us started!
        """)
        result = parse_steps_response(text)
        assert len(result.stages) == 1
        assert result.stages[0].label == "Foundation"
        assert len(result.stages[0].steps) == 1
        step = result.stages[0].steps[0]
        assert step.label == "Set up project structure"
        assert "directory layout" in step.instructions
        assert step.files == ["src/main.py", "src/utils.py"]
        assert step.done is False

    def test_multiple_stages_and_steps(self) -> None:
        """Multiple stages each with multiple steps."""
        text = textwrap.dedent("""\
            <zing:steps>
              <stage label="Stage 1">
                <step label="Step 1.1">
                  <instructions>Do first thing</instructions>
                  <files>a.py</files>
                </step>
                <step label="Step 1.2">
                  <instructions>Do second thing</instructions>
                  <files>b.py
            c.py</files>
                </step>
              </stage>
              <stage label="Stage 2">
                <step label="Step 2.1">
                  <instructions>Do third thing</instructions>
                  <files>d.py</files>
                </step>
              </stage>
            </zing:steps>
        """)
        result = parse_steps_response(text)
        assert len(result.stages) == 2
        assert result.stages[0].label == "Stage 1"
        assert len(result.stages[0].steps) == 2
        assert result.stages[1].label == "Stage 2"
        assert len(result.stages[1].steps) == 1
        assert result.stages[0].steps[1].files == ["b.py", "c.py"]

    def test_missing_tag_raises(self) -> None:
        """If <zing:steps> is not found, raise ValidationError."""
        with pytest.raises(ValidationError, match="Could not find"):
            parse_steps_response("No XML here at all.")

    def test_malformed_xml_raises(self) -> None:
        """If the XML is malformed, raise ValidationError."""
        text = "<zing:steps><stage><unclosed></zing:steps>"
        with pytest.raises(ValidationError, match="Malformed XML"):
            parse_steps_response(text)

    def test_empty_stages_raises(self) -> None:
        """<zing:steps> with no <stage> children should raise."""
        text = "<zing:steps></zing:steps>"
        with pytest.raises(ValidationError, match="at least one stage"):
            parse_steps_response(text)

    def test_step_with_empty_files(self) -> None:
        """A step with an empty <files> element produces an empty list."""
        text = textwrap.dedent("""\
            <zing:steps>
              <stage label="S1">
                <step label="No files needed">
                  <instructions>Think about it</instructions>
                  <files></files>
                </step>
              </stage>
            </zing:steps>
        """)
        result = parse_steps_response(text)
        assert result.stages[0].steps[0].files == []

    def test_step_with_no_files_element(self) -> None:
        """A step missing <files> entirely still parses with empty file list."""
        text = textwrap.dedent("""\
            <zing:steps>
              <stage label="S1">
                <step label="Planning only">
                  <instructions>Just plan</instructions>
                </step>
              </stage>
            </zing:steps>
        """)
        result = parse_steps_response(text)
        assert result.stages[0].steps[0].files == []

    def test_all_steps_default_to_not_done(self) -> None:
        """Steps parsed from Claude response should always have done=False."""
        text = textwrap.dedent("""\
            <zing:steps>
              <stage label="S1">
                <step label="Step A">
                  <instructions>Do A</instructions>
                  <files>a.py</files>
                </step>
                <step label="Step B">
                  <instructions>Do B</instructions>
                  <files>b.py</files>
                </step>
              </stage>
            </zing:steps>
        """)
        result = parse_steps_response(text)
        for stage in result.stages:
            for step in stage.steps:
                assert step.done is False

    def test_instructions_with_markdown(self) -> None:
        """Instructions can contain markdown formatting."""
        text = textwrap.dedent("""\
            <zing:steps>
              <stage label="S1">
                <step label="Rich instructions">
                  <instructions>Use **bold** and *italic* and `code`.</instructions>
                  <files>readme.md</files>
                </step>
              </stage>
            </zing:steps>
        """)
        result = parse_steps_response(text)
        assert "**bold**" in result.stages[0].steps[0].instructions


# ===========================================================================
# parse_audit_response
# ===========================================================================


class TestParseAuditResponse:
    """Tests for parsing <zing:audit> from Claude response text."""

    def test_basic_extraction(self) -> None:
        """Extract audit groups from text with surrounding prose."""
        text = textwrap.dedent("""\
            I've grouped the files for audit:

            <zing:audit>
              <group>src/main.py
            src/utils.py</group>
              <group>tests/test_main.py</group>
            </zing:audit>

            These groups are organized by module.
        """)
        result = parse_audit_response(text)
        assert len(result) == 2
        assert result[0].files == ["src/main.py", "src/utils.py"]
        assert result[1].files == ["tests/test_main.py"]

    def test_single_group(self) -> None:
        """A single group with multiple files."""
        text = textwrap.dedent("""\
            <zing:audit>
              <group>a.py
            b.py
            c.py</group>
            </zing:audit>
        """)
        result = parse_audit_response(text)
        assert len(result) == 1
        assert result[0].files == ["a.py", "b.py", "c.py"]

    def test_multiple_groups(self) -> None:
        """Multiple groups each with one file."""
        text = textwrap.dedent("""\
            <zing:audit>
              <group>alpha.py</group>
              <group>beta.py</group>
              <group>gamma.py</group>
            </zing:audit>
        """)
        result = parse_audit_response(text)
        assert len(result) == 3
        assert result[0].files == ["alpha.py"]
        assert result[1].files == ["beta.py"]
        assert result[2].files == ["gamma.py"]

    def test_missing_tag_raises(self) -> None:
        """If <zing:audit> is not found, raise ValidationError."""
        with pytest.raises(ValidationError, match="Could not find"):
            parse_audit_response("Just some text, no XML.")

    def test_malformed_xml_raises(self) -> None:
        """If the XML is malformed, raise ValidationError."""
        text = "<zing:audit><group><unclosed></zing:audit>"
        with pytest.raises(ValidationError, match="Malformed XML"):
            parse_audit_response(text)

    def test_no_groups_raises(self) -> None:
        """<zing:audit> with no <group> children should raise."""
        text = "<zing:audit></zing:audit>"
        with pytest.raises(ValidationError, match="at least one group"):
            parse_audit_response(text)

    def test_whitespace_handling(self) -> None:
        """Extra whitespace around file names is stripped."""
        text = textwrap.dedent("""\
            <zing:audit>
              <group>
                file1.py
                file2.py
              </group>
            </zing:audit>
        """)
        result = parse_audit_response(text)
        assert result[0].files == ["file1.py", "file2.py"]

    def test_empty_group_text_produces_empty_files(self) -> None:
        """A group with only whitespace results in empty file list.

        Note: this still counts as a group for the 'at least one' check,
        but has no files.
        """
        text = textwrap.dedent("""\
            <zing:audit>
              <group>valid.py</group>
              <group>   </group>
            </zing:audit>
        """)
        result = parse_audit_response(text)
        assert len(result) == 2
        assert result[0].files == ["valid.py"]
        assert result[1].files == []


# ===========================================================================
# ValidationError
# ===========================================================================


class TestValidationError:
    """Tests for the ValidationError exception class."""

    def test_is_exception(self) -> None:
        assert issubclass(ValidationError, Exception)

    def test_message(self) -> None:
        err = ValidationError("something went wrong")
        assert str(err) == "something went wrong"

    def test_raise_and_catch(self) -> None:
        with pytest.raises(ValidationError):
            raise ValidationError("test")
