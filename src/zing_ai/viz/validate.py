"""Validate graph.json against the schema and cross-reference rules.

Returns structured issues that Claude can act on (the MCP gate re-emits
them; the CLI prints them).
"""

from __future__ import annotations

import difflib
import json
from collections.abc import Iterable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import jsonschema

SCHEMA_PATH = Path(__file__).parent / "schema" / "graph.schema.json"


@dataclass
class ValidationIssue:
    """One validation problem found in a viz JSON, with location + recovery hints."""

    pointer: str
    message: str
    available: list[str] = field(default_factory=list)
    suggestion: str | None = None

    def format(self, file_path: str) -> str:
        lines = [f"{file_path}:{self.pointer}: {self.message}"]
        if self.available:
            lines.append(f"  available: {', '.join(self.available)}")
        if self.suggestion:
            lines.append(f'  suggestion: did you mean "{self.suggestion}"?')
        return "\n".join(lines)


def validate(graph: dict[str, Any]) -> list[ValidationIssue]:
    """Return all validation issues. Empty list = valid."""
    issues: list[ValidationIssue] = []
    issues.extend(_validate_schema(graph))
    if not issues:
        issues.extend(_validate_xrefs(graph))
        issues.extend(_validate_uniqueness(graph))
    return issues


def _validate_schema(graph: dict[str, Any]) -> Iterable[ValidationIssue]:
    schema = json.loads(SCHEMA_PATH.read_text())
    validator = jsonschema.Draft202012Validator(schema)
    for err in validator.iter_errors(graph):
        ptr = "/" + "/".join(str(p) for p in err.absolute_path)
        yield ValidationIssue(pointer=ptr, message=err.message)


def _validate_xrefs(graph: dict[str, Any]) -> Iterable[ValidationIssue]:
    steps_by_num = {s["step"]: s for s in graph["steps"]}

    for i, cf in enumerate(graph.get("cross_flows", [])):
        for end in ("from", "to"):
            step_num = cf[f"{end}_step"]
            node_id = cf[f"{end}_node"]
            step = steps_by_num.get(step_num)
            if step is None:
                yield ValidationIssue(
                    pointer=f"/cross_flows/{i}/{end}_step",
                    message=f"step {step_num} does not exist",
                    available=[str(s["step"]) for s in graph["steps"]],
                )
                continue
            node_ids = [n["id"] for n in step["nodes"]]
            if node_id not in node_ids:
                hint = difflib.get_close_matches(node_id, node_ids, n=1, cutoff=0.6)
                yield ValidationIssue(
                    pointer=f"/cross_flows/{i}/{end}_node",
                    message=f'step {step_num} has no node with id "{node_id}"',
                    available=node_ids,
                    suggestion=hint[0] if hint else None,
                )

    kinds = set((graph.get("kinds") or {}).keys())
    if kinds:
        for i, cf in enumerate(graph.get("cross_flows", [])):
            if cf["kind"] not in kinds:
                hint = difflib.get_close_matches(cf["kind"], list(kinds), n=1, cutoff=0.5)
                yield ValidationIssue(
                    pointer=f"/cross_flows/{i}/kind",
                    message=f'unknown kind "{cf["kind"]}"',
                    available=sorted(kinds),
                    suggestion=hint[0] if hint else None,
                )

    for si, step in enumerate(graph["steps"]):
        node_ids = {n["id"] for n in step["nodes"]}
        for ei, edge in enumerate(step.get("edges", [])):
            for end in ("from", "to"):
                if edge[end] not in node_ids:
                    hint = difflib.get_close_matches(edge[end], list(node_ids), n=1, cutoff=0.6)
                    yield ValidationIssue(
                        pointer=f"/steps/{si}/edges/{ei}/{end}",
                        message=(f'step {step["step"]} has no node with id "{edge[end]}"'),
                        available=sorted(node_ids),
                        suggestion=hint[0] if hint else None,
                    )


def _validate_uniqueness(graph: dict[str, Any]) -> Iterable[ValidationIssue]:
    seen: dict[int, int] = {}
    for i, step in enumerate(graph["steps"]):
        n = step["step"]
        if n in seen:
            yield ValidationIssue(
                pointer=f"/steps/{i}/step",
                message=f"duplicate step number {n} (also at /steps/{seen[n]})",
            )
        seen[n] = i
    seen_id: dict[str, int] = {}
    for i, step in enumerate(graph["steps"]):
        sid = step["id"]
        if sid in seen_id:
            yield ValidationIssue(
                pointer=f"/steps/{i}/id",
                message=f'duplicate step id "{sid}" (also at /steps/{seen_id[sid]})',
            )
        seen_id[sid] = i
    for si, step in enumerate(graph["steps"]):
        seen_node: dict[str, int] = {}
        for ni, node in enumerate(step["nodes"]):
            nid = node["id"]
            if nid in seen_node:
                yield ValidationIssue(
                    pointer=f"/steps/{si}/nodes/{ni}/id",
                    message=(
                        f'duplicate node id "{nid}" in step {step["step"]} '
                        f"(also at /steps/{si}/nodes/{seen_node[nid]})"
                    ),
                )
            seen_node[nid] = ni
